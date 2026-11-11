package auth

// Integration tests against a real PostgreSQL.
// Skipped unless WITTYCHAT_TEST_DB_URL is set, e.g.
//
//	WITTYCHAT_TEST_DB_URL=postgres://wittychat:wittychat@localhost:5432/wittychat_test?sslmode=disable
//
// 针对真实 PostgreSQL 的集成测试。未设置 WITTYCHAT_TEST_DB_URL 时跳过。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func testStore(t *testing.T) (*Store, context.Context) {
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
	return NewStore(pool), ctx
}

func uniqEmail() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return fmt.Sprintf("t-%s@example.test", hex.EncodeToString(b))
}

func TestPasswordHashVerify(t *testing.T) {
	phc, err := HashPassword("s3cret-password")
	if err != nil {
		t.Fatal(err)
	}
	ok, err := VerifyPassword("s3cret-password", phc)
	if err != nil || !ok {
		t.Fatal("correct password rejected")
	}
	ok, _ = VerifyPassword("wrong", phc)
	if ok {
		t.Fatal("wrong password accepted")
	}
}

func TestUserActivationSessionFlow(t *testing.T) {
	s, ctx := testStore(t)
	email := uniqEmail()

	u, err := s.CreateUser(ctx, email, "Test User", false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if u.Status != StatusPending {
		t.Fatalf("new user status = %q, want pending", u.Status)
	}
	if _, err := s.CreateUser(ctx, email, "Dup", false); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("duplicate email: want ErrEmailTaken, got %v", err)
	}

	// Activation link: peek → consume → one-time.
	token, _, err := s.CreateActivationLink(ctx, u.ID, LinkKindActivation, u.ID)
	if err != nil {
		t.Fatalf("CreateActivationLink: %v", err)
	}
	peeked, _, _, _, err := s.PeekLink(ctx, token)
	if err != nil || peeked != email {
		t.Fatalf("PeekLink: %v %q", err, peeked)
	}
	uid, kind, err := s.ConsumeLink(ctx, token)
	if err != nil || uid != u.ID || kind != LinkKindActivation {
		t.Fatalf("ConsumeLink: %v", err)
	}
	if _, _, err := s.ConsumeLink(ctx, token); !errors.Is(err, ErrLinkInvalid) {
		t.Fatalf("reuse link: want ErrLinkInvalid, got %v", err)
	}

	// Set password → credential version 1 → activate.
	phc, _ := HashPassword("pw-pw-pw-pw")
	if err := s.SetPassword(ctx, u.ID, phc); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	hash, _, ver, err := s.PasswordAuth(ctx, u.ID)
	if err != nil || ver < 1 {
		t.Fatalf("PasswordAuth: %v ver=%d", err, ver)
	}
	if ok, _ := VerifyPassword("pw-pw-pw-pw", hash); !ok {
		t.Fatal("stored hash does not verify")
	}
	if err := s.ActivateUser(ctx, u.ID); err != nil {
		t.Fatalf("ActivateUser: %v", err)
	}
	u, _ = s.UserByID(ctx, u.ID)
	if u.Status != StatusActive {
		t.Fatalf("status = %q, want active", u.Status)
	}

	// Session: create → validate → revoke → invalid.
	tok, sess, err := s.CreateSession(ctx, u.ID, "test", "test", ver)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	got, err := s.ValidateSession(ctx, tok)
	if err != nil || got.ID != sess.ID {
		t.Fatalf("ValidateSession: %v", err)
	}
	if err := s.RevokeSession(ctx, u.ID, sess.ID); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	if _, err := s.ValidateSession(ctx, tok); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("revoked session: want ErrSessionInvalid, got %v", err)
	}
}

func TestPasswordChangeKillsSessions(t *testing.T) {
	s, ctx := testStore(t)
	u, _ := s.CreateUser(ctx, uniqEmail(), "T", false)
	phc, _ := HashPassword("first-password")
	_ = s.SetPassword(ctx, u.ID, phc)
	_ = s.ActivateUser(ctx, u.ID)

	_, _, ver, _ := s.PasswordAuth(ctx, u.ID)
	tok, _, _ := s.CreateSession(ctx, u.ID, "t", "t", ver)
	if _, err := s.ValidateSession(ctx, tok); err != nil {
		t.Fatalf("session should be valid: %v", err)
	}

	// Password change bumps credential_version → old sessions die.
	phc2, _ := HashPassword("second-password")
	_ = s.SetPassword(ctx, u.ID, phc2)
	if _, err := s.ValidateSession(ctx, tok); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("want ErrSessionInvalid after password change, got %v", err)
	}
}

func TestSuspendKillsSessions(t *testing.T) {
	s, ctx := testStore(t)
	u, _ := s.CreateUser(ctx, uniqEmail(), "T", false)
	phc, _ := HashPassword("pw")
	_ = s.SetPassword(ctx, u.ID, phc)
	_ = s.ActivateUser(ctx, u.ID)
	_, _, ver, _ := s.PasswordAuth(ctx, u.ID)
	tok, _, _ := s.CreateSession(ctx, u.ID, "t", "t", ver)

	if err := s.SetStatus(ctx, u.ID, StatusSuspended, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ValidateSession(ctx, tok); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("want ErrSessionInvalid after suspend, got %v", err)
	}
}
