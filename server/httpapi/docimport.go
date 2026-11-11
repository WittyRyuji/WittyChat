package httpapi

// Staged doc import: the server stores a payload on an empty doc; the first
// client to open it claims + applies it through the real Yjs editor path.
// 暂存导入：服务端在空文档上存载荷，首个打开的客户端认领并经 Yjs 写入。
// POST /docs/{id}/import/stage {payload, source} — owner only, empty log, ≤16MiB
// POST /docs/{id}/import/claim — editor rights; 204 empty/applied, 409 claimed, 200 payload
// POST /docs/{id}/objects {object_ids} — link objects to the doc for ACL

import (
	"encoding/json"
	"net/http"
	"wittychat/core/docs"

	"github.com/google/uuid"
)

const maxImportPayload = 16 << 20

func registerDocImport(mux *http.ServeMux, d *Deps) {
	mux.HandleFunc("POST /api/v1/docs/{id}/import/stage", d.docImportStage)
	mux.HandleFunc("POST /api/v1/docs/{id}/import/claim", d.docImportClaim)
	mux.HandleFunc("POST /api/v1/docs/{id}/objects", d.docObjectsAdd)
}

func (d *Deps) docImportStage(w http.ResponseWriter, r *http.Request) {
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
	owner, err := d.Docs.OwnerOf(r.Context(), docID)
	if err != nil || owner != uid {
		http.Error(w, "forbidden / 仅属主可暂存", http.StatusForbidden)
		return
	}
	var in struct {
		Payload json.RawMessage `json:"payload"`
		Source  string          `json:"source"`
	}
	// payload ≤ 16MiB: read a bounded body
	r.Body = http.MaxBytesReader(w, r.Body, maxImportPayload+4096)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if len(in.Payload) == 0 || len(in.Payload) > maxImportPayload {
		http.Error(w, "payload missing or >16MiB / 载荷为空或超 16MiB", http.StatusRequestEntityTooLarge)
		return
	}
	// shape check
	var shape struct {
		T string `json:"type"`
	}
	if err := json.Unmarshal(in.Payload, &shape); err != nil ||
		(shape.T != "html" && shape.T != "cells" && shape.T != "file") {
		http.Error(w, "payload.type must be html|cells|file / 类型须为 html|cells|file", http.StatusUnprocessableEntity)
		return
	}
	if err := d.Docs.Stage(r.Context(), docID, in.Payload, in.Source); err != nil {
		http.Error(w, "doc log non-empty or doc missing / 文档已有内容或不存在", http.StatusConflict)
		return
	}
	d.docAudit(r, uid, "doc.import.stage", docID, map[string]any{"source": in.Source})
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "staged"})
}

func (d *Deps) docImportClaim(w http.ResponseWriter, r *http.Request) {
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
	if !d.Docs.CanAccess(r.Context(), docID, uid) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	payload, st, err := d.Docs.Claim(r.Context(), docID)
	if err != nil {
		http.Error(w, "failed", http.StatusInternalServerError)
		return
	}
	switch st {
	case docs.ClaimOK:
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(payload) //nolint:gosec // G705: stored jsonb served as application/json, never rendered as HTML
	case docs.ClaimTaken:
		http.Error(w, "claimed / 已被认领", http.StatusConflict)
	default: // ClaimEmpty | ClaimApplied
		w.WriteHeader(http.StatusNoContent)
	}
}

func (d *Deps) docObjectsAdd(w http.ResponseWriter, r *http.Request) {
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
	if !d.Docs.CanAccess(r.Context(), docID, uid) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var in struct {
		ObjectIDs []uuid.UUID `json:"object_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if len(in.ObjectIDs) > 100 {
		http.Error(w, "at most 100 object_ids / 最多 100 个", http.StatusBadRequest)
		return
	}
	for _, oid := range in.ObjectIDs {
		if _, err := d.canReadObject(r.Context(), oid, uid); err != nil {
			http.Error(w, "object not readable / 对象无权引用", http.StatusForbidden)
			return
		}
	}
	for _, oid := range in.ObjectIDs {
		if _, err := d.Pool.Exec(r.Context(),
			`INSERT INTO doc_objects (doc_id,object_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
			docID, oid); err != nil {
			http.Error(w, "failed", http.StatusInternalServerError)
			return
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]int{"linked": len(in.ObjectIDs)})
}
