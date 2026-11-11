// devfeishu — dev-only mock Feishu OpenAPI on 127.0.0.1:1180.
// Serves token/wiki/drive/export/contact endpoints; the folder `fldMine`
// holds a docx + sheet owned by admin@wittyloong.com, a docx owned by
// alice@wittyloong.com, one slides, one forbidden docx.
// Usage: go run ./scripts/devfeishu   (then PUT the app base_url to this address)
package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

//go:embed testdata/doc.docx
var docxBytes []byte

//go:embed testdata/sheet.xlsx
var xlsxBytes []byte

func main() {
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
	mux.HandleFunc("/open-apis/drive/v1/files", func(w http.ResponseWriter, r *http.Request) {
		var files []map[string]any
		if r.URL.Query().Get("folder_token") == "fldMine" {
			files = []map[string]any{
				{"token": "dMine", "type": "docx", "name": "我的文档", "owner_id": "ou_admin"},
				{"token": "dAlice", "type": "docx", "name": "Alice的文档", "owner_id": "ou_alice"},
				{"token": "sMine", "type": "sheet", "name": "我的表格", "owner_id": "ou_admin"},
				{"token": "sl1", "type": "slides", "name": "演示", "owner_id": "ou_admin"},
				{"token": "dForb", "type": "docx", "name": "无权限文档", "owner_id": "ou_other"},
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"files": files}})
	})
	mux.HandleFunc("/open-apis/drive/v1/export_tasks", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["token"] == "dForb" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"ticket": "tk_" + in["token"]}})
	})
	mux.HandleFunc("/open-apis/drive/v1/export_tasks/", func(w http.ResponseWriter, r *http.Request) {
		// /export_tasks/<tk>?token=<tok> → done + file_token=tok; file/<ft>/download → bytes
		p := r.URL.Path
		if len(p) > len("/open-apis/drive/v1/export_tasks/file/") && p[:len("/open-apis/drive/v1/export_tasks/file/")] == "/open-apis/drive/v1/export_tasks/file/" {
			ft := p[len("/open-apis/drive/v1/export_tasks/file/") : len(p)-len("/download")]
			if ft == "sMine" {
				_, _ = w.Write(xlsxBytes)
			} else {
				_, _ = w.Write(docxBytes)
			}
			return
		}
		tok := r.URL.Query().Get("token")
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
			"result": map[string]any{"job_status": 0, "file_token": tok},
		}})
	})
	mux.HandleFunc("/open-apis/contact/v3/users/", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Path[len("/open-apis/contact/v3/users/"):]
		email := map[string]string{"ou_admin": "admin@wittyloong.com", "ou_alice": "alice@wittyloong.com", "ou_other": "other@feishu.test"}[id]
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"user": map[string]any{"enterprise_email": email}}})
	})
	mux.HandleFunc("/open-apis/wiki/v2/spaces/get_node", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"node": map[string]any{"obj_token": "dMine", "obj_type": "docx", "title": "我的文档", "owner": "ou_admin", "space_id": "sp1", "has_child": false}}})
	})
	mux.HandleFunc("/open-apis/drive/v1/metas/batch_query", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"metas": []map[string]any{{"doc_token": "dMine", "title": "我的文档", "owner_id": "ou_admin"}}}})
	})
	fmt.Println("devfeishu on 127.0.0.1:1180")
	srv := &http.Server{Addr: "127.0.0.1:1180", Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	_ = srv.ListenAndServe()
}
