package docs

// Integration tests against a real PostgreSQL.
// Skipped unless WITTYCHAT_TEST_DB_URL is set.
// 针对真实 PG 的集成测试；未设置 WITTYCHAT_TEST_DB_URL 时跳过。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"wittychat/core/im"
	"wittychat/core/objects"

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
		if _, err := s.AppendUpdate(ctx, d.ID, []byte(fmt.Sprintf("u%d", i)), 0); err != nil {
			t.Fatal(err)
		}
	}
	ups, maxSeq, err := s.LoadUpdates(ctx, d.ID, 0)
	if err != nil || len(ups) != 10 || maxSeq == 0 {
		t.Fatalf("log after appends: n=%d max=%d err=%v", len(ups), maxSeq, err)
	}
	done, _, err := s.CompactIfLarge(ctx, d.ID, []byte("early-snap"), maxSeq, 0, false)
	if err != nil || done {
		t.Fatalf("compact below threshold: done=%v err=%v", done, err)
	}
	ups, _, _ = s.LoadUpdates(ctx, d.ID, 0)
	if len(ups) != 10 {
		t.Fatalf("below-threshold log mutated: n=%d", len(ups))
	}

	// Past the threshold: rows <= watermark are archived, not lost.
	for i := 10; i < 55; i++ {
		if _, err := s.AppendUpdate(ctx, d.ID, []byte(fmt.Sprintf("u%d", i)), 0); err != nil {
			t.Fatal(err)
		}
	}
	_, maxSeq, _ = s.LoadUpdates(ctx, d.ID, 0)
	done, _, err = s.CompactIfLarge(ctx, d.ID, []byte("SNAP"), maxSeq, 0, false)
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
	if _, err := s.AppendUpdate(ctx, d.ID, []byte("abcd"), 8); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendUpdate(ctx, d.ID, []byte("abcd"), 8); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendUpdate(ctx, d.ID, []byte("x"), 8); !errors.Is(err, ErrLogFull) {
		t.Fatalf("over-cap append: want ErrLogFull, got %v", err)
	}
}

// A short-but-over-cap log must compact despite the row threshold —
// otherwise a doc at docs.log_max_mb can never recover.
// 行少但字节超上限的日志必须可压实——否则到顶的文档永远无法恢复。
func TestCompactOverCapFewRows(t *testing.T) {
	s, _, ctx, mk := testEnv(t)
	owner := mk()
	d, err := s.Create(ctx, owner, "fat-doc", "doc", nil)
	if err != nil {
		t.Fatal(err)
	}
	big := make([]byte, 512)
	if _, err := s.AppendUpdate(ctx, d.ID, big, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendUpdate(ctx, d.ID, big, 0); err != nil {
		t.Fatal(err)
	}
	_, maxSeq, err := s.LoadUpdates(ctx, d.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	// 2 rows < compactMinRows but log_bytes (1024) >= minBytes (512).
	done, _, err := s.CompactIfLarge(ctx, d.ID, []byte("SNAP"), maxSeq, 512, false)
	if err != nil || !done {
		t.Fatalf("compact over-cap short log: done=%v err=%v", done, err)
	}
	ups, _, _ := s.LoadUpdates(ctx, d.ID, 0)
	if len(ups) != 1 || string(ups[0]) != "SNAP" {
		t.Fatalf("post-compact log: n=%d", len(ups))
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
	if _, err := s.AppendUpdate(ctx, d.ID, []byte("x"), 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("append to deleted doc: want ErrNotFound, got %v", err)
	}
}

func TestStageClaim(t *testing.T) {
	s, _, ctx, mk := testEnv(t)
	me := mk()
	d, err := s.Create(ctx, me, "imp", "doc", nil)
	if err != nil {
		t.Fatal(err)
	}
	staged := func() bool {
		var has bool
		if err := s.pool.QueryRow(ctx,
			`SELECT import_payload IS NOT NULL FROM docs WHERE id=$1`, d.ID).Scan(&has); err != nil {
			t.Fatal(err)
		}
		return has
	}
	claim := func(want ClaimStatus, step string) json.RawMessage {
		t.Helper()
		p, st, err := s.Claim(ctx, d.ID)
		if err != nil || st != want {
			t.Fatalf("%s: status=%d err=%v want %d", step, st, err, want)
		}
		return p
	}
	claim(ClaimEmpty, "nothing staged")
	if err := s.Stage(ctx, d.ID, json.RawMessage(`{"type":"html","html":"<p>hi</p>"}`), "file:x.docx"); err != nil {
		t.Fatal("stage:", err)
	}
	if !staged() {
		t.Fatal("payload not stored")
	}
	if p := claim(ClaimOK, "first claim"); !strings.Contains(string(p), "<p>hi</p>") {
		t.Fatalf("payload=%s", p)
	}
	claim(ClaimTaken, "within lease")
	if _, err := s.pool.Exec(ctx,
		`UPDATE docs SET import_claimed_at=now()-interval '3 minutes' WHERE id=$1`, d.ID); err != nil {
		t.Fatal(err)
	}
	claim(ClaimOK, "lease expired")
	// the claimer wrote content → next claim clears the stale payload
	if _, err := s.AppendUpdate(ctx, d.ID, []byte{1, 2, 3}, 1<<20); err != nil {
		t.Fatal("append:", err)
	}
	// the append itself drops the applied payload — nothing lingers
	if staged() {
		t.Fatal("payload not cleared by the first update")
	}
	claim(ClaimEmpty, "after apply")
	if err := s.Stage(ctx, d.ID, json.RawMessage(`{"type":"html","html":"<p>x</p>"}`), "f"); err == nil {
		t.Fatal("stage on non-empty log accepted")
	}
}

// TestDocObjectACLAndGC: an object linked to a doc follows the doc ACL
// (incl. left members losing access) and is exempt from orphan GC.
// 文档图片随文档 ACL（退群即失权），且不被孤儿回收。
func TestDocObjectACLAndGC(t *testing.T) {
	s, ims, ctx, mk := testEnv(t)
	owner, member, bound, outsider := mk(), mk(), mk(), mk()
	obj, err := objects.NewStore(s.pool, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	put := func() uuid.UUID {
		t.Helper()
		data := "docimg " + uuid.Must(uuid.NewV7()).String()
		o, _, err := obj.Put(ctx, owner, uuid.Nil, "doc", "i.png", "image/png",
			strings.NewReader(data), int64(len(data))+1)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.pool.Exec(ctx,
			`UPDATE objects SET created_at=now()-interval '8 days', last_accessed_at=now()-interval '8 days' WHERE id=$1`,
			o.ID); err != nil {
			t.Fatal(err)
		}
		return o.ID
	}
	linked, loose := put(), put()
	shareGrp, err := ims.CreateGroup(ctx, owner, "doc-share", false, []uuid.UUID{member})
	if err != nil {
		t.Fatal(err)
	}
	boundGrp, err := ims.CreateGroup(ctx, owner, "doc-bound", false, []uuid.UUID{bound})
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.Create(ctx, owner, "with-img", "doc", &boundGrp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Share(ctx, d.ID, shareGrp.ID, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO doc_objects (doc_id,object_id) VALUES ($1,$2)`, d.ID, linked); err != nil {
		t.Fatal(err)
	}
	for who, want := range map[uuid.UUID]bool{owner: true, member: true, bound: true, outsider: false} {
		if got := s.CanReadObject(ctx, linked, who); got != want {
			t.Fatalf("CanReadObject(%s)=%v want %v", who, got, want)
		}
	}
	if s.CanReadObject(ctx, loose, member) {
		t.Fatal("unlinked object readable via doc")
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE conv_members SET left_at=now() WHERE conv_id=$1 AND user_id=$2`, shareGrp.ID, member); err != nil {
		t.Fatal(err)
	}
	if s.CanReadObject(ctx, linked, member) {
		t.Fatal("member who left the conv still reads the doc image")
	}
	if _, err := obj.OrphanGC(ctx); err != nil {
		t.Fatal(err)
	}
	status := func(id uuid.UUID) string {
		var st string
		_ = s.pool.QueryRow(ctx, `SELECT status FROM objects WHERE id=$1`, id).Scan(&st)
		return st
	}
	if st := status(linked); st != "ready" {
		t.Fatalf("linked doc object status=%s, want ready", st)
	}
	if st := status(loose); st != "deleted" {
		t.Fatalf("unlinked doc object status=%s, want deleted", st)
	}
}
