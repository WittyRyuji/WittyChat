package feishuimport

// Whole-tenant migration against a mock Feishu: directory + provisioning +
// bot links, mail via batch_get (raw and rebuilt), incremental passes, the
// failure journal, permission errors, OAuth personal import, orchestration.
// 整体迁移：通讯录与建号、机器人链接、邮件（原文/重建）、增量、失败台账、
// 权限错误、员工授权导入、编排。

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"wittychat/core/auth"
	"wittychat/core/mail"

	"github.com/google/uuid"
)

func enc(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

type migMock struct {
	mu        sync.Mutex
	sfx       string
	bot       []string        // open_ids messaged
	botText   []string        // message bodies
	inbox     []string        // INBOX message ids (newest first)
	missing   map[string]bool // batch_get omits these once
	listCalls int
	got       map[string]int  // batch_get count per id
	denied    map[string]bool // mailbox → 403
	userEmail string          // OAuth user_info email
	refreshes int
}

func (m *migMock) email(who string) string { return who + "-" + m.sfx + "@wittyloong.com" }

func newMigMock(t *testing.T, m *migMock) *httptest.Server {
	t.Helper()
	m.got = map[string]int{}
	ok := func(w http.ResponseWriter, data any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": data})
	}
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/open-apis/auth/v3/tenant_access_token/internal", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "tenant_access_token": "tok", "expire": 3600})
	})
	mux.HandleFunc("/open-apis/contact/v3/departments/0/children", func(w http.ResponseWriter, _ *http.Request) {
		ok(w, map[string]any{"items": []map[string]any{{"open_department_id": "od1"}}, "has_more": false})
	})
	mux.HandleFunc("/open-apis/contact/v3/users/find_by_department", func(w http.ResponseWriter, r *http.Request) {
		active := map[string]any{"is_activated": true}
		var items []map[string]any
		switch r.URL.Query().Get("department_id") {
		case "0":
			items = []map[string]any{
				{"open_id": "ou_old", "name": "老员工", "enterprise_email": strings.ToUpper(m.email("old")), "status": active},
				{"open_id": "ou_new", "name": "新员工", "enterprise_email": m.email("new"), "status": active},
				{"open_id": "ou_gone", "name": "离职", "enterprise_email": m.email("gone"), "status": map[string]any{"is_resigned": true}},
				{"open_id": "ou_noemail", "name": "无邮箱", "status": active},
			}
		case "od1":
			items = []map[string]any{
				{"open_id": "ou_old", "name": "老员工", "enterprise_email": m.email("old"), "status": active},
				{"open_id": "ou_new2", "name": "新员工二", "email": m.email("new2"), "status": active},
			}
		}
		ok(w, map[string]any{"items": items, "has_more": false})
	})
	mux.HandleFunc("/open-apis/im/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		m.mu.Lock()
		m.bot = append(m.bot, in["receive_id"])
		m.botText = append(m.botText, in["content"])
		m.mu.Unlock()
		if in["receive_id"] == "ou_new2" { // not in the app's availability scope
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 230013, "msg": "bot has no availability to this user"})
			return
		}
		ok(w, map[string]any{"message_id": "om_1"})
	})
	mux.HandleFunc("/open-apis/mail/v1/user_mailboxes/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/open-apis/mail/v1/user_mailboxes/")
		parts := strings.Split(rest, "/")
		mb := parts[0]
		if m.denied[mb] {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch {
		case len(parts) == 2 && parts[1] == "folders":
			ok(w, map[string]any{"items": []map[string]any{
				{"id": "INBOX", "name": "收件箱", "folder_type": 1, "parent_folder_id": "0"},
				{"id": "SENT", "name": "已发送", "folder_type": 1, "parent_folder_id": "0"},
				{"id": "f-proj", "name": "项目", "folder_type": 2, "parent_folder_id": "0"},
				{"id": "f-2024", "name": "2024", "folder_type": 2, "parent_folder_id": "f-proj"},
			}})
		case len(parts) == 2 && parts[1] == "messages":
			m.mu.Lock()
			m.listCalls++
			var ids []string
			switch r.URL.Query().Get("folder_id") {
			case "INBOX":
				ids = append(ids, m.inbox...)
			case "SENT":
				ids = []string{"s1"}
			case "f-2024":
				ids = []string{"p1"}
			}
			m.mu.Unlock()
			start := 0
			if pt := r.URL.Query().Get("page_token"); pt != "" {
				_, _ = fmt.Sscanf(pt, "p%d", &start)
			}
			end := min(start+20, len(ids))
			out := map[string]any{"items": ids[start:end], "has_more": end < len(ids)}
			if end < len(ids) {
				out["page_token"] = fmt.Sprintf("p%d", end)
			}
			ok(w, out)
		case len(parts) == 3 && parts[2] == "batch_get":
			var in struct {
				IDs []string `json:"message_ids"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			var msgs []map[string]any
			m.mu.Lock()
			for _, id := range in.IDs {
				m.got[id]++
				if m.missing[id] {
					delete(m.missing, id)
					continue
				}
				msgs = append(msgs, m.message(id, srv.URL))
			}
			m.mu.Unlock()
			ok(w, map[string]any{"messages": msgs})
		case len(parts) == 5 && parts[4] == "download_url":
			var urls []map[string]string
			for _, id := range r.URL.Query()["attachment_ids"] {
				urls = append(urls, map[string]string{"attachment_id": id, "download_url": srv.URL + "/dl/" + id})
			}
			ok(w, map[string]any{"download_urls": urls})
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte("%PDF-1.4 big attachment"))
	})
	// OAuth + user-token drive
	mux.HandleFunc("/oauth/v3/token", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		m.mu.Lock()
		if in["grant_type"] == "refresh_token" {
			m.refreshes++
		}
		n := m.refreshes
		m.mu.Unlock()
		if in["grant_type"] == "authorization_code" && in["code"] != "good-code" {
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 20003, "error": "invalid_grant", "error_description": "code expired"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0, "access_token": fmt.Sprintf("u-%d", n),
			"refresh_token": fmt.Sprintf("r-%d", n), "expires_in": 7200, "refresh_token_expires_in": 604800,
		})
	})
	mux.HandleFunc("/open-apis/authen/v1/user_info", func(w http.ResponseWriter, _ *http.Request) {
		ok(w, map[string]any{"open_id": "ou_me_user", "enterprise_email": m.userEmail})
	})
	mux.HandleFunc("/open-apis/drive/explorer/v2/root_folder/meta", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer u-") {
			w.WriteHeader(http.StatusForbidden) // the app itself has no "my space"
			return
		}
		ok(w, map[string]any{"token": "fldRoot"})
	})
	mux.HandleFunc("/open-apis/drive/v1/files", func(w http.ResponseWriter, r *http.Request) {
		var files []map[string]any
		if r.URL.Query().Get("folder_token") == "fldRoot" {
			files = []map[string]any{
				{"token": "dOwn", "type": "docx", "name": "我的周报", "owner_id": "ou_me_user"},
				{"token": "dShared", "type": "docx", "name": "别人放进来的", "owner_id": "ou_someone"},
			}
		}
		ok(w, map[string]any{"files": files, "has_more": false})
	})
	mux.HandleFunc("/open-apis/wiki/v2/spaces/my_library/nodes", func(w http.ResponseWriter, _ *http.Request) {
		ok(w, map[string]any{"items": []map[string]any{}, "has_more": false})
	})
	mux.HandleFunc("/open-apis/wiki/v2/spaces", func(w http.ResponseWriter, _ *http.Request) {
		ok(w, map[string]any{"items": []map[string]any{}, "has_more": false})
	})
	mux.HandleFunc("/open-apis/contact/v3/users/ou_someone", func(w http.ResponseWriter, _ *http.Request) {
		ok(w, map[string]any{"user": map[string]any{"enterprise_email": "someone@feishu.test"}})
	})
	mux.HandleFunc("/open-apis/drive/v1/export_tasks", func(w http.ResponseWriter, _ *http.Request) {
		ok(w, map[string]any{"ticket": "tk"})
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
		ok(w, map[string]any{"result": map[string]any{"job_status": 0, "file_token": "f"}})
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// message renders a Feishu batch_get item. i1 carries raw MIME; the rest are
// field-only; i2 has an inline attachment body, i3 needs download_url.
func (m *migMock) message(id, _ string) map[string]any {
	base := map[string]any{
		"message_id": id, "subject": "主题 " + id, "internal_date": "1700000000000", "message_state": 1,
		"head_from":       map[string]string{"mail_address": "Boss@Example.com", "name": "老板"},
		"to":              []map[string]string{{"mail_address": m.email("old"), "name": "老员工"}},
		"body_plain_text": enc("正文 " + id), "body_html": enc("<p>正文 " + id + "</p>"),
		"smtp_message_id": "<" + id + "@feishu.test>", "label_ids": []string{"UNREAD"}, "folder_id": "INBOX",
	}
	switch id {
	case "i1":
		raw := "From: Raw Sender <raw@example.com>\r\nTo: " + m.email("old") + "\r\nSubject: =?UTF-8?B?" +
			base64.StdEncoding.EncodeToString([]byte("原文邮件")) + "?=\r\nMessage-ID: <raw-1@example.com>\r\n" +
			"Date: Mon, 02 Jan 2023 15:04:05 +0800\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n原文正文\r\n"
		base["raw"] = enc(raw)
		base["label_ids"] = []string{"FLAGGED"}
	case "i2":
		base["attachments"] = []map[string]any{{"id": "a2", "filename": "说明.txt", "body": enc("hello"), "attachment_type": 1}}
	case "i3":
		base["attachments"] = []map[string]any{{"id": "a3", "filename": "大附件.pdf", "attachment_type": 2}}
	case "s1":
		base["message_state"] = 2
		base["label_ids"] = []string{}
	}
	return base
}

func migEnv(t *testing.T) (*Store, context.Context, *migMock, *httptest.Server, uuid.UUID) {
	t.Helper()
	s, ctx, admin, _, sfx := env(t)
	s.WithMigration(mail.NewStore(s.pool, "wittyloong.com"), auth.NewStore(s.pool), "http://wc.test")
	// The tests share one PG: clear migration state left by earlier runs.
	for _, q := range []string{
		`DELETE FROM feishu_people`, `DELETE FROM feishu_migration`, `DELETE FROM feishu_user_tokens`,
		`DELETE FROM bg_tasks WHERE kind IN ('feishu_mail','feishu_migrate','feishu_import')`,
	} {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	m := &migMock{sfx: sfx, missing: map[string]bool{}, denied: map[string]bool{}}
	for i := 25; i >= 1; i-- {
		m.inbox = append(m.inbox, fmt.Sprintf("i%d", i))
	}
	srv := newMigMock(t, m)
	_ = s.ClearApp(ctx)
	if err := s.SetApp(ctx, "cli_test", "secret", srv.URL, admin); err != nil {
		t.Fatal(err)
	}
	return s, ctx, m, srv, admin
}

func mkUser(ctx context.Context, t *testing.T, s *Store, email string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO users (id,email,display_name,status,is_admin) VALUES ($1,$2,'u','active',false) RETURNING id`,
		uuid.Must(uuid.NewV7()), email).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func person(ctx context.Context, t *testing.T, s *Store, openID string) Person {
	t.Helper()
	ps, err := s.People(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.OpenID == openID {
			return p
		}
	}
	t.Fatalf("person %s not found", openID)
	return Person{}
}

func TestDirectoryAndProvision(t *testing.T) {
	s, ctx, m, _, admin := migEnv(t)
	old := mkUser(ctx, t, s, m.email("old"))
	st, err := s.SyncDirectory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Seen != 5 || st.Active != 4 || st.Matched != 1 || st.Unmatched != 2 || st.NoEmail != 1 {
		t.Fatalf("stats %+v", st)
	}
	if p := person(ctx, t, s, "ou_old"); p.UserID == nil || *p.UserID != old {
		t.Fatalf("ou_old not linked (case-insensitive email): %+v", p)
	}
	n, err := s.Provision(ctx, admin)
	if err != nil || n != 2 {
		t.Fatalf("provision n=%d err=%v", n, err)
	}
	pn := person(ctx, t, s, "ou_new")
	if pn.UserID == nil || !pn.Provisioned || pn.UserStatus != "pending" || pn.LinkSentAt == nil || pn.LinkError != "" {
		t.Fatalf("ou_new: %+v", pn)
	}
	var addr string
	_ = s.pool.QueryRow(ctx, `SELECT address FROM mail_addresses WHERE user_id=$1 AND kind='primary'`, *pn.UserID).Scan(&addr)
	if addr != m.email("new") {
		t.Fatalf("primary address %q", addr)
	}
	if p := person(ctx, t, s, "ou_new2"); p.LinkSentAt != nil || !strings.Contains(p.LinkError, "230013") {
		t.Fatalf("bot failure not recorded: %+v", p)
	}
	if p := person(ctx, t, s, "ou_gone"); p.UserID != nil {
		t.Fatal("resigned person got an account")
	}
	if len(m.bot) != 2 || !strings.Contains(m.botText[0], "http://wc.test/activate?token=") {
		t.Fatalf("bot sends %v %v", m.bot, m.botText)
	}
	// idempotent: no new accounts, no new messages
	if _, err := s.SyncDirectory(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Provision(ctx, admin); n != 0 || len(m.bot) != 2 {
		t.Fatalf("re-provision n=%d sends=%d", n, len(m.bot))
	}
	if n, err := s.ResendLinks(ctx, admin, []string{"ou_new"}); err != nil || n != 1 || len(m.bot) != 3 {
		t.Fatalf("resend n=%d err=%v sends=%d", n, err, len(m.bot))
	}
}

func runMail(ctx context.Context, t *testing.T, s *Store, openID string) {
	t.Helper()
	for i := 0; i < 50; i++ {
		more, err := s.RunMail(ctx, openID, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			return
		}
	}
	t.Fatal("mail pass did not finish")
}

func TestMailMigration(t *testing.T) {
	s, ctx, m, _, _ := migEnv(t)
	old := mkUser(ctx, t, s, m.email("old"))
	if _, err := s.SyncDirectory(ctx); err != nil {
		t.Fatal(err)
	}
	m.missing["i7"] = true // first batch_get omits it → journaled, retried next pass
	if err := s.StartMailFor(ctx, "ou_old", true); err != nil {
		t.Fatal(err)
	}
	runMail(ctx, t, s, "ou_old")
	p := person(ctx, t, s, "ou_old")
	if p.MailStatus != "done" || p.MailDone != 26 || p.MailFailed != 1 {
		t.Fatalf("after full pass: %+v", p)
	}
	count := func(where string, args ...any) int {
		var n int
		q := `SELECT count(*) FROM mail_messages mm JOIN mail_folders f ON f.id=mm.folder_id WHERE mm.owner_user_id=$1 AND ` + where
		if err := s.pool.QueryRow(ctx, q, append([]any{old}, args...)...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(`f.kind='inbox'`); n != 24 {
		t.Fatalf("inbox=%d want 24", n)
	}
	if n := count(`f.kind='sent' AND mm.direction='out'`); n != 1 {
		t.Fatalf("sent=%d", n)
	}
	if n := count(`f.kind='custom' AND f.name='项目/2024'`); n != 1 {
		t.Fatalf("custom folder path missing")
	}
	var subj string
	var starred, read bool
	var raw *uuid.UUID
	_ = s.pool.QueryRow(ctx, `SELECT subject,is_starred,is_read,raw_object_id FROM mail_messages WHERE owner_user_id=$1 AND rfc_msgid='raw-1@example.com'`, old).
		Scan(&subj, &starred, &read, &raw)
	if subj != "原文邮件" || !starred || !read || raw == nil {
		t.Fatalf("raw message: %q starred=%v read=%v raw=%v", subj, starred, read, raw)
	}
	if n := count(`mm.is_read=false AND mm.rfc_msgid='i2@feishu.test'`); n != 1 {
		t.Fatal("UNREAD label not honoured")
	}
	var atts int
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM mail_attachments a JOIN mail_messages mm ON mm.id=a.message_id
		WHERE mm.owner_user_id=$1 AND mm.rfc_msgid IN ('i2@feishu.test','i3@feishu.test')`, old).Scan(&atts)
	if atts != 2 {
		t.Fatalf("attachments=%d want 2 (inline body + download_url)", atts)
	}
	// incremental: one new message on top → only the first INBOX page is read
	m.mu.Lock()
	m.inbox = append([]string{"i26"}, m.inbox...)
	m.listCalls = 0
	m.mu.Unlock()
	if err := s.StartMailFor(ctx, "ou_old", false); err != nil {
		t.Fatal(err)
	}
	runMail(ctx, t, s, "ou_old")
	p = person(ctx, t, s, "ou_old")
	if p.MailDone != 28 || p.MailFailed != 0 { // i26 + the retried i7
		t.Fatalf("after incremental: %+v", p)
	}
	// INBOX page 1 holds the new mail so page 2 is read too and found fully
	// imported; then SENT, 项目, 2024 stop at their first page.
	if m.listCalls != 5 {
		t.Fatalf("incremental listed %d pages, want 5", m.listCalls)
	}
	for id, n := range m.got {
		if n > 1 && id != "i7" {
			t.Fatalf("%s fetched %d times", id, n)
		}
	}
	// permission problem → readable error, no retry
	m.denied[m.email("old")] = true
	_ = s.StartMailFor(ctx, "ou_old", true)
	if more, err := s.RunMail(ctx, "ou_old", time.Minute); more || err != nil {
		t.Fatalf("denied: more=%v err=%v", more, err)
	}
	if p := person(ctx, t, s, "ou_old"); p.MailStatus != "error" || !strings.Contains(p.MailError, "邮件数据权限") {
		t.Fatalf("denied: %+v", p)
	}
}

func TestOAuthPersonalImport(t *testing.T) {
	s, ctx, m, srv, _ := migEnv(t)
	me := mkUser(ctx, t, s, m.email("oauth"))
	m.userEmail = m.email("oauth")
	u, err := s.OAuthStart(ctx, me)
	if err != nil || !strings.HasPrefix(u, srv.URL+"/open-apis/authen/v1/authorize?") ||
		!strings.Contains(u, "redirect_uri=http%3A%2F%2Fwc.test%2Fapi%2Fv1%2Ffeishu%2Foauth%2Fcallback") {
		t.Fatalf("authorize url %q err=%v", u, err)
	}
	state := u[strings.Index(u, "state=")+6:]
	if _, err := s.OAuthCallback(ctx, "bad-code", state); err == nil {
		t.Fatal("bad code accepted")
	}
	if _, err := s.OAuthCallback(ctx, "good-code", state); err == nil {
		t.Fatal("state reused")
	}
	// someone else's Feishu account → refused, nothing stored
	other := mkUser(ctx, t, s, m.email("other"))
	u2, _ := s.OAuthStart(ctx, other)
	if _, err := s.OAuthCallback(ctx, "good-code", u2[strings.Index(u2, "state=")+6:]); err == nil || !strings.Contains(err.Error(), "邮箱不一致") {
		t.Fatalf("mismatch: %v", err)
	}
	if s.Authorized(ctx, other) {
		t.Fatal("mismatched account stored a token")
	}
	u3, _ := s.OAuthStart(ctx, me)
	job, err := s.OAuthCallback(ctx, "good-code", u3[strings.Index(u3, "state=")+6:])
	if err != nil || !s.Authorized(ctx, me) {
		t.Fatalf("callback: %v", err)
	}
	var sealed []byte
	_ = s.pool.QueryRow(ctx, `SELECT sealed_refresh FROM feishu_user_tokens WHERE user_id=$1`, me).Scan(&sealed)
	if bytes.Contains(sealed, []byte("r-0")) {
		t.Fatal("refresh token stored in plaintext")
	}
	runToEnd(ctx, t, s, job, time.Minute, 3)
	if st, _ := itemState(ctx, t, s, job, "dOwn"); st != "done" {
		t.Fatalf("own doc: %s", st)
	}
	if st, e := itemState(ctx, t, s, job, "dShared"); st != "skipped" || !strings.Contains(e, "不是你的文档") {
		t.Fatalf("shared doc: %s %q", st, e)
	}
	if got := docsOf(ctx, t, s, me); len(got) != 1 || got[0] != "我的空间/我的周报" {
		t.Fatalf("docs %v", got)
	}
	if m.refreshes != 1 {
		t.Fatalf("refreshes=%d", m.refreshes)
	}
	// authorizing again reuses the job: no duplicate docs
	u4, _ := s.OAuthStart(ctx, me)
	job2, err := s.OAuthCallback(ctx, "good-code", u4[strings.Index(u4, "state=")+6:])
	if err != nil || job2 != job {
		t.Fatalf("re-authorize: job %v vs %v err=%v", job2, job, err)
	}
	runToEnd(ctx, t, s, job, time.Minute, 3)
	if got := docsOf(ctx, t, s, me); len(got) != 1 {
		t.Fatalf("re-authorize duplicated docs: %v", got)
	}
}

func TestRunMigratePipeline(t *testing.T) {
	s, ctx, m, _, admin := migEnv(t)
	mkUser(ctx, t, s, m.email("old"))
	if err := s.StartMigration(ctx, admin, true, true); err != nil {
		t.Fatal(err)
	}
	if err := s.RunMigrate(ctx); err != nil {
		t.Fatal(err)
	}
	mg, err := s.GetMigration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if mg.Status != "syncing" || mg.People != 4 || mg.Linked != 3 || mg.Provisioned != 2 {
		t.Fatalf("migration %+v", mg)
	}
	var tasks int
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM bg_tasks WHERE kind='feishu_mail' AND status='queued'`).Scan(&tasks)
	if tasks != 3 {
		t.Fatalf("mail tasks=%d want 3", tasks)
	}
	// every linked person got exactly one docs notice in addition to links
	notes := 0
	for _, b := range m.botText {
		if strings.Contains(b, "一键导入我的飞书文档") {
			notes++
		}
	}
	if notes != 3 {
		t.Fatalf("docs notices=%d", notes)
	}
	for _, id := range []string{"ou_old", "ou_new", "ou_new2"} {
		runMail(ctx, t, s, id)
	}
	if err := s.FinishMigration(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"ou_old", "ou_new", "ou_new2"} {
		runMail(ctx, t, s, id)
	}
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if mg, _ := s.GetMigration(ctx); mg.Status != "finished" || mg.FinishedAt == nil {
		t.Fatalf("not finished: %+v", mg)
	}
}
