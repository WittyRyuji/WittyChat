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

func (s *Store) path(sha [32]byte) string {
	h := hex.EncodeToString(sha[:])
	return filepath.Join(s.dataDir, "objects", h[:2], h)
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

	// Dedup: same sha256 already exists → reuse object, drop temp.
	var existing Object
	var sha []byte
	err = s.pool.QueryRow(ctx,
		`SELECT id, owner_user, conv_id, purpose, name, size_bytes, sha256, mime, location, status, created_at
		 FROM objects WHERE sha256=$1 AND status<>'deleted' LIMIT 1`, sum[:]).
		Scan(&existing.ID, &existing.Owner, &existing.ConvID, &existing.Purpose, &existing.Name,
			&existing.Size, &sha, &existing.Mime, &existing.Location, &existing.Status, &existing.CreatedAt)
	if err == nil {
		copy(existing.SHA256[:], sha)
		return &existing, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, err
	}

	dst := s.path(sum)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return nil, false, err
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return nil, false, err
	}

	obj := &Object{
		ID: uuid.Must(uuid.NewV7()), Owner: owner, ConvID: &convID, Purpose: purpose,
		Name: name, Size: n, SHA256: sum, Mime: mime, Location: "local", Status: "ready",
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO objects (id, owner_user, conv_id, purpose, name, size_bytes, sha256, mime, location, status)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'local','ready')`,
			obj.ID, owner, convID, purpose, name, n, sum[:], mime); err != nil {
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
	f, err := os.Open(s.path(o.SHA256))
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

// ThumbPath is where the worker writes thumbnails.
func (s *Store) ThumbPath(id uuid.UUID) string {
	return filepath.Join(s.dataDir, "thumbs", id.String()+".jpg")
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
