package httpapi

// Per-user preference endpoints — cross-device personal settings
// (mail trust, image autoload, UI toggles). Plain KV; keys are
// client-owned "pref.*" strings stored as-is.
//
// 用户偏好接口：跨设备个人信息（邮件信任、自动载图、界面开关）。
// 纯 KV，键为客户端自有 "pref.*" 字符串原样存放。

import (
	"context"
	"encoding/json"
	"net/http"
	"wittychat/shared/apitypes"

	"github.com/danielgtaylor/huma/v2"
)

func registerPrefs(api huma.API, d *Deps) {
	sec := []map[string][]string{{"bearer": {}}}

	huma.Register(api, huma.Operation{
		OperationID: "prefs-get", Method: http.MethodGet,
		Path: "/api/v1/me/prefs", Summary: "All my preferences / 我的全部偏好",
		Tags: []string{"prefs"}, Security: sec,
	}, d.getPrefs)

	huma.Register(api, huma.Operation{
		OperationID: "prefs-merge", Method: http.MethodPatch,
		Path: "/api/v1/me/prefs", Summary: "Merge prefs (null deletes a key) / 合并偏好（null 删除键）",
		Tags: []string{"prefs"}, Security: sec,
	}, d.mergePrefs)
}

func (d *Deps) getPrefs(ctx context.Context, in *struct {
	Headers
},
) (*struct{ Body apitypes.PrefsResponse }, error) {
	_, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	prefs, err := d.Settings.UAll(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	return &struct{ Body apitypes.PrefsResponse }{
		Body: apitypes.PrefsResponse{Prefs: prefs},
	}, nil
}

func (d *Deps) mergePrefs(ctx context.Context, in *struct {
	Headers
	Body apitypes.PrefsMergeRequest
},
) (*struct{ Body apitypes.PrefsResponse }, error) {
	_, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	for k, raw := range in.Body.Prefs {
		if len(k) == 0 || len(k) > 128 {
			return nil, huma.Error400BadRequest("bad pref key / 非法键名")
		}
		if raw == nil || string(raw) == "null" {
			if err := d.Settings.UDelete(ctx, u.ID, k); err != nil {
				return nil, err
			}
			continue
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, huma.Error400BadRequest("bad pref value / 非法值")
		}
		if err := d.Settings.USet(ctx, u.ID, k, v); err != nil {
			return nil, err
		}
	}
	prefs, err := d.Settings.UAll(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	return &struct{ Body apitypes.PrefsResponse }{
		Body: apitypes.PrefsResponse{Prefs: prefs},
	}, nil
}
