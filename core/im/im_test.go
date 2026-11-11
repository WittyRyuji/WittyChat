package im

// Integration tests against a real PostgreSQL.
// Skipped unless WITTYCHAT_TEST_DB_URL is set.
// 针对真实 PG 的集成测试；未设置 WITTYCHAT_TEST_DB_URL 时跳过。

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"testing"
	"time"
	"wittychat/core/objects"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testEnv(t *testing.T) (*Store, context.Context, func(email string) uuid.UUID) {
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

	mkUser := func(email string) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		err := pool.QueryRow(ctx,
			`INSERT INTO users (id, email, display_name, status, is_admin)
			 VALUES ($1,$2,$3,'active',false) RETURNING id`,
			uuid.Must(uuid.NewV7()), email, email).Scan(&id)
		if err != nil {
			t.Fatalf("mkUser: %v", err)
		}
		return id
	}
	return NewStore(pool), ctx, mkUser
}

func uniqEmail() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return fmt.Sprintf("im-%s@example.test", hex.EncodeToString(b))
}

func TestDMDedupAndMessaging(t *testing.T) {
	s, ctx, mk := testEnv(t)
	a, b := mk(uniqEmail()), mk(uniqEmail())

	c1, err := s.CreateDM(ctx, a, b)
	if err != nil {
		t.Fatalf("CreateDM: %v", err)
	}
	c2, err := s.CreateDM(ctx, b, a)
	if err != nil {
		t.Fatalf("CreateDM reverse: %v", err)
	}
	if c1.ID != c2.ID {
		t.Fatal("dm not deduplicated")
	}
	if _, err := s.CreateDM(ctx, a, a); !errors.Is(err, ErrSelfDM) {
		t.Fatalf("self dm: want ErrSelfDM, got %v", err)
	}

	// Send → op_seq 1; idempotent resend returns same op.
	msgID := uuid.Must(uuid.NewV7())
	op1, dup, err := s.SendMessage(ctx, a, c1.ID, msgID, "hello **world**", 0, nil, false, nil)
	if err != nil || dup {
		t.Fatalf("send: %v dup=%v", err, dup)
	}
	op2, dup2, err := s.SendMessage(ctx, a, c1.ID, msgID, "hello **world**", 0, nil, false, nil)
	if err != nil || !dup2 || op2.Seq != op1.Seq {
		t.Fatalf("idempotent resend: dup=%v seq %d vs %d", dup2, op2.Seq, op1.Seq)
	}
	if op1.Seq != 1 {
		t.Fatalf("first op seq = %d, want 1", op1.Seq)
	}

	// op_seq continuity.
	opB, _, _ := s.SendMessage(ctx, b, c1.ID, uuid.Must(uuid.NewV7()), "reply", 0, nil, false, nil)
	if opB.Seq != 2 {
		t.Fatalf("second op seq = %d, want 2", opB.Seq)
	}

	// Fetch after watermark: b at seq 0 sees both; at seq 1 sees only seq 2.
	ops, _, latest, err := s.FetchOpsAfter(ctx, b, c1.ID, 0, 200)
	if err != nil || len(ops) != 2 || latest != 2 {
		t.Fatalf("fetch all: n=%d latest=%d err=%v", len(ops), latest, err)
	}
	ops, _, _, _ = s.FetchOpsAfter(ctx, b, c1.ID, 1, 200)
	if len(ops) != 1 || ops[0].Seq != 2 {
		t.Fatalf("fetch delta: got %+v", ops)
	}

	// Non-member cannot read nor send.
	stranger := mk(uniqEmail())
	if _, _, _, err := s.FetchOpsAfter(ctx, stranger, c1.ID, 0, 10); !errors.Is(err, ErrNotMember) {
		t.Fatalf("stranger read: want ErrNotMember, got %v", err)
	}
	if _, _, err := s.SendMessage(ctx, stranger, c1.ID, uuid.Must(uuid.NewV7()), "x", 0, nil, false, nil); !errors.Is(err, ErrNotMember) {
		t.Fatalf("stranger send: want ErrNotMember, got %v", err)
	}
}

func TestEditRecallEffectiveView(t *testing.T) {
	s, ctx, mk := testEnv(t)
	a, b := mk(uniqEmail()), mk(uniqEmail())
	c, _ := s.CreateDM(ctx, a, b)

	msg, _, _ := s.SendMessage(ctx, a, c.ID, uuid.Must(uuid.NewV7()), "original", 0, nil, false, nil)

	// Edit by author works; other member cannot edit.
	if _, err := s.EditMessage(ctx, a, c.ID, msg.Seq, "edited text"); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if _, err := s.EditMessage(ctx, b, c.ID, msg.Seq, "hijack"); !errors.Is(err, ErrNoPermission) {
		t.Fatalf("foreign edit: want ErrNoPermission, got %v", err)
	}

	ops, _, _, _ := s.FetchOpsAfter(ctx, b, c.ID, 0, 200)
	var m *Op
	for _, o := range ops {
		if o.Seq == msg.Seq {
			m = o
		}
	}
	if m == nil || !m.Edited || m.Body != "edited text" {
		t.Fatalf("effective view edit: %+v", m)
	}

	// Recall by author; other member cannot recall.
	if _, err := s.RecallMessage(ctx, b, c.ID, msg.Seq); !errors.Is(err, ErrNoPermission) {
		t.Fatalf("foreign recall: want ErrNoPermission, got %v", err)
	}
	if _, err := s.RecallMessage(ctx, a, c.ID, msg.Seq); err != nil {
		t.Fatalf("recall: %v", err)
	}
	ops, _, _, _ = s.FetchOpsAfter(ctx, b, c.ID, 0, 200)
	for _, o := range ops {
		if o.Seq == msg.Seq {
			if !o.Recalled || o.Body != "" {
				t.Fatalf("recalled message not redacted: %+v", o)
			}
		}
	}
}

func TestGroupManageAndSearch(t *testing.T) {
	s, ctx, mk := testEnv(t)
	owner, m1, m2, outsider := mk(uniqEmail()), mk(uniqEmail()), mk(uniqEmail()), mk(uniqEmail())

	g, err := s.CreateGroup(ctx, owner, "team-alpha", true, []uuid.UUID{m1})
	if err != nil {
		t.Fatal(err)
	}

	// Rename: member can't, owner can; title lands in directory.
	if _, err := s.RenameConv(ctx, m1, g.ID, "x"); !errors.Is(err, ErrNoPermission) {
		t.Fatalf("member rename: want ErrNoPermission, got %v", err)
	}
	// Unique suffix — a dirty dev DB may hold stale public "team-beta".
	beta := "team-beta-" + uuid.Must(uuid.NewV7()).String()[:8]
	if _, err := s.RenameConv(ctx, owner, g.ID, beta); err != nil {
		t.Fatalf("rename: %v", err)
	}
	dir, _ := s.Directory(ctx, m1)
	if dir[0].Title != beta {
		t.Fatalf("title=%q", dir[0].Title)
	}

	// Public search + join.
	hits, err := s.SearchPublicGroups(ctx, outsider, beta, 10)
	if err != nil || len(hits) != 1 || hits[0].Joined {
		t.Fatalf("public search: %v %+v", err, hits)
	}
	if _, err := s.JoinPublicGroup(ctx, g.ID, outsider); err != nil {
		t.Fatalf("join public: %v", err)
	}
	if !s.IsMember(ctx, g.ID, outsider) {
		t.Fatal("joiner not member")
	}
	// Non-public conv can't be self-joined.
	priv, _ := s.CreateGroup(ctx, owner, "secret", false, nil)
	if _, err := s.JoinPublicGroup(ctx, priv.ID, m2); !errors.Is(err, ErrNoPermission) {
		t.Fatalf("join private: want ErrNoPermission, got %v", err)
	}

	// Roles: owner grants admin; non-owner can't; transfer hands over.
	if _, err := s.SetMemberRole(ctx, m1, g.ID, m2, RoleAdmin); !errors.Is(err, ErrNoPermission) {
		t.Fatalf("set role by member: want ErrNoPermission, got %v", err)
	}
	if _, err := s.SetMemberRole(ctx, owner, g.ID, m1, RoleAdmin); err != nil {
		t.Fatalf("set admin: %v", err)
	}
	role, _ := s.MemberRole(ctx, g.ID, m1)
	if role != RoleAdmin {
		t.Fatalf("role=%q", role)
	}
	if _, err := s.TransferOwner(ctx, m1, g.ID, m2); !errors.Is(err, ErrNoPermission) {
		t.Fatalf("transfer by admin: want ErrNoPermission, got %v", err)
	}
	if _, err := s.TransferOwner(ctx, owner, g.ID, m1); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	role, _ = s.MemberRole(ctx, g.ID, m1)
	if role != RoleOwner {
		t.Fatalf("new owner role=%q", role)
	}
	role, _ = s.MemberRole(ctx, g.ID, owner)
	if role != RoleAdmin {
		t.Fatalf("old owner role=%q", role)
	}

	// Search: outsider (now member) finds it; m2 (not member of any) doesn't.
	msgID := uuid.Must(uuid.NewV7())
	if _, _, err := s.SendMessage(ctx, m1, g.ID, msgID, "部署手册 draft", 0, nil, false, nil); err != nil {
		t.Fatal(err)
	}
	found, err := s.SearchMessages(ctx, outsider, "手册", 10)
	if err != nil || len(found) != 1 {
		t.Fatalf("search zh: %v n=%d", err, len(found))
	}
	if found[0].ActorID == nil || *found[0].ActorID != m1 {
		t.Fatalf("search omitted stable actor id: %+v", found[0])
	}
	if _, err := s.pool.Exec(ctx, `UPDATE users SET display_name='renamed author' WHERE id=$1`, m1); err != nil {
		t.Fatal(err)
	}
	found, err = s.SearchMessages(ctx, outsider, "手册", 10)
	if err != nil || len(found) != 1 || found[0].ActorName != "renamed author" ||
		found[0].ActorID == nil || *found[0].ActorID != m1 {
		t.Fatalf("search identity after rename: %+v, %v", found, err)
	}
	found, _ = s.SearchMessages(ctx, m2, "手册", 10)
	if len(found) != 0 {
		t.Fatalf("non-member search leaked %d hits", len(found))
	}
}

func TestGroupAvatarLifecycle(t *testing.T) {
	s, ctx, mk := testEnv(t)
	owner, member, outsider := mk(uniqEmail()), mk(uniqEmail()), mk(uniqEmail())
	g, err := s.CreateGroup(ctx, owner, "avatar lifecycle", false, []uuid.UUID{member})
	if err != nil {
		t.Fatal(err)
	}
	objs, err := objects.NewStore(s.pool, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var imageBytes bytes.Buffer
	if err := png.Encode(&imageBytes, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	put := func(uid uuid.UUID, purpose string, data []byte) *objects.Object {
		t.Helper()
		o, _, err := objs.Put(ctx, uid, uuid.Nil, purpose, "image.png", "image/png", bytes.NewReader(data), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	docImage := put(owner, "doc", imageBytes.Bytes())
	avatar := put(owner, "avatar", imageBytes.Bytes())
	if avatar.ID != docImage.ID || avatar.Purpose != "doc" {
		t.Fatal("fixture must exercise cross-purpose dedup")
	}
	foreign := put(member, "avatar", imageBytes.Bytes())
	if _, err := s.SetGroupAvatar(ctx, member, g.ID, foreign.ID); !errors.Is(err, ErrNoPermission) {
		t.Fatalf("ordinary member may set avatar: %v", err)
	}
	if _, err := s.SetGroupAvatar(ctx, owner, g.ID, foreign.ID); !errors.Is(err, objects.ErrForbidden) {
		t.Fatalf("foreign image accepted: %v", err)
	}
	fakeImage := put(owner, "avatar", []byte("not a PNG despite the supplied MIME"))
	if _, err := s.SetGroupAvatar(ctx, owner, g.ID, fakeImage.ID); !errors.Is(err, objects.ErrNotImage) {
		t.Fatalf("forged image MIME accepted: %v", err)
	}
	for _, state := range []string{"pending", "deleted"} {
		if _, err := s.pool.Exec(ctx, `UPDATE objects SET status=$2 WHERE id=$1`, avatar.ID, state); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetGroupAvatar(ctx, owner, g.ID, avatar.ID); !errors.Is(err, objects.ErrNotReady) {
			t.Fatalf("%s avatar accepted: %v", state, err)
		}
	}
	if _, err := s.pool.Exec(ctx, `UPDATE objects SET status='ready' WHERE id=$1`, avatar.ID); err != nil {
		t.Fatal(err)
	}
	op, err := s.SetGroupAvatar(ctx, owner, g.ID, avatar.ID)
	if err != nil || op.Kind != OpAvatar || op.Seq != 1 {
		t.Fatalf("set avatar/failed writes rolled back: %+v, %v", op, err)
	}
	if id, err := s.GroupAvatarFor(ctx, g.ID, member); err != nil || id != avatar.ID {
		t.Fatalf("member cannot see avatar: %s, %v", id, err)
	}
	if _, err := s.GroupAvatarFor(ctx, g.ID, outsider); !errors.Is(err, ErrNotMember) {
		t.Fatalf("outsider can see avatar: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE objects SET created_at=now()-interval '8 days' WHERE id=$1`, avatar.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := objs.OrphanGC(ctx); err != nil {
		t.Fatal(err)
	}
	purged, err := objs.AdminPurge(ctx, owner, time.Now().Add(-9*24*time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range purged {
		if o.ID == avatar.ID {
			t.Fatal("admin purge deleted the group avatar")
		}
	}
	current, err := objs.Peek(ctx, avatar.ID)
	if err != nil || current.Status != "ready" {
		t.Fatalf("group avatar did not survive cleanup: %+v, %v", current, err)
	}
	if _, err := s.SetMemberRole(ctx, owner, g.ID, member, RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetGroupAvatar(ctx, member, g.ID, foreign.ID); err != nil {
		t.Fatalf("group admin cannot set own image: %v", err)
	}
}

func TestGroupMembersAndRead(t *testing.T) {
	s, ctx, mk := testEnv(t)
	owner, m1, m2, outsider := mk(uniqEmail()), mk(uniqEmail()), mk(uniqEmail()), mk(uniqEmail())

	g, err := s.CreateGroup(ctx, owner, "team", false, []uuid.UUID{m1})
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}

	// Member (non-admin) cannot add; owner can.
	if _, err := s.AddMembers(ctx, m1, g.ID, []uuid.UUID{m2}); !errors.Is(err, ErrNoPermission) {
		t.Fatalf("member add: want ErrNoPermission, got %v", err)
	}
	added, err := s.AddMembers(ctx, owner, g.ID, []uuid.UUID{m2})
	if err != nil || len(added) != 1 {
		t.Fatalf("owner add: %v added=%v", err, added)
	}
	// Re-adding an active member is a no-op.
	added, _ = s.AddMembers(ctx, owner, g.ID, []uuid.UUID{m2})
	if len(added) != 0 {
		t.Fatalf("re-add should be no-op, added=%v", added)
	}

	// Directory: m2 sees the group with unread.
	if _, _, err := s.SendMessage(ctx, owner, g.ID, uuid.Must(uuid.NewV7()), "welcome", 0, nil, false, nil); err != nil {
		t.Fatal(err)
	}
	dir, err := s.Directory(ctx, m2)
	if err != nil || len(dir) != 1 {
		t.Fatalf("directory: %v n=%d", err, len(dir))
	}
	e := dir[0]
	if e.Unread != 1 || e.Preview != "welcome" || e.Title != "team" {
		t.Fatalf("directory entry: %+v", e)
	}
	// member_add ops precede the message; m2's watermark is 0, unread counts only messages.
	if e.LastOpSeq < 2 {
		t.Fatalf("last_op_seq=%d", e.LastOpSeq)
	}

	// MarkRead advances watermark → unread 0.
	if err := s.MarkRead(ctx, g.ID, m2, e.LastOpSeq); err != nil {
		t.Fatal(err)
	}
	dir, _ = s.Directory(ctx, m2)
	if dir[0].Unread != 0 {
		t.Fatalf("unread after read = %d", dir[0].Unread)
	}
	// Watermark is monotonic — older seq doesn't regress.
	if err := s.MarkRead(ctx, g.ID, m2, 1); err != nil {
		t.Fatal(err)
	}

	// Outsider sees empty directory.
	dirOut, _ := s.Directory(ctx, outsider)
	if len(dirOut) != 0 {
		t.Fatalf("outsider directory n=%d", len(dirOut))
	}

	// Leave → no longer member; rejoin restores (left_at cleared).
	if err := s.RemoveMember(ctx, m2, g.ID, m2); err != nil {
		t.Fatalf("self leave: %v", err)
	}
	if s.IsMember(ctx, g.ID, m2) {
		t.Fatal("still member after leave")
	}
}
