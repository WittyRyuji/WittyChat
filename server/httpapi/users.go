// Profile endpoints: self-service rename / avatar, avatar download.
// Avatars are workspace-public: any authenticated user may fetch anyone's.
//
// 个人资料端点：自助改名/换头像、头像下发。头像为工作区内公开——
// 任何已登录用户可取他人头像。
package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"wittychat/core/audit"
	"wittychat/core/objects"
	"wittychat/shared/apitypes"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
)

func registerProfile(api huma.API, mux *http.ServeMux, d *Deps) {
	sec := []map[string][]string{{"bearer": {}}}

	huma.Register(api, huma.Operation{
		OperationID: "update-me", Method: "PATCH",
		Path: "/api/v1/users/me", Summary: "Update own profile (name/avatar) / 修改昵称或头像",
		Security: sec,
	}, d.updateMe)

	mux.HandleFunc("GET /api/v1/users/{id}/avatar", d.userAvatar)
	mux.HandleFunc("GET /api/v1/conversations/{id}/avatar", d.groupAvatar)
}

func (d *Deps) updateMe(ctx context.Context, in *struct {
	Headers
	Body apitypes.UpdateMeRequest
},
) (*struct{ Body apitypes.LoginResponse }, error) {
	_, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	var name *string
	if n := strings.TrimSpace(in.Body.DisplayName); n != "" {
		name = &n
	}
	var avatar *uuid.UUID
	if in.Body.AvatarObjectID != "" {
		oid, err := uuid.Parse(in.Body.AvatarObjectID)
		if err != nil {
			return nil, huma.Error400BadRequest("invalid avatar_object_id")
		}
		avatar = &oid
	}
	if name == nil && avatar == nil {
		return nil, huma.Error400BadRequest("nothing to update")
	}
	if err := d.Store.UpdateProfile(ctx, u.ID, name, avatar); err != nil {
		return nil, imageObjectErr(err)
	}
	if name != nil {
		d.Audit.Log(ctx, &u.ID, audit.ActorUser, "profile.rename", u.Email, nil, clientIPCtx(ctx))
	}
	if avatar != nil {
		d.Audit.Log(ctx, &u.ID, audit.ActorUser, "profile.avatar", u.Email, nil, clientIPCtx(ctx))
	}
	fresh, err := d.Store.UserByID(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	if d.Hub != nil {
		av := ""
		if fresh.AvatarObjectID != nil {
			av = fresh.AvatarObjectID.String()
		}
		d.Hub.BroadcastUser(fresh.ID, fresh.DisplayName, av)
	}
	return &struct{ Body apitypes.LoginResponse }{
		Body: apitypes.LoginResponse{User: publicUser(fresh)},
	}, nil
}

// Shared error mapping for user/group avatars and the workspace logo.
// 用户/群头像与组织 Logo 共用的图片校验错误。
func imageObjectErr(err error) error {
	switch {
	case errors.Is(err, objects.ErrNotFound):
		return huma.Error400BadRequest("image not found / 图片不存在，请重新上传")
	case errors.Is(err, objects.ErrForbidden):
		return huma.Error403Forbidden("image must be your own upload / 须为本人上传的图片")
	case errors.Is(err, objects.ErrNotReady):
		return huma.Error400BadRequest("image unavailable / 图片已删除或尚未完成上传")
	case errors.Is(err, objects.ErrNotImage):
		return huma.Error400BadRequest("invalid image / 请上传有效的 PNG、JPEG、GIF、WebP 或 BMP 图片")
	default:
		return err
	}
}

// Versioned avatar URLs must resolve to the requested object, not a newer
// avatar that would then be cached under the old version.
// 带版本的头像 URL 必须对应所请求对象，避免把新头像缓存到旧版本名下。
func avatarVersion(w http.ResponseWriter, r *http.Request, id uuid.UUID) bool {
	version := r.URL.Query().Get("v")
	if version != "" && version != id.String() {
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "avatar changed", http.StatusNotFound)
		return false
	}
	if version == "" {
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Cache-Control", "private, max-age=300")
	}
	return true
}

// GET /users/{id}/avatar — streams the user's avatar object.
// Authenticated workspace members only (bearer or ?token= for <img>).
func (d *Deps) userAvatar(w http.ResponseWriter, r *http.Request) {
	_, err := d.bearerUser(r)
	if err != nil {
		if tok := r.URL.Query().Get("token"); tok != "" {
			_, _, err = d.requireUser(r.Context(), "Bearer "+tok)
		}
	}
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	target, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	u, err := d.Store.UserByID(r.Context(), target)
	if err != nil || u.AvatarObjectID == nil {
		http.Error(w, "no avatar", http.StatusNotFound)
		return
	}
	if !avatarVersion(w, r, *u.AvatarObjectID) {
		return
	}
	obj, err := d.Objects.Peek(r.Context(), *u.AvatarObjectID)
	if err != nil || obj.Status != "ready" {
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "no avatar", http.StatusNotFound)
		return
	}
	rc, err := d.Objects.Open(r.Context(), obj)
	if err != nil {
		w.Header().Set("Cache-Control", "no-store")
		if errors.Is(err, objects.ErrNotReady) {
			http.Error(w, "not ready", http.StatusConflict)
			return
		}
		http.Error(w, "read failed", http.StatusInternalServerError)
		return
	}
	defer func() { _ = rc.Close() }()
	mime := obj.Mime
	if mime == "" {
		mime = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = io.Copy(w, rc)
}

// GET /conversations/{id}/avatar — the group's avatar, members only.
// 群头像：仅当前成员可取。
func (d *Deps) groupAvatar(w http.ResponseWriter, r *http.Request) {
	uid, err := d.bearerUser(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	convID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	oid, err := d.IM.GroupAvatarFor(r.Context(), convID, uid)
	if err != nil {
		http.Error(w, "no avatar", http.StatusNotFound)
		return
	}
	if !avatarVersion(w, r, oid) {
		return
	}
	obj, err := d.Objects.Peek(r.Context(), oid)
	if err != nil || obj.Status != "ready" {
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "no avatar", http.StatusNotFound)
		return
	}
	rc, err := d.Objects.Open(r.Context(), obj)
	if err != nil {
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "read failed", http.StatusInternalServerError)
		return
	}
	defer func() { _ = rc.Close() }()
	w.Header().Set("Content-Type", obj.Mime)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = io.Copy(w, rc)
}
