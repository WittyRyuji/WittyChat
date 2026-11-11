import { useCallback, useEffect, useState } from "react";
import {
	type MIAccount,
	type MIJob,
	type MIPreset,
	type MITestResult,
	miApi,
} from "./api";

const fmtBytes = (n: number) =>
	n > 1e9
		? `${(n / 1e9).toFixed(1)}GB`
		: n > 1e6
			? `${(n / 1e6).toFixed(1)}MB`
			: `${(n / 1e3).toFixed(1)}KB`;
const STATUS_LABEL: Record<string, string> = {
	draft: "草稿",
	running: "运行中",
	paused: "已暂停",
	done: "已完成",
	cancelled: "已取消",
	pending: "待处理",
	queued: "排队中",
	error: "失败",
};

export function MailImport() {
	const [jobs, setJobs] = useState<MIJob[]>([]);
	const [sel, setSel] = useState<string | null>(null);
	const [showCreate, setShowCreate] = useState(false);
	const refresh = useCallback(() => {
		miApi
			.jobs()
			.then(setJobs)
			.catch(() => {});
	}, []);
	useEffect(refresh, [refresh]);
	if (sel)
		return (
			<JobDetail
				id={sel}
				onBack={() => {
					setSel(null);
					refresh();
				}}
			/>
		);
	return (
		<div>
			<div className="row" style={{ justifyContent: "space-between" }}>
				<h3>邮件搬家（IMAP）</h3>
				<button
					type="button"
					className="primary"
					onClick={() => setShowCreate(!showCreate)}
				>
					{showCreate ? "收起" : "新建搬家任务"}
				</button>
			</div>
			{showCreate && (
				<CreateForm
					onCreated={(id) => {
						setShowCreate(false);
						setSel(id);
					}}
				/>
			)}
			<Guidance />
			<table className="tbl">
				<thead>
					<tr>
						<th>名称</th>
						<th>来源</th>
						<th>状态</th>
						<th>账号</th>
						<th>进度</th>
						<th>失败</th>
					</tr>
				</thead>
				<tbody>
					{jobs.map((j) => (
						<tr
							key={j.id}
							style={{ cursor: "pointer" }}
							onClick={() => setSel(j.id)}
						>
							<td>{j.name}</td>
							<td>{j.provider === "feishu" ? "飞书邮箱" : j.host}</td>
							<td>
								{STATUS_LABEL[j.status] ?? j.status}
								{j.secrets_purged ? " · 凭据已清除" : ""}
							</td>
							<td>{j.accounts}</td>
							<td>
								{j.msgs_done}/{j.msgs_total}
							</td>
							<td>{/* per-job failure count via detail */}-</td>
						</tr>
					))}
					{jobs.length === 0 && (
						<tr>
							<td colSpan={6} className="muted">
								暂无搬家任务
							</td>
						</tr>
					)}
				</tbody>
			</table>
		</div>
	);
}

function Guidance() {
	return (
		<div className="panel">
			<b>迁移步骤</b>
			<ol style={{ margin: "8px 0 0 18px", lineHeight: 1.7 }}>
				<li>飞书管理后台 → 邮箱 → 邮箱工具 → 第三方邮箱客户端：开启</li>
				<li>
					员工在飞书桌面端 设置 → 邮箱 → 第三方邮箱客户端登录：生成专用密码
				</li>
				<li>汇总 CSV 上传</li>
				<li>测试连接</li>
				<li>开始搬历史邮件</li>
				<li>将域名 MX 切到 WittyChat</li>
				<li>再跑一次增量补齐切换期间的邮件</li>
				<li>完成并清除凭据</li>
			</ol>
		</div>
	);
}

function CreateForm({ onCreated }: { onCreated: (id: string) => void }) {
	const [presets, setPresets] = useState<MIPreset[]>([]);
	const [f, setF] = useState({
		name: "",
		provider: "custom",
		host: "",
		port: 993,
		tls_mode: "tls",
		since: "",
		add_alias: true,
	});
	const [err, setErr] = useState("");
	const [busy, setBusy] = useState(false);
	useEffect(() => {
		miApi
			.presets()
			.then(setPresets)
			.catch(() => {});
	}, []);
	const help = presets.find((p) => p.key === f.provider)?.help ?? "";
	return (
		<div className="panel">
			<div className="row">
				<label>
					预设
					<select
						value={f.provider}
						onChange={(e) => {
							const p = presets.find((x) => x.key === e.target.value);
							setF((s) => ({
								...s,
								provider: e.target.value,
								host: p?.host ?? s.host,
								port: p?.port ?? s.port,
								tls_mode: p?.tls_mode ?? s.tls_mode,
							}));
						}}
					>
						{presets.map((p) => (
							<option key={p.key} value={p.key}>
								{p.name}
							</option>
						))}
						{presets.length === 0 && (
							<option value="custom">自定义 IMAP</option>
						)}
					</select>
				</label>
				<label>
					任务名{" "}
					<input
						value={f.name}
						onChange={(e) => setF({ ...f, name: e.target.value })}
						placeholder="飞书邮件搬家"
					/>
				</label>
			</div>
			<div className="row">
				<label>
					服务器{" "}
					<input
						value={f.host}
						onChange={(e) => setF({ ...f, host: e.target.value })}
						placeholder="imap.feishu.cn"
					/>
				</label>
				<label>
					端口{" "}
					<input
						type="number"
						value={f.port}
						onChange={(e) => setF({ ...f, port: +e.target.value })}
					/>
				</label>
				<label>
					加密
					<select
						value={f.tls_mode}
						onChange={(e) => setF({ ...f, tls_mode: e.target.value })}
					>
						<option value="tls">TLS（993）</option>
						<option value="starttls">STARTTLS（143）</option>
						<option value="plain">明文（仅本机调试）</option>
					</select>
				</label>
				<label>
					起始日期（可选）{" "}
					<input
						type="date"
						value={f.since}
						onChange={(e) => setF({ ...f, since: e.target.value })}
					/>
				</label>
				<label>
					<input
						type="checkbox"
						checked={f.add_alias}
						onChange={(e) => setF({ ...f, add_alias: e.target.checked })}
					/>{" "}
					旧地址加为别名
				</label>
			</div>
			{help && (
				<div className="muted" style={{ fontSize: 12, margin: "6px 0" }}>
					{help}
				</div>
			)}
			{err && <div className="err">{err}</div>}
			<button
				type="button"
				className="primary"
				disabled={busy || !f.name || !f.host}
				onClick={async () => {
					setBusy(true);
					setErr("");
					try {
						const r = await miApi.createJob({
							name: f.name,
							provider: f.provider,
							host: f.host,
							port: f.port,
							tls_mode: f.tls_mode,
							since: f.since ? `${f.since}T00:00:00Z` : null,
							add_alias: f.add_alias,
						});
						onCreated(r.id);
					} catch (e) {
						setErr(String(e));
					} finally {
						setBusy(false);
					}
				}}
			>
				创建
			</button>
		</div>
	);
}

function JobDetail({ id, onBack }: { id: string; onBack: () => void }) {
	const [d, setD] = useState<{
		job: MIJob;
		accounts: MIAccount[];
		failures: {
			folder: string;
			uid: number;
			error: string;
			attempts: number;
		}[];
	} | null>(null);
	const [csv, setCsv] = useState("");
	const [csvResult, setCsvResult] = useState<string>("");
	const [testRes, setTestRes] = useState<MITestResult[] | null>(null);
	const [busy, setBusy] = useState("");
	const running = d?.job.status === "running";
	const load = useCallback(() => {
		miApi
			.job(id)
			.then(setD)
			.catch(() => {});
	}, [id]);
	useEffect(load, [load]);
	// auto-refresh every 3s while running / 运行中 3 秒自动刷新
	useEffect(() => {
		if (!running) return;
		const t = setInterval(load, 3000);
		return () => clearInterval(t);
	}, [running, load]);
	if (!d) return <div>加载中…</div>;
	const j = d.job;
	const act = async (fn: () => Promise<unknown>, label: string) => {
		setBusy(label);
		try {
			await fn();
			load();
		} catch (e) {
			alert(String(e));
		} finally {
			setBusy("");
		}
	};
	const uploadCsv = async () => {
		setBusy("csv");
		try {
			const r = await miApi.accounts(id, csv);
			setCsvResult(
				`新增 ${r.added}，更新 ${r.updated}${r.errors.length ? `，${r.errors.length} 行错误` : ""}` +
					(r.errors.length
						? `\n${r.errors.map((e) => `第${e.line}行：${e.error}`).join("\n")}`
						: ""),
			);
			setCsv("");
			load();
		} catch (e) {
			alert(String(e));
		} finally {
			setBusy("");
		}
	};
	return (
		<div>
			<div className="row">
				<button type="button" className="ghost" onClick={onBack}>
					← 返回
				</button>
				<h3>{j.name}</h3>
				<span className="muted">
					{STATUS_LABEL[j.status] ?? j.status} · {j.host}:{j.port}/{j.tls_mode}
				</span>
			</div>
			{j.secrets_purged && (
				<div className="card warn">凭据已清除 — 无法再次启动；可删除任务。</div>
			)}
			<div className="row" style={{ gap: 8 }}>
				{(j.status === "draft" || j.status === "paused") &&
					!j.secrets_purged && (
						<button
							type="button"
							className="primary"
							disabled={!!busy}
							onClick={() => act(() => miApi.start(id), "start")}
						>
							{j.status === "draft" ? "开始" : "继续"}
						</button>
					)}
				{j.status === "done" && !j.secrets_purged && (
					<button
						type="button"
						className="primary"
						disabled={!!busy}
						onClick={() => act(() => miApi.start(id), "start")}
					>
						再跑一次（增量）
					</button>
				)}
				{j.status === "running" && (
					<button
						type="button"
						disabled={!!busy}
						onClick={() => act(() => miApi.pause(id), "pause")}
					>
						暂停
					</button>
				)}
				{j.status !== "cancelled" && j.status !== "done" && (
					<button
						type="button"
						disabled={!!busy}
						onClick={() => {
							if (confirm("确定取消？已导入的邮件保留，运行停止。"))
								act(() => miApi.cancel(id), "cancel");
						}}
					>
						取消
					</button>
				)}
				{j.status === "done" && !j.secrets_purged && (
					<button
						type="button"
						disabled={!!busy}
						onClick={() => {
							if (confirm("清除所有账号密码？之后无法再增量导入。"))
								act(() => miApi.finish(id), "finish");
						}}
					>
						完成并清除凭据
					</button>
				)}
				{j.status !== "running" && (
					<button
						type="button"
						className="ghost"
						disabled={!!busy}
						onClick={() => {
							if (confirm("删除任务？已导入的邮件保留，账号与映射删除。"))
								act(() => miApi.del(id).then(() => onBack()), "del");
						}}
					>
						删除任务
					</button>
				)}
				<button
					type="button"
					disabled={!!busy}
					onClick={async () => {
						setBusy("test");
						try {
							setTestRes(await miApi.test(id));
						} catch (e) {
							alert(String(e));
						} finally {
							setBusy("");
						}
					}}
				>
					测试连接
				</button>
			</div>
			{testRes && (
				<div className="panel">
					{testRes.map((r) => {
						const acct = d.accounts.find((a) => a.id === r.account_id);
						return (
							<div
								key={r.account_id}
								className={r.ok ? "ok" : "err"}
								style={{ fontSize: 13 }}
							>
								{acct?.source_login ?? r.account_id}：
								{r.ok ? `连接成功（${r.folders} 个目录）` : r.error}
							</div>
						);
					})}
				</div>
			)}
			<div className="panel">
				<b>CSV 账号上传</b>
				<div className="muted" style={{ fontSize: 12, margin: "4px 0" }}>
					格式：source_email,password,target_email（可带表头行）。
					<a
						href="data:text/csv,source_email%2Cpassword%2Ctarget_email%0Azhang%40feishu.cn%2CAPP_PASS%2Calice%40wittyloong.com%0A"
						download="mail-import-template.csv"
						style={{ marginLeft: 8 }}
					>
						下载模板
					</a>
				</div>
				<textarea
					rows={5}
					style={{ width: "100%" }}
					value={csv}
					onChange={(e) => setCsv(e.target.value)}
					placeholder={
						"source_email,password,target_email\nzhang@feishu.cn,app-pass,alice@wittyloong.com"
					}
				/>
				<div className="row">
					<button
						type="button"
						className="primary"
						disabled={!!busy || !csv.trim() || j.status === "running"}
						onClick={uploadCsv}
					>
						上传
					</button>
					<label>
						或选择文件{" "}
						<input
							type="file"
							accept=".csv"
							onChange={async (e) => {
								const f = e.target.files?.[0];
								if (f) setCsv(await f.text());
							}}
						/>
					</label>
				</div>
				{csvResult && (
					<pre
						style={{ fontSize: 12, whiteSpace: "pre-wrap", margin: "6px 0" }}
					>
						{csvResult}
					</pre>
				)}
			</div>
			<table className="tbl">
				<thead>
					<tr>
						<th>来源账号</th>
						<th>目标</th>
						<th>状态</th>
						<th>进度</th>
						<th>完成/跳过/失败</th>
						<th>字节</th>
						<th>备注/错误</th>
						<th></th>
					</tr>
				</thead>
				<tbody>
					{d.accounts.map((a) => (
						<AcctRow
							key={a.id}
							a={a}
							failures={d.failures}
							jobId={id}
							onChange={load}
						/>
					))}
					{d.accounts.length === 0 && (
						<tr>
							<td colSpan={8} className="muted">
								先上传 CSV 账号
							</td>
						</tr>
					)}
				</tbody>
			</table>
		</div>
	);
}

function AcctRow({
	a,
	failures,
	jobId,
	onChange,
}: {
	a: MIAccount;
	jobId: string;
	failures: { folder: string; uid: number; error: string; attempts: number }[];
	onChange: () => void;
}) {
	const [open, setOpen] = useState(false);
	const mine = failures;
	const pct = a.msgs_total ? Math.round((100 * a.msgs_done) / a.msgs_total) : 0;
	return (
		<>
			<tr>
				<td>{a.source_login}</td>
				<td className="muted">{a.target_user.slice(0, 8)}…</td>
				<td>{STATUS_LABEL[a.status] ?? a.status}</td>
				<td>
					<div className="bar">
						<div className="fill" style={{ width: `${pct}%` }} />
					</div>
					<span style={{ fontSize: 11 }}>
						{a.msgs_done}/{a.msgs_total}
					</span>
				</td>
				<td>
					{a.msgs_done}/{a.msgs_skipped}/{a.msgs_failed}
				</td>
				<td>{fmtBytes(a.bytes_done)}</td>
				<td style={{ fontSize: 12 }}>
					{a.note && <div className="muted">{a.note}</div>}
					{a.last_error && <div className="err">{a.last_error}</div>}
				</td>
				<td>
					{a.failures_count > 0 && (
						<button
							type="button"
							className="ghost"
							onClick={() => setOpen(!open)}
						>
							{open ? "收起" : `${a.failures_count} 失败`}
						</button>
					)}
					<button
						type="button"
						className="ghost"
						onClick={async () => {
							if (confirm(`删除账号 ${a.source_login}？`)) {
								await miApi
									.accountDel(jobId, a.id)
									.catch((e) => alert(String(e)));
								onChange();
							}
						}}
					>
						删
					</button>
				</td>
			</tr>
			{open &&
				mine.map((f) => (
					<tr key={`${f.folder}-${f.uid}`}>
						<td colSpan={8} style={{ fontSize: 12, paddingLeft: 20 }}>
							{f.folder} uid {f.uid} · 第{f.attempts}次：{f.error}
						</td>
					</tr>
				))}
		</>
	);
}
