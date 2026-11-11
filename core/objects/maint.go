package objects

// Worker maintenance jobs: orphan GC, hash verify, cold→S3 migration,
// antivirus scanner interface.
//
// Worker 维护任务：孤儿对象 GC、哈希校验、冷数据迁移 S3、杀毒接口。

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
		path = s.localFile(o.ID, o.SHA256)
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
		// Reference-aware cleanup: quarantining an infected object must
		// not unlink a file another clean object shares. / 引用感知清理：
		// 隔离感染对象不能连累共享同文件的干净对象。
		s.RemoveFiles(ctx, o)
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
	rc, err := s.openBytes(ctx, o)
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

// EnqueueVerifyAll fans a verify task out to every ready LOCAL object
// (batch GC pass). S3 objects are skipped — S3 does its own integrity
// checking and a full read costs egress; local disk bit-rot is the real
// risk. / 只对本地 ready 对象派发校验：S3 自带完整性校验且全量回读
// 消耗出口流量；本地盘位腐烂才是真正风险。
func (s *Store) EnqueueVerifyAll(ctx context.Context) (int, error) {
	// Keyset over ALL ready objects in 1000-row pages — the old LIMIT-10000
	// oldest-first pass never reached newer objects.
	// 键集翻页扫全部 ready 对象——旧的 LIMIT 10000 最老优先写法永远轮不到新对象。
	n := 0
	last := uuid.Nil
	for {
		rows, err := s.pool.Query(ctx,
			`SELECT id FROM objects WHERE status='ready' AND location='local' AND id > $1 ORDER BY id LIMIT 1000`, last)
		if err != nil {
			return n, err
		}
		got := 0
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return n, err
			}
			if err := s.Enqueue(ctx, "verify", map[string]any{"object_id": id.String()}); err != nil {
				rows.Close()
				return n, err
			}
			n++
			got++
			last = id
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return n, err
		}
		if got == 0 {
			return n, nil
		}
	}
}

// OrphanGC deletes objects that are pending for >7d (aborted uploads)
// or ready-but-unreferenced for >7d. Only "upload then attach" purposes
// are collectable; every reference kind (message, mail, HR request,
// avatar, workspace logo) keeps an object alive. Stickers live on the
// client shelf (unknown to the server) and are never collected.
// Returns count marked deleted.
//
// 孤儿 GC：pending 超 7 天，或 ready 且无任何引用超 7 天。只回收"先传后
// 附"用途；消息/邮件/HR/头像/工作区 logo 任一引用都会保活。表情存在客户
// 端表情架（服务端不可知），永不回收。
func (s *Store) OrphanGC(ctx context.Context) (int, error) {
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	rows, err := s.pool.Query(ctx,
		`SELECT o.id, o.sha256, o.location FROM objects o
		 WHERE o.status<>'deleted' AND o.created_at < $1
		   AND (o.status='pending'
		        OR (COALESCE(o.purpose,'') IN ('attachment','chat','mail','hr','doc')
		        AND NOT EXISTS (SELECT 1 FROM attachment_refs a WHERE a.object_id=o.id)
		        AND NOT EXISTS (SELECT 1 FROM mail_attachments p WHERE p.object_id=o.id)
		        AND NOT EXISTS (SELECT 1 FROM mail_messages mm WHERE mm.raw_object_id=o.id)
		AND NOT EXISTS (SELECT 1 FROM doc_objects dob WHERE dob.object_id=o.id)
		        AND NOT EXISTS (SELECT 1 FROM hr_requests h
		                        WHERE h.payload->'attachment_object_ids' ? o.id::text)
		        AND NOT EXISTS (SELECT 1 FROM users u WHERE u.avatar_object_id=o.id)
		        AND NOT EXISTS (SELECT 1 FROM workspace_settings w
		                        WHERE w.key='workspace.logo_object_id'
		                          AND w.value #>> '{}' = o.id::text)))`,
		cutoff)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id uuid.UUID
		var loc string
		var sha []byte
		if err := rows.Scan(&id, &sha, &loc); err != nil {
			return n, err
		}
		if _, err := s.pool.Exec(ctx,
			`UPDATE objects SET status='deleted' WHERE id=$1`, id); err == nil {
			// Reference-aware cleanup — the file may be shared with a
			// same-content live object. / 引用感知清理——文件可能与同内容
			// 在用对象共享。
			o := &Object{ID: id, Location: loc}
			copy(o.SHA256[:], sha)
			s.RemoveFiles(ctx, o)
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
		f, err := os.Open(s.localFile(c.id, c.sha))
		if err != nil {
			continue
		}
		key := s.s3.key(c.sha)
		var localKeys []string
		lr, lerr := s.pool.Query(ctx,
			`SELECT storage_key FROM object_segments WHERE object_id=$1`, c.id)
		if lerr == nil {
			for lr.Next() {
				var k string
				if lr.Scan(&k) == nil {
					localKeys = append(localKeys, k)
				}
			}
			lr.Close()
		}
		// Upload + re-point inside the advisory-locked section: a GC must
		// not be able to delete the fresh S3 object between PutObject and
		// the segment update that references it. Rollback on upload error
		// keeps everything local. / 上传与改引用同处锁段：GC 不能在
		// PutObject 与建引用之间删掉刚上传的 S3 对象；上传失败回滚则维持本地。
		err = func() error {
			tx, err := s.pool.Begin(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx,
				`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key); err != nil {
				return err
			}
			if _, err := s.s3.client.PutObject(ctx, &s3.PutObjectInput{
				Bucket: aws.String(s.s3.cfg.Bucket), Key: aws.String(key), Body: f,
			}); err != nil {
				return fmt.Errorf("s3 put %s: %w", c.id, err)
			}
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
		_ = f.Close()
		if err != nil {
			return n, err
		}
		// Local file leaves only when no other object/version still points
		// at its key. / 本地文件仅当无其它对象/版本引用其 key 时才删。
		for _, lk := range localKeys {
			_ = s.dropKeyIfOrphaned(ctx, lk, "local")
		}
		n++
	}
	return n, nil
}

// keyReferenced reports whether any NON-DELETED object's segments or any
// retained version's segments still point at storage key `key` (a local
// relative path or an S3 key). Content-addressed files are shared between
// objects — delete a file only when this returns false, never by sha alone.
// 判断存储 key（本地相对路径或 S3 key）是否仍被未删除对象的分段或保留版本
// 引用。内容寻址文件在对象间共享——仅当返回 false 才可删除，绝不按 sha 直删。
func (s *Store) keyReferenced(ctx context.Context, q Querier, key string) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM object_segments g
			JOIN objects o ON o.id = g.object_id AND o.status <> 'deleted'
			WHERE g.storage_key = $1)
		OR EXISTS(
			SELECT 1 FROM object_versions v
			WHERE v.segments @> jsonb_build_array(jsonb_build_object('key', $1::text)))`,
		key).Scan(&ok)
	return ok, err
}

// segmentKeys extracts the storage keys recorded for a version row —
// the segments jsonb when present, else the seg-0 storage_key column.
// 取版本行记录的全部存储 key：优先 segments，缺失时退回 seg0 的 storage_key。
func segmentKeys(storageKey string, segments []byte) []string {
	var segs []struct {
		Key string `json:"key"`
	}
	if len(segments) > 0 {
		_ = json.Unmarshal(segments, &segs)
	}
	out := make([]string, 0, len(segs)+1)
	for _, sg := range segs {
		if sg.Key != "" {
			out = append(out, sg.Key)
		}
	}
	if len(out) == 0 && storageKey != "" {
		out = append(out, storageKey)
	}
	return out
}

// dropKeyIfOrphaned removes the stored bytes for `key` once nothing
// references it — local file under dataDir or S3 object. The reference
// check and the delete run while holding a session-scoped advisory lock
// on the key, and writers that create a reference take the matching
// transaction-scoped lock — so the check can't go stale mid-delete.
// 无任何引用后移除 key 对应字节：本地文件（dataDir 下）或 S3 对象。
// 查引用与删文件在 key 的会话级 advisory lock 内进行，建引用的写入方
// 持同一 key 的事务级锁——检查不会在删除中途过期。
func (s *Store) dropKeyIfOrphaned(ctx context.Context, key, loc string) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx,
		`SELECT pg_advisory_lock(hashtextextended($1, 0))`, key); err != nil {
		return err
	}
	defer func() {
		// Fresh context: the caller's may already be canceled — the lock
		// is session-scoped and must be released explicitly.
		// 用新 ctx：调用方的可能已取消——会话级锁必须显式释放。
		ctx2, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = conn.Exec(ctx2, `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, key)
	}()
	refs, err := s.keyReferenced(ctx, conn, key)
	if err != nil || refs {
		return err
	}
	if loc == "s3" && s.s3 != nil {
		_, err = s.s3.client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(s.s3.cfg.Bucket), Key: aws.String(key),
		})
		return err
	}
	// storage_key is a dataDir-relative path stored with forward slashes.
	// storage_key 是相对 dataDir 的正斜杠路径。
	return os.Remove(filepath.Join(s.dataDir, filepath.FromSlash(key)))
}

// PurgeOldVersions deletes version-history rows older than `older`
// (the audit-retention window) and removes their stored bytes — local
// files under the recorded sha, S3 objects under each segment key.
// 版本保留清除：删除超过审计保留期的历史版本并移除其字节——本地按
// 记录的 sha 删文件，S3 按分段 key 删对象。
func (s *Store) PurgeOldVersions(ctx context.Context, older time.Duration) (int, error) {
	cutoff := time.Now().Add(-older)
	rows, err := s.pool.Query(ctx,
		`SELECT object_id, version, location, storage_key, segments
		 FROM object_versions WHERE created_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	type ver struct {
		obj      uuid.UUID
		ver      int
		loc      string
		key      string
		segments []byte
	}
	var list []ver
	for rows.Next() {
		var v ver
		if err := rows.Scan(&v.obj, &v.ver, &v.loc, &v.key, &v.segments); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, v)
	}
	rows.Close()
	n := 0
	for _, v := range list {
		if _, err := s.pool.Exec(ctx,
			`DELETE FROM object_versions WHERE object_id=$1 AND version=$2 AND created_at < $3`,
			v.obj, v.ver, cutoff); err != nil {
			return n, err
		}
		// Row is gone — anything still pointing at these keys is another
		// live object or another retained version, so leave shared bytes.
		// 行已删——仍指向这些 key 的引用来自其它在用对象或保留版本，
		// 共享字节必须保留。
		for _, key := range segmentKeys(v.key, v.segments) {
			_ = s.dropKeyIfOrphaned(ctx, key, v.loc)
		}
		n++
	}
	return n, nil
}

var ErrS3Only = errors.New("operation requires s3")

// EnqueueIfDue enqueues a {} task of `kind` at most once per `every`:
// advisory-locked so concurrent workers can't double-fire; skipped while
// an equal-kind row is queued/running or finished within the window — a
// failed run simply retries on the next tick.
// 按周期入队：advisory 锁防并发重复；有同种 queued/running 或窗口内已
// 完成则跳过——失败的任务下个周期自动重试。
func (s *Store) EnqueueIfDue(ctx context.Context, kind string, every time.Duration) (bool, error) {
	fired := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "sched:"+kind); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM bg_tasks WHERE kind=$1
			   AND (status IN ('queued','running')
			        OR (status='done' AND updated_at > now() - $2::interval)))`,
			kind, every).Scan(&exists); err != nil || exists {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO bg_tasks (kind, payload) VALUES ($1,'{}')`, kind); err != nil {
			return err
		}
		fired = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return fired, nil
}
