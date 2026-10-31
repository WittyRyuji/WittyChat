-- Internal HR: unified request table (leave/contract/expense) with a
-- single-step approval flow, plus daily attendance (check-in/out).
-- 内部人力系统：统一申请表（请假/合同/报销，单级审批）+ 每日考勤打卡。

CREATE TABLE hr_requests (
    id            uuid PRIMARY KEY,
    kind          text NOT NULL CHECK (kind IN ('leave','contract','expense')),
    requester     uuid NOT NULL REFERENCES users (id),
    status        text NOT NULL DEFAULT 'pending'
                  CHECK (status IN ('pending','approved','rejected','cancelled')),
    payload       jsonb NOT NULL,
    decided_by    uuid REFERENCES users (id),
    decided_at    timestamptz,
    decision_note text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX hr_requests_requester_idx ON hr_requests (requester, created_at DESC);
CREATE INDEX hr_requests_pending_idx ON hr_requests (created_at) WHERE status = 'pending';

-- One row per user per work day: first check-in wins check_in,
-- latest check-out wins check_out (re-checkout allowed same day).
-- 每用户每天一行：当天首次打卡记 check_in，最后一次签退记 check_out。
CREATE TABLE hr_attendance (
    id         uuid PRIMARY KEY,
    user_id    uuid NOT NULL REFERENCES users (id),
    work_date  date NOT NULL,
    check_in   timestamptz NOT NULL,
    check_out  timestamptz,
    note       text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, work_date)
);
CREATE INDEX hr_attendance_user_idx ON hr_attendance (user_id, work_date);
