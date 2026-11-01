package httpapi

// Native collaborative docs — Feishu-style in-app documents.
// REST: create/list/get/rename. Realtime editing rides the Yjs sync
// relay in docsync.go at GET /ws/doc/{id}?token=...
//
// 原生协同文档（飞书式应用内文档）。REST 管元数据；实时共编走
// docsync.go 的 Yjs 中继 /ws/doc/{id}?token=...

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
)

func registerDocs(mux *http.ServeMux, d *Deps) {
	if d.Docs == nil {
		return
	}
	mux.HandleFunc("POST /api/v1/docs", d.createDoc)
	mux.HandleFunc("GET /api/v1/docs", d.listDocs)
	mux.HandleFunc("GET /api/v1/docs/{id}", d.getDoc)
	mux.HandleFunc("POST /api/v1/docs/{id}/rename", d.renameDoc)
	mux.HandleFunc("GET /ws/doc/{id}", d.docWS)
}

type createDocReq struct {
	Title  string     `json:"title"`
	ConvID *uuid.UUID `json:"conv_id"`
}

func (d *Deps) createDoc(w http.ResponseWriter, r *http.Request) {
	uid, err := d.bearerUser(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var in createDocReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	doc, err := d.Docs.Create(r.Context(), uid, in.Title, in.ConvID)
	if err != nil {
		http.Error(w, "cannot create / 无权在会话中建文档", http.StatusForbidden)
		return
	}
	_ = json.NewEncoder(w).Encode(doc)
}

func (d *Deps) listDocs(w http.ResponseWriter, r *http.Request) {
	uid, err := d.bearerUser(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	list, err := d.Docs.List(r.Context(), uid)
	if err != nil {
		http.Error(w, "failed", http.StatusInternalServerError)
		return
	}
	for i := range list {
		list[i].Editors = d.docEditors(list[i].ID)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"docs": list})
}

// docMeta — JSON view for getDoc. 元信息 + 实时在线人数。
func (d *Deps) getDoc(w http.ResponseWriter, r *http.Request) {
	uid, err := d.bearerUser(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	docID, err := uuid.Parse(r.PathValue("id"))
	if err != nil || !d.Docs.CanAccess(r.Context(), docID, uid) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	doc, err := d.Docs.Get(r.Context(), docID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	doc.Editors = d.docEditors(docID)
	_ = json.NewEncoder(w).Encode(doc)
}

func (d *Deps) renameDoc(w http.ResponseWriter, r *http.Request) {
	uid, err := d.bearerUser(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	docID, err := uuid.Parse(r.PathValue("id"))
	if err != nil || !d.Docs.CanAccess(r.Context(), docID, uid) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var in struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if err := d.Docs.Rename(r.Context(), docID, in.Title); err != nil {
		http.Error(w, "failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// docEditors — live connection count for the doc (broker in docsync.go).
func (d *Deps) docEditors(docID uuid.UUID) int {
	return docBrokerCount(docID)
}

// DocMember exposes the access check to the WS relay.
func (d *Deps) docAccess(ctx context.Context, docID, uid uuid.UUID) bool {
	return d.Docs.CanAccess(ctx, docID, uid)
}
