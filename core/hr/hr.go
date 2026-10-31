// Package hr implements the internal lightweight HR module:
// unified requests (leave / contract / expense) with a single-step
// approval flow, and daily attendance (check-in/out + monthly calc).
//
// 内部轻量人力系统：统一申请（请假/合同/报销，单级审批）+ 每日考勤
// （打卡/签退 + 月度出勤计算）。管理员审批；申请人可撤销未决申请。
package hr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotPending = errors.New("hr: request not pending")
	ErrNotFound   = errors.New("hr: not found")
	ErrBadPayload = errors.New("hr: invalid payload")
	ErrNotClocked = errors.New("hr: not checked in")
)

const (
	KindLeave    = "leave"
	KindContract = "contract"
	KindExpense  = "expense"

	StatusPending   = "pending"
	StatusApproved  = "approved"
	StatusRejected  = "rejected"
	StatusCancelled = "cancelled"
)

// Request — one approval-flow item. Payload is kind-specific JSON.
// Request — 单条审批事项，payload 按 kind 存类型化字段。
type Request struct {
	ID           uuid.UUID       `json:"id"`
	Kind         string          `json:"kind"`
	Requester    uuid.UUID       `json:"requester"`
	Status       string          `json:"status"`
	Payload      json.RawMessage `json:"payload"`
	DecidedBy    *uuid.UUID      `json:"decided_by,omitempty"`
	DecidedAt    *time.Time      `json:"decided_at,omitempty"`
	DecisionNote string          `json:"decision_note,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	// joined display fields / 列表展示用关联字段
	RequesterName  string `json:"requester_name,omitempty"`
	RequesterEmail string `json:"requester_email,omitempty"`
	DeciderName    string `json:"decider_name,omitempty"`
}

// Day — one attendance row. Minutes/Late computed on read.
// Day — 单天考勤。工时/迟到读取时计算。
type Day struct {
	WorkDate string     `json:"work_date"`
	CheckIn  time.Time  `json:"check_in"`
	CheckOut *time.Time `json:"check_out,omitempty"`
	Note     string     `json:"note,omitempty"`
	Minutes  int        `json:"minutes"`
	Late     bool       `json:"late"`
}

// Summary — monthly aggregate.
type Summary struct {
	UserID          uuid.UUID `json:"user_id"`
	UserName        string    `json:"user_name,omitempty"`
	Days            int       `json:"days"`
	TotalMinutes    int       `json:"total_minutes"`
	LateDays        int       `json:"late_days"`
	MissingCheckout int       `json:"missing_checkout"`
	LeaveDays       float64   `json:"leave_days"`
}

type Store struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// ---------- requests / 申请 ----------

const reqCols = `r.id, r.kind, r.requester, r.status, r.payload,
	r.decided_by, r.decided_at, coalesce(r.decision_note,''),
	r.created_at, ru.display_name, ru.email, coalesce(du.display_name,'')`

func (s *Store) Create(ctx context.Context, kind string, requester uuid.UUID, payload json.RawMessage) (*Request, error) {
	if err := ValidatePayload(kind, payload); err != nil {
		return nil, err
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO hr_requests (id, kind, requester, payload) VALUES ($1,$2,$3,$4)`,
		id, kind, requester, payload); err != nil {
		return nil, fmt.Errorf("hr create: %w", err)
	}
	return s.Get(ctx, id)
}

func scanReq(r *Request) []any {
	return []any{&r.ID, &r.Kind, &r.Requester, &r.Status, &r.Payload,
		&r.DecidedBy, &r.DecidedAt, &r.DecisionNote, &r.CreatedAt,
		&r.RequesterName, &r.RequesterEmail, &r.DeciderName}
}

const reqFrom = ` FROM hr_requests r
	JOIN users ru ON ru.id = r.requester
	LEFT JOIN users du ON du.id = r.decided_by`

func (s *Store) Get(ctx context.Context, id uuid.UUID) (*Request, error) {
	var r Request
	err := s.pool.QueryRow(ctx,
		`SELECT `+reqCols+reqFrom+` WHERE r.id=$1`, id).Scan(scanReq(&r)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &r, err
}

// List: scope=mine (own) / pending (queue) / all / history (decided — own
// for members, everyone for admins).
// history = 已办结申请：普通成员只看自己的，管理员看全员。
func (s *Store) List(ctx context.Context, scope string, userID uuid.UUID, isAdmin bool, limit int) ([]Request, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	var where string
	var args []any
	switch scope {
	case "mine":
		where, args = ` WHERE r.requester=$1`, []any{userID}
	case "pending":
		where, args = ` WHERE r.status='pending'`, nil
	case "history":
		if isAdmin {
			where, args = ` WHERE r.status != 'pending'`, nil
		} else {
			where, args = ` WHERE r.status != 'pending' AND r.requester=$1`, []any{userID}
		}
	default: // all
		where, args = ``, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+reqCols+reqFrom+where+` ORDER BY r.created_at DESC LIMIT `+fmt.Sprint(limit), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Request
	for rows.Next() {
		var r Request
		if err := rows.Scan(scanReq(&r)...); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Decide — approve/reject a pending request. One-step flow.
func (s *Store) Decide(ctx context.Context, id, approver uuid.UUID, approve bool, note string) (*Request, error) {
	status := StatusRejected
	if approve {
		status = StatusApproved
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE hr_requests SET status=$2, decided_by=$3, decided_at=now(),
			decision_note=$4, updated_at=now()
		 WHERE id=$1 AND status='pending'`, id, status, approver, note)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotPending
	}
	return s.Get(ctx, id)
}

// Cancel — requester withdraws a still-pending request.
func (s *Store) Cancel(ctx context.Context, id, requester uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE hr_requests SET status='cancelled', updated_at=now()
		 WHERE id=$1 AND requester=$2 AND status='pending'`, id, requester)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotPending
	}
	return nil
}

// ---------- attendance / 考勤 ----------

// workDate resolves a timestamp to its local work-date string (YYYY-MM-DD)
// in the server timezone — daily rows key on this.
func workDate(t time.Time) string { return t.Format("2006-01-02") }

func (s *Store) CheckIn(ctx context.Context, userID uuid.UUID, at time.Time, note string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO hr_attendance (id, user_id, work_date, check_in, note)
		 VALUES ($1,$2,$3,$4,$5)
		 ON CONFLICT (user_id, work_date) DO NOTHING`,
		uuid.Must(uuid.NewV7()), userID, workDate(at), at, note)
	return err
}

func (s *Store) CheckOut(ctx context.Context, userID uuid.UUID, at time.Time) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE hr_attendance SET check_out=$3, updated_at=now()
		 WHERE user_id=$1 AND work_date=$2`, userID, workDate(at), at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotClocked
	}
	return nil
}

// Month — a user's daily rows for YYYY-MM plus computed minutes/lateness.
// workStart like "09:00" defines the late threshold (local time).
func (s *Store) Month(ctx context.Context, userID uuid.UUID, month string, workStart time.Duration) ([]Day, Summary, error) {
	start, err := time.Parse("2006-01", month)
	if err != nil {
		return nil, Summary{}, fmt.Errorf("bad month %q", month)
	}
	end := start.AddDate(0, 1, 0)
	rows, err := s.pool.Query(ctx,
		`SELECT work_date::text, check_in, check_out, note
		 FROM hr_attendance
		 WHERE user_id=$1 AND work_date >= $2 AND work_date < $3
		 ORDER BY work_date`, userID, start.Format("2006-01-02"), end.Format("2006-01-02"))
	if err != nil {
		return nil, Summary{}, err
	}
	defer rows.Close()
	sum := Summary{UserID: userID}
	var days []Day
	for rows.Next() {
		var d Day
		var ci time.Time
		var cout *time.Time
		if err := rows.Scan(&d.WorkDate, &ci, &cout, &d.Note); err != nil {
			return nil, Summary{}, err
		}
		d.CheckIn, d.CheckOut = ci, cout
		if cout != nil {
			d.Minutes = int(cout.Sub(ci).Minutes())
			if d.Minutes < 0 {
				d.Minutes = 0
			}
		}
		// late = first check-in after workStart on the local clock
		d.Late = ci.Hour()*60+ci.Minute() > int(workStart.Minutes())
		sum.Days++
		sum.TotalMinutes += d.Minutes
		if d.Late {
			sum.LateDays++
		}
		if cout == nil {
			sum.MissingCheckout++
		}
		days = append(days, d)
	}
	return days, sum, rows.Err()
}

// SummaryAll — per-user monthly aggregate for the admin view.
func (s *Store) SummaryAll(ctx context.Context, month string, workStart time.Duration) ([]Summary, error) {
	start, err := time.Parse("2006-01", month)
	if err != nil {
		return nil, fmt.Errorf("bad month %q", month)
	}
	end := start.AddDate(0, 1, 0)
	// All active employees get a row — "全员假勤" means zero-record months too.
	// 全员视图：在职员工都列行，全月无记录显示 0。
	rows, err := s.pool.Query(ctx,
		`SELECT u.id, u.display_name, count(a.id),
			coalesce(sum(extract(epoch from (a.check_out - a.check_in))/60),0)::int,
			count(a.id) FILTER (WHERE a.check_out IS NULL)
		 FROM users u
		 LEFT JOIN hr_attendance a ON a.user_id = u.id
		   AND a.work_date >= $1 AND a.work_date < $2
		 WHERE u.status = 'active'
		 GROUP BY u.id, u.display_name ORDER BY u.display_name`,
		start.Format("2006-01-02"), end.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Summary
	for rows.Next() {
		var s Summary
		if err := rows.Scan(&s.UserID, &s.UserName, &s.Days, &s.TotalMinutes, &s.MissingCheckout); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if rows.Err() != nil {
		return nil, rows.Err()
	}
	// late days need per-row inspection — fill in a second pass.
	for i := range out {
		days, _, err := s.Month(ctx, out[i].UserID, month, workStart)
		if err == nil {
			for _, d := range days {
				if d.Late {
					out[i].LateDays++
				}
			}
		}
	}
	// 假勤合并：本月已批准请假天数（与月份重叠段， capped by payload.days）
	return s.fillLeaveDays(ctx, out, start, end)
}

// fillLeaveDays sums approved-leave days overlapping [start,end) per user.
// Days come from payload.days when present, capped by the date-range overlap.
// fillLeaveDays 汇总每人本月已批准请假天数：优先取 payload.days，封顶于与
// 月份重叠的日期段。
func (s *Store) fillLeaveDays(ctx context.Context, out []Summary, start, end time.Time) ([]Summary, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT hr.requester, u.display_name, coalesce(sum(
			least(
				coalesce(nullif(hr.payload->>'days','')::float8, 9999),
				least((hr.payload->>'end_date')::date, $2::date - 1)
					- greatest((hr.payload->>'start_date')::date, $1::date) + 1
			)), 0)::float8
		 FROM hr_requests hr JOIN users u ON u.id = hr.requester
		 WHERE hr.kind='leave' AND hr.status='approved'
		   AND (hr.payload->>'start_date')::date < $2::date
		   AND (hr.payload->>'end_date')::date >= $1::date
		 GROUP BY hr.requester, u.display_name`,
		start.Format("2006-01-02"), end.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	leave := map[uuid.UUID]float64{}
	names := map[uuid.UUID]string{}
	for rows.Next() {
		var uid uuid.UUID
		var name string
		var n float64
		if err := rows.Scan(&uid, &name, &n); err != nil {
			return nil, err
		}
		leave[uid], names[uid] = n, name
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// users with only leave (no attendance rows) must appear too — merge.
	seen := map[uuid.UUID]int{}
	for i := range out {
		seen[out[i].UserID] = i
		out[i].LeaveDays = leave[out[i].UserID]
	}
	for uid, n := range leave {
		if _, ok := seen[uid]; !ok && n > 0 {
			out = append(out, Summary{UserID: uid, UserName: names[uid], LeaveDays: n})
		}
	}
	return out, nil
}

// ValidatePayload — kind-specific required-field checks.
// ValidatePayload — 按类型校验必填字段。
func ValidatePayload(kind string, p json.RawMessage) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(p, &m); err != nil || len(m) == 0 {
		return ErrBadPayload
	}
	str := func(k string) string {
		var s string
		_ = json.Unmarshal(m[k], &s)
		return s
	}
	switch kind {
	case KindLeave:
		if str("leave_type") == "" || str("start_date") == "" || str("end_date") == "" {
			return ErrBadPayload
		}
		if _, err := time.Parse("2006-01-02", str("start_date")); err != nil {
			return ErrBadPayload
		}
		if _, err := time.Parse("2006-01-02", str("end_date")); err != nil {
			return ErrBadPayload
		}
	case KindContract:
		if str("title") == "" || str("counterparty") == "" {
			return ErrBadPayload
		}
	case KindExpense:
		var cents int64
		if err := json.Unmarshal(m["amount_cents"], &cents); err != nil || cents <= 0 {
			return ErrBadPayload
		}
		if str("category") == "" {
			return ErrBadPayload
		}
	default:
		return ErrBadPayload
	}
	return nil
}
