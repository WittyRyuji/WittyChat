package httpapi

// Feishu cloud-doc import API: admin sets the workspace app once; any user
// creates self-import jobs by pasting Feishu links. 飞书导入 API。

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"strings"
	"wittychat/core/audit"
	"wittychat/core/feishuimport"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
)

type fiJobOut struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Mode      string `json:"mode"`
	Status    string `json:"status"`
	CreatedBy string `json:"created_by"`
	Total     int    `json:"total"`
	Done      int    `json:"done"`
	Skipped   int    `json:"skipped"`
	Failed    int    `json:"failed"`
	Pending   int    `json:"pending"`
}
type fiItemOut struct {
	ObjType string   `json:"obj_type"`
	Title   string   `json:"title"`
	Path    string   `json:"path"`
	Status  string   `json:"status"`
	Error   string   `json:"error"`
	DocIDs  []string `json:"doc_ids"`
}

func registerFeishuImport(api huma.API, d *Deps) {
	huma.Register(api, huma.Operation{Method: "GET", Path: "/api/v1/admin/feishu/app"}, d.fiAppGet)
	huma.Register(api, huma.Operation{Method: "PUT", Path: "/api/v1/admin/feishu/app"}, d.fiAppSet)
	huma.Register(api, huma.Operation{Method: "DELETE", Path: "/api/v1/admin/feishu/app"}, d.fiAppClear)
	huma.Register(api, huma.Operation{Method: "GET", Path: "/api/v1/feishu/status"}, d.fiStatus)
	huma.Register(api, huma.Operation{Method: "POST", Path: "/api/v1/feishu/oauth/start"}, d.fiOAuthStart)
	huma.Register(api, huma.Operation{Method: "GET", Path: "/api/v1/admin/feishu/migration"}, d.fiMigGet)
	huma.Register(api, huma.Operation{Method: "POST", Path: "/api/v1/admin/feishu/migration/start"}, d.fiMigStart)
	huma.Register(api, huma.Operation{Method: "POST", Path: "/api/v1/admin/feishu/migration/finish"}, d.fiMigFinish)
	huma.Register(api, huma.Operation{Method: "POST", Path: "/api/v1/admin/feishu/directory/sync"}, d.fiDirSync)
	huma.Register(api, huma.Operation{Method: "GET", Path: "/api/v1/admin/feishu/people"}, d.fiPeople)
	huma.Register(api, huma.Operation{Method: "POST", Path: "/api/v1/admin/feishu/people/resend"}, d.fiResend)
	huma.Register(api, huma.Operation{Method: "POST", Path: "/api/v1/admin/feishu/people/{open_id}/mail"}, d.fiPersonMail)
	huma.Register(api, huma.Operation{Method: "POST", Path: "/api/v1/feishu/jobs"}, d.fiJobCreate)
	huma.Register(api, huma.Operation{Method: "GET", Path: "/api/v1/feishu/jobs"}, d.fiJobList)
	huma.Register(api, huma.Operation{Method: "GET", Path: "/api/v1/feishu/jobs/{id}"}, d.fiJobDetail)
	huma.Register(api, huma.Operation{Method: "POST", Path: "/api/v1/feishu/jobs/{id}/start"}, d.fiJobStart)
	huma.Register(api, huma.Operation{Method: "POST", Path: "/api/v1/feishu/jobs/{id}/pause"}, d.fiJobPause)
	huma.Register(api, huma.Operation{Method: "POST", Path: "/api/v1/feishu/jobs/{id}/cancel"}, d.fiJobCancel)
	huma.Register(api, huma.Operation{Method: "DELETE", Path: "/api/v1/feishu/jobs/{id}"}, d.fiJobDelete)
}

// ownerOrAdmin gates a job read/act to its creator or an admin.
// 属主或管理员。
func (d *Deps) fiOwnerOrAdmin(ctx context.Context, uid uuid.UUID, isAdmin bool, jobID uuid.UUID) error {
	j, _, err := d.Feishu.JobDetail(ctx, jobID)
	if err != nil {
		return huma.Error404NotFound("job not found")
	}
	if j.CreatedBy != uid && !isAdmin {
		return huma.Error403Forbidden("not your job / 非本人任务")
	}
	return nil
}

func (d *Deps) fiAppGet(ctx context.Context, in *struct{ Headers }) (*struct {
	Body map[string]any `json:"body"`
}, error,
) {
	if _, _, err := d.requireAdmin(ctx, in.Authorization); err != nil {
		return nil, err
	}
	appID, baseURL, cfg := d.Feishu.GetApp(ctx)
	return &struct {
		Body map[string]any `json:"body"`
	}{Body: map[string]any{"configured": cfg, "app_id": appID, "base_url": baseURL}}, nil
}

func (d *Deps) fiAppSet(ctx context.Context, in *struct {
	Headers
	Body struct {
		AppID     string `json:"app_id" required:"true"`
		AppSecret string `json:"app_secret" required:"true"`
		BaseURL   string `json:"base_url"`
	}
},
) (*struct{}, error) {
	_, u, err := d.requireAdmin(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	base := in.Body.BaseURL
	if base == "" {
		base = "https://open.feishu.cn"
	}
	if err := d.Feishu.SetApp(ctx, in.Body.AppID, in.Body.AppSecret, base, u.ID); err != nil {
		return nil, huma.Error422UnprocessableEntity("凭据无效：" + err.Error())
	}
	d.Audit.Log(ctx, &u.ID, audit.ActorAdmin, "feishu.app.set", "", nil, clientIPCtx(ctx))
	return &struct{}{}, nil
}

func (d *Deps) fiAppClear(ctx context.Context, in *struct{ Headers }) (*struct{}, error) {
	_, u, err := d.requireAdmin(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	if err := d.Feishu.ClearApp(ctx); err != nil {
		return nil, huma.Error500InternalServerError("failed")
	}
	d.Audit.Log(ctx, &u.ID, audit.ActorAdmin, "feishu.app.clear", "", nil, clientIPCtx(ctx))
	return &struct{}{}, nil
}

func (d *Deps) fiStatus(ctx context.Context, in *struct{ Headers }) (*struct {
	Body map[string]bool `json:"body"`
}, error,
) {
	_, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	_, _, cfg := d.Feishu.GetApp(ctx)
	return &struct {
		Body map[string]bool `json:"body"`
	}{Body: map[string]bool{"configured": cfg, "authorized": cfg && d.Feishu.Authorized(ctx, u.ID)}}, nil
}

func (d *Deps) fiJobCreate(ctx context.Context, in *struct {
	Headers
	Body struct {
		Name  string   `json:"name" required:"false"`
		Links []string `json:"links" required:"true"`
		Mode  string   `json:"mode" required:"false"`
	}
}) (*struct {
	Body map[string]string `json:"body"`
}, error,
) {
	_, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	mode := in.Body.Mode
	if mode == "" {
		mode = "self"
	}
	if mode == "admin" && !u.IsAdmin {
		return nil, huma.Error403Forbidden("admin mode requires admin / 批量导入需管理员")
	}
	if len(in.Body.Links) == 0 || len(in.Body.Links) > 50 {
		return nil, huma.Error422UnprocessableEntity("links must be 1..50")
	}
	var roots []feishuimport.Root
	var bad []string
	for _, link := range in.Body.Links {
		r, ok := feishuimport.ParseFeishuURL(link)
		if !ok {
			bad = append(bad, link)
			continue
		}
		parts := strings.Split(strings.Trim(strings.TrimSpace(link), "/"), "/")
		if r.Name == "" && len(parts) > 0 {
			r.Name = parts[len(parts)-1]
		}
		roots = append(roots, r)
	}
	if len(bad) > 0 {
		return nil, huma.Error422UnprocessableEntity("无法识别的链接：" + strings.Join(bad, ", "))
	}
	rb, _ := json.Marshal(roots)
	name := in.Body.Name
	if name == "" {
		name = "飞书导入"
	}
	j, err := d.Feishu.CreateJob(ctx, u.ID, u.IsAdmin, name, mode, rb)
	if err != nil {
		return nil, huma.Error422UnprocessableEntity(err.Error())
	}
	if err := d.Feishu.StartJob(ctx, j.ID); err != nil {
		return nil, huma.Error500InternalServerError("start failed")
	}
	d.Audit.Log(ctx, &u.ID, audit.ActorUser, "feishu.job.create", j.ID.String(), map[string]any{"mode": mode, "links": len(roots)}, clientIPCtx(ctx))
	return &struct {
		Body map[string]string `json:"body"`
	}{Body: map[string]string{"id": j.ID.String()}}, nil
}

func (d *Deps) fiJobList(ctx context.Context, in *struct {
	Headers
	All int `query:"all"`
}) (*struct {
	Body []fiJobOut `json:"body"`
}, error,
) {
	_, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	jobs, err := d.Feishu.ListJobs(ctx, u.ID, in.All == 1 && u.IsAdmin)
	if err != nil {
		return nil, huma.Error500InternalServerError("failed")
	}
	out := []fiJobOut{}
	for _, j := range jobs {
		out = append(out, fiJobOut{
			ID: j.ID.String(), Name: j.Name, Mode: j.Mode, Status: j.Status,
			CreatedBy: j.CreatedBy.String(), Total: j.Total, Done: j.Done,
			Skipped: j.Skipped, Failed: j.Failed, Pending: j.Pending,
		})
	}
	return &struct {
		Body []fiJobOut `json:"body"`
	}{Body: out}, nil
}

func (d *Deps) fiJobDetail(ctx context.Context, in *struct {
	Headers
	ID string `path:"id"`
}) (*struct {
	Body map[string]any `json:"body"`
}, error,
) {
	_, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	jid, err := uuid.Parse(in.ID)
	if err != nil {
		return nil, huma.Error400BadRequest("bad id")
	}
	if err := d.fiOwnerOrAdmin(ctx, u.ID, u.IsAdmin, jid); err != nil {
		return nil, err
	}
	j, items, err := d.Feishu.JobDetail(ctx, jid)
	if err != nil {
		return nil, huma.Error404NotFound("not found")
	}
	out := []fiItemOut{}
	for _, it := range items {
		ids := []string{}
		for _, id := range it.DocIDs {
			ids = append(ids, id.String())
		}
		out = append(out, fiItemOut{
			ObjType: it.ObjType, Title: it.Title, Path: it.Path,
			Status: it.Status, Error: it.Error, DocIDs: ids,
		})
	}
	// Counts over ALL items (the list above is capped) — same shape as the
	// job list so the client reads one type. 计数覆盖全部条目，与列表同构。
	jo := fiJobOut{
		ID: j.ID.String(), Name: j.Name, Mode: j.Mode, Status: j.Status,
		CreatedBy: j.CreatedBy.String(),
	}
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*), count(*) FILTER (WHERE status='done'), count(*) FILTER (WHERE status='skipped'),
		        count(*) FILTER (WHERE status='failed'), count(*) FILTER (WHERE status='pending')
		 FROM feishu_import_items WHERE job_id=$1`, jid).
		Scan(&jo.Total, &jo.Done, &jo.Skipped, &jo.Failed, &jo.Pending); err != nil {
		return nil, huma.Error500InternalServerError("failed")
	}
	return &struct {
		Body map[string]any `json:"body"`
	}{Body: map[string]any{
		"job":        jo,
		"last_error": j.LastError,
		"items":      out,
	}}, nil
}

type fiActIn struct {
	Headers
	ID string `path:"id"`
}

func (d *Deps) fiJobDo(ctx context.Context, in *fiActIn, act string) error {
	_, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return err
	}
	jid, err := uuid.Parse(in.ID)
	if err != nil {
		return huma.Error400BadRequest("bad id")
	}
	if err := d.fiOwnerOrAdmin(ctx, u.ID, u.IsAdmin, jid); err != nil {
		return err
	}
	var actErr error
	switch act {
	case "start":
		actErr = d.Feishu.StartJob(ctx, jid)
	case "pause":
		actErr = d.Feishu.PauseJob(ctx, jid)
	case "cancel":
		actErr = d.Feishu.CancelJob(ctx, jid)
	default:
		actErr = d.Feishu.DeleteJob(ctx, jid)
	}
	if actErr != nil {
		return huma.Error409Conflict(actErr.Error())
	}
	d.Audit.Log(ctx, &u.ID, audit.ActorUser, "feishu.job."+act, jid.String(), nil, clientIPCtx(ctx))
	return nil
}

func (d *Deps) fiJobStart(ctx context.Context, in *fiActIn) (*struct{}, error) {
	return &struct{}{}, d.fiJobDo(ctx, in, "start")
}

func (d *Deps) fiJobPause(ctx context.Context, in *fiActIn) (*struct{}, error) {
	return &struct{}{}, d.fiJobDo(ctx, in, "pause")
}

func (d *Deps) fiJobCancel(ctx context.Context, in *fiActIn) (*struct{}, error) {
	return &struct{}{}, d.fiJobDo(ctx, in, "cancel")
}

func (d *Deps) fiJobDelete(ctx context.Context, in *fiActIn) (*struct{}, error) {
	return &struct{}{}, d.fiJobDo(ctx, in, "delete")
}

// ---- whole-tenant migration (admin) + one-click personal import ----

type fiMigOut struct {
	*feishuimport.Migration
	RedirectURL string `json:"redirect_url"`
	PublicURL   string `json:"public_url"`
	Configured  bool   `json:"configured"`
}

func (d *Deps) fiMigGet(ctx context.Context, in *struct{ Headers }) (*struct{ Body fiMigOut }, error) {
	if _, _, err := d.requireAdmin(ctx, in.Authorization); err != nil {
		return nil, err
	}
	m, err := d.Feishu.GetMigration(ctx)
	if err != nil {
		return nil, huma.Error500InternalServerError("failed")
	}
	_, _, cfg := d.Feishu.GetApp(ctx)
	return &struct{ Body fiMigOut }{Body: fiMigOut{
		Migration: m, RedirectURL: d.Feishu.OAuthRedirectURL(), PublicURL: d.PublicURL, Configured: cfg,
	}}, nil
}

func (d *Deps) fiMigStart(ctx context.Context, in *struct {
	Headers
	Body struct {
		Provision  bool   `json:"provision"`
		Continuous bool   `json:"continuous"`
		Scope      string `json:"scope"`
	}
},
) (*struct{}, error) {
	_, u, err := d.requireAdmin(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	if err := d.Feishu.StartMigrationScope(ctx, u.ID, in.Body.Provision, in.Body.Continuous, in.Body.Scope); err != nil {
		return nil, huma.Error409Conflict(err.Error())
	}
	d.Audit.Log(ctx, &u.ID, audit.ActorAdmin, "feishu.migration.start", "",
		map[string]any{"provision": in.Body.Provision, "continuous": in.Body.Continuous, "scope": in.Body.Scope}, clientIPCtx(ctx))
	return &struct{}{}, nil
}

func (d *Deps) fiMigFinish(ctx context.Context, in *struct{ Headers }) (*struct{}, error) {
	_, u, err := d.requireAdmin(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	if err := d.Feishu.FinishMigration(ctx); err != nil {
		return nil, huma.Error409Conflict(err.Error())
	}
	d.Audit.Log(ctx, &u.ID, audit.ActorAdmin, "feishu.migration.finish", "", nil, clientIPCtx(ctx))
	return &struct{}{}, nil
}

func (d *Deps) fiDirSync(ctx context.Context, in *struct{ Headers }) (*struct {
	Body feishuimport.DirStats
}, error,
) {
	_, u, err := d.requireAdmin(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	st, err := d.Feishu.SyncDirectory(ctx)
	if err != nil {
		return nil, huma.Error422UnprocessableEntity(err.Error())
	}
	d.Audit.Log(ctx, &u.ID, audit.ActorAdmin, "feishu.directory.sync", "", st, clientIPCtx(ctx))
	return &struct{ Body feishuimport.DirStats }{Body: st}, nil
}

func (d *Deps) fiPeople(ctx context.Context, in *struct{ Headers }) (*struct {
	Body []feishuimport.Person
}, error,
) {
	if _, _, err := d.requireAdmin(ctx, in.Authorization); err != nil {
		return nil, err
	}
	ps, err := d.Feishu.People(ctx)
	if err != nil {
		return nil, huma.Error500InternalServerError("failed")
	}
	return &struct{ Body []feishuimport.Person }{Body: ps}, nil
}

func (d *Deps) fiResend(ctx context.Context, in *struct {
	Headers
	Body struct {
		OpenIDs []string `json:"open_ids" required:"false"`
	}
}) (*struct {
	Body map[string]int `json:"body"`
}, error,
) {
	_, u, err := d.requireAdmin(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	ids := in.Body.OpenIDs
	if ids == nil {
		ids = []string{}
	}
	n, err := d.Feishu.ResendLinks(ctx, u.ID, ids)
	if err != nil {
		return nil, huma.Error500InternalServerError("failed")
	}
	d.Audit.Log(ctx, &u.ID, audit.ActorAdmin, "feishu.people.resend", "", map[string]any{"count": n}, clientIPCtx(ctx))
	return &struct {
		Body map[string]int `json:"body"`
	}{Body: map[string]int{"sent": n}}, nil
}

func (d *Deps) fiPersonMail(ctx context.Context, in *struct {
	Headers
	OpenID string `path:"open_id"`
},
) (*struct{}, error) {
	_, u, err := d.requireAdmin(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	if err := d.Feishu.StartMailFor(ctx, in.OpenID, true); err != nil {
		return nil, huma.Error500InternalServerError("failed")
	}
	d.Audit.Log(ctx, &u.ID, audit.ActorAdmin, "feishu.person.mail", in.OpenID, nil, clientIPCtx(ctx))
	return &struct{}{}, nil
}

func (d *Deps) fiOAuthStart(ctx context.Context, in *struct{ Headers }) (*struct {
	Body map[string]string `json:"body"`
}, error,
) {
	_, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	link, err := d.Feishu.OAuthStart(ctx, u.ID)
	if err != nil {
		return nil, huma.Error409Conflict(err.Error())
	}
	return &struct {
		Body map[string]string `json:"body"`
	}{Body: map[string]string{"url": link}}, nil
}

// fiOAuthCallback is where Feishu sends the employee's browser back. It
// answers with a small page (the employee returns to the desktop app).
// 飞书授权回调：返回一个小页面，员工回到桌面端即可。
func (d *Deps) fiOAuthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	title, msg := "授权成功", "已开始导入你的飞书文档。现在可以关闭这个页面，回到 WittyChat 查看进度。"
	if e := q.Get("error"); e != "" {
		title, msg = "未授权", "你取消了授权。需要时可以在 WittyChat 里重新点击导入。"
	} else if job, err := d.Feishu.OAuthCallback(r.Context(), q.Get("code"), q.Get("state")); err != nil {
		title, msg = "授权失败", err.Error()
	} else if d.Audit != nil {
		d.Audit.Log(r.Context(), nil, audit.ActorSystem, "feishu.oauth.linked", job.String(), nil, clientIPCtx(r.Context()))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>%s</title>
<body style="font-family:system-ui,'Microsoft YaHei',sans-serif;background:#111216;color:#e4e4e7;display:grid;place-items:center;height:100vh;margin:0">
<div style="max-width:420px;text-align:center"><h2>%s</h2><p style="color:#9a9ca6;line-height:1.7">%s</p></div></body>`,
		html.EscapeString(title), html.EscapeString(title), html.EscapeString(msg))
}
