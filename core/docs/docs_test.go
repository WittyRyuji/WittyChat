package docs

// Integration tests against a real PostgreSQL.
// Skipped unless WITTYCHAT_TEST_DB_URL is set.
// 针对真实 PG 的集成测试；未设置 WITTYCHAT_TEST_DB_URL 时跳过。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"testing"
	"wittychat/core/im"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testEnv(t *testing.T) (*Store, *im.Store, context.Context, func() uuid.UUID) {
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
	ims := im.NewStore(pool)
	mk := func() uuid.UUID {
		t.Helper()
		var id uuid.UUID
		b := make([]byte, 6)
		_, _ = rand.Read(b)
		email := fmt.Sprintf("docs-%s@example.test", hex.EncodeToString(b))
		if err := pool.QueryRow(ctx,
			`INSERT INTO users (id, email, display_name, status, is_admin)
			 VALUES ($1,$2,$3,'active',false) RETURNING id`,
			uuid.Must(uuid.NewV7()), email, email).Scan(&id); err != nil {
			t.Fatalf("mkUser: %v", err)
		}
		return id
	}
	return New(pool, ims.IsMember, ims.MemberRole), ims, ctx, mk
}

func TestShareAccessLifecycle(t *testing.T) {
	s, ims, ctx, mk := testEnv(t)
	owner, member, outsider := mk(), mk(), mk()

	grp, err := ims.CreateGroup(ctx, owner, "docs-grp", false, []uuid.UUID{member})
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.Create(ctx, owner, "shared-doc", "doc", nil)
	if err != nil {
		t.Fatal(err)
	}

	if s.CanAccess(ctx, d.ID, member) || s.CanAccess(ctx, d.ID, outsider) {
		t.Fatal("personal doc accessible before share")
	}
	if err := s.Share(ctx, d.ID, grp.ID, owner); err != nil {
		t.Fatal(err)
	}
	if !s.CanAccess(ctx, d.ID, member) {
		t.Fatal("conv member cannot read shared doc")
	}
	if s.CanAccess(ctx, d.ID, outsider) {
		t.Fatal("outsider gained access via share")
	}
	listed, err := s.ListByConv(ctx, grp.ID)
	if err != nil || len(listed) != 1 || listed[0].ID != d.ID {
		t.Fatalf("ListByConv: %v %+v", err, listed)
	}

	if err := s.Unshare(ctx, d.ID, grp.ID); err != nil {
		t.Fatal(err)
	}
	if s.CanAccess(ctx, d.ID, member) {
		t.Fatal("member still has access after unshare")
	}
	if !s.CanAccess(ctx, d.ID, owner) {
		t.Fatal("owner lost access after unshare")
	}
}

func TestCompactThresholdAndArchive(t *testing.T) {
	s, ims, ctx, mk := testEnv(t)
	owner, member := mk(), mk()
	grp, err := ims.CreateGroup(ctx, owner, "cmp-grp", false, []uuid.UUID{member})
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.Create(ctx, owner, "cmp-doc", "doc", &grp.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Below the threshold: snapshot is appended as an ordinary update.
	// (seq is a GLOBAL identity across docs — watermarks come from
	// LoadUpdates' maxSeq, never a per-doc count.)
	for i := 0; i < 10; i++ {
		if err := s.AppendUpdate(ctx, d.ID, []byte(fmt.Sprintf("u%d", i)), 0); err != nil {
			t.Fatal(err)
		}
	}
	ups, maxSeq, err := s.LoadUpdates(ctx, d.ID, 0)
	if err != nil || len(ups) != 10 || maxSeq == 0 {
		t.Fatalf("log after appends: n=%d max=%d err=%v", len(ups), maxSeq, err)
	}
	done, err := s.CompactIfLarge(ctx, d.ID, []byte("early-snap"), maxSeq)
	if err != nil || done {
		t.Fatalf("compact below threshold: done=%v err=%v", done, err)
	}
	ups, _, _ = s.LoadUpdates(ctx, d.ID, 0)
	if len(ups) != 10 {
		t.Fatalf("below-threshold log mutated: n=%d", len(ups))
	}

	// Past the threshold: rows <= watermark are archived, not lost.
	for i := 10; i < 55; i++ {
		if err := s.AppendUpdate(ctx, d.ID, []byte(fmt.Sprintf("u%d", i)), 0); err != nil {
			t.Fatal(err)
		}
	}
	_, maxSeq, _ = s.LoadUpdates(ctx, d.ID, 0)
	done, err = s.CompactIfLarge(ctx, d.ID, []byte("SNAP"), maxSeq)
	if err != nil || !done {
		t.Fatalf("compact: done=%v err=%v", done, err)
	}
	ups, _, _ = s.LoadUpdates(ctx, d.ID, 0)
	if len(ups) != 1 || string(ups[0]) != "SNAP" {
		t.Fatalf("post-compact log: n=%d %q", len(ups), ups)
	}
	var archived int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM doc_update_archive WHERE doc_id=$1`, d.ID).Scan(&archived); err != nil {
		t.Fatal(err)
	}
	if archived != 55 {
		t.Fatalf("archived=%d want 55 (audit retention)", archived)
	}
}

func TestUpdateLogCap(t *testing.T) {
	s, _, ctx, mk := testEnv(t)
	owner := mk()
	d, err := s.Create(ctx, owner, "cap-doc", "doc", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendUpdate(ctx, d.ID, []byte("abcd"), 8); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendUpdate(ctx, d.ID, []byte("abcd"), 8); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendUpdate(ctx, d.ID, []byte("x"), 8); !errors.Is(err, ErrLogFull) {
		t.Fatalf("over-cap append: want ErrLogFull, got %v", err)
	}
}

func TestSoftDeleteHides(t *testing.T) {
	s, _, ctx, mk := testEnv(t)
	owner, other := mk(), mk()
	d, err := s.Create(ctx, owner, "gone", "doc", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, d.ID, owner); err != nil {
		t.Fatal(err)
	}
	if s.CanAccess(ctx, d.ID, owner) || s.CanAccess(ctx, d.ID, other) {
		t.Fatal("soft-deleted doc still accessible")
	}
	if _, err := s.OwnerOf(ctx, d.ID); err == nil {
		t.Fatal("OwnerOf resolved a deleted doc")
	}
	if err := s.AppendUpdate(ctx, d.ID, []byte("x"), 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("append to deleted doc: want ErrNotFound, got %v", err)
	}
}
