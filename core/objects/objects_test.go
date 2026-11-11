package objects

// Integration tests against a real PostgreSQL.
// Skipped unless WITTYCHAT_TEST_DB_URL is set.
// 针对真实 PG 的集成测试；未设置 WITTYCHAT_TEST_DB_URL 时跳过。

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testEnv(t *testing.T) (*Store, context.Context, func() uuid.UUID) {
	t.Helper()
	dsn := os.Getenv("WITTYCHAT_TEST_DB_URL")
	if dsn == "" {
		t.Skip("WITTYCHAT_TEST_DB_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	dir := t.TempDir()
	s, err := NewStore(pool, dir)
	if err != nil {
		t.Fatal(err)
	}
	mk := func() uuid.UUID {
		t.Helper()
		var id uuid.UUID
		b := make([]byte, 6)
		_, _ = rand.Read(b)
		email := fmt.Sprintf("obj-%s@example.test", hex.EncodeToString(b))
		if err := pool.QueryRow(ctx,
			`INSERT INTO users (id, email, display_name, status, is_admin)
			 VALUES ($1,$2,$3,'active',false) RETURNING id`,
			uuid.Must(uuid.NewV7()), email, email).Scan(&id); err != nil {
			t.Fatalf("mkUser: %v", err)
		}
		return id
	}
	return s, ctx, mk
}

// ReplaceBytes must preserve the prior bytes fully addressable in
// object_versions (all segments, not just seg 0) and the retained history
// must count toward UsedBytes quota.
func TestReplaceBytesVersionsAndQuota(t *testing.T) {
	s, ctx, mk := testEnv(t)
	owner := mk()
	pool := s.pool

	obj, dup, err := s.Put(ctx, owner, uuid.Nil, "attach", "a.txt", "text/plain",
		bytes.NewReader([]byte("v1-bytes")), 1<<20)
	if err != nil || dup {
		t.Fatalf("put: %v dup=%v", err, dup)
	}
	// Add a second segment so the version snapshot has >1 row to preserve.
	if _, err := pool.Exec(ctx,
		`INSERT INTO object_segments (object_id, seg_index, size_bytes, storage_key)
		 VALUES ($1,1,4,'seg1.key')`, obj.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE objects SET segment_count=2 WHERE id=$1`, obj.ID); err != nil {
		t.Fatal(err)
	}

	before, err := s.UsedBytes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceBytes(ctx, obj.ID, bytes.NewReader([]byte("v2-bytes-longer")),
		"text/plain", 1<<20, &owner); err != nil {
		t.Fatalf("replace: %v", err)
	}

	var ver int
	var segs int
	var oldSize int64
	if err := pool.QueryRow(ctx,
		`SELECT version, jsonb_array_length(segments), size_bytes
		 FROM object_versions WHERE object_id=$1`, obj.ID).Scan(&ver, &segs, &oldSize); err != nil {
		t.Fatalf("object_versions row: %v", err)
	}
	if ver != 1 || oldSize != 8 {
		t.Fatalf("version=%d size=%d", ver, oldSize)
	}
	if segs != 2 {
		t.Fatalf("segments snapshot=%d want 2 (full list, restorable)", segs)
	}

	after, err := s.UsedBytes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// delta = new live (15B "v2-bytes-longer") + retained v1 (8B) − old live (8B).
	// If versions didn't count the delta would be 15−8=7 instead.
	if delta := after - before; delta != 15 {
		t.Fatalf("quota delta=%d want 15 (live 15 + version 8 − old live 8)", delta)
	}
}

// PurgeOldVersions drops version rows past the retention window.
func TestPurgeOldVersions(t *testing.T) {
	s, ctx, mk := testEnv(t)
	owner := mk()
	pool := s.pool
	obj, _, err := s.Put(ctx, owner, uuid.Nil, "attach", "b.txt", "text/plain",
		bytes.NewReader([]byte("old")), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceBytes(ctx, obj.ID, bytes.NewReader([]byte("new")),
		"", 1<<20, &owner); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE object_versions SET created_at=now()-interval '400 days' WHERE object_id=$1`, obj.ID); err != nil {
		t.Fatal(err)
	}
	n, err := s.PurgeOldVersions(ctx, 365*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var left int
	_ = pool.QueryRow(ctx,
		`SELECT count(*) FROM object_versions WHERE object_id=$1`, obj.ID).Scan(&left)
	if n != 1 || left != 0 {
		t.Fatalf("purged=%d left=%d", n, left)
	}
}

// Files are content-addressed and shared between objects — PurgeOldVersions
// must not unlink a physical file the live object or another object still
// references. Regression for the edit-back case A→B→A.
func TestPurgeSharedFileKept(t *testing.T) {
	s, ctx, mk := testEnv(t)
	alice, bob := mk(), mk()
	put := func(owner uuid.UUID, content string) *Object {
		o, _, err := s.Put(ctx, owner, uuid.Nil, "attach", "f.bin", "application/octet-stream",
			bytes.NewReader([]byte(content)), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	readAll := func(o *Object) string {
		r, err := s.Open(ctx, o)
		if err != nil {
			t.Fatalf("open %s: %v", o.ID, err)
		}
		defer func() { _ = r.Close() }()
		got, _ := io.ReadAll(r)
		return string(got)
	}

	v1 := "content-V1-" + uuid.NewString()
	o1 := put(alice, v1)
	o2 := put(bob, v1) // same bytes → same physical file under o1's dir
	if _, err := s.ReplaceBytes(ctx, o1.ID, bytes.NewReader([]byte("content-V2")), "", 1<<20, &alice); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceBytes(ctx, o1.ID, bytes.NewReader([]byte(v1)), "", 1<<20, &alice); err != nil {
		t.Fatal(err)
	}
	// Backdate all version rows past the window so both purge.
	if _, err := s.pool.Exec(ctx,
		`UPDATE object_versions SET created_at=now()-interval '2 days'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PurgeOldVersions(ctx, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	// v1's file is still the live file of o1 AND the file of o2 — both
	// must survive; v2's orphaned file may go.
	if got := readAll(o1); got != v1 {
		t.Fatalf("o1 content %q want %q", got, v1)
	}
	if got := readAll(o2); got != v1 {
		t.Fatalf("o2 content %q want %q", got, v1)
	}
}

// OrphanGC must not unlink a file still referenced by a live object with
// the same content hash.
func TestOrphanGCSharedFileKept(t *testing.T) {
	s, ctx, mk := testEnv(t)
	alice, bob := mk(), mk()
	same := "same-content-" + uuid.NewString()

	o1, _, err := s.Put(ctx, alice, uuid.Nil, "attach", "a.bin", "", bytes.NewReader([]byte(same)), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	o2, _, err := s.Put(ctx, bob, uuid.Nil, "hr", "b.bin", "", bytes.NewReader([]byte(same)), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	// Keep o2 alive via an HR reference; backdate o1 past the 7d window.
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO hr_requests (id, requester, kind, payload)
		 VALUES ($1,$2,'leave', jsonb_build_object('attachment_object_ids', jsonb_build_array($3::text)))`,
		uuid.Must(uuid.NewV7()), bob, o2.ID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE objects SET status='pending', created_at=now()-interval '8 days' WHERE id=$1`, o1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OrphanGC(ctx); err != nil {
		t.Fatal(err)
	}
	var st string
	if err := s.pool.QueryRow(ctx,
		`SELECT status FROM objects WHERE id=$1`, o1.ID).Scan(&st); err != nil {
		t.Fatal(err)
	}
	if st != "deleted" {
		t.Fatalf("o1 status %q want deleted", st)
	}
	r, err := s.Open(ctx, o2)
	if err != nil {
		t.Fatalf("o2 unreadable after gc: %v", err)
	}
	defer func() { _ = r.Close() }()
	got, _ := io.ReadAll(r)
	if string(got) != same {
		t.Fatalf("o2 content %q want %q", got, same)
	}
}

// Concurrent duplicate uploads (retry, double-click, two tabs) raced the
// dedup check, died on the unique index, then deleted the shared file the
// winner had just published — leaving the surviving row unreadable.
// The advisory-locked Put must dedup or share cleanly every time.
func TestPutConcurrentDuplicate(t *testing.T) {
	s, ctx, mk := testEnv(t)
	owner := mk()
	content := "race-me-" + uuid.NewString()
	for trial := 0; trial < 8; trial++ {
		payload := content + fmt.Sprint(trial)
		type res struct {
			o   *Object
			err error
		}
		ch := make(chan res, 2)
		start := make(chan struct{})
		put := func() {
			<-start
			o, _, err := s.Put(ctx, owner, uuid.Nil, "attach", "r.bin", "",
				bytes.NewReader([]byte(payload)), 1<<20)
			ch <- res{o, err}
		}
		go put()
		go put()
		close(start)
		r1, r2 := <-ch, <-ch
		if r1.err != nil || r2.err != nil {
			t.Fatalf("trial %d: errs %v / %v", trial, r1.err, r2.err)
		}
		// Every returned object must be readable — the loser must not
		// have unlinked the winner's shared file. / 两个返回对象都必须
		// 可读——败者不得删掉胜者的共享文件。
		for _, r := range []res{r1, r2} {
			rc, err := s.Open(ctx, r.o)
			if err != nil {
				t.Fatalf("trial %d: open %s: %v", trial, r.o.ID, err)
			}
			got, _ := io.ReadAll(rc)
			_ = rc.Close()
			if string(got) != payload {
				t.Fatalf("trial %d: content %q want %q", trial, got, payload)
			}
		}
	}
}

// Two owners uploading identical bytes share the same-day file; both
// rows must stay readable regardless of upload order.
func TestPutSharedAcrossOwners(t *testing.T) {
	s, ctx, mk := testEnv(t)
	alice, bob := mk(), mk()
	same := "shared-" + uuid.NewString()
	oa, _, err := s.Put(ctx, alice, uuid.Nil, "attach", "a.bin", "", bytes.NewReader([]byte(same)), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	ob, _, err := s.Put(ctx, bob, uuid.Nil, "attach", "b.bin", "", bytes.NewReader([]byte(same)), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range []*Object{oa, ob} {
		rc, err := s.Open(ctx, o)
		if err != nil {
			t.Fatalf("open %s: %v", o.ID, err)
		}
		got, _ := io.ReadAll(rc)
		_ = rc.Close()
		if string(got) != same {
			t.Fatalf("content %q want %q", got, same)
		}
	}
}

// ReplaceBytes onto content another object of the same owner already
// holds hits the scoped unique index and rolls back — the failure path
// must not delete the other object's shared file.
func TestReplaceCollisionKeepsShared(t *testing.T) {
	s, ctx, mk := testEnv(t)
	owner := mk()
	bContent := "B-content-" + uuid.NewString()
	ox, _, err := s.Put(ctx, owner, uuid.Nil, "attach", "x.bin", "",
		bytes.NewReader([]byte("A-content-"+uuid.NewString())), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	oy, _, err := s.Put(ctx, owner, uuid.Nil, "attach", "y.bin", "",
		bytes.NewReader([]byte(bContent)), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceBytes(ctx, ox.ID, bytes.NewReader([]byte(bContent)),
		"", 1<<20, &owner); err == nil {
		t.Fatal("replace to an existing sha should fail on the unique index")
	}
	// Y's bytes must survive X's failed edit.
	rc, err := s.Open(ctx, oy)
	if err != nil {
		t.Fatalf("y unreadable after x's failed replace: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != bContent {
		t.Fatalf("y content %q want %q", got, bContent)
	}
}

// Admin purge of one owner's objects must not delete the file a
// different owner's same-content object still uses.
func TestAdminPurgeSharedFileKept(t *testing.T) {
	s, ctx, mk := testEnv(t)
	alice, bob := mk(), mk()
	same := "purge-shared-" + uuid.NewString()
	oa, _, err := s.Put(ctx, alice, uuid.Nil, "attach", "a.bin", "", bytes.NewReader([]byte(same)), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	ob, _, err := s.Put(ctx, bob, uuid.Nil, "attach", "b.bin", "", bytes.NewReader([]byte(same)), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := s.AdminPurge(ctx, alice, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for i := range deleted {
		if deleted[i].ID == oa.ID {
			found = true
		}
		s.RemoveFiles(ctx, &deleted[i])
	}
	if !found {
		t.Fatalf("purge did not return alice's object (deleted=%d)", len(deleted))
	}
	rc, err := s.Open(ctx, ob)
	if err != nil {
		t.Fatalf("bob's shared object unreadable after purge: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != same {
		t.Fatalf("bob content %q want %q", got, same)
	}
}

// Only actual core-asset references grant protection; a claimed purpose
// cannot hide an ordinary upload. / 真实引用保活，伪造用途不能隐藏普通上传。
func TestAdminSkipsCoreAssets(t *testing.T) {
	s, ctx, mk := testEnv(t)
	alice := mk()
	put := func(purpose string) uuid.UUID {
		t.Helper()
		o, _, err := s.Put(ctx, alice, uuid.Nil, purpose, purpose+".png", "image/png",
			bytes.NewReader([]byte(purpose+uuid.NewString())), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		return o.ID
	}
	plain, unusedLogo, unusedAvatar := put("attachment"), put("workspace_logo"), put("avatar")
	avatar, logoBySetting, groupAv := put("attachment"), put("attachment"), put("attachment")
	if _, err := s.pool.Exec(ctx, `UPDATE users SET avatar_object_id=$2 WHERE id=$1`, alice, avatar); err != nil {
		t.Fatal(err)
	}
	var prev []byte
	_ = s.pool.QueryRow(ctx, `SELECT value FROM workspace_settings WHERE key='workspace.logo_object_id'`).Scan(&prev)
	if _, err := s.pool.Exec(ctx, `INSERT INTO workspace_settings (key,value) VALUES ('workspace.logo_object_id',to_jsonb($1::text))
		ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value`, logoBySetting.String()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if prev == nil {
			_, _ = s.pool.Exec(context.Background(), `DELETE FROM workspace_settings WHERE key='workspace.logo_object_id'`)
		} else {
			_, _ = s.pool.Exec(context.Background(), `UPDATE workspace_settings SET value=$1 WHERE key='workspace.logo_object_id'`, prev)
		}
	})
	var conv uuid.UUID
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO conversations (id,kind,title,avatar_object_id) VALUES ($1,'group','core-asset',$2) RETURNING id`,
		uuid.Must(uuid.NewV7()), groupAv).Scan(&conv); err != nil {
		t.Fatal(err)
	}
	from, to := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	list, err := s.AdminList(ctx, alice, from, to)
	if err != nil {
		t.Fatal(err)
	}
	ordinary := map[uuid.UUID]bool{plain: true, unusedLogo: true, unusedAvatar: true}
	if len(list) != len(ordinary) {
		t.Fatalf("admin list must show unbound uploads, including claimed core purposes: %+v", list)
	}
	for _, o := range list {
		if !ordinary[o.ID] {
			t.Fatalf("admin list shows core asset %s", o.ID)
		}
	}
	deleted, err := s.AdminPurge(ctx, alice, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != len(ordinary) {
		t.Fatalf("purge must delete ordinary uploads: %d deleted", len(deleted))
	}
	for _, o := range deleted {
		if !ordinary[o.ID] {
			t.Fatalf("purge touched core asset %s", o.ID)
		}
	}
	for _, id := range []uuid.UUID{avatar, logoBySetting, groupAv} {
		var st string
		if err := s.pool.QueryRow(ctx, `SELECT status FROM objects WHERE id=$1`, id).Scan(&st); err != nil {
			t.Fatal(err)
		}
		if st != "ready" {
			t.Fatalf("core asset %s was purged", id)
		}
	}
}

// Cleanup must recheck references committed while it waited for the image
// row lock. This exercises the actual PostgreSQL interleaving, not a timing
// assumption about which goroutine ran first.
// 清理等图片行锁期间新增的头像引用必须保活；用 PG 阻塞关系确认交错次序。
func TestCoreAssetBindingSurvivesCleanupRace(t *testing.T) {
	for _, mode := range []string{"admin", "gc"} {
		t.Run(mode, func(t *testing.T) {
			s, parent, mk := testEnv(t)
			ctx, cancel := context.WithTimeout(parent, 15*time.Second)
			defer cancel()
			owner := mk()
			obj, _, err := s.Put(ctx, owner, uuid.Nil, "doc", "avatar.png", "image/png",
				bytes.NewReader(pngBytes(t, 16, 16)), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.pool.Exec(ctx,
				`UPDATE objects SET created_at=now()-interval '8 days' WHERE id=$1`, obj.ID); err != nil {
				t.Fatal(err)
			}
			tx, err := s.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(parent) }()
			if err := RequireReadyImage(ctx, tx, obj.ID, owner); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				if mode == "admin" {
					_, err := s.AdminPurge(ctx, owner, time.Now().Add(-9*24*time.Hour), time.Now().Add(time.Hour))
					done <- err
				} else {
					_, err := s.OrphanGC(ctx)
					done <- err
				}
			}()
			// Wait until cleanup has taken its candidate snapshot and is
			// blocked on our binding transaction's object lock.
			for {
				var blocked bool
				if err := s.pool.QueryRow(ctx,
					`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1::int=ANY(pg_blocking_pids(pid)))`,
					tx.Conn().PgConn().PID()).Scan(&blocked); err != nil {
					t.Fatal(err)
				}
				if blocked {
					break
				}
				select {
				case err := <-done:
					t.Fatalf("cleanup did not wait for binding: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(10 * time.Millisecond):
				}
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO conversations (id,kind,title,avatar_object_id) VALUES ($1,'group','avatar-race',$2)`,
				uuid.Must(uuid.NewV7()), obj.ID); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			current, err := s.Peek(ctx, obj.ID)
			if err != nil || current.Status != "ready" {
				t.Fatalf("newly bound avatar was deleted: %+v, %v", current, err)
			}
		})
	}
}

func TestUnboundIdentityUploadsExpire(t *testing.T) {
	s, ctx, mk := testEnv(t)
	owner := mk()
	for i, purpose := range []string{"avatar", "workspace_logo"} {
		o, _, err := s.Put(ctx, owner, uuid.Nil, purpose, "unused.png", "image/png",
			bytes.NewReader(pngBytes(t, i+1, 1)), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.pool.Exec(ctx,
			`UPDATE objects SET created_at=now()-interval '8 days' WHERE id=$1`, o.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.OrphanGC(ctx); err != nil {
			t.Fatal(err)
		}
		current, err := s.Peek(ctx, o.ID)
		if err != nil || current.Status != "deleted" {
			t.Fatalf("unbound %s did not expire: %+v, %v", purpose, current, err)
		}
	}
}

// AV quarantine of an infected object must not unlink a file a clean
// same-content object still references.
type detectScanner struct{ hit string }

func (d detectScanner) Scan(context.Context, string) (string, error) { return d.hit, nil }

func TestScanSharedFileKept(t *testing.T) {
	s, ctx, mk := testEnv(t)
	alice, bob := mk(), mk()
	same := "infected-" + uuid.NewString()
	oa, _, err := s.Put(ctx, alice, uuid.Nil, "attach", "a.bin", "", bytes.NewReader([]byte(same)), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	ob, _, err := s.Put(ctx, bob, uuid.Nil, "attach", "b.bin", "", bytes.NewReader([]byte(same)), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	det, err := s.ScanObject(ctx, oa.ID, detectScanner{hit: "EICAR-Test"})
	if err != nil || det == "" {
		t.Fatalf("scan: det=%q err=%v", det, err)
	}
	rc, err := s.Open(ctx, ob)
	if err != nil {
		t.Fatalf("bob's clean object unreadable after quarantine: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != same {
		t.Fatalf("bob content %q want %q", got, same)
	}
}

// dimsOf reads the width/height/dims_checked triple for one row —
// every assertion in these tests is row-scoped (the test DB is shared
// with other packages' tests, so global counts are off-limits).
// dimsOf 读取单行 (width,height,dims_checked)——断言只针对具体行，
// 测试库与他包共用，不断言全局计数。
func dimsOf(t *testing.T, s *Store, ctx context.Context, id uuid.UUID) (*int, *int, bool) {
	t.Helper()
	var w, h *int
	var checked bool
	if err := s.pool.QueryRow(ctx,
		`SELECT width, height, dims_checked FROM objects WHERE id=$1`, id).
		Scan(&w, &h, &checked); err != nil {
		t.Fatalf("dimsOf %s: %v", id, err)
	}
	return w, h, checked
}

func corruptPNG() []byte {
	b := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	return append(b, bytes.Repeat([]byte{0x99}, 128)...)
}

// Put must sniff dims at upload (EXIF-aware for JPEG) and mark the row
// checked; non-image bytes get NULL dims but still count as checked.
func TestPutImageDims(t *testing.T) {
	s, ctx, mk := testEnv(t)
	owner := mk()

	obj, dup, err := s.Put(ctx, owner, uuid.Nil, "attach", "a.png", "image/png",
		bytes.NewReader(pngBytes(t, 517, 389)), 1<<20)
	if err != nil || dup {
		t.Fatalf("put png: %v dup=%v", err, dup)
	}
	if obj.Width == nil || *obj.Width != 517 || obj.Height == nil || *obj.Height != 389 {
		t.Fatalf("png dims %v,%v want 517,389", obj.Width, obj.Height)
	}
	if _, _, checked := dimsOf(t, s, ctx, obj.ID); !checked {
		t.Fatal("png row dims_checked=false")
	}

	// Stored 40×30, orientation 6 → reported as the displayed 30×40.
	jpg, dup, err := s.Put(ctx, owner, uuid.Nil, "attach", "b.jpg", "image/jpeg",
		bytes.NewReader(jpegWithEXIF(t, 40, 30, 6, false)), 1<<20)
	if err != nil || dup {
		t.Fatalf("put jpeg: %v dup=%v", err, dup)
	}
	if jpg.Width == nil || *jpg.Width != 30 || jpg.Height == nil || *jpg.Height != 40 {
		t.Fatalf("jpeg o6 dims %v,%v want 30,40", jpg.Width, jpg.Height)
	}

	txt, dup, err := s.Put(ctx, owner, uuid.Nil, "attach", "c.txt", "text/plain",
		bytes.NewReader([]byte("hello")), 1<<20)
	if err != nil || dup {
		t.Fatalf("put text: %v dup=%v", err, dup)
	}
	w, h, checked := dimsOf(t, s, ctx, txt.ID)
	if w != nil || h != nil || !checked {
		t.Fatalf("text row: w=%v h=%v checked=%v want nil,nil,true", w, h, checked)
	}
}

// The sweep re-sniffs unchecked image rows once: decodable → dims +
// checked, undecodable → checked with NULL dims, bytes never truncated,
// and a second pass leaves the rows alone.
func TestBackfillDims(t *testing.T) {
	s, ctx, mk := testEnv(t)
	owner := mk()
	pngRaw := pngBytes(t, 11, 7)
	badRaw := corruptPNG()

	good, _, err := s.Put(ctx, owner, uuid.Nil, "attach", "good.png", "image/png",
		bytes.NewReader(pngRaw), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	bad, _, err := s.Put(ctx, owner, uuid.Nil, "attach", "bad.png", "image/png",
		bytes.NewReader(badRaw), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate rows that predate this code: dims unknown, unchecked.
	if _, err := s.pool.Exec(ctx,
		`UPDATE objects SET width=NULL, height=NULL, dims_checked=false
		 WHERE id IN ($1,$2)`, good.ID, bad.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := s.BackfillDims(ctx); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if w, h, checked := dimsOf(t, s, ctx, good.ID); !checked || w == nil || *w != 11 || h == nil || *h != 7 {
		t.Fatalf("good row: w=%v h=%v checked=%v want 11,7,true", w, h, checked)
	}
	if w, h, checked := dimsOf(t, s, ctx, bad.ID); !checked || w != nil || h != nil {
		t.Fatalf("bad row: w=%v h=%v checked=%v want nil,nil,true", w, h, checked)
	}

	// The sniff must not consume the stream: stored bytes come back whole.
	for _, tc := range []struct {
		o   *Object
		raw []byte
	}{{good, pngRaw}, {bad, badRaw}} {
		rc, err := s.Open(ctx, tc.o)
		if err != nil {
			t.Fatalf("open %s: %v", tc.o.ID, err)
		}
		got, _ := io.ReadAll(rc)
		_ = rc.Close()
		if !bytes.Equal(got, tc.raw) {
			t.Fatalf("%s bytes: got %d want %d", tc.o.ID, len(got), len(tc.raw))
		}
	}

	// Second pass: checked rows aren't selected, nothing changes.
	if _, err := s.BackfillDims(ctx); err != nil {
		t.Fatalf("backfill 2: %v", err)
	}
	if w, h, checked := dimsOf(t, s, ctx, good.ID); !checked || w == nil || *w != 11 || *h != 7 {
		t.Fatalf("good row after pass 2 changed: w=%v h=%v checked=%v", w, h, checked)
	}
	if w, h, checked := dimsOf(t, s, ctx, bad.ID); !checked || w != nil || h != nil {
		t.Fatalf("bad row after pass 2 changed: w=%v h=%v checked=%v", w, h, checked)
	}
}

// Dedup onto a legacy (unchecked) row backfills the dims — same sha,
// same bytes, same dims.
func TestPutDedupBackfillsDims(t *testing.T) {
	s, ctx, mk := testEnv(t)
	owner := mk()
	raw := pngBytes(t, 13, 9)

	obj, dup, err := s.Put(ctx, owner, uuid.Nil, "attach", "d.png", "image/png",
		bytes.NewReader(raw), 1<<20)
	if err != nil || dup {
		t.Fatalf("put: %v dup=%v", err, dup)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE objects SET width=NULL, height=NULL, dims_checked=false WHERE id=$1`, obj.ID); err != nil {
		t.Fatal(err)
	}

	again, dup, err := s.Put(ctx, owner, uuid.Nil, "attach", "d2.png", "image/png",
		bytes.NewReader(raw), 1<<20)
	if err != nil || !dup {
		t.Fatalf("re-put: %v dup=%v want dedup", err, dup)
	}
	if again.ID != obj.ID {
		t.Fatalf("dedup returned different object %s want %s", again.ID, obj.ID)
	}
	if again.Width == nil || *again.Width != 13 || again.Height == nil || *again.Height != 9 {
		t.Fatalf("returned dims %v,%v want 13,9", again.Width, again.Height)
	}
	if w, h, checked := dimsOf(t, s, ctx, obj.ID); !checked || w == nil || *w != 13 || *h != 9 {
		t.Fatalf("row: w=%v h=%v checked=%v want 13,9,true", w, h, checked)
	}
}

// ReplaceBytes re-sniffs the new bytes' dims and keeps the row checked.
func TestReplaceBytesDims(t *testing.T) {
	s, ctx, mk := testEnv(t)
	owner := mk()

	obj, _, err := s.Put(ctx, owner, uuid.Nil, "attach", "e.png", "image/png",
		bytes.NewReader(pngBytes(t, 10, 10)), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	o, err := s.ReplaceBytes(ctx, obj.ID, bytes.NewReader(pngBytes(t, 22, 14)),
		"image/png", 1<<20, &owner)
	if err != nil {
		t.Fatal(err)
	}
	if o.Width == nil || *o.Width != 22 || o.Height == nil || *o.Height != 14 {
		t.Fatalf("replaced dims %v,%v want 22,14", o.Width, o.Height)
	}
	if w, h, checked := dimsOf(t, s, ctx, obj.ID); !checked || w == nil || *w != 22 || *h != 14 {
		t.Fatalf("row: w=%v h=%v checked=%v want 22,14,true", w, h, checked)
	}
}

// EnqueueIfDue: first call fires, second is suppressed while the task is
// queued/running, a done task older than the window refires, a running
// one suppresses.
func TestEnqueueIfDue(t *testing.T) {
	s, ctx, _ := testEnv(t)
	var kb [8]byte
	_, _ = rand.Read(kb[:])
	kind := fmt.Sprintf("test-sched-%x", kb[:])
	fired, err := s.EnqueueIfDue(ctx, kind, time.Hour)
	if err != nil || !fired {
		t.Fatalf("first call: fired=%v err=%v", fired, err)
	}
	fired, err = s.EnqueueIfDue(ctx, kind, time.Hour)
	if err != nil || fired {
		t.Fatalf("second call: fired=%v want false (queued row exists)", fired)
	}
	// Mark it done long ago → refires.
	if _, err := s.pool.Exec(ctx,
		`UPDATE bg_tasks SET status='done', updated_at=now()-interval '2 hours' WHERE kind=$1`, kind); err != nil {
		t.Fatal(err)
	}
	fired, err = s.EnqueueIfDue(ctx, kind, time.Hour)
	if err != nil || !fired {
		t.Fatalf("after stale done: fired=%v want true", fired)
	}
	// A running row suppresses.
	if _, err := s.pool.Exec(ctx,
		`UPDATE bg_tasks SET status='running' WHERE kind=$1`, kind); err != nil {
		t.Fatal(err)
	}
	fired, err = s.EnqueueIfDue(ctx, kind, time.Hour)
	if err != nil || fired {
		t.Fatalf("running row: fired=%v want false", fired)
	}
}
