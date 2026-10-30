package objects

// Worker maintenance jobs: orphan GC, hash verify, cold→S3 migration,
// antivirus scanner interface.
//
// Worker 维护任务：孤儿对象 GC、哈希校验、冷数据迁移 S3、杀毒接口。

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
)

// Scanner is the antivirus plugin interface. Implementations can wrap
// ClamAV (clamd INSTREAM), a file-scan SaaS, or stay the builtin Noop.
type Scanner interface {
	// Scan returns "" when clean, or a signature name when detected.
	Scan(ctx context.Context, path string) (detection string, err error)
}

// NoopScanner passes everything — default until a real AV is wired.
type NoopScanner struct{}

func (NoopScanner) Scan(context.Context, string) (string, error) { return "", nil }

// ScanObject runs the AV plugin over an object's local bytes. S3 objects
// stream to a temp file first. Detection quarantines: status→'deleted'
// (row stays for audit; file removed).
func (s *Store) ScanObject(ctx context.Context, id uuid.UUID, sc Scanner) (string, error) {
	o, err := s.Peek(ctx, id)
	if err != nil {
		return "", err
	}
	if o.Status != "ready" {
		return "", nil
	}
	var path string
	if o.Location == "local" {
		path = s.path(o.SHA256)
	} else {
		rc, err := s.openS3(ctx, o)
		if err != nil {
			return "", err
		}
		defer func() { _ = rc.Close() }()
		tmp, err := os.CreateTemp("", "avscan-*")
		if err != nil {
			return "", err
		}
		defer func() { _ = os.Remove(tmp.Name()); _ = tmp.Close() }()
		if _, err := io.Copy(tmp, rc); err != nil {
			return "", err
		}
		path = tmp.Name()
	}
	det, err := sc.Scan(ctx, path)
	if err != nil {
		return "", err
	}
	if det != "" {
		_, _ = s.pool.Exec(ctx, `UPDATE objects SET status='deleted' WHERE id=$1`, id)
		if o.Location == "local" {
			_ = os.Remove(s.path(o.SHA256))
		}
		log.Printf("objects: QUARANTINE %s (%s) — %s", id, o.Name, det)
	}
	return det, nil
}

// VerifyHash re-hashes the stored bytes and compares to objects.sha256.
// Mismatch → status stays but caller logs an integrity event.
func (s *Store) VerifyHash(ctx context.Context, id uuid.UUID) (bool, error) {
	o, err := s.Peek(ctx, id)
	if err != nil {
		return false, err
	}
	rc, err := s.Open(ctx, o)
	if err != nil {
		return false, err
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		return false, err
	}
	var got [32]byte
	copy(got[:], h.Sum(nil))
	return got == o.SHA256, nil
}

// EnqueueVerifyAll fans a verify task out to every ready object (batch GC pass).
func (s *Store) EnqueueVerifyAll(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id FROM objects WHERE status='ready' ORDER BY created_at LIMIT 10000`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return n, err
		}
		if err := s.Enqueue(ctx, "verify", map[string]any{"object_id": id.String()}); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

// OrphanGC deletes objects that are pending for >7d (aborted uploads)
// or ready-but-unreferenced for >7d (no attachment_refs, no mail_parts).
// Returns count marked deleted.
func (s *Store) OrphanGC(ctx context.Context) (int, error) {
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	rows, err := s.pool.Query(ctx,
		`SELECT o.id, o.sha256, o.location FROM objects o
		 WHERE o.status<>'deleted' AND o.created_at < $1
		   AND (o.status='pending'
		        OR (NOT EXISTS (SELECT 1 FROM attachment_refs a WHERE a.object_id=o.id)
		        AND NOT EXISTS (SELECT 1 FROM mail_attachments p WHERE p.object_id=o.id)))`,
		cutoff)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id uuid.UUID
		var sha []byte
		var loc string
		if err := rows.Scan(&id, &sha, &loc); err != nil {
			return n, err
		}
		if _, err := s.pool.Exec(ctx,
			`UPDATE objects SET status='deleted' WHERE id=$1`, id); err == nil {
			if loc == "local" && len(sha) == 32 {
				var fixed [32]byte
				copy(fixed[:], sha)
				_ = os.Remove(s.path(fixed))
			}
			n++
		}
	}
	return n, rows.Err()
}

// MigrateCold moves local objects untouched for `older` to S3.
func (s *Store) MigrateCold(ctx context.Context, older time.Duration, limit int) (int, error) {
	if s.s3 == nil {
		return 0, ErrS3Disabled
	}
	cutoff := time.Now().Add(-older)
	rows, err := s.pool.Query(ctx,
		`SELECT id, sha256, size_bytes FROM objects
		 WHERE status='ready' AND location='local'
		   AND COALESCE(last_accessed_at, created_at) < $1
		 ORDER BY created_at LIMIT $2`, cutoff, limit)
	if err != nil {
		return 0, err
	}
	type cand struct {
		id   uuid.UUID
		sha  [32]byte
		size int64
	}
	var list []cand
	for rows.Next() {
		var c cand
		var sha []byte
		if err := rows.Scan(&c.id, &sha, &c.size); err != nil {
			rows.Close()
			return 0, err
		}
		copy(c.sha[:], sha)
		list = append(list, c)
	}
	rows.Close()
	n := 0
	for _, c := range list {
		f, err := os.Open(s.path(c.sha))
		if err != nil {
			continue
		}
		key := s.s3.key(c.sha)
		_, err = s.s3.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(s.s3.cfg.Bucket), Key: aws.String(key), Body: f,
		})
		_ = f.Close()
		if err != nil {
			return n, fmt.Errorf("s3 put %s: %w", c.id, err)
		}
		// Update row + segment atomically, then remove the local file.
		err = func() error {
			tx, err := s.pool.Begin(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx,
				`UPDATE objects SET location='s3' WHERE id=$1`, c.id); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx,
				`UPDATE object_segments SET storage_key=$2 WHERE object_id=$1`, c.id, key); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}()
		if err != nil {
			return n, err
		}
		_ = os.Remove(s.path(c.sha))
		n++
	}
	return n, nil
}

var ErrS3Only = errors.New("operation requires s3")
