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
		obj, err := d.Objects.Peek(ctx, oid)
		if err != nil {
			return nil, huma.Error400BadRequest("avatar object not found")
		}
		if obj.Owner != u.ID {
			return nil, huma.Error403Forbidden("avatar must be your own upload")
		}
		avatar = &oid
	}
	if name == nil && avatar == nil {
		return nil, huma.Error400BadRequest("nothing to update")
	}
	if err := d.Store.UpdateProfile(ctx, u.ID, name, avatar); err != nil {
		return nil, err
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
	return &struct{ Body apitypes.LoginResponse }{
		Body: apitypes.LoginResponse{User: publicUser(fresh)},
	}, nil
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
	obj, err := d.Objects.Peek(r.Context(), *u.AvatarObjectID)
	if err != nil {
		http.Error(w, "no avatar", http.StatusNotFound)
		return
	}
	rc, err := d.Objects.Open(r.Context(), obj)
	if err != nil {
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
	w.Header().Set("Cache-Control", "private, max-age=300")
	_, _ = io.Copy(w, rc)
}
