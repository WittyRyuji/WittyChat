// HR page — internal lightweight HR: leave/contract/expense requests with
// single-step admin approval, plus daily check-in/out and monthly
// attendance calculation.
//
// 人力页：请假/合同/报销申请（管理员单级审批）+ 每日打卡签退 + 月度出勤计算。
import { useCallback, useEffect, useState } from "react";
import { type HRDay, type HRRequest, type HRSummary, hrApi, type UserPublic } from "./api";

type Tab = "requests" | "approvals" | "attendance";

const KIND_LABEL: Record<string, string> = {
  leave: "请假",
  contract: "合同",
  expense: "报销",
};
const STATUS_LABEL: Record<string, string> = {
  pending: "待审批",
  approved: "已批准",
  rejected: "已驳回",
  cancelled: "已撤销",
};

function payloadSummary(r: HRRequest): string {
  const p = r.payload as Record<string, unknown>;
  switch (r.kind) {
    case "leave":
      return `${p.leave_type ?? ""} ${p.start_date ?? ""}~${p.end_date ?? ""} ${p.days ?? ""}天 ${p.reason ?? ""}`;
    case "contract":
      return `${p.title ?? ""} · ${p.counterparty ?? ""}${p.amount_cents ? ` ¥${(Number(p.amount_cents) / 100).toFixed(2)}` : ""}`;
    case "expense":
      return `${p.category ?? ""} ¥${((Number(p.amount_cents) || 0) / 100).toFixed(2)} ${p.memo ?? ""}`;
    default:
      return JSON.stringify(p);
  }
}

function fmtHM(min: number): string {
  const h = Math.floor(min / 60);
  const m = min % 60;
  return h > 0 ? `${h}h${m}m` : `${m}m`;
}
function fmtTime(iso?: string): string {
  return iso
    ? new Date(iso).toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit" })
    : "—";
}
function curMonth(): string {
  const d = new Date();
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}`;
}

export function HR({ me }: { me: UserPublic }) {
  const [tab, setTab] = useState<Tab>("requests");
  return (
    <div className="chat">
      <aside className="sidebar">
        <div className="side-head">
          <span className="me">人力</span>
        </div>
        <div className="conv-list">
          {(
            [
              ["requests", "我的申请"],
              ...(me.is_admin ? ([["approvals", "审批"]] as const) : []),
              ["attendance", "考勤"],
            ] as [Tab, string][]
          ).map(([k, name]) => (
            <button
              type="button"
              key={k}
              className={`conv ${tab === k ? "active" : ""}`}
              onClick={() => setTab(k)}
            >
              <span className="conv-title">{name}</span>
            </button>
          ))}
        </div>
      </aside>
      <section className="main settings-body">
        {tab === "requests" && <MyRequests />}
        {tab === "approvals" && me.is_admin && <Approvals />}
        {tab === "attendance" && <Attendance me={me} />}
      </section>
    </div>
  );
}

// ---------- my requests + new request form ----------

function MyRequests() {
  const [rows, setRows] = useState<HRRequest[]>([]);
  const [kind, setKind] = useState<"leave" | "contract" | "expense">("leave");
  const [f, setF] = useState<Record<string, string>>({});
  const [msg, setMsg] = useState("");

  const reload = useCallback(() => {
    hrApi
      .list("mine")
      .then((r) => setRows(r.requests))
      .catch(() => {});
  }, []);
  useEffect(reload, [reload]);

  const set =
    (k: string) =>
    (e: React.ChangeEvent<HTMLInputElement | HTMLTextAreaElement | HTMLSelectElement>) =>
      setF((prev) => ({ ...prev, [k]: e.target.value }));

  const submit = async () => {
    setMsg("");
    const payload: Record<string, unknown> = {};
    if (kind === "leave") {
      Object.assign(payload, {
        leave_type: f.leave_type || "annual",
        start_date: f.start_date,
        end_date: f.end_date,
        days: Number(f.days || 0) || undefined,
        reason: f.reason,
      });
    } else if (kind === "contract") {
      Object.assign(payload, {
        title: f.title,
        counterparty: f.counterparty,
        amount_cents: f.amount ? Math.round(Number(f.amount) * 100) : undefined,
        reason: f.reason,
      });
    } else {
      Object.assign(payload, {
        category: f.category || "other",
        amount_cents: Math.round(Number(f.amount || 0) * 100),
        currency: f.currency || "CNY",
        invoice_date: f.invoice_date || undefined,
        memo: f.memo,
      });
    }
    try {
      await hrApi.create(kind, payload);
      setF({});
      setMsg("已提交");
      reload();
    } catch (e) {
      setMsg(`提交失败: ${e instanceof Error ? e.message : e}`);
    }
  };

  return (
    <div className="hr-page">
      <h3>新建申请</h3>
      <div className="hr-form">
        <select
          value={kind}
          onChange={(e) => {
            setKind(e.target.value as typeof kind);
            setF({});
          }}
        >
          <option value="leave">请假</option>
          <option value="contract">合同申请</option>
          <option value="expense">报销</option>
        </select>
        {kind === "leave" && (
          <>
            <select value={f.leave_type ?? "annual"} onChange={set("leave_type")}>
              <option value="annual">年假</option>
              <option value="sick">病假</option>
              <option value="personal">事假</option>
              <option value="other">其他</option>
            </select>
            <input type="date" value={f.start_date ?? ""} onChange={set("start_date")} />
            <input type="date" value={f.end_date ?? ""} onChange={set("end_date")} />
            <input
              type="number"
              min="0.5"
              step="0.5"
              placeholder="天数"
              value={f.days ?? ""}
              onChange={set("days")}
            />
            <input placeholder="事由" value={f.reason ?? ""} onChange={set("reason")} />
          </>
        )}
        {kind === "contract" && (
          <>
            <input placeholder="合同标题" value={f.title ?? ""} onChange={set("title")} />
            <input
              placeholder="对方单位"
              value={f.counterparty ?? ""}
              onChange={set("counterparty")}
            />
            <input
              type="number"
              min="0"
              step="0.01"
              placeholder="金额（元，可空）"
              value={f.amount ?? ""}
              onChange={set("amount")}
            />
            <input placeholder="事由" value={f.reason ?? ""} onChange={set("reason")} />
          </>
        )}
        {kind === "expense" && (
          <>
            <select value={f.category ?? "travel"} onChange={set("category")}>
              <option value="travel">差旅</option>
              <option value="meal">餐饮</option>
              <option value="office">办公</option>
              <option value="other">其他</option>
            </select>
            <input
              type="number"
              min="0"
              step="0.01"
              placeholder="金额（元）"
              value={f.amount ?? ""}
              onChange={set("amount")}
            />
            <input type="date" value={f.invoice_date ?? ""} onChange={set("invoice_date")} />
            <input placeholder="备注" value={f.memo ?? ""} onChange={set("memo")} />
          </>
        )}
        <button type="button" className="primary" onClick={submit}>
          提交
        </button>
        {msg && <span className="dim">{msg}</span>}
      </div>

      <h3>我的申请</h3>
      <div className="hr-list">
        {rows.length === 0 && <p className="dim">暂无申请</p>}
        {rows.map((r) => (
          <div key={r.id} className="hr-row">
            <span className={`hr-kind k-${r.kind}`}>{KIND_LABEL[r.kind]}</span>
            <span className="hr-sum">{payloadSummary(r)}</span>
            <span className={`hr-status s-${r.status}`}>{STATUS_LABEL[r.status]}</span>
            <span className="dim">{new Date(r.created_at).toLocaleDateString("zh-CN")}</span>
            {r.status === "pending" && (
              <button
                type="button"
                className="ghost"
                onClick={() => hrApi.cancel(r.id).then(reload)}
              >
                撤销
              </button>
            )}
            {r.decision_note && <span className="dim">批注:{r.decision_note}</span>}
          </div>
        ))}
      </div>
    </div>
  );
}

// ---------- admin approval queue ----------

function Approvals() {
  const [rows, setRows] = useState<HRRequest[]>([]);
  const [note, setNote] = useState<Record<string, string>>({});
  const reload = useCallback(() => {
    hrApi
      .list("pending")
      .then((r) => setRows(r.requests))
      .catch(() => {});
  }, []);
  useEffect(reload, [reload]);

  const decide = async (id: string, approve: boolean) => {
    try {
      await hrApi.decide(id, approve, note[id]);
    } catch {
      /* row disappears only on success anyway */
    }
    reload();
  };

  return (
    <div className="hr-page">
      <h3>待审批（{rows.length}）</h3>
      <div className="hr-list">
        {rows.length === 0 && <p className="dim">没有待审批事项</p>}
        {rows.map((r) => (
          <div key={r.id} className="hr-row">
            <span className={`hr-kind k-${r.kind}`}>{KIND_LABEL[r.kind]}</span>
            <span className="hr-sum">
              <b>{r.requester_name}</b> {payloadSummary(r)}
            </span>
            <input
              placeholder="批注"
              value={note[r.id] ?? ""}
              onChange={(e) => setNote((p) => ({ ...p, [r.id]: e.target.value }))}
            />
            <button type="button" className="primary" onClick={() => decide(r.id, true)}>
              批准
            </button>
            <button type="button" className="ghost" onClick={() => decide(r.id, false)}>
              驳回
            </button>
          </div>
        ))}
      </div>
    </div>
  );
}

// ---------- attendance / 考勤 ----------

function Attendance({ me }: { me: UserPublic }) {
  const [month, setMonth] = useState(curMonth());
  const [days, setDays] = useState<HRDay[]>([]);
  const [sum, setSum] = useState<HRSummary | null>(null);
  const [all, setAll] = useState<HRSummary[]>([]);
  const [msg, setMsg] = useState("");

  const reload = useCallback(() => {
    hrApi
      .myMonth(month)
      .then((r) => {
        setDays(r.days);
        setSum(r.summary);
      })
      .catch(() => {});
    if (me.is_admin) {
      hrApi
        .monthSummary(month)
        .then((r) => setAll(r.rows))
        .catch(() => setAll([]));
    }
  }, [month, me.is_admin]);
  useEffect(reload, [reload]);

  const today = days.find((d) => d.work_date === new Date().toISOString().slice(0, 10));

  const clock = async (out: boolean) => {
    setMsg("");
    try {
      if (out) await hrApi.checkOut();
      else await hrApi.checkIn();
      reload();
    } catch (e) {
      setMsg(e instanceof Error ? e.message : String(e));
    }
  };

  return (
    <div className="hr-page">
      <h3>考勤</h3>
      <div className="hr-clock">
        <button type="button" className="primary" disabled={!!today} onClick={() => clock(false)}>
          上班打卡{today ? ` ${fmtTime(today.check_in)}` : ""}
        </button>
        <button type="button" className="primary" disabled={!today} onClick={() => clock(true)}>
          下班签退{today?.check_out ? ` ${fmtTime(today.check_out)}` : ""}
        </button>
        {msg && <span className="dim">{msg}</span>}
      </div>

      <div className="hr-month">
        <input type="month" value={month} onChange={(e) => setMonth(e.target.value)} />
        {sum && (
          <span className="dim">
            出勤 {sum.days} 天 · 工时 {fmtHM(sum.total_minutes)} · 迟到 {sum.late_days} 天
            {sum.missing_checkout > 0 ? ` · ${sum.missing_checkout} 天未签退` : ""}
          </span>
        )}
      </div>

      <table className="hr-table">
        <thead>
          <tr>
            <th>日期</th>
            <th>打卡</th>
            <th>签退</th>
            <th>工时</th>
            <th>状态</th>
          </tr>
        </thead>
        <tbody>
          {days.map((d) => (
            <tr key={d.work_date}>
              <td>{d.work_date}</td>
              <td>{fmtTime(d.check_in)}</td>
              <td>{fmtTime(d.check_out)}</td>
              <td>{d.minutes ? fmtHM(d.minutes) : "—"}</td>
              <td>{d.late ? <span className="hr-status s-rejected">迟到</span> : ""}</td>
            </tr>
          ))}
          {days.length === 0 && (
            <tr>
              <td colSpan={5} className="dim">
                本月无记录
              </td>
            </tr>
          )}
        </tbody>
      </table>

      {me.is_admin && all.length > 0 && (
        <>
          <h3>全员 {month} 出勤</h3>
          <table className="hr-table">
            <thead>
              <tr>
                <th>成员</th>
                <th>天数</th>
                <th>总工时</th>
                <th>迟到</th>
                <th>未签退</th>
              </tr>
            </thead>
            <tbody>
              {all.map((s) => (
                <tr key={s.user_id}>
                  <td>{s.user_name}</td>
                  <td>{s.days}</td>
                  <td>{fmtHM(s.total_minutes)}</td>
                  <td>{s.late_days}</td>
                  <td>{s.missing_checkout}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </>
      )}
    </div>
  );
}
