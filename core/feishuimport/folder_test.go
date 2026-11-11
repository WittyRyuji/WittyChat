package feishuimport

// Folder-root scenarios against a switchable mock: admin owner mapping,
// failure handling without retry loops, rate limiting, budgets and pause.
// 文件夹场景：属主映射、失败不死循环、限流重试、预算分块与暂停。
import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// folderMock: drive folder f0 = {d1 (ou_a), d2 (ou_b), sub/{d3 (ou_a)}}
// with switchable failures and call counters.
type folderMock struct {
	mu          sync.Mutex
	emails      map[string]string // open_id → email
	forbid      map[string]bool   // doc token → export create returns 403
	rateOnce    map[string]bool   // doc token → first export create returns 429
	exports     map[string]int    // doc token → export create calls
	emptyBodies int               // export creates that arrived without a token
	lists       int               // folder list calls
	polls       int
}

func newFolderMock(t *testing.T, m *folderMock) *httptest.Server {
	t.Helper()
	m.exports = map[string]int{}
	ok := func(w http.ResponseWriter, data any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": data})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/open-apis/auth/v3/tenant_access_token/internal", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "tenant_access_token": "tok", "expire": 3600})
	})
	mux.HandleFunc("/open-apis/drive/v1/files", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.lists++
		m.mu.Unlock()
		files := []map[string]any{}
		switch r.URL.Query().Get("folder_token") {
		case "f0":
			files = []map[string]any{
				{"token": "d1", "type": "docx", "name": "甲", "owner_id": "ou_a"},
				{"token": "d2", "type": "docx", "name": "乙", "owner_id": "ou_b"},
				{"token": "sub", "type": "folder", "name": "子目录", "owner_id": "ou_a"},
			}
		case "sub":
			files = []map[string]any{{"token": "d3", "type": "docx", "name": "丙", "owner_id": "ou_a"}}
		}
		ok(w, map[string]any{"files": files, "has_more": false})
	})
	mux.HandleFunc("/open-apis/drive/v1/export_tasks", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		tok := in["token"]
		m.mu.Lock()
		defer m.mu.Unlock()
		if tok == "" {
			m.emptyBodies++
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		m.exports[tok]++
		if m.rateOnce[tok] {
			delete(m.rateOnce, tok)
			w.Header().Set("x-ogw-ratelimit-reset", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if m.forbid[tok] {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		ok(w, map[string]any{"ticket": "tk-" + tok})
	})
	mux.HandleFunc("/open-apis/drive/v1/export_tasks/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/download") {
			var b bytes.Buffer
			zw := zip.NewWriter(&b)
			f, _ := zw.Create("word/document.xml")
			_, _ = f.Write([]byte(`<w:document xmlns:w="w"><w:body/></w:document>`))
			_ = zw.Close()
			_, _ = w.Write(b.Bytes())
			return
		}
		m.mu.Lock()
		m.polls++
		first := m.polls == 1
		m.mu.Unlock()
		if first { // the very first poll: still processing
			ok(w, map[string]any{"result": map[string]any{"job_status": 2}})
			return
		}
		ok(w, map[string]any{"result": map[string]any{"job_status": 0, "file_token": "file"}})
	})
	mux.HandleFunc("/open-apis/contact/v3/users/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/open-apis/contact/v3/users/")
		m.mu.Lock()
		email := m.emails[id]
		m.mu.Unlock()
		ok(w, map[string]any{"user": map[string]any{"enterprise_email": email}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func runToEnd(ctx context.Context, t *testing.T, s *Store, id uuid.UUID, budget time.Duration, maxRuns int) int {
	t.Helper()
	for i := 1; i <= maxRuns; i++ {
		more, err := s.Run(ctx, id, budget)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if !more {
			return i
		}
	}
	t.Fatalf("job did not finish within %d runs (retry loop?)", maxRuns)
	return 0
}

func itemState(ctx context.Context, t *testing.T, s *Store, job uuid.UUID, tok string) (string, string) {
	t.Helper()
	var st, e string
	if err := s.pool.QueryRow(ctx,
		`SELECT status,COALESCE(error,'') FROM feishu_import_items WHERE job_id=$1 AND obj_token=$2`, job, tok).
		Scan(&st, &e); err != nil {
		t.Fatalf("item %s: %v", tok, err)
	}
	return st, e
}

func docsOf(ctx context.Context, t *testing.T, s *Store, owner uuid.UUID) []string {
	t.Helper()
	rows, err := s.pool.Query(ctx,
		`SELECT title FROM docs WHERE owner=$1 AND import_source LIKE 'feishu:%' AND deleted_at IS NULL ORDER BY title`, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var x string
		_ = rows.Scan(&x)
		out = append(out, x)
	}
	return out
}

const folderRoot = `[{"kind":"folder","id":"f0","name":"根"}]`

// Admin mode: owners mapped by email (unmapped → creator), nested folder
// walked, a forbidden doc fails ONCE (no retry loop), a 429 on export create
// is retried with its body intact, and a re-start retries only the failure.
func TestAdminFolderImport(t *testing.T) {
	s, ctx, me, other, sfx := env(t)
	m := &folderMock{
		emails:   map[string]string{"ou_a": "OTHER-" + sfx + "@x.test", "ou_b": "nobody@feishu.test"},
		forbid:   map[string]bool{"d2": true},
		rateOnce: map[string]bool{"d1": true},
	}
	srv := newFolderMock(t, m)
	if err := s.SetApp(ctx, "a", "p", srv.URL, me); err != nil {
		t.Fatal(err)
	}
	j, err := s.CreateJob(ctx, me, true, "adm", "admin", json.RawMessage(folderRoot))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.StartJob(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if n := runToEnd(ctx, t, s, j.ID, time.Minute, 2); n != 1 {
		t.Fatalf("took %d runs, want 1", n)
	}
	var st string
	_ = s.pool.QueryRow(ctx, `SELECT status FROM feishu_import_jobs WHERE id=$1`, j.ID).Scan(&st)
	if st != "done" {
		t.Fatalf("job status=%s want done", st)
	}
	if st, e := itemState(ctx, t, s, j.ID, "d2"); st != "failed" || !strings.Contains(e, "分享给应用") {
		t.Fatalf("d2: %s %q", st, e)
	}
	if m.exports["d2"] != 1 {
		t.Fatalf("forbidden doc exported %d times, want exactly 1", m.exports["d2"])
	}
	if m.exports["d1"] != 2 || m.emptyBodies != 0 {
		t.Fatalf("429 retry: d1 attempts=%d emptyBodies=%d", m.exports["d1"], m.emptyBodies)
	}
	// ou_a → `other` (case-insensitive email); nested path kept in the title
	if got := docsOf(ctx, t, s, other); len(got) != 2 || got[0] != "根/子目录/丙" || got[1] != "根/甲" {
		t.Fatalf("other's docs: %v", got)
	}
	if got := docsOf(ctx, t, s, me); len(got) != 0 {
		t.Fatalf("creator's docs: %v", got)
	}
	// permission granted → re-start retries just the failure; unmapped owner → creator
	m.mu.Lock()
	m.forbid = nil
	m.mu.Unlock()
	if err := s.StartJob(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	runToEnd(ctx, t, s, j.ID, time.Minute, 2)
	if st, _ := itemState(ctx, t, s, j.ID, "d2"); st != "done" {
		t.Fatalf("d2 after retry: %s", st)
	}
	if got := docsOf(ctx, t, s, me); len(got) != 1 || got[0] != "根/乙" {
		t.Fatalf("creator's docs after retry: %v", got)
	}
	if got := docsOf(ctx, t, s, other); len(got) != 2 {
		t.Fatalf("re-run duplicated docs: %v", got)
	}
	if m.exports["d1"] != 2 || m.exports["d3"] != 1 {
		t.Fatalf("done items re-exported: %v", m.exports)
	}
}

// Self mode: only the creator's own documents are exported at all.
func TestSelfSkipsForeignWithoutExport(t *testing.T) {
	s, ctx, me, _, sfx := env(t)
	m := &folderMock{emails: map[string]string{"ou_a": "me-" + sfx + "@wittyloong.com", "ou_b": "colleague@feishu.test"}}
	srv := newFolderMock(t, m)
	if err := s.SetApp(ctx, "a", "p", srv.URL, me); err != nil {
		t.Fatal(err)
	}
	j, _ := s.CreateJob(ctx, me, false, "x", "self", json.RawMessage(folderRoot))
	if err := s.StartJob(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	runToEnd(ctx, t, s, j.ID, time.Minute, 2)
	if st, e := itemState(ctx, t, s, j.ID, "d2"); st != "skipped" || !strings.Contains(e, "不是你的文档") {
		t.Fatalf("colleague doc: %s %q", st, e)
	}
	if m.exports["d2"] != 0 {
		t.Fatalf("colleague's doc was exported %d times", m.exports["d2"])
	}
	if got := docsOf(ctx, t, s, me); len(got) != 2 {
		t.Fatalf("own docs: %v", got)
	}
	// owner with no resolvable email → fail closed
	m.mu.Lock()
	m.emails["ou_a"] = ""
	m.mu.Unlock()
	j2, _ := s.CreateJob(ctx, me, false, "y", "self", json.RawMessage(folderRoot))
	_ = s.StartJob(ctx, j2.ID)
	runToEnd(ctx, t, s, j2.ID, time.Minute, 2)
	if st, e := itemState(ctx, t, s, j2.ID, "d1"); st != "failed" || !strings.Contains(e, "无法确认") {
		t.Fatalf("unresolvable owner: %s %q", st, e)
	}
}

// Tiny budgets converge to the single-run result; discovery is not redone
// once finished; pause stops between items.
func TestBudgetAndPause(t *testing.T) {
	s, ctx, me, _, sfx := env(t)
	mine := "me-" + sfx + "@wittyloong.com"
	m := &folderMock{emails: map[string]string{"ou_a": mine, "ou_b": mine}}
	srv := newFolderMock(t, m)
	if err := s.SetApp(ctx, "a", "p", srv.URL, me); err != nil {
		t.Fatal(err)
	}
	j, _ := s.CreateJob(ctx, me, false, "b", "self", json.RawMessage(folderRoot))
	_ = s.StartJob(ctx, j.ID)
	more, err := s.Run(ctx, j.ID, time.Nanosecond)
	if err != nil || !more {
		t.Fatalf("tiny budget: more=%v err=%v", more, err)
	}
	oneWalk := m.lists
	if err := s.PauseJob(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if more, _ := s.Run(ctx, j.ID, time.Minute); more {
		t.Fatal("paused job asked for more")
	}
	if got := docsOf(ctx, t, s, me); len(got) != 0 {
		t.Fatalf("paused job imported: %v", got)
	}
	if err := s.StartJob(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	runs := runToEnd(ctx, t, s, j.ID, 1500*time.Millisecond, 20)
	if got := docsOf(ctx, t, s, me); len(got) != 3 {
		t.Fatalf("after %d runs docs=%v want 3", runs, got)
	}
	// StartJob re-discovers once; later chunks must not walk the tree again
	if m.lists > 2*oneWalk {
		t.Fatalf("discovery repeated per chunk: lists=%d (one walk=%d, %d runs)", m.lists, oneWalk, runs)
	}
	for tok, n := range m.exports {
		if n != 1 {
			t.Fatalf("%s exported %d times: %v", tok, n, m.exports)
		}
	}
}
