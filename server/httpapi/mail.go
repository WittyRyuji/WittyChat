package httpapi

// Mail REST: folders, message list/get/search, compose (internal direct
// delivery + external enqueue), flags, move, aliases.
//
// 邮件 REST：文件夹/列表/详情/搜索/发送（内部直投+外部入队）/标记/移动/别名。

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"wittychat/core/mail"

	"github.com/google/uuid"
)

func registerMail(mux *http.ServeMux, d *Deps) {
	mux.HandleFunc("GET /api/v1/mail/folders", d.mailFolders)
	mux.HandleFunc("GET /api/v1/mail/messages", d.mailList) // ?folder=&offset=&limit=
	mux.HandleFunc("GET /api/v1/mail/search", d.mailSearch) // ?q=
	mux.HandleFunc("GET /api/v1/mail/messages/{id}", d.mailGet)
	mux.HandleFunc("POST /api/v1/mail/send", d.mailSend)
	mux.HandleFunc("POST /api/v1/mail/messages/{id}/flags", d.mailFlags)
	mux.HandleFunc("POST /api/v1/mail/messages/{id}/move", d.mailMove)
	mux.HandleFunc("GET /api/v1/mail/addresses", d.mailAddresses)
	mux.HandleFunc("POST /api/v1/mail/addresses", d.mailAddAddress)
	mux.HandleFunc("DELETE /api/v1/mail/addresses/{id}", d.mailDelAddress)
}

func (d *Deps) mailUser(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	uid, err := d.bearerUser(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return uuid.Nil, false
	}
	return uid, true
}

func (d *Deps) mailFolders(w http.ResponseWriter, r *http.Request) {
	uid, ok := d.mailUser(w, r)
	if !ok {
		return
	}
	fs, err := d.Mail.Folders(r.Context(), uid)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(fs)
}

func (d *Deps) mailList(w http.ResponseWriter, r *http.Request) {
	uid, ok := d.mailUser(w, r)
	if !ok {
		return
	}
	folder, err := uuid.Parse(r.URL.Query().Get("folder"))
	if err != nil {
		http.Error(w, "folder required", http.StatusBadRequest)
		return
	}
	ms, err := d.Mail.List(r.Context(), uid, folder, 50, atoiDef(r.URL.Query().Get("offset"), 0))
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(ms)
}

func (d *Deps) mailGet(w http.ResponseWriter, r *http.Request) {
	uid, ok := d.mailUser(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	m, err := d.Mail.Get(r.Context(), uid, id)
	if errors.Is(err, mail.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	_ = d.Mail.SetFlags(r.Context(), uid, id, boolp(true), nil) // mark read on open
	_ = json.NewEncoder(w).Encode(m)
}

func (d *Deps) mailSearch(w http.ResponseWriter, r *http.Request) {
	uid, ok := d.mailUser(w, r)
	if !ok {
		return
	}
	q := r.URL.Query().Get("q")
	if q == "" {
		http.Error(w, "q required", http.StatusBadRequest)
		return
	}
	ms, err := d.Mail.Search(r.Context(), uid, q, 50)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(ms)
}

func (d *Deps) mailSend(w http.ResponseWriter, r *http.Request) {
	uid, ok := d.mailUser(w, r)
	if !ok {
		return
	}
	var req struct {
		To        []string    `json:"to"`
		Cc        []string    `json:"cc"`
		Subject   string      `json:"subject"`
		Body      string      `json:"body"`
		InReplyTo string      `json:"in_reply_to"`
		ObjectIDs []uuid.UUID `json:"object_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.To) == 0 {
		http.Error(w, "bad request / 请求无效", http.StatusBadRequest)
		return
	}
	msgID, external, err := d.Mail.Compose(r.Context(), uid, req.To, req.Cc,
		req.Subject, req.Body, req.InReplyTo, req.ObjectIDs)
	if err != nil {
		http.Error(w, "send failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	for _, rcpt := range external {
		_ = d.Objects.Enqueue(r.Context(), "mail_send", map[string]any{
			"message_id": msgID.String(), "rcpt": rcpt,
		})
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": msgID.String(), "external_queued": len(external),
	})
}

func (d *Deps) mailFlags(w http.ResponseWriter, r *http.Request) {
	uid, ok := d.mailUser(w, r)
	if !ok {
		return
	}
	id, _ := uuid.Parse(r.PathValue("id"))
	var req struct {
		Read    *bool `json:"read"`
		Starred *bool `json:"starred"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := d.Mail.SetFlags(r.Context(), uid, id, req.Read, req.Starred); err != nil {
		http.Error(w, "failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d *Deps) mailMove(w http.ResponseWriter, r *http.Request) {
	uid, ok := d.mailUser(w, r)
	if !ok {
		return
	}
	id, _ := uuid.Parse(r.PathValue("id"))
	var req struct {
		Folder uuid.UUID `json:"folder"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := d.Mail.Move(r.Context(), uid, id, req.Folder); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d *Deps) mailAddresses(w http.ResponseWriter, r *http.Request) {
	uid, ok := d.mailUser(w, r)
	if !ok {
		return
	}
	as, err := d.Mail.Addresses(r.Context(), uid)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(as)
}

func (d *Deps) mailAddAddress(w http.ResponseWriter, r *http.Request) {
	uid, ok := d.mailUser(w, r)
	if !ok {
		return
	}
	var req struct {
		Address string `json:"address"`
		Kind    string `json:"kind"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Address == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Kind == "" {
		req.Kind = "alias"
	}
	if err := d.Mail.AddAddress(r.Context(), uid, req.Address, req.Kind); err != nil {
		http.Error(w, "failed", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (d *Deps) mailDelAddress(w http.ResponseWriter, r *http.Request) {
	uid, ok := d.mailUser(w, r)
	if !ok {
		return
	}
	id, _ := uuid.Parse(r.PathValue("id"))
	_ = d.Mail.RemoveAddress(r.Context(), uid, id)
	w.WriteHeader(http.StatusNoContent)
}

func boolp(b bool) *bool { return &b }

func atoiDef(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
