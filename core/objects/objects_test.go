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
