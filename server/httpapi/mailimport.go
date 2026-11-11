// Mail-import admin API: job CRUD, CSV account upload, test/start/pause/
// cancel/finish. All mutating calls audit (never with secrets).
// 邮箱导入管理接口：任务增删改、CSV 账号、测试/启停/完成。变更全审计，
// 绝不落密码。

package httpapi

import (
	"context"
	"net/http"
	"time"
	"wittychat/core/audit"
	"wittychat/core/mailimport"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
)

func registerMailImport(api huma.API, d *Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "mail-import-presets", Method: http.MethodGet,
		Path: "/api/v1/admin/mail-import/presets", Summary: "IMAP provider presets / IMAP 预设",
		Tags: []string{"admin"}, Security: []map[string][]string{{"bearer": {}}},
	}, d.miPresets)
	huma.Register(api, huma.Operation{
		OperationID: "mail-import-jobs", Method: http.MethodGet,
		Path: "/api/v1/admin/mail-import/jobs", Summary: "List import jobs / 导入任务列表",
		Tags: []string{"admin"}, Security: []map[string][]string{{"bearer": {}}},
	}, d.miJobList)
	huma.Register(api, huma.Operation{
		OperationID: "mail-import-job-create", Method: http.MethodPost,
		Path: "/api/v1/admin/mail-import/jobs", Summary: "Create import job / 创建导入任务",
		Tags: []string{"admin"}, Security: []map[string][]string{{"bearer": {}}},
	}, d.miJobCreate)
	huma.Register(api, huma.Operation{
		OperationID: "mail-import-job-get", Method: http.MethodGet,
		Path: "/api/v1/admin/mail-import/jobs/{id}", Summary: "Job detail / 任务详情",
		Tags: []string{"admin"}, Security: []map[string][]string{{"bearer": {}}},
	}, d.miJobGet)
	huma.Register(api, huma.Operation{
		OperationID: "mail-import-accounts", Method: http.MethodPost,
		Path: "/api/v1/admin/mail-import/jobs/{id}/accounts", Summary: "Upload CSV accounts / CSV 账号",
		Tags: []string{"admin"}, Security: []map[string][]string{{"bearer": {}}},
	}, d.miAccountsAdd)
	huma.Register(api, huma.Operation{
		OperationID: "mail-import-account-del", Method: http.MethodDelete,
		Path: "/api/v1/admin/mail-import/jobs/{id}/accounts/{aid}", Summary: "Remove account / 删除账号",
		Tags: []string{"admin"}, Security: []map[string][]string{{"bearer": {}}},
	}, d.miAccountDel)
	huma.Register(api, huma.Operation{
		OperationID: "mail-import-test", Method: http.MethodPost,
		Path: "/api/v1/admin/mail-import/jobs/{id}/test", Summary: "Test logins / 测试连接",
		Tags: []string{"admin"}, Security: []map[string][]string{{"bearer": {}}},
	}, d.miTest)
	huma.Register(api, huma.Operation{
		OperationID: "mail-import-start", Method: http.MethodPost,
		Path: "/api/v1/admin/mail-import/jobs/{id}/start", Summary: "Start or re-run / 启动或增量重跑",
		Tags: []string{"admin"}, Security: []map[string][]string{{"bearer": {}}},
	}, d.miStart)
	huma.Register(api, huma.Operation{
		OperationID: "mail-import-pause", Method: http.MethodPost,
		Path: "/api/v1/admin/mail-import/jobs/{id}/pause", Summary: "Pause / 暂停",
		Tags: []string{"admin"}, Security: []map[string][]string{{"bearer": {}}},
	}, d.miPause)
	huma.Register(api, huma.Operation{
		OperationID: "mail-import-cancel", Method: http.MethodPost,
		Path: "/api/v1/admin/mail-import/jobs/{id}/cancel", Summary: "Cancel / 取消",
		Tags: []string{"admin"}, Security: []map[string][]string{{"bearer": {}}},
	}, d.miCancel)
	huma.Register(api, huma.Operation{
		OperationID: "mail-import-finish", Method: http.MethodPost,
		Path: "/api/v1/admin/mail-import/jobs/{id}/finish", Summary: "Finish: purge secrets / 完成清密码",
		Tags: []string{"admin"}, Security: []map[string][]string{{"bearer": {}}},
	}, d.miFinish)
	huma.Register(api, huma.Operation{
		OperationID: "mail-import-job-del", Method: http.MethodDelete,
		Path: "/api/v1/admin/mail-import/jobs/{id}", Summary: "Delete job / 删除任务",
		Tags: []string{"admin"}, Security: []map[string][]string{{"bearer": {}}},
	}, d.miDelete)
}

type miJobOut struct {
	ID        uuid.UUID  `json:"id"`
	Name      string     `json:"name"`
	Provider  string     `json:"provider"`
	Host      string     `json:"host"`
	Port      int        `json:"port"`
	TLSMode   string     `json:"tls_mode"`
	Since     *time.Time `json:"since"`
	AddAlias  bool       `json:"add_alias"`
	Status    string     `json:"status"`
	Purged    bool       `json:"secrets_purged"`
	Accounts  int        `json:"accounts"`
	MsgsDone  int64      `json:"msgs_done"`
	MsgsTotal int64      `json:"msgs_total"`
	CreatedAt time.Time  `json:"created_at"`
}

func (d *Deps) miPresets(ctx context.Context, in *struct {
	Headers
}) (*struct {
	Body []map[string]any `json:"body"`
}, error,
) {
	if _, _, err := d.requireAdmin(ctx, in.Authorization); err != nil {
		return nil, err
	}
	return &struct {
		Body []map[string]any `json:"body"`
	}{Body: []map[string]any{
		{
			"key": "feishu", "name": "飞书邮箱", "host": "imap.feishu.cn", "port": 993, "tls_mode": "tls",
			"help": "管理员在飞书管理后台 → 邮箱 → 邮箱工具 → 第三方邮箱客户端 开启；员工在飞书桌面端 设置 → 邮箱 → 第三方邮箱客户端登录 生成专用密码",
		},
		{
			"key": "custom", "name": "自定义 IMAP", "host": "", "port": 993, "tls_mode": "tls",
			"help": "任意支持 IMAP 的邮箱服务器；TLS 端口通常 993",
		},
	}}, nil
}

func (d *Deps) miJobList(ctx context.Context, in *struct {
	Headers
}) (*struct {
	Body []miJobOut `json:"body"`
}, error,
) {
	if _, _, err := d.requireAdmin(ctx, in.Authorization); err != nil {
		return nil, err
	}
	rows, err := d.Pool.Query(ctx,
		`SELECT j.id,j.name,j.provider,j.host,j.port,j.tls_mode,j.since,j.add_alias,j.status,j.secrets_purged,j.created_at,
		        count(a.id), COALESCE(sum(a.msgs_done),0), COALESCE(sum(a.msgs_total),0)
		 FROM mail_import_jobs j LEFT JOIN mail_import_accounts a ON a.job_id=j.id
		 GROUP BY j.id ORDER BY j.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []miJobOut
	for rows.Next() {
		var j miJobOut
		if err := rows.Scan(&j.ID, &j.Name, &j.Provider, &j.Host, &j.Port, &j.TLSMode,
			&j.Since, &j.AddAlias, &j.Status, &j.Purged, &j.CreatedAt,
			&j.Accounts, &j.MsgsDone, &j.MsgsTotal); err == nil {
			out = append(out, j)
		}
	}
	return &struct {
		Body []miJobOut `json:"body"`
	}{Body: out}, nil
}

func (d *Deps) miJobCreate(ctx context.Context, in *struct {
	Headers
	Body struct {
		Name     string     `json:"name" required:"true"`
		Provider string     `json:"provider"`
		Host     string     `json:"host" required:"true"`
		Port     int        `json:"port" required:"true"`
		TLSMode  string     `json:"tls_mode" required:"true"`
		Since    *time.Time `json:"since"`
		AddAlias bool       `json:"add_alias"`
	}
}) (*struct {
	Body map[string]string `json:"body"`
}, error,
) {
	_, admin, err := d.requireAdmin(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	id, err := d.Import.CreateJob(ctx, mailimport.JobParams{
		Name: in.Body.Name, Provider: in.Body.Provider, Host: in.Body.Host,
		Port: in.Body.Port, TLSMode: in.Body.TLSMode, Since: in.Body.Since,
		AddAlias: in.Body.AddAlias, CreatedBy: admin.ID,
	})
	if err != nil {
		return nil, huma.Error422UnprocessableEntity(err.Error())
	}
	d.Audit.Log(ctx, &admin.ID, audit.ActorAdmin, "mailimport.job.create", in.Body.Name,
		map[string]string{"host": in.Body.Host}, clientIPCtx(ctx))
	return &struct {
		Body map[string]string `json:"body"`
	}{Body: map[string]string{"id": id.String()}}, nil
}

func (d *Deps) miJobGet(ctx context.Context, in *struct {
	Headers
	ID uuid.UUID `path:"id"`
}) (*struct {
	Body map[string]any `json:"body"`
}, error,
) {
	if _, _, err := d.requireAdmin(ctx, in.Authorization); err != nil {
		return nil, err
	}
	var j miJobOut
	if err := d.Pool.QueryRow(ctx,
		`SELECT id,name,provider,host,port,tls_mode,since,add_alias,status,secrets_purged,created_at
		 FROM mail_import_jobs WHERE id=$1`, in.ID).
		Scan(&j.ID, &j.Name, &j.Provider, &j.Host, &j.Port, &j.TLSMode,
			&j.Since, &j.AddAlias, &j.Status, &j.Purged, &j.CreatedAt); err != nil {
		return nil, huma.Error404NotFound("job not found")
	}
	rows, err := d.Pool.Query(ctx,
		`SELECT id,source_login,target_user,status,msgs_total,msgs_done,msgs_skipped,msgs_failed,
		        bytes_done,COALESCE(last_error,''),COALESCE(note,''),started_at,finished_at
		 FROM mail_import_accounts WHERE job_id=$1 ORDER BY source_login`, in.ID)
	if err != nil {
		return nil, err
	}
	var accounts []map[string]any
	ids := map[uuid.UUID]bool{}
	for rows.Next() {
		var aid, target uuid.UUID
		var login, status, lerr, note string
		var mt, md, ms, mf int
		var by int64
		var st, fin *time.Time
		if err := rows.Scan(&aid, &login, &target, &status, &mt, &md, &ms, &mf,
			&by, &lerr, &note, &st, &fin); err == nil {
			ids[aid] = true
			var fcnt int
			_ = d.Pool.QueryRow(ctx,
				`SELECT count(*) FROM mail_import_failures WHERE account_id=$1`, aid).Scan(&fcnt)
			accounts = append(accounts, map[string]any{
				"id": aid, "source_login": login, "target_user": target, "status": status,
				"failures_count": fcnt,
				"msgs_total":     mt, "msgs_done": md, "msgs_skipped": ms, "msgs_failed": mf,
				"bytes_done": by, "last_error": lerr, "note": note,
				"started_at": st, "finished_at": fin,
			})
		}
	}
	rows.Close()
	var failures []map[string]any
	failRows, ferr := d.Pool.Query(ctx,
		`SELECT source_name,uid,error,attempts FROM mail_import_failures
		 WHERE account_id = ANY($1) ORDER BY updated_at DESC LIMIT 20`, keysUUID(ids))
	if ferr == nil {
		defer failRows.Close()
		for failRows.Next() {
			var src, e string
			var u int64
			var att int
			if err := failRows.Scan(&src, &u, &e, &att); err == nil {
				failures = append(failures, map[string]any{
					"folder": src, "uid": u, "error": e, "attempts": att,
				})
			}
		}
	}
	var folders []map[string]any
	frows, err := d.Pool.Query(ctx,
		`SELECT account_id,source_name,target_folder_id,uidvalidity,last_uid,msgs_total,msgs_done
		 FROM mail_import_folders WHERE account_id = ANY($1)`, keysUUID(ids))
	if err == nil {
		defer frows.Close()
		for frows.Next() {
			var aid, fid uuid.UUID
			var src string
			var uv, lu int64
			var mt, md int
			if err := frows.Scan(&aid, &src, &fid, &uv, &lu, &mt, &md); err == nil {
				folders = append(folders, map[string]any{
					"account_id": aid, "source_name": src, "target_folder_id": fid,
					"uidvalidity": uv, "last_uid": lu, "msgs_total": mt, "msgs_done": md,
				})
			}
		}
	}
	return &struct {
		Body map[string]any `json:"body"`
	}{Body: map[string]any{"job": j, "accounts": accounts, "folders": folders, "failures": failures}}, nil
}

func keysUUID(m map[uuid.UUID]bool) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func (d *Deps) miAccountsAdd(ctx context.Context, in *struct {
	Headers
	ID   uuid.UUID `path:"id"`
	Body struct {
		CSV string `json:"csv" required:"true"`
	}
}) (*struct {
	Body map[string]any `json:"body"`
}, error,
) {
	_, admin, err := d.requireAdmin(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	added, updated, errs := d.Import.AddAccountsCSV(ctx, in.ID, in.Body.CSV)
	d.Audit.Log(ctx, &admin.ID, audit.ActorAdmin, "mailimport.accounts.upload", in.ID.String(),
		map[string]int{"added": added, "updated": updated, "errors": len(errs)}, clientIPCtx(ctx))
	return &struct {
		Body map[string]any `json:"body"`
	}{Body: map[string]any{"added": added, "updated": updated, "errors": errs}}, nil
}

func (d *Deps) miAccountDel(ctx context.Context, in *struct {
	Headers
	ID  uuid.UUID `path:"id"`
	AID uuid.UUID `path:"aid"`
},
) (*struct{}, error) {
	_, admin, err := d.requireAdmin(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	if _, err := d.Pool.Exec(ctx,
		`DELETE FROM mail_import_accounts WHERE id=$1 AND job_id=$2`, in.AID, in.ID); err != nil {
		return nil, err
	}
	d.Audit.Log(ctx, &admin.ID, audit.ActorAdmin, "mailimport.account.delete", in.AID.String(), nil, clientIPCtx(ctx))
	return &struct{}{}, nil
}

func (d *Deps) miTest(ctx context.Context, in *struct {
	Headers
	ID   uuid.UUID `path:"id"`
	Body struct {
		AccountIDs []uuid.UUID `json:"account_ids"`
	}
}) (*struct {
	Body []mailimport.TestResult `json:"body"`
}, error,
) {
	_, admin, err := d.requireAdmin(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	res, err := d.Import.TestAccounts(ctx, in.ID, in.Body.AccountIDs)
	if err != nil {
		return nil, err
	}
	d.Audit.Log(ctx, &admin.ID, audit.ActorAdmin, "mailimport.test", in.ID.String(), nil, clientIPCtx(ctx))
	return &struct {
		Body []mailimport.TestResult `json:"body"`
	}{Body: res}, nil
}

func (d *Deps) miStart(ctx context.Context, in *struct {
	Headers
	ID uuid.UUID `path:"id"`
},
) (*struct{}, error) {
	_, admin, err := d.requireAdmin(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	if err := d.Import.StartJob(ctx, in.ID); err != nil {
		return nil, huma.Error422UnprocessableEntity(err.Error())
	}
	d.Audit.Log(ctx, &admin.ID, audit.ActorAdmin, "mailimport.start", in.ID.String(), nil, clientIPCtx(ctx))
	return &struct{}{}, nil
}

func (d *Deps) miPause(ctx context.Context, in *struct {
	Headers
	ID uuid.UUID `path:"id"`
},
) (*struct{}, error) {
	_, admin, err := d.requireAdmin(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	if err := d.Import.PauseJob(ctx, in.ID); err != nil {
		return nil, err
	}
	d.Audit.Log(ctx, &admin.ID, audit.ActorAdmin, "mailimport.pause", in.ID.String(), nil, clientIPCtx(ctx))
	return &struct{}{}, nil
}

func (d *Deps) miCancel(ctx context.Context, in *struct {
	Headers
	ID uuid.UUID `path:"id"`
},
) (*struct{}, error) {
	_, admin, err := d.requireAdmin(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	if err := d.Import.CancelJob(ctx, in.ID); err != nil {
		return nil, err
	}
	d.Audit.Log(ctx, &admin.ID, audit.ActorAdmin, "mailimport.cancel", in.ID.String(), nil, clientIPCtx(ctx))
	return &struct{}{}, nil
}

func (d *Deps) miFinish(ctx context.Context, in *struct {
	Headers
	ID uuid.UUID `path:"id"`
},
) (*struct{}, error) {
	_, admin, err := d.requireAdmin(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	if err := d.Import.FinishJob(ctx, in.ID); err != nil {
		return nil, err
	}
	d.Audit.Log(ctx, &admin.ID, audit.ActorAdmin, "mailimport.finish", in.ID.String(), nil, clientIPCtx(ctx))
	return &struct{}{}, nil
}

func (d *Deps) miDelete(ctx context.Context, in *struct {
	Headers
	ID uuid.UUID `path:"id"`
},
) (*struct{}, error) {
	_, admin, err := d.requireAdmin(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	if err := d.Import.DeleteJob(ctx, in.ID); err != nil {
		return nil, huma.Error422UnprocessableEntity(err.Error())
	}
	d.Audit.Log(ctx, &admin.ID, audit.ActorAdmin, "mailimport.delete", in.ID.String(), nil, clientIPCtx(ctx))
	return &struct{}{}, nil
}
