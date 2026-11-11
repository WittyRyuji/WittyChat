package settings

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"os"
	"testing"
	"wittychat/core/objects"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSetManyAtomicAndUnchangedLogo(t *testing.T) {
	dsn := os.Getenv("WITTYCHAT_TEST_DB_URL")
	if dsn == "" {
		t.Skip("WITTYCHAT_TEST_DB_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	s := New(pool)
	const logoKey = "workspace.logo_object_id"
	testKey := "zz-review-" + uuid.NewString()
	keys := []string{logoKey, KeyIMMessageMaxChars, testKey}
	original, err := s.All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, other := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	for _, id := range []uuid.UUID{owner, other} {
		if _, err := pool.Exec(ctx, `INSERT INTO users (id,email,display_name,status,is_admin)
			VALUES ($1,$2,'settings test','active',true)`, id, id.String()+"@settings.test"); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		for _, key := range keys {
			if raw, ok := original[key]; ok {
				_, err = pool.Exec(ctx, `INSERT INTO workspace_settings (key,value,updated_by) VALUES ($1,$2,NULL)
					ON CONFLICT (key) DO UPDATE SET value=$2,updated_by=NULL`, key, raw)
			} else {
				_, err = pool.Exec(ctx, `DELETE FROM workspace_settings WHERE key=$1`, key)
			}
			if err != nil {
				t.Error(err)
			}
		}
		_, _ = pool.Exec(ctx, `DELETE FROM object_segments WHERE object_id IN (SELECT id FROM objects WHERE owner_user=ANY($1))`, []uuid.UUID{owner, other})
		_, _ = pool.Exec(ctx, `DELETE FROM objects WHERE owner_user=ANY($1)`, []uuid.UUID{owner, other})
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id=ANY($1)`, []uuid.UUID{owner, other})
	}()
	store, err := objects.NewStore(pool, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	pic, _, err := store.Put(ctx, owner, uuid.Nil, "workspace_logo", "logo.png", "image/png", &pngBytes, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	svg, _, err := store.Put(ctx, owner, uuid.Nil, "workspace_logo", "legacy.svg", "image/svg+xml",
		bytes.NewBufferString(`<svg xmlns="http://www.w3.org/2000/svg"/>`), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for _, logo := range []uuid.UUID{pic.ID, svg.ID} {
		// Seed pre-existing data, including legacy SVG that cannot be newly bound.
		// 模拟升级前数据；SVG 允许原样保留，但不能新绑定。
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_settings (key,value) VALUES ($1,to_jsonb($2::text))
			ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value`, logoKey, logo.String()); err != nil {
			t.Fatal(err)
		}
		for _, admin := range []uuid.UUID{owner, other} {
			if err := s.SetMany(ctx, map[string]any{logoKey: "urn:uuid:" + logo.String(), KeyIMMessageMaxChars: 1234}, admin); err != nil {
				t.Fatalf("unchanged logo blocked policy save: %v", err)
			}
			if got := s.Str(ctx, logoKey, ""); got != logo.String() {
				t.Fatalf("unchanged logo was rebound: %s", got)
			}
		}
	}
	assertPolicy := func() {
		t.Helper()
		if got := s.Int(ctx, KeyIMMessageMaxChars, 0); got != 1234 {
			t.Fatalf("failed batch partially committed: %d", got)
		}
	}
	if err := s.SetMany(ctx, map[string]any{logoKey: pic.ID.String(), KeyIMMessageMaxChars: 2000}, other); !errors.Is(err, objects.ErrForbidden) {
		t.Fatalf("new foreign logo must still be rejected: %v", err)
	}
	assertPolicy()
	for _, invalid := range []any{-1, 1.5, "100", nil} {
		if err := s.SetMany(ctx, map[string]any{testKey: "no partial write", KeyIMMessageMaxChars: invalid}, owner); !errors.Is(err, ErrInvalidSetting) {
			t.Fatalf("invalid numeric setting accepted: %v, %v", invalid, err)
		}
		assertPolicy()
	}
	// PostgreSQL rejects the NUL after the first (sorted) key has been written.
	// 数据库在第二次写入拒绝 NUL，第一项也必须回滚，而非仅做前置校验。
	if err := s.SetMany(ctx, map[string]any{KeyIMMessageMaxChars: 2000, testKey: "\x00"}, owner); err == nil {
		t.Fatal("expected database write failure")
	}
	assertPolicy()
	if err := s.SetMany(ctx, map[string]any{logoKey: pic.ID.String(), KeyIMMessageMaxChars: 2000}, owner); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMany(ctx, map[string]any{logoKey: ""}, other); err != nil {
		t.Fatal(err)
	}
	if got := s.Str(ctx, logoKey, "unset"); got != "unset" {
		t.Fatalf("logo removal failed: %s", got)
	}
	// Verify unrelated keys were not added by failed patches.
	if _, ok := s.get(ctx, testKey); ok {
		t.Fatal("failed batch left a setting behind")
	}
}
