package apitypes

// HR API payloads / 人力系统 API 载荷。

import "encoding/json"

// HRRequestCreate — kind + free-form payload validated server-side per kind:
//
//	leave:    {leave_type, start_date, end_date, days?, reason?}
//	contract: {title, counterparty, amount_cents?, attachment_object_id?, reason?}
//	expense:  {category, amount_cents, currency?, invoice_date?, attachment_object_id?, memo?}
type HRRequestCreate struct {
	Kind    string          `json:"kind" required:"true" enum:"leave,contract,expense"`
	Payload json.RawMessage `json:"payload" required:"true"`
}

// HRRequest — request row as returned to clients.
type HRRequest struct {
	ID             string          `json:"id"`
	Kind           string          `json:"kind"`
	Requester      string          `json:"requester"`
	RequesterName  string          `json:"requester_name,omitempty"`
	RequesterEmail string          `json:"requester_email,omitempty"`
	Status         string          `json:"status"`
	Payload        json.RawMessage `json:"payload"`
	DecidedBy      string          `json:"decided_by,omitempty"`
	DeciderName    string          `json:"decider_name,omitempty"`
	DecidedAt      string          `json:"decided_at,omitempty"`
	DecisionNote   string          `json:"decision_note,omitempty"`
	CreatedAt      string          `json:"created_at"`
}

type HRRequestList struct {
	Requests []HRRequest `json:"requests"`
}

// HRDecide — approve=true / reject=false with optional note.
type HRDecide struct {
	Approve bool   `json:"approve"`
	Note    string `json:"note,omitempty" maxLength:"500"`
}

type HRCheckIn struct {
	Note string `json:"note,omitempty" maxLength:"200"`
}

type HRDay struct {
	WorkDate string `json:"work_date"`
	CheckIn  string `json:"check_in"`
	CheckOut string `json:"check_out,omitempty"`
	Note     string `json:"note,omitempty"`
	Minutes  int    `json:"minutes"`
	Late     bool   `json:"late"`
}

type HRSummary struct {
	UserID          string  `json:"user_id"`
	UserName        string  `json:"user_name,omitempty"`
	Days            int     `json:"days"`
	TotalMinutes    int     `json:"total_minutes"`
	LateDays        int     `json:"late_days"`
	MissingCheckout int     `json:"missing_checkout"`
	LeaveDays       float64 `json:"leave_days"`
}

type HRMonthResponse struct {
	Days    []HRDay   `json:"days"`
	Summary HRSummary `json:"summary"`
}

type HRSummaryList struct {
	Rows []HRSummary `json:"rows"`
}
