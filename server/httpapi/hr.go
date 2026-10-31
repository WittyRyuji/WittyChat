package httpapi

// Internal HR endpoints — unified approval requests + daily attendance.
// Approval is single-step by workspace admins; requesters see/cancel their
// own, admins see queues and monthly attendance for everyone.
//
// 内部人力接口：统一审批申请 + 每日考勤。管理员单级审批；申请人可见/撤销
// 自己的申请，管理员看待办队列和全员月度出勤。

import (
	"context"
	"errors"
	"net/http"
	"time"

	"wittychat/core/hr"
	"wittychat/shared/apitypes"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
)

func registerHR(api huma.API, d *Deps) {
	sec := []map[string][]string{{"bearer": {}}}

	huma.Register(api, huma.Operation{
		OperationID: "hr-req-create", Method: http.MethodPost,
		Path: "/api/v1/hr/requests", Summary: "New HR request (leave/contract/expense) / 新建人力申请",
		Tags: []string{"hr"}, Security: sec,
	}, d.hrCreate)

	huma.Register(api, huma.Operation{
		OperationID: "hr-req-list", Method: http.MethodGet,
		Path: "/api/v1/hr/requests", Summary: "List requests: mine|pending|all / 申请列表",
		Tags: []string{"hr"}, Security: sec,
	}, d.hrList)

	huma.Register(api, huma.Operation{
		OperationID: "hr-req-decide", Method: http.MethodPost,
		Path: "/api/v1/hr/requests/{id}/decide", Summary: "Approve/reject (admin) / 审批",
		Tags: []string{"hr"}, Security: sec,
	}, d.hrDecide)

	huma.Register(api, huma.Operation{
		OperationID: "hr-req-cancel", Method: http.MethodPost,
		Path: "/api/v1/hr/requests/{id}/cancel", Summary: "Withdraw my pending request / 撤销我的申请",
		Tags: []string{"hr"}, Security: sec,
	}, d.hrCancel)

	huma.Register(api, huma.Operation{
		OperationID: "hr-checkin", Method: http.MethodPost,
		Path: "/api/v1/hr/attendance/check-in", Summary: "Check in / 上班打卡",
		Tags: []string{"hr"}, Security: sec,
	}, d.hrCheckIn)

	huma.Register(api, huma.Operation{
		OperationID: "hr-checkout", Method: http.MethodPost,
		Path: "/api/v1/hr/attendance/check-out", Summary: "Check out / 下班签退",
		Tags: []string{"hr"}, Security: sec,
	}, d.hrCheckOut)

	huma.Register(api, huma.Operation{
		OperationID: "hr-att-mine", Method: http.MethodGet,
		Path: "/api/v1/hr/attendance/mine", Summary: "My month / 我的月度考勤",
		Tags: []string{"hr"}, Security: sec,
	}, d.hrMonthMine)

	huma.Register(api, huma.Operation{
		OperationID: "hr-att-summary", Method: http.MethodGet,
		Path: "/api/v1/hr/attendance/summary", Summary: "All users monthly summary (admin) / 全员月度出勤",
		Tags: []string{"hr"}, Security: sec,
	}, d.hrMonthSummary)
}

func toHRRequest(r *hr.Request) apitypes.HRRequest {
	out := apitypes.HRRequest{
		ID: r.ID.String(), Kind: r.Kind, Requester: r.Requester.String(),
		RequesterName: r.RequesterName, RequesterEmail: r.RequesterEmail,
		Status: r.Status, Payload: r.Payload, DeciderName: r.DeciderName,
		DecisionNote: r.DecisionNote, CreatedAt: r.CreatedAt.Format(time.RFC3339),
	}
	if r.DecidedBy != nil {
		out.DecidedBy = r.DecidedBy.String()
	}
	if r.DecidedAt != nil {
		out.DecidedAt = r.DecidedAt.Format(time.RFC3339)
	}
	return out
}

func (d *Deps) hrCreate(ctx context.Context, in *struct {
	Headers
	Body apitypes.HRRequestCreate
},
) (*struct{ Body apitypes.HRRequest }, error) {
	_, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	r, err := d.HR.Create(ctx, in.Body.Kind, u.ID, in.Body.Payload)
	if errors.Is(err, hr.ErrBadPayload) {
		return nil, huma.Error400BadRequest("invalid payload for kind / 载荷不符合类型要求")
	}
	if err != nil {
		return nil, err
	}
	d.Audit.Log(ctx, &u.ID, "user", "hr.request.create:"+r.Kind, r.ID.String(), nil, clientIP(in.XForwardedFor))
	return &struct{ Body apitypes.HRRequest }{Body: toHRRequest(r)}, nil
}

func (d *Deps) hrList(ctx context.Context, in *struct {
	Headers
	Scope string `query:"scope" default:"mine" enum:"mine,pending,all,history"`
	Limit int    `query:"limit" default:"100"`
},
) (*struct{ Body apitypes.HRRequestList }, error) {
	_, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	if (in.Scope == "pending" || in.Scope == "all") && !u.IsAdmin {
		return nil, huma.Error403Forbidden("pending/all scopes are admin-only / 队列视图仅管理员")
	}
	rows, err := d.HR.List(ctx, in.Scope, u.ID, u.IsAdmin, in.Limit)
	if err != nil {
		return nil, err
	}
	out := apitypes.HRRequestList{Requests: []apitypes.HRRequest{}}
	for _, r := range rows {
		r := r
		out.Requests = append(out.Requests, toHRRequest(&r))
	}
	return &struct{ Body apitypes.HRRequestList }{Body: out}, nil
}

func (d *Deps) hrDecide(ctx context.Context, in *struct {
	Headers
	ID   string `path:"id"`
	Body apitypes.HRDecide
},
) (*struct{ Body apitypes.HRRequest }, error) {
	_, u, err := d.requireAdmin(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(in.ID)
	if err != nil {
		return nil, huma.Error400BadRequest("bad id")
	}
	r, err := d.HR.Decide(ctx, id, u.ID, in.Body.Approve, in.Body.Note)
	if errors.Is(err, hr.ErrNotPending) {
		return nil, huma.Error409Conflict("request not pending / 申请已处理")
	}
	if err != nil {
		return nil, err
	}
	action := "hr.request.reject"
	if in.Body.Approve {
		action = "hr.request.approve"
	}
	d.Audit.Log(ctx, &u.ID, "admin", action+":"+r.Kind, r.ID.String(),
		map[string]any{"requester": r.Requester.String(), "note": in.Body.Note}, clientIP(in.XForwardedFor))
	return &struct{ Body apitypes.HRRequest }{Body: toHRRequest(r)}, nil
}

func (d *Deps) hrCancel(ctx context.Context, in *struct {
	Headers
	ID string `path:"id"`
},
) (*struct{}, error) {
	_, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(in.ID)
	if err != nil {
		return nil, huma.Error400BadRequest("bad id")
	}
	if err := d.HR.Cancel(ctx, id, u.ID); errors.Is(err, hr.ErrNotPending) {
		return nil, huma.Error409Conflict("request not pending or not yours / 申请不可撤销")
	} else if err != nil {
		return nil, err
	}
	return &struct{}{}, nil
}

func (d *Deps) hrCheckIn(ctx context.Context, in *struct {
	Headers
	Body apitypes.HRCheckIn
},
) (*struct{}, error) {
	_, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	if err := d.HR.CheckIn(ctx, u.ID, time.Now(), in.Body.Note); err != nil {
		return nil, err
	}
	return &struct{}{}, nil
}

func (d *Deps) hrCheckOut(ctx context.Context, in *struct {
	Headers
},
) (*struct{}, error) {
	_, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	if err := d.HR.CheckOut(ctx, u.ID, time.Now()); errors.Is(err, hr.ErrNotClocked) {
		return nil, huma.Error409Conflict("not checked in today / 今日未打卡")
	} else if err != nil {
		return nil, err
	}
	return &struct{}{}, nil
}

// workStart resolves hr.work_start ("HH:MM", default 09:00) to a duration.
func (d *Deps) workStart(ctx context.Context) time.Duration {
	s := d.Settings.Str(ctx, "hr.work_start", "09:00")
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 9 * time.Hour
	}
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute
}

func (d *Deps) hrMonthMine(ctx context.Context, in *struct {
	Headers
	Month string `query:"month" required:"true"`
},
) (*struct{ Body apitypes.HRMonthResponse }, error) {
	_, u, err := d.requireUser(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	days, sum, err := d.HR.Month(ctx, u.ID, in.Month, d.workStart(ctx))
	if err != nil {
		return nil, huma.Error400BadRequest("bad month / 月份格式应为 YYYY-MM")
	}
	return &struct{ Body apitypes.HRMonthResponse }{Body: hrMonthResp(u.ID.String(), "", days, sum)}, nil
}

func (d *Deps) hrMonthSummary(ctx context.Context, in *struct {
	Headers
	Month string `query:"month" required:"true"`
},
) (*struct{ Body apitypes.HRSummaryList }, error) {
	_, _, err := d.requireAdmin(ctx, in.Authorization)
	if err != nil {
		return nil, err
	}
	rows, err := d.HR.SummaryAll(ctx, in.Month, d.workStart(ctx))
	if err != nil {
		return nil, huma.Error400BadRequest("bad month / 月份格式应为 YYYY-MM")
	}
	out := apitypes.HRSummaryList{Rows: []apitypes.HRSummary{}}
	for _, s := range rows {
		out.Rows = append(out.Rows, apitypes.HRSummary{
			UserID: s.UserID.String(), UserName: s.UserName,
			Days: s.Days, TotalMinutes: s.TotalMinutes,
			LateDays: s.LateDays, MissingCheckout: s.MissingCheckout,
			LeaveDays: s.LeaveDays,
		})
	}
	return &struct{ Body apitypes.HRSummaryList }{Body: out}, nil
}

func hrMonthResp(uid, uname string, days []hr.Day, sum hr.Summary) apitypes.HRMonthResponse {
	out := apitypes.HRMonthResponse{Days: []apitypes.HRDay{}}
	for _, d := range days {
		day := apitypes.HRDay{
			WorkDate: d.WorkDate, CheckIn: d.CheckIn.Format(time.RFC3339),
			Note: d.Note, Minutes: d.Minutes, Late: d.Late,
		}
		if d.CheckOut != nil {
			day.CheckOut = d.CheckOut.Format(time.RFC3339)
		}
		out.Days = append(out.Days, day)
	}
	out.Summary = apitypes.HRSummary{
		UserID: uid, UserName: uname, Days: sum.Days,
		TotalMinutes: sum.TotalMinutes, LateDays: sum.LateDays,
		MissingCheckout: sum.MissingCheckout,
	}
	return out
}
