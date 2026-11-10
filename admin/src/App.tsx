// Admin console: users lifecycle, security policy, audit viewer.
// 管理后台：用户生命周期、安全策略、审计查看器。
import { type FormEvent, useCallback, useEffect, useState } from "react";
import {
	type AdminObject,
	type AuditEvent,
	api,
	getToken,
	setToken,
	type User,
} from "./api";

const STATUS_CN: Record<string, string> = {
	pending: "待激活",
	active: "正常",
	suspended: "已停用",
	offboarded: "已离职",
};

export function App() {
	const [me, setMe] = useState<User | null>(null);
	const [ready, setReady] = useState(false);
	const [tab, setTab] = useState<"users" | "storage" | "security" | "audit">(
		"users",
	);

	useEffect(() => {
		if (!getToken()) {
			setReady(true);
			return;
		}
		api
			.me()
			.then((d) => (d.user.is_admin ? setMe(d.user) : setToken("")))
			.catch(() => setToken(""))
			.finally(() => setReady(true));
	}, []);

	if (!ready) return <div className="center">…</div>;
	if (!me) return <Login onLogin={setMe} />;
	return (
		<div className="shell">
			<header className="topbar">
				<b>WittyChat 管理后台</b>
				<nav>
					{(["users", "storage", "security", "audit"] as const).map((t) => (
						<button
							key={t}
							type="button"
							className={tab === t ? "on" : ""}
							onClick={() => setTab(t)}
						>
							{t === "users"
								? "用户"
								: t === "storage"
									? "存储"
									: t === "security"
										? "安全策略"
										: "审计日志"}
						</button>
					))}
				</nav>
				<span className="who">{me.display_name}</span>
				<button
					type="button"
					className="ghost"
					onClick={() => {
						api.logout().catch(() => {});
						setToken("");
						setMe(null);
					}}
				>
					登出
				</button>
			</header>
			{tab === "users" && <Users me={me} />}
			{tab === "storage" && <Storage />}
			{tab === "security" && <Security />}
			{tab === "audit" && <Audit />}
		</div>
	);
}

function Login({ onLogin }: { onLogin: (u: User) => void }) {
	const [email, setEmail] = useState("");
	const [pass, setPass] = useState("");
	const [err, setErr] = useState("");
	const submit = async (e: FormEvent) => {
		e.preventDefault();
		try {
			const d = await api.login(email, pass);
			if (!d.user.is_admin) {
				setErr("需要管理员账号");
				return;
			}
			setToken(d.session_token);
			onLogin(d.user);
		} catch (ex) {
			setErr(ex instanceof Error ? ex.message : "登录失败");
		}
	};
	return (
		<div className="center">
			<form className="card" onSubmit={submit}>
				<h2>WittyChat 管理后台</h2>
				<input
					placeholder="管理员邮箱"
					value={email}
					onChange={(e) => setEmail(e.target.value)}
				/>
				<input
					type="password"
					placeholder="密码"
					value={pass}
					onChange={(e) => setPass(e.target.value)}
				/>
				{err && <p className="err">{err}</p>}
				<button type="submit" className="primary">
					登录
				</button>
			</form>
		</div>
	);
}

// ---------- users ----------

function Users({ me }: { me: User }) {
	const [users, setUsers] = useState<User[]>([]);
	const [email, setEmail] = useState("");
	const [name, setName] = useState("");
	const [asAdmin, setAsAdmin] = useState(false);
	const [link, setLink] = useState<{ url: string; exp: string } | null>(null);
	const [err, setErr] = useState("");

	const load = useCallback(
		() =>
			api
				.users()
				.then((d) => setUsers(d.users))
				.catch((e) => setErr(String(e))),
		[],
	);
	useEffect(() => {
		load();
	}, [load]);

	const wrap = (fn: () => Promise<unknown>) => async () => {
		setErr("");
		try {
			await fn();
			await load();
		} catch (e) {
			setErr(e instanceof Error ? e.message : "操作失败");
		}
	};

	const create = wrap(async () => {
		const d = await api.createUser(email, name, asAdmin);
		setLink({ url: d.activation_url, exp: d.expires_at });
		setEmail("");
		setName("");
		setAsAdmin(false);
	});

	const issueLink = (id: string, kind: "activation" | "reset") =>
		wrap(async () => {
			const d = await api.newLink(id, kind);
			setLink({ url: d.activation_url, exp: d.expires_at });
		});

	const offboard = (u: User) =>
		wrap(async () => {
			const to = users.find((x) => x.is_admin && x.id !== u.id);
			if (!to) throw new Error("没有其他管理员可交接");
			if (!window.confirm(`把 ${u.email} 的留存数据交接给 ${to.email}？`))
				return;
			await api.offboard(u.id, to.id);
		});

	return (
		<section>
			<div className="panel">
				<h3>开通账号</h3>
				<div className="row">
					<input
						placeholder="邮箱"
						value={email}
						onChange={(e) => setEmail(e.target.value)}
					/>
					<input
						placeholder="显示名"
						value={name}
						onChange={(e) => setName(e.target.value)}
					/>
					<label>
						<input
							type="checkbox"
							checked={asAdmin}
							onChange={(e) => setAsAdmin(e.target.checked)}
						/>
						管理员
					</label>
					<button
						type="button"
						className="primary"
						disabled={!email || !name}
						onClick={create}
					>
						创建并签发激活链接
					</button>
				</div>
				{link && (
					<p className="link-out">
						激活链接（72h 一次性）：
						<code>{link.url}</code>
						<button
							type="button"
							className="ghost"
							onClick={() => navigator.clipboard.writeText(link.url)}
						>
							复制
						</button>
					</p>
				)}
				{err && <p className="err">{err}</p>}
			</div>
			<table className="tbl">
				<thead>
					<tr>
						<th>邮箱</th>
						<th>显示名</th>
						<th>状态</th>
						<th>角色</th>
						<th>2FA</th>
						<th>操作</th>
					</tr>
				</thead>
				<tbody>
					{users.map((u) => (
						<tr key={u.id}>
							<td>{u.email}</td>
							<td>{u.display_name}</td>
							<td>
								<span className={`st st-${u.status}`}>
									{STATUS_CN[u.status] ?? u.status}
								</span>
							</td>
							<td>{u.is_admin ? "管理员" : "成员"}</td>
							<td>{u.totp_enabled ? "✓" : "—"}</td>
							<td className="ops">
								{u.status === "pending" && (
									<button
										type="button"
										className="ghost"
										onClick={issueLink(u.id, "activation")}
									>
										激活链接
									</button>
								)}
								{u.status === "active" && (
									<button
										type="button"
										className="ghost"
										onClick={issueLink(u.id, "reset")}
									>
										重置密码链接
									</button>
								)}
								{u.status === "active" && u.id !== me.id && (
									<>
										<button
											type="button"
											className="ghost"
											onClick={wrap(() => api.deactivate(u.id))}
										>
											停用
										</button>
										<button
											type="button"
											className="ghost danger"
											onClick={offboard(u)}
										>
											离职交接
										</button>
									</>
								)}
								{u.status === "suspended" && (
									<button
										type="button"
										className="ghost"
										onClick={wrap(() => api.reactivate(u.id))}
									>
										恢复
									</button>
								)}
							</td>
						</tr>
					))}
				</tbody>
			</table>
		</section>
	);
}

// ---------- storage / 存储治理 ----------

function fmtSize(n: number): string {
	if (n < 1024) return `${n}B`;
	if (n < 1 << 20) return `${(n / 1024).toFixed(1)}KB`;
	return `${(n / (1 << 20)).toFixed(1)}MB`;
}

function Storage() {
	const today = new Date().toISOString().slice(0, 10);
	const [owner, setOwner] = useState("");
	const [day, setDay] = useState(today);
	const [objs, setObjs] = useState<AdminObject[]>([]);
	const [msg, setMsg] = useState("");
	const [err, setErr] = useState("");

	const load = useCallback(() => {
		setErr("");
		const next = new Date(day);
		next.setDate(next.getDate() + 1); // backend 'to' is exclusive / 后端 to 排他
		api
			.objects(owner.trim(), day, next.toISOString().slice(0, 10))
			.then((d) => setObjs(d.objects))
			.catch((e) => setErr(String(e)));
	}, [owner, day]);
	useEffect(load, [load]);

	const purge = async () => {
		setMsg("");
		setErr("");
		const who = owner.trim() || "全部用户";
		if (
			!window.confirm(
				`删除 ${who} 在 ${day} 上传的全部 ${objs.length} 个附件？文件不可恢复。`,
			)
		)
			return;
		try {
			const d = await api.purgeObjects(owner.trim(), day);
			setMsg(`已删除 ${d.deleted} 个对象`);
			load();
		} catch (e) {
			setErr(e instanceof Error ? e.message : "删除失败");
		}
	};

	return (
		<section>
			<div className="panel row">
				<input
					placeholder="属主邮箱（空 = 全部）"
					value={owner}
					onChange={(e) => setOwner(e.target.value)}
				/>
				<input
					type="date"
					value={day}
					onChange={(e) => setDay(e.target.value)}
				/>
				<button type="button" className="ghost" onClick={load}>
					刷新
				</button>
				<button
					type="button"
					className="ghost danger"
					disabled={objs.length === 0}
					onClick={purge}
				>
					删除当天全部附件（{objs.length}）
				</button>
				{msg && <span className="ok">{msg}</span>}
				{err && <span className="err">{err}</span>}
			</div>
			<table className="tbl">
				<thead>
					<tr>
						<th>时间</th>
						<th>属主</th>
						<th>名称</th>
						<th>用途</th>
						<th>类型</th>
						<th>大小</th>
						<th>会话</th>
					</tr>
				</thead>
				<tbody>
					{objs.map((o) => (
						<tr key={o.id}>
							<td className="dim">{new Date(o.created_at).toLocaleString()}</td>
							<td>{o.owner_email}</td>
							<td>{o.name}</td>
							<td>
								<code>{o.purpose}</code>
							</td>
							<td className="dim">{o.mime}</td>
							<td>{fmtSize(o.size_bytes)}</td>
							<td className="dim">
								{o.conv_id ? `${o.conv_id.slice(0, 8)}…` : "—"}
							</td>
						</tr>
					))}
				</tbody>
			</table>
		</section>
	);
}

// ---------- security policy ----------

const POLICY_FIELDS: {
	key: string;
	label: string;
	kind: "int" | "bool" | "str";
}[] = [
	{ key: "workspace.name", label: "组织名称", kind: "str" },
	{ key: "security.min_password", label: "密码最小长度", kind: "int" },
	{
		key: "security.lock_threshold",
		label: "登录失败限速阈值(次)",
		kind: "int",
	},
	{
		key: "security.lock_minutes",
		label: "同IP失败锁定时长(分钟)",
		kind: "int",
	},
	{ key: "im.message_max_chars", label: "消息字数上限(≤5000)", kind: "int" },
	{ key: "im.recall_timeout_sec", label: "撤回时限(秒)", kind: "int" },
	{ key: "im.read_receipts", label: "已读回执", kind: "bool" },
	{ key: "im.at_all", label: "@所有人权限(all/admin)", kind: "str" },
	{ key: "storage.upload_max_mb", label: "附件上限(MB)", kind: "int" },
	{ key: "storage.quota_bytes", label: "工作区配额(字节,0=不限)", kind: "int" },
	{ key: "docs.log_max_mb", label: "协作文档日志上限(MB)", kind: "int" },
	{ key: "audit.retention_days", label: "审计/版本保留天数", kind: "int" },
];

function Security() {
	const [vals, setVals] = useState<Record<string, unknown>>({});
	const [msg, setMsg] = useState("");
	const [logoId, setLogoId] = useState("");
	const [logoVer, setLogoVer] = useState(0);
	useEffect(() => {
		api
			.settingsGet()
			.then((d) => {
				setVals(d.settings);
				setLogoId(String(d.settings["workspace.logo_object_id"] ?? ""));
			})
			.catch(() => {});
	}, []);

	const uploadLogo = async (f: File) => {
		setMsg("");
		try {
			const o = await api.uploadLogo(f);
			await api.settingsSet({ "workspace.logo_object_id": o.id });
			setLogoId(o.id);
			setLogoVer((v) => v + 1);
			setMsg("Logo 已更新");
		} catch (e) {
			setMsg(e instanceof Error ? e.message : "上传失败");
		}
	};
	const removeLogo = async () => {
		setMsg("");
		try {
			await api.settingsSet({ "workspace.logo_object_id": "" });
			setLogoId("");
		} catch (e) {
			setMsg(e instanceof Error ? e.message : "移除失败");
		}
	};

	const save = async () => {
		setMsg("");
		try {
			await api.settingsSet(vals);
			setMsg("已保存");
		} catch (e) {
			setMsg(e instanceof Error ? e.message : "保存失败");
		}
	};

	return (
		<section className="panel">
			<h3>安全与工作区策略</h3>
			<div className="row">
				<span className="flabel">组织 Logo</span>
				{logoId ? (
					<img
						src={`${api.logoUrl()}&v=${logoVer}`}
						className="wslogo"
						alt="组织 Logo"
					/>
				) : (
					<span className="dim">未设置（客户端显示组织名首字母）</span>
				)}
				<label className="primary wslogo-btn">
					{logoId ? "更换" : "上传"}
					<input
						type="file"
						accept="image/*"
						hidden
						onChange={(e) => {
							const f = e.target.files?.[0];
							if (f) uploadLogo(f);
							e.target.value = "";
						}}
					/>
				</label>
				{logoId && (
					<button type="button" className="ghost" onClick={removeLogo}>
						移除
					</button>
				)}
			</div>
			{POLICY_FIELDS.map((f) => (
				<div key={f.key} className="row">
					<span className="flabel">{f.label}</span>
					{f.kind === "bool" ? (
						<input
							type="checkbox"
							checked={vals[f.key] === true}
							onChange={(e) =>
								setVals((v) => ({ ...v, [f.key]: e.target.checked }))
							}
						/>
					) : (
						<input
							value={String(vals[f.key] ?? "")}
							onChange={(e) =>
								setVals((v) => ({
									...v,
									[f.key]:
										f.kind === "int" ? Number(e.target.value) : e.target.value,
								}))
							}
						/>
					)}
					<code className="dim">{f.key}</code>
				</div>
			))}
			<button type="button" className="primary" onClick={save}>
				保存
			</button>
			{msg && <p className="ok">{msg}</p>}
			<p className="dim">变更即时生效；撤回时限等热键会同步刷新服务端缓存。</p>
		</section>
	);
}

// ---------- audit ----------

function Audit() {
	const [action, setAction] = useState("");
	const [actor, setActor] = useState("");
	const [events, setEvents] = useState<AuditEvent[]>([]);
	const load = useCallback(() => {
		api
			.audit(action, actor)
			.then((d) => setEvents(d.events))
			.catch(() => {});
	}, [action, actor]);
	useEffect(() => {
		const t = setTimeout(load, 250);
		return () => clearTimeout(t);
	}, [load]);

	return (
		<section>
			<div className="panel row">
				<input
					placeholder="按 action 过滤 (如 admin.user)"
					value={action}
					onChange={(e) => setAction(e.target.value)}
				/>
				<input
					placeholder="按 actor 过滤"
					value={actor}
					onChange={(e) => setActor(e.target.value)}
				/>
				<span className="dim">查看行为本身会被记录</span>
			</div>
			<table className="tbl">
				<thead>
					<tr>
						<th>时间</th>
						<th>actor</th>
						<th>action</th>
						<th>target</th>
						<th>detail</th>
					</tr>
				</thead>
				<tbody>
					{events.map((e) => (
						<tr key={e.id}>
							<td className="dim">{new Date(e.created_at).toLocaleString()}</td>
							<td>
								{e.actor_kind}
								{e.actor_id ? `·${e.actor_id.slice(0, 8)}` : ""}
							</td>
							<td>
								<code>{e.action}</code>
							</td>
							<td>{e.target}</td>
							<td className="dim">{JSON.stringify(e.detail).slice(0, 80)}</td>
						</tr>
					))}
				</tbody>
			</table>
		</section>
	);
}
