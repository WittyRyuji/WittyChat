// Package objects is the unified object store: metadata in PG, bytes on
// local disk (P0) with an s3 location reserved for later. Dedup by SHA-256
// gives instant-upload for free; downloads are authorized by conv membership.
//
// objects 包是统一对象存储：元数据在 PG，字节在本地磁盘（P0），
// 预留 s3 位置。SHA-256 去重即秒传；下载按会话成员鉴权。
package objects

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound  = errors.New("object not found")
	ErrForbidden = errors.New("no access to object")
	ErrTooLarge  = errors.New("object exceeds size limit")
	ErrNotReady  = errors.New("object still uploading")
)

// MaxDirectUpload — P0 direct upload ceiling. S3 multipart comes later.
// P0 直传上限；S3 multipart 后续补。
const MaxDirectUpload = 5 << 20 // 5 MiB

type Object struct {
	ID        uuid.UUID
	Owner     uuid.UUID
	ConvID    *uuid.UUID
	Purpose   string
	Name      string
	Size      int64
	SHA256    [32]byte
	Mime      string
	Location  string
	Status    string
	CreatedAt time.Time
}

type Store struct {
	pool    *pgxpool.Pool
	dataDir string // <dataDir>/objects/ab/<sha256hex>
	s3      *S3    // nil → local only / 未启用 S3
}

func NewStore(pool *pgxpool.Pool, dataDir string) (*Store, error) {
	s := &Store{pool: pool, dataDir: dataDir}
	for _, d := range []string{"objects", "thumbs", "tmp"} {
		if err := os.MkdirAll(filepath.Join(dataDir, d), 0o750); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// dateDir derives YYYY/MM/DD from a uuidv7's embedded ms timestamp — the
// object's creation day, so files partition by date and stay admin-scannable.
// dateDir 从 uuidv7 内嵌毫秒时间戳推出 YYYY/MM/DD —— 即对象创建日期，
// 文件按日期分区，便于后台按天管理。
func dateDir(id uuid.UUID) string {
	sec, _ := id.Time().UnixTime()
	return time.Unix(sec, 0).UTC().Format("2006/01/02")
}

func (s *Store) path(id uuid.UUID, sha [32]byte) string {
	h := hex.EncodeToString(sha[:])
	return filepath.Join(s.dataDir, "objects", dateDir(id), h)
}

// legacyPath is the pre-date-partitioned layout (<dataDir>/objects/ab/<sha>).
// legacyPath 是日期分区前的旧布局，读旧对象时兜底。
func (s *Store) legacyPath(sha [32]byte) string {
	h := hex.EncodeToString(sha[:])
	return filepath.Join(s.dataDir, "objects", h[:2], h)
}

// localFile resolves the on-disk path for an object — date-partitioned
// layout first, legacy layout fallback for pre-partition uploads.
// localFile 解析对象落盘路径——先日期分区布局，旧上传走旧布局兜底。
func (s *Store) localFile(id uuid.UUID, sha [32]byte) string {
	p := s.path(id, sha)
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return s.legacyPath(sha)
}

// Querier abstracts pool/tx.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Put streams r to disk hashing as it goes (never buffers whole file),
// dedups on sha256, registers the object. Returns (object, deduplicated).
//
// Put 流式写盘同时算哈希（不整读内存），按 sha256 秒传去重后登记对象。
func (s *Store) Put(ctx context.Context, owner, convID uuid.UUID, purpose, name, mime string,
	r io.Reader, maxBytes int64,
) (*Object, bool, error) {
	tmp, err := os.CreateTemp(filepath.Join(s.dataDir, "tmp"), "up-*")
	if err != nil {
		return nil, false, err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }() // no-op once renamed

	h := sha256.New()
	n, err := io.Copy(tmp, io.TeeReader(io.LimitReader(r, maxBytes+1), h))
	if err != nil {
		_ = tmp.Close()
		return nil, false, err
	}
	_ = tmp.Close()
	if n > maxBytes {
		return nil, false, ErrTooLarge
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))

	// Dedup: same sha256 by the same owner into the same conv → reuse the row.
	// Cross-owner/conv uploads need their own row for correct ownership + ACL;
	// identical bytes still share the sha-named file on disk.
	// 去重仅限同主同会话；跨主/跨会话各自建行以保归属与 ACL，磁盘文件仍共享。
	var existing Object
	var sha []byte
	err = s.pool.QueryRow(ctx,
		`SELECT id, owner_user, conv_id, purpose, name, size_bytes, sha256, mime, location, status, created_at
		 FROM objects WHERE sha256=$1 AND owner_user=$2 AND conv_id IS NOT DISTINCT FROM $3::uuid
		   AND status<>'deleted' LIMIT 1`, sum[:], owner, nilUUID(convID)).
		Scan(&existing.ID, &existing.Owner, &existing.ConvID, &existing.Purpose, &existing.Name,
			&existing.Size, &sha, &existing.Mime, &existing.Location, &existing.Status, &existing.CreatedAt)
	if err == nil {
		copy(existing.SHA256[:], sha)
		return &existing, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, err
	}

	obj := &Object{
		ID: uuid.Must(uuid.NewV7()), Owner: owner, Purpose: purpose,
		Name: name, Size: n, SHA256: sum, Mime: mime, Location: "local", Status: "ready",
	}
	dst := s.path(obj.ID, sum)
	rel, _ := filepath.Rel(s.dataDir, dst)
	var convArg any // uuid.Nil → NULL (non-conv objects: mail/avatar/…)
	if convID != uuid.Nil {
		obj.ConvID = &convID
		convArg = convID
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Serialize reference creation vs. reference-aware deletion on the
		// storage key — a GC can't unlink the file between our rename and
		// our segment insert, and it can't delete a key we're about to
		// reference. / 按存储 key 串行化"建引用"与"查引用后删除"——
		// GC 无法在落盘与插引用之间删文件，也不会删我们正要引用的 key。
		if _, err := tx.Exec(ctx,
			`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, rel); err != nil {
			return err
		}
		// Double-checked dedup inside the lock: a concurrent identical
		// upload that passed the outside check commits first → reuse its
		// row instead of dying on the unique index and deleting the file
		// it just published. / 锁内去重复查：并发同内容上传若已先提交，
		// 复用其行——而不是撞唯一索引后误删它刚发布的文件。
		err := tx.QueryRow(ctx,
			`SELECT id, owner_user, conv_id, purpose, name, size_bytes, sha256, mime, location, status, created_at
			 FROM objects WHERE sha256=$1 AND owner_user=$2 AND conv_id IS NOT DISTINCT FROM $3::uuid
			   AND status<>'deleted' LIMIT 1`, sum[:], owner, nilUUID(convID)).
			Scan(&existing.ID, &existing.Owner, &existing.ConvID, &existing.Purpose, &existing.Name,
				&existing.Size, &sha, &existing.Mime, &existing.Location, &existing.Status, &existing.CreatedAt)
		if err == nil {
			copy(existing.SHA256[:], sha)
			return errDedup
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return err
		}
		// Publish the bytes idempotently: same sha ⇒ same content, so an
		// existing file is already correct — never overwrite or remove it.
		// 幂等发布字节：同 sha 即同内容，已存在的文件就是对的——不覆盖不删除。
		if _, err := os.Stat(dst); errors.Is(err, os.ErrNotExist) {
			if err := os.Rename(tmpPath, dst); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO objects (id, owner_user, conv_id, purpose, name, size_bytes, sha256, mime, location, status)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'local','ready')`,
			obj.ID, owner, convArg, purpose, name, n, sum[:], mime); err != nil {
			return err
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO object_segments (object_id, seg_index, size_bytes, storage_key) VALUES ($1,0,$2,$3)`,
			obj.ID, n, rel)
		return err
	})
	if err != nil {
		if errors.Is(err, errDedup) {
			return &existing, true, nil
		}
		// Rolled back after the rename? Drop our bytes ONLY if nothing
		// else references the key — another object may legitimately share
		// it. / 落盘后回滚：仅当无人引用该 key 才删——文件可能是共享的。
		_ = s.dropKeyIfOrphaned(ctx, rel, "local")
		return nil, false, err
	}
	return obj, false, nil
}

// errDedup marks "an identical row was committed by a racing upload"
// inside the advisory-locked Put transaction. / 标记锁内去重命中：
// 并发相同上传已提交，复用其行。
var errDedup = errors.New("dedup: committed by racing upload")

// ReplaceBytes streams new content into the store and re-points an
// existing object at it — the in-place "save" path for office editing.
// The new file lands locally under the object's own date partition at its
// new sha (location flips to local even if the object had been migrated
// to S3). The prior bytes are never deleted: their location/key is
// recorded in object_versions for audit and restore. The sha256 unique
// index means a pathological collision (edited bytes identical to another
// live object) is reported rather than absorbed. Stale thumbnails go.
//
// ReplaceBytes 流式写入新内容并把既有对象重新指过去——在线编辑的
// 原位保存路径。新文件按新 sha 落在本地日期分区（即使原对象已冷迁到 S3
// 也切回 local）。旧字节永不删除：其位置/键记入 object_versions 供审计与
// 恢复。sha256 唯一索引下的病态冲突上报而非吞掉。旧缩略图清除。
func (s *Store) ReplaceBytes(ctx context.Context, id uuid.UUID, r io.Reader, mime string, maxBytes int64, by *uuid.UUID) (*Object, error) {
	o, err := s.Peek(ctx, id)
	if err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(filepath.Join(s.dataDir, "tmp"), "edit-*")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	h := sha256.New()
	n, err := io.Copy(tmp, io.TeeReader(io.LimitReader(r, maxBytes+1), h))
	if err != nil {
		_ = tmp.Close()
		return nil, err
	}
	_ = tmp.Close()
	if n > maxBytes {
		return nil, ErrTooLarge
	}
	if n == 0 {
		return nil, errors.New("empty body")
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	dst := s.path(o.ID, sum)
	rel, _ := filepath.Rel(s.dataDir, dst)
	if mime == "" {
		mime = o.Mime
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Same serialization as Put: the file may share its storage key
		// with another object of identical content — cleanup must consult
		// references, never the sha alone. / 与 Put 同一串行域：该存储
		// key 可能被同内容对象共享——清理必须查引用而非按 sha 直删。
		if _, err := tx.Exec(ctx,
			`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, rel); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return err
		}
		if _, err := os.Stat(dst); errors.Is(err, os.ErrNotExist) {
			if err := os.Rename(tmpPath, dst); err != nil {
				return err
			}
		}
		// Snapshot the current version before re-pointing, with the full
		// segment list so old bytes stay restorable/GC-able. 先记录旧版本，
		// 含全部分段，旧字节才可恢复、GC 才能删对文件。
		if _, err := tx.Exec(ctx, `
			INSERT INTO object_versions
			  (object_id, version, sha256, size_bytes, mime, location, storage_key, segments, replaced_by)
			SELECT o.id,
			       COALESCE((SELECT max(v.version) FROM object_versions v WHERE v.object_id=o.id), 0) + 1,
			       o.sha256, o.size_bytes, COALESCE(o.mime,''), o.location,
			       COALESCE((SELECT g.storage_key FROM object_segments g
			                 WHERE g.object_id=o.id AND g.seg_index=0), ''),
			       (SELECT COALESCE(jsonb_agg(
			          jsonb_build_object('seg', g.seg_index, 'size', g.size_bytes, 'key', g.storage_key)
			          ORDER BY g.seg_index), '[]'::jsonb)
			        FROM object_segments g WHERE g.object_id=o.id),
			       $2
			FROM objects o WHERE o.id=$1 AND o.status<>'deleted'`, id, by); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE objects SET sha256=$2, size_bytes=$3, mime=$4, location='local', segment_count=1
			 WHERE id=$1 AND status<>'deleted'`,
			id, sum[:], n, mime); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM object_segments WHERE object_id=$1 AND seg_index>0`, id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO object_segments (object_id, seg_index, size_bytes, storage_key)
			VALUES ($1, 0, $2, $3)
			ON CONFLICT (object_id, seg_index) DO UPDATE
			  SET size_bytes=EXCLUDED.size_bytes, storage_key=EXCLUDED.storage_key`,
			id, n, rel)
		return err
	})
	if err != nil {
		// Rolled back (e.g. sha collision on the scoped unique index):
		// remove our file only when unreferenced — an identical-content
		// object may share the same storage key. / 回滚（如唯一索引
		// 冲突）：仅当 key 无引用才删文件——同内容对象可能共享它。
		_ = s.dropKeyIfOrphaned(ctx, rel, "local")
		return nil, err
	}
	_ = os.Remove(s.ThumbPath(id)) // stale preview must not survive the edit / 旧缩略图不留
	o.SHA256, o.Size, o.Mime, o.Location = sum, n, mime, "local"
	return o, nil
}

// Get returns metadata if the user may access the object:
// owner, or member of the object's conversation.
// Get 返回元数据，前提是有权访问：上传者本人或对象所属会话的成员。
func (s *Store) Get(ctx context.Context, id, userID uuid.UUID) (*Object, error) {
	var o Object
	var sha []byte
	err := s.pool.QueryRow(ctx,
		`SELECT id, owner_user, conv_id, purpose, name, size_bytes, sha256, mime, location, status, created_at
		 FROM objects WHERE id=$1 AND status<>'deleted'`, id).
		Scan(&o.ID, &o.Owner, &o.ConvID, &o.Purpose, &o.Name, &o.Size,
			&sha, &o.Mime, &o.Location, &o.Status, &o.CreatedAt)
	copy(o.SHA256[:], sha)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if o.Owner == userID {
		return &o, nil
	}
	if o.ConvID != nil {
		var one int
		if err := s.pool.QueryRow(ctx,
			`SELECT 1 FROM conv_members WHERE conv_id=$1 AND user_id=$2 AND left_at IS NULL`,
			*o.ConvID, userID).Scan(&one); err == nil {
			return &o, nil
		}
	}
	return nil, ErrForbidden
}

// Open streams object bytes for download (local or s3 by location).
func (s *Store) Open(ctx context.Context, o *Object) (io.ReadCloser, error) {
	if o.Status != "ready" {
		return nil, ErrNotReady
	}
	if o.Location == "s3" {
		if s.s3 == nil {
			return nil, ErrNotReady
		}
		return s.openS3(ctx, o)
	}
	f, err := os.Open(s.path(o.ID, o.SHA256))
	if errors.Is(err, os.ErrNotExist) {
		f, err = os.Open(s.legacyPath(o.SHA256)) // pre-partition layout / 旧布局兜底
	}
	if err != nil {
		return nil, err
	}
	// Best-effort access-time touch for the cold-migration signal.
	_, _ = s.pool.Exec(ctx, `UPDATE objects SET last_accessed_at=now() WHERE id=$1`, o.ID)
	return f, nil
}

// Peek loads metadata with no ACL check — for signed-URL flows where
// possession of the signature is itself the authorization.
func (s *Store) Peek(ctx context.Context, id uuid.UUID) (*Object, error) {
	var o Object
	var sha []byte
	err := s.pool.QueryRow(ctx,
		`SELECT id, owner_user, conv_id, purpose, name, size_bytes, sha256, mime,
		        location, status, created_at
		 FROM objects WHERE id=$1`, id).
		Scan(&o.ID, &o.Owner, &o.ConvID, &o.Purpose, &o.Name, &o.Size,
			&sha, &o.Mime, &o.Location, &o.Status, &o.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	copy(o.SHA256[:], sha)
	return &o, nil
}

// UsedBytes sums ready-object sizes for the workspace quota check.
// Retained version history counts too — otherwise an editor could grow
// storage without limit by re-saving. 配额统计含保留的历史版本，
// 否则反复保存可绕过配额膨胀存储。
func (s *Store) UsedBytes(ctx context.Context) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `
		SELECT (SELECT COALESCE(SUM(size_bytes),0) FROM objects WHERE status='ready')
		     + (SELECT COALESCE(SUM(size_bytes),0) FROM object_versions)`).Scan(&n)
	return n, err
}

// LinkAttachments binds objects to a message op (attachment_refs).
func (s *Store) LinkAttachments(ctx context.Context, convID uuid.UUID, opSeq int64, objectIDs []uuid.UUID) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		for _, oid := range objectIDs {
			if _, err := tx.Exec(ctx,
				`INSERT INTO attachment_refs (conv_id, op_seq, object_id) VALUES ($1,$2,$3)
				 ON CONFLICT DO NOTHING`, convID, opSeq, oid); err != nil {
				return err
			}
		}
		return nil
	})
}

// AttachmentsOf lists objects linked to an op.
func (s *Store) AttachmentsOf(ctx context.Context, convID uuid.UUID, opSeq int64) ([]*Object, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT o.id, o.name, o.size_bytes, o.mime FROM attachment_refs r
		 JOIN objects o ON o.id = r.object_id
		 WHERE r.conv_id=$1 AND r.op_seq=$2 AND o.status<>'deleted'`, convID, opSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Object
	for rows.Next() {
		o := &Object{}
		if err := rows.Scan(&o.ID, &o.Name, &o.Size, &o.Mime); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ThumbPath is where thumbnails live — date-partitioned like objects.
func (s *Store) ThumbPath(id uuid.UUID) string {
	return filepath.Join(s.dataDir, "thumbs", dateDir(id), id.String()+".jpg")
}

// ThumbPathLegacy is the pre-partition thumbs/<id>.jpg — read fallback.
func (s *Store) ThumbPathLegacy(id uuid.UUID) string {
	return filepath.Join(s.dataDir, "thumbs", id.String()+".jpg")
}

// WriteThumb stores an externally-produced thumbnail (client-side resize).
// WriteThumb 保存外部生成的缩略图（客户端压缩后上传）。
func (s *Store) WriteThumb(id uuid.UUID, r io.Reader) error {
	p := s.ThumbPath(id)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	//nolint:gosec // path derived from parsed uuid under fixed data dir
	f, err := os.Create(p)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(f, r)
	return err
}

// AdminObject is one row of the admin storage listing.
type AdminObject struct {
	ID        uuid.UUID  `json:"id"`
	Owner     uuid.UUID  `json:"owner"`
	OwnerMail string     `json:"owner_email"`
	Name      string     `json:"name"`
	Purpose   string     `json:"purpose"`
	Mime      string     `json:"mime"`
	Size      int64      `json:"size_bytes"`
	ConvID    *uuid.UUID `json:"conv_id,omitempty"`
	Status    string     `json:"status"`
	CreatedAt time.Time  `json:"created_at"`
}

// AdminList returns objects for storage management — filterable by owner
// (uuid.Nil = all) and created_at range [from,to).
// AdminList 返回存储管理用的对象清单——可按属主与创建日期过滤。
func (s *Store) AdminList(ctx context.Context, owner uuid.UUID, from, to time.Time) ([]AdminObject, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT o.id, o.owner_user, u.email, o.name, o.purpose, o.mime, o.size_bytes,
		        o.conv_id, o.status, o.created_at
		 FROM objects o JOIN users u ON u.id = o.owner_user
		 WHERE ($1::uuid IS NULL OR o.owner_user=$1)
		   AND o.created_at >= $2 AND o.created_at < $3
		   AND o.status <> 'deleted'
		 ORDER BY o.created_at DESC LIMIT 1000`,
		nilUUID(owner), from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminObject{}
	for rows.Next() {
		var o AdminObject
		if err := rows.Scan(&o.ID, &o.Owner, &o.OwnerMail, &o.Name, &o.Purpose,
			&o.Mime, &o.Size, &o.ConvID, &o.Status, &o.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// AdminPurge soft-deletes objects of an owner created in [from,to), returns
// the affected objects so the caller can remove files/thumbs.
// AdminPurge 软删某属主在指定日期区间内的对象，返回受影响对象供调用方清理文件。
func (s *Store) AdminPurge(ctx context.Context, owner uuid.UUID, from, to time.Time) ([]Object, error) {
	var deleted []Object
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`UPDATE objects SET status='deleted'
			 WHERE ($1::uuid IS NULL OR owner_user=$1)
			   AND created_at >= $2 AND created_at < $3 AND status<>'deleted'
			 RETURNING id, owner_user, conv_id, purpose, name, size_bytes, sha256, mime, location, status, created_at`,
			nilUUID(owner), from, to)
		if err != nil {
			return err
		}
		defer rows.Close()
		ids := []uuid.UUID{}
		for rows.Next() {
			var o Object
			var sha []byte
			if err := rows.Scan(&o.ID, &o.Owner, &o.ConvID, &o.Purpose, &o.Name,
				&o.Size, &sha, &o.Mime, &o.Location, &o.Status, &o.CreatedAt); err != nil {
				return err
			}
			copy(o.SHA256[:], sha)
			deleted = append(deleted, o)
			ids = append(ids, o.ID)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		// Detach references so dead objects don't dangle.
		// 解除引用，避免悬挂指针（头像/邮件附件/会话附件）。
		if _, err := tx.Exec(ctx, `UPDATE users SET avatar_object_id=NULL WHERE avatar_object_id=ANY($1)`, ids); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM mail_attachments WHERE object_id=ANY($1)`, ids); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM attachment_refs WHERE object_id=ANY($1)`, ids)
		return err
	})
	return deleted, err
}

// RemoveFiles deletes a (soft-)deleted object's payload bytes and
// thumbnails. Payload keys go through the reference check — another live
// object or retained version with identical content keeps its shared
// file. Objects with no segment rows (aborted uploads) fall back to the
// path derived from their id+sha. The legacy hash-only file is removed
// only when nothing references the sha at all.
// RemoveFiles 删除（已软删）对象的负载字节与缩略图。负载 key 走引用
// 检查——同内容的在用对象或保留版本共享的文件保留。无分段行的对象
// （中断的上传）回退用 id+sha 推路径。旧哈希布局文件仅在该 sha 完全
// 无引用时才删。
func (s *Store) RemoveFiles(ctx context.Context, o *Object) {
	var keys []string
	rows, err := s.pool.Query(ctx,
		`SELECT storage_key FROM object_segments WHERE object_id=$1`, o.ID)
	if err == nil {
		for rows.Next() {
			var k string
			if rows.Scan(&k) == nil && k != "" {
				keys = append(keys, k)
			}
		}
		rows.Close()
	}
	if len(keys) == 0 && o.Location != "s3" {
		rel, _ := filepath.Rel(s.dataDir, s.path(o.ID, o.SHA256))
		keys = append(keys, rel)
	}
	for _, k := range keys {
		_ = s.dropKeyIfOrphaned(ctx, k, o.Location)
	}
	if !s.shaReferenced(ctx, o.SHA256) {
		_ = os.Remove(s.legacyPath(o.SHA256))
	}
	_ = os.Remove(s.ThumbPath(o.ID))
	_ = os.Remove(s.ThumbPathLegacy(o.ID))
}

// shaReferenced reports whether any live object or retained version uses
// this content hash — the conservative guard before removing a file from
// the legacy hash-only layout (no per-key record exists for it).
// 判断该内容哈希是否仍被在用对象或保留版本引用——删除旧哈希布局文件前
// 的保守兜底（旧布局没有按 key 的记录可查）。
func (s *Store) shaReferenced(ctx context.Context, sha [32]byte) bool {
	var ok bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM objects WHERE sha256=$1 AND status<>'deleted')
		OR EXISTS(SELECT 1 FROM object_versions WHERE sha256=$1)`, sha[:]).Scan(&ok)
	return err == nil && ok
}

// nilUUID maps uuid.Nil → nil for optional :uuid params.
func nilUUID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

// ---------- task queue / 任务队列 ----------

// Enqueue adds a background task.
func (s *Store) Enqueue(ctx context.Context, kind string, payload map[string]any) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO bg_tasks (kind, payload) VALUES ($1,$2)`, kind, payload)
	return err
}

// Task is one claimed unit of background work.
type Task struct {
	ID       int64
	Kind     string
	Payload  map[string]any
	Attempts int // attempts so far, including this claim / 已尝试次数（含本次）
}

// Claim atomically takes the oldest due task (SKIP LOCKED).
func (s *Store) Claim(ctx context.Context, worker string) (*Task, error) {
	var t Task
	err := s.pool.QueryRow(ctx, `
UPDATE bg_tasks SET status='running', locked_by=$1, locked_at=now(), attempts=attempts+1, updated_at=now()
WHERE id = (SELECT id FROM bg_tasks WHERE status='queued' AND run_after<=now()
            ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED)
RETURNING id, kind, payload, attempts`, worker).
		Scan(&t.ID, &t.Kind, &t.Payload, &t.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &t, err
}

// Done marks a task finished.
func (s *Store) Done(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE bg_tasks SET status='done', updated_at=now() WHERE id=$1`, id)
	return err
}

// Fail marks a task failed; retries with backoff until 5 attempts.
func (s *Store) Fail(ctx context.Context, id int64, attempts int, cause string) error {
	if attempts >= 5 {
		_, err := s.pool.Exec(ctx,
			`UPDATE bg_tasks SET status='failed', updated_at=now(), payload=payload||$2::jsonb WHERE id=$1`,
			id, fmt.Sprintf(`{"error":%q}`, cause))
		return err
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE bg_tasks SET status='queued', run_after=now() + ($2 * interval '1 minute'),
		   updated_at=now(), payload=payload||$3::jsonb WHERE id=$1`,
		id, attempts, fmt.Sprintf(`{"error":%q}`, cause))
	return err
}
