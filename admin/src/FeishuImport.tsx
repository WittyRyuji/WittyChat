// 飞书迁移 — one wizard: app credentials, one-click whole-tenant migration
// (accounts, mail, wiki, personal-docs invitations), per-person progress.
// 飞书迁移向导：应用凭据、一键整体迁移（账号、邮件、知识库、个人文档邀请）、逐人进度。
import { useCallback, useEffect, useState } from "react";
import { type FIJob, type FIMigration, type FIPerson, fiApi } from "./api";

const PREP = [
	"飞书开放平台创建企业自建应用，添加「机器人」能力，可用范围设为全部成员",
	"通讯录：权限范围设为全部成员；开通 contact:contact.base:readonly、contact:user.base:readonly、contact:user.email:readonly",
	"邮箱：开通 mail:user_mailbox.message:readonly、mail:user_mailbox.message.body:read、mail:user_mailbox.message.subject:read、mail:user_mailbox.message.address:read、mail:user_mailbox.folder:read，邮件数据权限范围设为全部成员",
	"云文档：开通 docs:document:export、drive:drive:readonly、wiki:wiki:readonly；需要搬的知识库把应用加为成员",
	"机器人消息：开通 im:message:send_as_bot（用来给员工发激活链接和导入提醒）",
	"安全设置 → 重定向 URL：添加下面的回调地址；然后创建版本并发布",
];

const STATUS: Record<FIMigration["status"], string> = {
	idle: "未开始",
	running: "迁移中",
	syncing: "持续同步中（可随时切换 MX）",
	finishing: "收尾中（最后一轮全量）",
	finished: "已完成",
	error: "出错",
};

const MAIL: Record<string, string> = {
	none: "—",
	queued: "排队中",
	running: "搬迁中",
	done: "已完成",
	error: "出错",
};

function account(p: FIPerson): string {
	if (!p.user_id) return p.email ? "未匹配" : "无邮箱";
	if (p.provisioned)
		return p.user_status === "active" ? "新建·已激活" : "新建·待激活";
	return "已有账号";
}

const when = (s?: string) =>
	s
		? new Date(s).toLocaleString("zh-CN", {
				month: "2-digit",
				day: "2-digit",
				hour: "2-digit",
				minute: "2-digit",
			})
		: "";

export function FeishuImport() {
	const [app, setApp] = useState<{
		configured: boolean;
		app_id: string;
		base_url: string;
	} | null>(null);
	const [mig, setMig] = useState<FIMigration | null>(null);
	const [people, setPeople] = useState<FIPerson[]>([]);
	const [jobs, setJobs] = useState<FIJob[]>([]);
	const [appID, setAppID] = useState("");
	const [secret, setSecret] = useState("");
	const [provision, setProvision] = useState(true);
	const [continuous, setContinuous] = useState(true);
	const [err, setErr] = useState("");
	const [note, setNote] = useState("");
	const [busy, setBusy] = useState(false);

	const load = useCallback(() => {
		fiApi
			.app()
			.then(setApp)
			.catch(() => setErr("加载失败"));
		fiApi
			.migration()
			.then(setMig)
			.catch(() => {});
		fiApi
			.people()
			.then((l) => setPeople(l ?? []))
			.catch(() => {});
		fiApi
			.jobs()
			.then((l) => setJobs(l ?? []))
			.catch(() => {});
	}, []);
	useEffect(load, [load]);
	const live =
		mig?.status === "running" ||
		mig?.status === "syncing" ||
		mig?.status === "finishing";
	useEffect(() => {
		if (!live) return;
		const t = setInterval(load, 5000);
		return () => clearInterval(t);
	}, [live, load]);

	const run = async (fn: () => Promise<unknown>, done?: string) => {
		setBusy(true);
		setErr("");
		setNote("");
		try {
			await fn();
			if (done) setNote(done);
			load();
		} catch (e) {
			setErr(String(e));
		}
		setBusy(false);
	};

	const pending = people.filter(
		(p) => p.provisioned && p.user_status === "pending",
	).length;

	return (
		<section>
			<div className="panel">
				<h3>1. 飞书应用</h3>
				{app?.configured ? (
					<p className="ok">
						已配置 App ID {app.app_id}（{app.base_url}）
					</p>
				) : (
					<p className="warn">未配置</p>
				)}
				<label className="row">
					<span className="flabel">App ID</span>
					<input
						className="fs-input"
						name="feishu-app-id"
						autoComplete="off"
						value={appID}
						onChange={(e) => setAppID(e.target.value)}
					/>
				</label>
				<label className="row">
					<span className="flabel">App Secret</span>
					<input
						className="fs-input"
						type="password"
						name="feishu-app-secret"
						autoComplete="new-password"
						value={secret}
						onChange={(e) => setSecret(e.target.value)}
					/>
				</label>
				<div className="row">
					<span className="flabel" />
					<button
						type="button"
						className="primary"
						disabled={busy || !appID || !secret}
						onClick={() =>
							run(async () => {
								await fiApi.setApp(appID, secret);
								setSecret("");
							}, "已保存，凭据验证通过")
						}
					>
						保存
					</button>
					<button
						type="button"
						className="ghost danger"
						onClick={() =>
							confirm("清除飞书应用凭据？") && run(() => fiApi.clearApp())
						}
					>
						清除
					</button>
					<span className="muted">已保存的 Secret 不会回显</span>
				</div>
				{mig && (
					<div className="row">
						<span className="flabel">回调地址</span>
						<code>{mig.redirect_url}</code>
						{/localhost|127\.0\.0\.1/.test(mig.public_url) && (
							<span className="warn">
								当前对外地址是本机地址，员工电脑打不开；部署时请设置
								WITTYCHAT_PUBLIC_URL
							</span>
						)}
					</div>
				)}
				<details>
					<summary>应用需要的权限（一次性配置）</summary>
					<ol className="guide">
						{PREP.map((g) => (
							<li key={g}>{g}</li>
						))}
					</ol>
				</details>
			</div>

			<div className="panel">
				<h3>2. 整体迁移</h3>
				{mig && (
					<>
						<p>
							状态：<b>{STATUS[mig.status]}</b>
							{mig.last_error && (
								<span className="err"> — {mig.last_error}</span>
							)}
						</p>
						<p className="muted">
							在职 {mig.people} 人 · 已关联账号 {mig.linked} · 新建{" "}
							{mig.provisioned}
							（已激活 {mig.activated}）· 邮件完成 {mig.mail_done_people} 人 /
							进行中 {mig.mail_running_people} / 出错 {mig.mail_error_people} ·
							已搬 {mig.messages} 封 · 个人文档已授权 {mig.docs_authorized} 人
						</p>
					</>
				)}
				<label className="row">
					<input
						type="checkbox"
						checked={provision}
						onChange={(e) => setProvision(e.target.checked)}
					/>
					为没有 WittyChat 账号的员工自动建号，并由飞书机器人私信激活链接
				</label>
				<label className="row">
					<input
						type="checkbox"
						checked={continuous}
						onChange={(e) => setContinuous(e.target.checked)}
					/>
					持续同步新邮件（每 30 分钟），直到点「完成迁移」
				</label>
				<div className="row">
					<button
						type="button"
						disabled={busy || !app?.configured}
						onClick={() =>
							run(async () => {
								const st = await fiApi.dirSync();
								setNote(
									`通讯录：在职 ${st.Active} 人，已匹配 ${st.Matched}，待建号 ${st.Unmatched}，无邮箱 ${st.NoEmail}`,
								);
							})
						}
					>
						预览人员
					</button>
					<button
						type="button"
						className="primary"
						disabled={busy || !app?.configured || mig?.status === "finishing"}
						onClick={() =>
							confirm(
								"开始整体迁移？将同步通讯录、按设置建号、为每个人搬邮件、导入应用可见的知识库，并通知员工一键导入个人文档。",
							) &&
							run(
								() => fiApi.migStart(provision, continuous),
								"已开始，进度见下方",
							)
						}
					>
						{mig && mig.status !== "idle" ? "重新运行" : "开始迁移"}
					</button>
					<button
						type="button"
						disabled={
							busy || (mig?.status !== "running" && mig?.status !== "syncing")
						}
						onClick={() =>
							confirm(
								"确认域名 MX 已切到 WittyChat？将对每个人再跑一轮全量补齐，完成后停止同步。",
							) && run(() => fiApi.migFinish())
						}
					>
						完成迁移
					</button>
					{pending > 0 && (
						<button
							type="button"
							className="ghost"
							disabled={busy}
							onClick={() =>
								run(async () => {
									const r = await fiApi.resend();
									setNote(`已重发 ${r.sent} 个激活链接`);
								})
							}
						>
							给未激活的 {pending} 人重发链接
						</button>
					)}
				</div>
				<p className="muted">
					迁移开始后随时可以把域名 MX 指向
					WittyChat，持续同步会补上切换期间的邮件；确认无误后点「完成迁移」。
				</p>
				{note && <p className="ok">{note}</p>}
				{err && <p className="err">{err}</p>}
			</div>

			<div className="panel">
				<h3>3. 人员进度</h3>
				<table className="tbl">
					<thead>
						<tr>
							<th>姓名</th>
							<th>邮箱</th>
							<th>WittyChat 账号</th>
							<th>激活链接</th>
							<th>邮件</th>
							<th>个人文档</th>
							<th />
						</tr>
					</thead>
					<tbody>
						{people.map((p) => (
							<tr key={p.open_id}>
								<td>
									{p.name}
									{p.feishu_status !== "active" && (
										<span className="muted"> （{p.feishu_status}）</span>
									)}
								</td>
								<td>{p.email}</td>
								<td>{account(p)}</td>
								<td>
									{p.link_error ? (
										<span className="err">{p.link_error}</span>
									) : (
										when(p.link_sent_at)
									)}
								</td>
								<td>
									{MAIL[p.mail_status] ?? p.mail_status}
									{p.mail_done > 0 && ` · ${p.mail_done} 封`}
									{p.mail_failed > 0 && (
										<span className="err"> · 失败 {p.mail_failed}</span>
									)}
									{p.mail_error && <div className="err">{p.mail_error}</div>}
								</td>
								<td>{p.docs_authorized ? "已授权" : "—"}</td>
								<td>
									{p.user_id && p.mail_status !== "running" && (
										<button
											type="button"
											className="ghost"
											disabled={busy}
											onClick={() => run(() => fiApi.personMail(p.open_id))}
										>
											重搬邮件
										</button>
									)}
								</td>
							</tr>
						))}
					</tbody>
				</table>
				{people.length === 0 && (
					<p className="muted">
						还没有人员数据，先点「预览人员」或「开始迁移」。
					</p>
				)}
			</div>

			<div className="panel">
				<h3>文档导入任务</h3>
				<table className="tbl">
					<thead>
						<tr>
							<th>任务</th>
							<th>模式</th>
							<th>状态</th>
							<th>完成/跳过/失败/待</th>
						</tr>
					</thead>
					<tbody>
						{jobs.map((j) => (
							<tr key={j.id}>
								<td>{j.name}</td>
								<td>{j.mode === "admin" ? "批量" : "员工自助"}</td>
								<td>{j.status}</td>
								<td>
									{j.done}/{j.skipped}/{j.failed}/{j.pending}
								</td>
							</tr>
						))}
					</tbody>
				</table>
			</div>
		</section>
	);
}
