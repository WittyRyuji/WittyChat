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
