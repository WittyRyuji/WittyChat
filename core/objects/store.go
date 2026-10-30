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
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return nil, false, err
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return nil, false, err
	}
	var convArg any // uuid.Nil → NULL (non-conv objects: mail/avatar/…)
	if convID != uuid.Nil {
		obj.ConvID = &convID
		convArg = convID
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO objects (id, owner_user, conv_id, purpose, name, size_bytes, sha256, mime, location, status)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'local','ready')`,
			obj.ID, owner, convArg, purpose, name, n, sum[:], mime); err != nil {
			return err
		}
		rel, _ := filepath.Rel(s.dataDir, dst)
		_, err := tx.Exec(ctx,
			`INSERT INTO object_segments (object_id, seg_index, size_bytes, storage_key) VALUES ($1,0,$2,$3)`,
			obj.ID, n, rel)
		return err
	})
	if err != nil {
		_ = os.Remove(dst)
		return nil, false, err
	}
	return obj, false, nil
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
func (s *Store) UsedBytes(ctx context.Context) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(size_bytes),0) FROM objects WHERE status='ready'`).Scan(&n)
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

// RemoveFiles deletes an object's payload + thumbnail best-effort.
func (s *Store) RemoveFiles(o *Object) {
	_ = os.Remove(s.path(o.ID, o.SHA256))
	_ = os.Remove(s.legacyPath(o.SHA256))
	_ = os.Remove(s.ThumbPath(o.ID))
	_ = os.Remove(s.ThumbPathLegacy(o.ID))
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
