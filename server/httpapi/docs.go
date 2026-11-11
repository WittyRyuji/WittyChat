package httpapi

// Native collaborative docs — Feishu-style in-app documents.
// REST: create/list/get/rename/delete/share. Realtime editing rides the
// Yjs sync relay in docsync*.go at GET /ws/doc/{id}?token=...
//
// 原生协同文档（飞书式应用内文档）。REST 管元数据；实时共编走
// docsync*.go 的 Yjs 中继 /ws/doc/{id}?token=...

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"wittychat/core/audit"
	"wittychat/core/docs"

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
	mux.HandleFunc("POST /api/v1/docs/{id}/share", d.shareDoc)
	mux.HandleFunc("POST /api/v1/docs/{id}/unshare", d.unshareDoc)
	mux.HandleFunc("DELETE /api/v1/docs/{id}", d.deleteDoc)
	mux.HandleFunc("GET /api/v1/conversations/{id}/docs", d.convDocs)
	mux.HandleFunc("GET /ws/doc/{id}", d.docWS)
	registerDocImport(mux, d)
}

func (d *Deps) docAudit(r *http.Request, uid uuid.UUID, action string, docID uuid.UUID, detail any) {
	if d.Audit != nil {
		d.Audit.Log(r.Context(), &uid, audit.ActorUser, action, docID.String(), detail,
			clientIPCtx(r.Context()))
	}
}

type createDocReq struct {
	Title  string     `json:"title"`
	Kind   string     `json:"kind"`
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
	kind := in.Kind
	if kind == "" {
		kind = "doc"
	}
	doc, err := d.Docs.Create(r.Context(), uid, in.Title, kind, in.ConvID)
	switch {
	case errors.Is(err, docs.ErrBadKind):
		http.Error(w, "bad kind / 文档类型无效", http.StatusBadRequest)
		return
	case errors.Is(err, docs.ErrNotFound):
		http.Error(w, "cannot create / 无权在会话中建文档", http.StatusForbidden)
		return
	case err != nil:
		http.Error(w, "failed", http.StatusInternalServerError)
		return
	}
	d.docAudit(r, uid, "doc.create", doc.ID, map[string]any{"kind": doc.Kind, "conv_id": doc.ConvID})
	_ = json.NewEncoder(w).Encode(doc)
}

// shareDoc — POST /docs/{id}/share {conv_id}: grant a conv's members
// access (sending a doc link into a chat). Requires manage rights unless
// the doc is already visible to that conv; caller must be a member.
// 分享到会话（发送文档链接）：需管理权，若文档已对该会话可见则直接通过；
// 调用者须是会话成员。
func (d *Deps) shareDoc(w http.ResponseWriter, r *http.Request) {
	uid, err := d.bearerUser(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	docID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	var in struct {
		ConvID uuid.UUID `json:"conv_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.ConvID == uuid.Nil {
		http.Error(w, "conv_id required", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	if !d.Docs.CanAccess(ctx, docID, uid) || !d.IM.IsMember(ctx, in.ConvID, uid) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if d.Docs.AccessibleToConv(ctx, docID, in.ConvID) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !d.Docs.CanManage(ctx, docID, uid) {
		http.Error(w, "only the owner or group admins can share / 仅属主或群管理员可分享", http.StatusForbidden)
		return
	}
	if err := d.Docs.Share(ctx, docID, in.ConvID, uid); err != nil {
		http.Error(w, "failed", http.StatusInternalServerError)
		return
	}
	d.docAudit(r, uid, "doc.share", docID, map[string]any{"conv_id": in.ConvID})
	w.WriteHeader(http.StatusNoContent)
}

// unshareDoc — POST /docs/{id}/unshare {conv_id}: revoke a conv's access
// granted by an earlier share. Manager-only; live sockets of members who
// lose access get dropped by the periodic doc revalidation.
// 撤销分享：收回先前分享授予的会话访问权。仅管理者；失权成员的在线
// 连接由文档周期性复核断开。
func (d *Deps) unshareDoc(w http.ResponseWriter, r *http.Request) {
	uid, err := d.bearerUser(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	docID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	var in struct {
		ConvID uuid.UUID `json:"conv_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.ConvID == uuid.Nil {
		http.Error(w, "conv_id required", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	if !d.Docs.CanManage(ctx, docID, uid) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := d.Docs.Unshare(ctx, docID, in.ConvID); err != nil {
		http.Error(w, "failed", http.StatusInternalServerError)
		return
	}
	d.docRevalidateDoc(ctx, docID) // drop sockets whose access just ended / 断开已失权连接
	d.docAudit(r, uid, "doc.unshare", docID, map[string]any{"conv_id": in.ConvID})
	w.WriteHeader(http.StatusNoContent)
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

// convDocs — Feishu-style group docs panel: docs bound or shared to a conv.
// 群文档面板：列出绑定或分享到该会话的文档（成员可见）。
func (d *Deps) convDocs(w http.ResponseWriter, r *http.Request) {
	uid, err := d.bearerUser(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	convID, err := uuid.Parse(r.PathValue("id"))
	if err != nil || !d.IM.IsMember(r.Context(), convID, uid) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	list, err := d.Docs.ListByConv(r.Context(), convID)
	if err != nil {
		http.Error(w, "failed", http.StatusInternalServerError)
		return
	}
	for i := range list {
		list[i].Editors = d.docEditors(list[i].ID)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"docs": list})
}

// deleteDoc — owner, or owner/admin of the bound conv. Soft delete (kept
// for audit) + every live editor is disconnected.
// 删除：文档属主或绑定会话群主/管理员。软删除（留审计）并断开在线编辑者。
func (d *Deps) deleteDoc(w http.ResponseWriter, r *http.Request) {
	uid, err := d.bearerUser(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	docID, err := uuid.Parse(r.PathValue("id"))
	if err != nil || !d.Docs.CanManage(r.Context(), docID, uid) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := d.Docs.Delete(r.Context(), docID, uid); err != nil {
		http.Error(w, "failed", http.StatusInternalServerError)
		return
	}
	docCloseRoom(docID)
	d.docAudit(r, uid, "doc.delete", docID, nil)
	w.WriteHeader(http.StatusNoContent)
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
	d.docAudit(r, uid, "doc.rename", docID, map[string]any{"title": docs.NormTitle(in.Title)})
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
