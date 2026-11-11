// devfeishu — dev-only mock Feishu OpenAPI on 127.0.0.1:1180.
//
// Docs: the folder `fldMine` holds a docx + sheet owned by admin@wittyloong.com,
// a docx owned by alice@wittyloong.com, one slides, one forbidden docx.
//
// Whole-tenant migration: a directory of five people (Ryuji and Alice exist in
// the dev DB, dave/erin get provisioned, one resigned), a bot endpoint that
// logs every message to stdout, and per-person mailboxes (INBOX with 30
// messages — raw MIME and field-only, with attachments — SENT, 项目/2024).
//
// OAuth: /open-apis/authen/v1/authorize redirects straight back to the
// redirect_uri (no consent page) as DEVFEISHU_OAUTH_USER (default
// alice@wittyloong.com); that user's "我的空间" holds one own docx and one
// colleague's docx.
//
// Usage: go run ./scripts/devfeishu   (then PUT the app base_url to this address)
//
// 开发用模拟飞书：文档导入、整体迁移（通讯录/机器人/邮箱）与员工授权。
package main

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

//go:embed testdata/doc.docx
var docxBytes []byte

//go:embed testdata/sheet.xlsx
var xlsxBytes []byte

type person struct{ openID, name, email string }

var directory = []person{
	{"ou_admin", "Ryuji", "admin@wittyloong.com"},
	{"ou_alice", "Alice", "alice@wittyloong.com"},
	{"ou_dave", "Dave 王", "dave@wittyloong.com"},
	{"ou_erin", "Erin 李", "erin@wittyloong.com"},
}

func enc(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func ok(w http.ResponseWriter, data any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": data})
}

func openIDOf(email string) string {
	for _, p := range directory {
		if p.email == email {
			return p.openID
		}
	}
	return "ou_unknown"
}

// message renders one batch_get item for mailbox mb.
func message(mb, id string) map[string]any {
	var n int
	_, _ = fmt.Sscanf(id, "m%d", &n)
	when := time.Date(2025, 1, 1, 9, 0, 0, 0, time.FixedZone("CST", 8*3600)).Add(time.Duration(n) * 37 * time.Hour)
	m := map[string]any{
		"message_id": mb + "/" + id, "subject": fmt.Sprintf("飞书邮件 #%d", n),
		"internal_date": fmt.Sprint(when.UnixMilli()), "message_state": 1,
		"head_from":       map[string]string{"mail_address": "partner@example.com", "name": "合作方"},
		"to":              []map[string]string{{"mail_address": mb}},
		"body_plain_text": enc(fmt.Sprintf("第 %d 封邮件的正文。", n)),
		"body_html":       enc(fmt.Sprintf("<p>第 <b>%d</b> 封邮件的正文。</p>", n)),
		"smtp_message_id": fmt.Sprintf("<feishu-%s-%d@example.com>", strings.SplitN(mb, "@", 2)[0], n),
		"label_ids":       []string{}, "folder_id": "INBOX",
	}
	if n%3 == 0 {
		m["label_ids"] = []string{"UNREAD"}
	}
	if n%7 == 0 {
		m["label_ids"] = []string{"FLAGGED"}
	}
	switch {
	case n%5 == 0: // raw MIME with a GBK-free UTF-8 subject
		raw := "From: =?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte("原文发件人")) + "?= <raw@example.com>\r\n" +
			"To: " + mb + "\r\nSubject: =?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("原文邮件 #%d", n))) + "?=\r\n" +
			fmt.Sprintf("Message-ID: <raw-%d-%s>\r\n", n, mb) + "Date: " + when.Format(time.RFC1123Z) + "\r\n" +
			"Content-Type: text/plain; charset=utf-8\r\n\r\n这是一封带原始 MIME 的邮件。\r\n"
		m["raw"] = enc(raw)
	case n == 4:
		m["attachments"] = []map[string]any{{"id": "att-note", "filename": "会议纪要.txt", "body": enc("会议纪要：周五发布。"), "attachment_type": 1}}
	case n == 8:
		m["attachments"] = []map[string]any{{"id": "att-big", "filename": "季度报告.pdf", "attachment_type": 2}}
	}
	return m
}

func main() {
	oauthUser := os.Getenv("DEVFEISHU_OAUTH_USER")
	if oauthUser == "" {
		oauthUser = "alice@wittyloong.com"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/open-apis/auth/v3/tenant_access_token/internal", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["app_secret"] == "bad" {
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 99991663, "msg": "invalid app_secret"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "tenant_access_token": "tok", "expire": 3600})
	})
	userTok := func(r *http.Request) bool { return strings.HasPrefix(r.Header.Get("Authorization"), "Bearer u-") }
	mux.HandleFunc("/open-apis/drive/v1/files", func(w http.ResponseWriter, r *http.Request) {
		var files []map[string]any
		switch r.URL.Query().Get("folder_token") {
		case "fldMine":
			files = []map[string]any{
				{"token": "dMine", "type": "docx", "name": "我的文档", "owner_id": "ou_admin"},
				{"token": "dAlice", "type": "docx", "name": "Alice的文档", "owner_id": "ou_alice"},
				{"token": "sMine", "type": "sheet", "name": "我的表格", "owner_id": "ou_admin"},
				{"token": "sl1", "type": "slides", "name": "演示", "owner_id": "ou_admin"},
				{"token": "dForb", "type": "docx", "name": "无权限文档", "owner_id": "ou_other"},
			}
		case "fldUserRoot":
			own := openIDOf(oauthUser)
			files = []map[string]any{
				{"token": "dPersonal", "type": "docx", "name": "个人周报", "owner_id": own},
				{"token": "sPersonal", "type": "sheet", "name": "个人预算", "owner_id": own},
				{"token": "dColleague", "type": "docx", "name": "同事放进来的", "owner_id": "ou_dave"},
			}
		}
		ok(w, map[string]any{"files": files, "has_more": false})
	})
	mux.HandleFunc("/open-apis/drive/explorer/v2/root_folder/meta", func(w http.ResponseWriter, r *http.Request) {
		if !userTok(r) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		ok(w, map[string]any{"token": "fldUserRoot"}) //nolint:gosec // G101: a mock folder token, not a credential
	})
	mux.HandleFunc("/open-apis/drive/v1/export_tasks", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["token"] == "dForb" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		ok(w, map[string]any{"ticket": "tk_" + in["token"]})
	})
	mux.HandleFunc("/open-apis/drive/v1/export_tasks/", func(w http.ResponseWriter, r *http.Request) {
		const fp = "/open-apis/drive/v1/export_tasks/file/"
		if p := r.URL.Path; strings.HasPrefix(p, fp) {
			ft := strings.TrimSuffix(strings.TrimPrefix(p, fp), "/download")
			if strings.HasPrefix(ft, "s") {
				_, _ = w.Write(xlsxBytes)
			} else {
				_, _ = w.Write(docxBytes)
			}
			return
		}
		ok(w, map[string]any{"result": map[string]any{"job_status": 0, "file_token": r.URL.Query().Get("token")}})
	})
	mux.HandleFunc("/open-apis/contact/v3/users/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/open-apis/contact/v3/users/")
		email := "other@feishu.test"
		for _, p := range directory {
			if p.openID == id {
				email = p.email
			}
		}
		ok(w, map[string]any{"user": map[string]any{"enterprise_email": email}})
	})
	mux.HandleFunc("/open-apis/contact/v3/departments/0/children", func(w http.ResponseWriter, _ *http.Request) {
		ok(w, map[string]any{"items": []map[string]any{{"open_department_id": "od-rd"}}, "has_more": false})
	})
	mux.HandleFunc("/open-apis/contact/v3/users/find_by_department", func(w http.ResponseWriter, r *http.Request) {
		active := map[string]any{"is_activated": true}
		var items []map[string]any
		if r.URL.Query().Get("department_id") == "0" {
			for _, p := range directory[:2] {
				items = append(items, map[string]any{"open_id": p.openID, "name": p.name, "enterprise_email": p.email, "status": active})
			}
			items = append(items, map[string]any{
				"open_id": "ou_gone", "name": "已离职", "enterprise_email": "gone@wittyloong.com",
				"status": map[string]any{"is_resigned": true},
			})
		} else {
			for _, p := range directory[2:] {
				items = append(items, map[string]any{"open_id": p.openID, "name": p.name, "enterprise_email": p.email, "status": active})
			}
		}
		ok(w, map[string]any{"items": items, "has_more": false})
	})
	mux.HandleFunc("/open-apis/im/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		var c map[string]string
		_ = json.Unmarshal([]byte(in["content"]), &c)
		log.Printf("BOT → %s: %s", in["receive_id"], strings.ReplaceAll(c["text"], "\n", " | "))
		ok(w, map[string]any{"message_id": "om_dev"})
	})
	mux.HandleFunc("/open-apis/mail/v1/user_mailboxes/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/open-apis/mail/v1/user_mailboxes/"), "/")
		mb, _ := url.PathUnescape(parts[0])
		switch {
		case len(parts) == 2 && parts[1] == "folders":
			ok(w, map[string]any{"items": []map[string]any{
				{"id": "INBOX", "name": "收件箱", "folder_type": 1, "parent_folder_id": "0"},
				{"id": "SENT", "name": "已发送", "folder_type": 1, "parent_folder_id": "0"},
				{"id": "f-proj", "name": "项目", "folder_type": 2, "parent_folder_id": "0"},
				{"id": "f-2024", "name": "2024", "folder_type": 2, "parent_folder_id": "f-proj"},
			}})
		case len(parts) == 2 && parts[1] == "messages":
			var ids []string
			switch r.URL.Query().Get("folder_id") {
			case "INBOX":
				for i := 30; i >= 1; i-- {
					ids = append(ids, fmt.Sprintf("m%d", i))
				}
			case "SENT":
				ids = []string{"m101", "m102"}
			case "f-2024":
				ids = []string{"m201"}
			}
			start := 0
			_, _ = fmt.Sscanf(r.URL.Query().Get("page_token"), "p%d", &start)
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
			for _, id := range in.IDs {
				m := message(mb, id)
				if id == "m101" || id == "m102" {
					m["message_state"], m["folder_id"] = 2, "SENT"
					m["head_from"] = map[string]string{"mail_address": mb}
					m["to"] = []map[string]string{{"mail_address": "partner@example.com", "name": "合作方"}}
				}
				m["message_id"] = id
				msgs = append(msgs, m)
			}
			ok(w, map[string]any{"messages": msgs})
		case len(parts) == 5 && parts[4] == "download_url":
			var urls []map[string]string
			for _, id := range r.URL.Query()["attachment_ids"] {
				urls = append(urls, map[string]string{"attachment_id": id, "download_url": "http://127.0.0.1:1180/dl/" + id})
			}
			ok(w, map[string]any{"download_urls": urls})
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte("%PDF-1.4\n% devfeishu quarterly report\n"))
	})
	mux.HandleFunc("/open-apis/wiki/v2/spaces", func(w http.ResponseWriter, _ *http.Request) {
		ok(w, map[string]any{"items": []map[string]any{{"space_id": "sp-team", "name": "团队知识库"}}, "has_more": false})
	})
	mux.HandleFunc("/open-apis/wiki/v2/spaces/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/get_node") {
			ok(w, map[string]any{"node": map[string]any{"obj_token": "dMine", "obj_type": "docx", "title": "我的文档", "owner": "ou_admin", "space_id": "sp1", "has_child": false}})
			return
		}
		var items []map[string]any
		if strings.Contains(r.URL.Path, "/sp-team/") && r.URL.Query().Get("parent_node_token") == "" {
			items = []map[string]any{
				{"node_token": "wn1", "obj_token": "dWiki", "obj_type": "docx", "title": "新人手册", "node_type": "origin", "owner": "ou_alice"},
			}
		}
		ok(w, map[string]any{"items": items, "has_more": false})
	})
	mux.HandleFunc("/open-apis/drive/v1/metas/batch_query", func(w http.ResponseWriter, _ *http.Request) {
		ok(w, map[string]any{"metas": []map[string]any{{"doc_token": "dMine", "title": "我的文档", "owner_id": "ou_admin"}}})
	})
	// OAuth: no consent page — redirect straight back with a code.
	mux.HandleFunc("/open-apis/authen/v1/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		back, err := url.Parse(q.Get("redirect_uri"))
		if err != nil || (back.Hostname() != "localhost" && back.Hostname() != "127.0.0.1") {
			http.Error(w, "redirect_uri must be a local WittyChat server", http.StatusBadRequest)
			return
		}
		bq := back.Query()
		bq.Set("code", "dev-code")
		bq.Set("state", q.Get("state"))
		back.RawQuery = bq.Encode()
		http.Redirect(w, r, back.String(), http.StatusFound) //nolint:gosec // G710: redirect_uri restricted to loopback above; dev-only mock
	})
	mux.HandleFunc("/oauth/v3/token", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0, "access_token": "u-dev", "refresh_token": "r-dev",
			"expires_in": 7200, "refresh_token_expires_in": 604800,
		})
	})
	mux.HandleFunc("/open-apis/authen/v1/user_info", func(w http.ResponseWriter, _ *http.Request) {
		ok(w, map[string]any{"open_id": openIDOf(oauthUser), "enterprise_email": oauthUser})
	})
	fmt.Println("devfeishu on 127.0.0.1:1180")
	srv := &http.Server{Addr: "127.0.0.1:1180", Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	_ = srv.ListenAndServe()
}
