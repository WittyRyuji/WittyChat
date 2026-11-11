package feishuimport

// Mock Feishu OpenAPI + end-to-end import tests (real PG via WITTYCHAT_TEST_DB_URL).
import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
	"wittychat/core/docs"
	"wittychat/core/objects"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func tinyXlsx(t *testing.T) []byte {
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	w, _ := zw.Create("xl/workbook.xml")
	_, _ = w.Write([]byte(`<?xml version="1.0"?><workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheets><sheet name="表一"/><sheet name="表二"/></sheets></workbook>`))
	_ = zw.Close()
	return b.Bytes()
}

// mockFeishu serves token/wiki/drive/export/contact endpoints.
func mockFeishu(t *testing.T, meEmail string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/open-apis/auth/v3/tenant_access_token/internal", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["app_secret"] == "bad" {
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 99991663, "msg": "bad secret"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "tenant_access_token": "tok", "expire": 3600})
	})
	mux.HandleFunc("/open-apis/wiki/v2/spaces/sp1/nodes", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("parent_node_token") == "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"items": []map[string]any{
				{"node_token": "n1", "obj_token": "d1", "obj_type": "docx", "title": "我的文档", "node_type": "page", "owner": "ou_mine", "has_child": false},
				{"node_token": "n2", "obj_token": "s1", "obj_type": "sheet", "title": "报表", "node_type": "page", "owner": "ou_mine", "has_child": false},
				{"node_token": "n3", "obj_token": "c1", "obj_type": "docx", "title": "同事文档", "node_type": "page", "owner": "ou_colleague", "has_child": false},
				{"node_token": "n4", "obj_token": "sl1", "obj_type": "slides", "title": "演示", "node_type": "page", "owner": "ou_mine", "has_child": false},
				{"node_token": "n5", "obj_token": "sc", "obj_type": "docx", "title": "快捷", "node_type": "shortcut", "owner": "ou_mine", "has_child": false},
			}}})
		}
	})
	mux.HandleFunc("/open-apis/drive/v1/export_tasks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"ticket": "tk1"}})
	})
	mux.HandleFunc("/open-apis/drive/v1/export_tasks/tk1", func(w http.ResponseWriter, r *http.Request) {
		in := r.URL.Query().Get("token")
		ft := "fd1"
		if in == "s1" {
			ft = "fs1"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
			"result": map[string]any{"job_status": 0, "file_token": ft},
		}})
	})
	mux.HandleFunc("/open-apis/drive/v1/export_tasks/file/fd1/download", func(w http.ResponseWriter, r *http.Request) {
		var b bytes.Buffer
		zw := zip.NewWriter(&b)
		f, _ := zw.Create("word/document.xml")
		_, _ = f.Write([]byte(`<?xml version="1.0"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>hi</w:t></w:r></w:p></w:body></w:document>`))
		_ = zw.Close()
		_, _ = w.Write(b.Bytes())
	})
	mux.HandleFunc("/open-apis/drive/v1/export_tasks/file/fs1/download", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tinyXlsx(t))
	})
	mux.HandleFunc("/open-apis/contact/v3/users/ou_mine", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"user": map[string]any{"enterprise_email": meEmail}}})
	})
	mux.HandleFunc("/open-apis/contact/v3/users/ou_colleague", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"user": map[string]any{"enterprise_email": "other@feishu.test"}}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func env(t *testing.T) (*Store, context.Context, uuid.UUID, uuid.UUID, string) {
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
	mk := func(email string) uuid.UUID {
		var id uuid.UUID
		if err := pool.QueryRow(ctx,
			`INSERT INTO users (id,email,display_name,status,is_admin) VALUES ($1,$2,$3,'active',false) RETURNING id`,
			uuid.Must(uuid.NewV7()), email, "u").Scan(&id); err != nil {
			t.Fatalf("mkUser: %v", err)
		}
		return id
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	sfx := hex.EncodeToString(b)
	me := mk("me-" + sfx + "@wittyloong.com")
	// creator's feishu owner email resolves to this — make it an alias
	_, _ = pool.Exec(ctx, `INSERT INTO mail_addresses (id,user_id,address) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, uuid.Must(uuid.NewV7()), me, "me-"+sfx+"@wittyloong.com")
	other := mk("other-" + sfx + "@x.test")
	obj, err := objects.NewStore(pool, t.TempDir())
	if err != nil {
		t.Fatalf("objects: %v", err)
	}
	ds := docs.New(pool, func(ctx context.Context, a, b uuid.UUID) bool { return true },
		func(ctx context.Context, a, b uuid.UUID) (string, error) { return "member", nil })
	return New(pool, obj, ds, []byte("0123456789abcdef0123456789abcdef")), ctx, me, other, sfx
}

func TestParseFeishuURL(t *testing.T) {
	cases := map[string]string{
		"https://x.feishu.cn/docx/abc123":         "doc/docx/abc123",
		"https://x.feishu.cn/sheets/s1":           "doc/sheet/s1",
		"https://x.feishu.cn/wiki/w1":             "wiki_node//w1",
		"https://x.larksuite.com/drive/folder/f1": "folder//f1",
	}
	for u, want := range cases {
		r, ok := ParseFeishuURL(u)
		if !ok {
			t.Fatalf("%s not parsed", u)
		}
		got := r.Kind + "/" + r.Type + "/" + r.ID
		if got != want {
			t.Fatalf("%s → %s, want %s", u, got, want)
		}
	}
	if _, ok := ParseFeishuURL("https://evil.com/docx/x"); ok {
		t.Fatal("non-feishu host accepted")
	}
}

func TestSelfImport(t *testing.T) {
	s, ctx, me, _, sfx := env(t)
	srv := mockFeishu(t, "me-"+sfx+"@wittyloong.com")
	if err := s.SetApp(ctx, "app1", "secret1", srv.URL, me); err != nil {
		t.Fatal("SetApp:", err)
	}
	j, err := s.CreateJob(ctx, me, false, "self", "self",
		json.RawMessage(`[{"kind":"wiki_space","id":"sp1","name":"空间"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.StartJob(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		more, err := s.Run(ctx, j.ID, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	// my docx + sheet imported (owned by me), colleague docx skipped, slides skipped, shortcut ignored
	var n int
	_ = s.pool.QueryRow(ctx,
		`SELECT count(*) FROM feishu_import_items WHERE job_id=$1 AND status='done'`, j.ID).Scan(&n)
	if n != 2 {
		t.Fatalf("done=%d want 2", n)
	}
	var skipped int
	_ = s.pool.QueryRow(ctx,
		`SELECT count(*) FROM feishu_import_items WHERE job_id=$1 AND status='skipped'`, j.ID).Scan(&skipped)
	if skipped != 2 { // colleague + slides
		t.Fatalf("skipped=%d want 2", skipped)
	}
	var docCount int
	_ = s.pool.QueryRow(ctx,
		`SELECT count(*) FROM docs WHERE owner=$1 AND import_source LIKE 'feishu:%'`, me).Scan(&docCount)
	// docx→1 doc, xlsx→2 sheet docs (表一+表二)
	if docCount != 3 {
		t.Fatalf("docs=%d want 3", docCount)
	}
	var staged int
	_ = s.pool.QueryRow(ctx,
		`SELECT count(*) FROM docs d JOIN doc_objects do2 ON do2.doc_id=d.id WHERE d.owner=$1 AND d.import_payload IS NOT NULL`, me).Scan(&staged)
	if staged != 3 {
		t.Fatalf("staged+linked=%d want 3", staged)
	}
	// re-run → no new docs
	if err := s.StartJob(ctx, j.ID); err == nil {
		_, _ = s.Run(ctx, j.ID, 2*time.Second)
	}
	var docCount2 int
	_ = s.pool.QueryRow(ctx,
		`SELECT count(*) FROM docs WHERE owner=$1 AND import_source LIKE 'feishu:%'`, me).Scan(&docCount2)
	if docCount2 != docCount {
		t.Fatalf("re-run created docs: %d vs %d", docCount2, docCount)
	}
}

func TestSetAppBadSecret(t *testing.T) {
	s, ctx, me, _, _ := env(t)
	_ = s.ClearApp(ctx)
	srv := mockFeishu(t, "")
	if err := s.SetApp(ctx, "app1", "bad", srv.URL, me); err == nil {
		t.Fatal("bad secret accepted")
	}
	var n int
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM feishu_app`).Scan(&n)
	if n != 0 {
		t.Fatal("bad secret stored")
	}
}

func TestNoConfiguredApp(t *testing.T) {
	s, ctx, me, _, _ := env(t)
	_ = s.ClearApp(ctx)
	j, _ := s.CreateJob(ctx, me, false, "x", "self", json.RawMessage(`[]`))
	_ = s.StartJob(ctx, j.ID)
	_, _ = s.Run(ctx, j.ID, time.Second)
	var st string
	_ = s.pool.QueryRow(ctx, `SELECT status FROM feishu_import_jobs WHERE id=$1`, j.ID).Scan(&st)
	if st != "error" {
		t.Fatalf("status=%s want error", st)
	}
}

func TestNonAdminAdminJob(t *testing.T) {
	s, ctx, me, _, _ := env(t)
	if _, err := s.CreateJob(ctx, me, false, "x", "admin", json.RawMessage(`[]`)); err == nil {
		t.Fatal("non-admin admin job accepted")
	}
}

func TestStartJobEnqueuesOnce(t *testing.T) {
	s, ctx, me, _, _ := env(t)
	j, _ := s.CreateJob(ctx, me, false, "x", "self", json.RawMessage(`[]`))
	_ = s.StartJob(ctx, j.ID)
	var n int
	_ = s.pool.QueryRow(ctx,
		`SELECT count(*) FROM bg_tasks WHERE kind='feishu_import' AND status IN ('queued','running') AND payload->>'job_id'=$1`, j.ID).Scan(&n)
	if n != 1 {
		t.Fatalf("tasks=%d want 1", n)
	}
	// second start while running → rejected, still 1 task
	_ = s.StartJob(ctx, j.ID)
	_ = s.pool.QueryRow(ctx,
		`SELECT count(*) FROM bg_tasks WHERE kind='feishu_import' AND status IN ('queued','running') AND payload->>'job_id'=$1`, j.ID).Scan(&n)
	if n != 1 {
		t.Fatalf("after re-start tasks=%d want 1", n)
	}
}
