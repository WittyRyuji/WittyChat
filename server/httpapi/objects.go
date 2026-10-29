package httpapi

// Attachment endpoints: direct upload (<=5MB), member-authorized download,
// thumbnail passthrough. Upload enqueues a thumb task for images.
//
// 附件端点：直传（≤5MB）、会话成员鉴权下载、缩略图。
// 图片上传后入队缩略图任务。

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"wittychat/core/objects"

	"github.com/google/uuid"
)

func registerObjects(mux *http.ServeMux, d *Deps) {
	mux.HandleFunc("POST /api/v1/conversations/{id}/attachments", d.uploadAttachment)
	mux.HandleFunc("GET /api/v1/objects/{id}/download", d.downloadObject)
	mux.HandleFunc("GET /api/v1/objects/{id}/thumb", d.thumbObject)
}

func (d *Deps) bearerUser(r *http.Request) (uuid.UUID, error) {
	_, u, err := d.requireUser(r.Context(), r.Header.Get("Authorization"))
	if err != nil {
		return uuid.Nil, err
	}
	return u.ID, nil
}

func (d *Deps) uploadAttachment(w http.ResponseWriter, r *http.Request) {
	uid, err := d.bearerUser(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	convID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad conv id", http.StatusBadRequest)
		return
	}
	if !d.IM.IsMember(r.Context(), convID, uid) {
		http.Error(w, "not a member", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, objects.MaxDirectUpload+1<<20)
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file / 缺少文件", http.StatusBadRequest)
		return
	}
	defer func() { _ = file.Close() }()

	obj, dedup, err := d.Objects.Put(r.Context(), uid, convID, "attachment",
		header.Filename, header.Header.Get("Content-Type"), file, objects.MaxDirectUpload)
	if err != nil {
		if errors.Is(err, objects.ErrTooLarge) {
			http.Error(w, "file too large / 文件过大", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "upload failed", http.StatusInternalServerError)
		return
	}
	// Image → enqueue thumbnail (worker picks it up).
	if !dedup && strings.HasPrefix(obj.Mime, "image/") {
		_ = d.Objects.Enqueue(r.Context(), "thumb", map[string]any{
			"object_id": obj.ID.String(), "owner": uid.String(),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": obj.ID.String(), "name": obj.Name, "size": obj.Size,
		"mime": obj.Mime, "deduplicated": dedup,
	})
}

func (d *Deps) downloadObject(w http.ResponseWriter, r *http.Request) {
	uid, err := d.bearerUser(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	objID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	obj, err := d.Objects.Get(r.Context(), objID, uid)
	if err != nil {
		if errors.Is(err, objects.ErrForbidden) {
			http.Error(w, "forbidden", http.StatusForbidden)
		} else {
			http.Error(w, "not found", http.StatusNotFound)
		}
		return
	}
	rc, err := d.Objects.Open(r.Context(), obj)
	if err != nil {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	defer func() { _ = rc.Close() }()
	if obj.Mime != "" {
		w.Header().Set("Content-Type", obj.Mime)
	}
	w.Header().Set("Content-Disposition", `attachment; filename*=UTF-8''`+escapeName(obj.Name))
	_, _ = io.Copy(w, rc) // stream / 流式
}

func (d *Deps) thumbObject(w http.ResponseWriter, r *http.Request) {
	uid, err := d.bearerUser(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	objID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if _, err := d.Objects.Get(r.Context(), objID, uid); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// Path derived from a parsed uuid under a fixed data dir — safe.
	//nolint:gosec // no user-controlled path component
	http.ServeFile(w, r, d.Objects.ThumbPath(objID))
}

func escapeName(s string) string {
	// RFC 5987 minimal: percent-encode non-ASCII for the header.
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 0x20 && c < 0x7f && c != '"' && c != '%' {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			const hexd = "0123456789ABCDEF"
			b.WriteByte(hexd[c>>4])
			b.WriteByte(hexd[c&0xf])
		}
	}
	return b.String()
}
