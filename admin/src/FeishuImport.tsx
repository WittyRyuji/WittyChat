// 飞书导入 — workspace Feishu app config + read-only job list.
import { useEffect, useState } from "react";
import { type FIJob, fiApi } from "./api";

const GUIDE = [
	"飞书开放平台创建企业自建应用，添加机器人能力",
	"开通权限：docs:document:export、drive:drive:readonly、wiki:wiki:readonly、contact:user.email:readonly、contact:user.base:readonly，发布版本",
	"在飞书建一个群，把应用作为群机器人加入",
	"员工把要搬的文件夹/文档分享给这个群（知识库：把群加为成员）",
	"员工在 WittyChat 桌面端 文档 → 从飞书导入，粘贴链接",
];

export function FeishuImport() {
	const [app, setApp] = useState<{
		configured: boolean;
		app_id: string;
		base_url: string;
	} | null>(null);
	const [jobs, setJobs] = useState<FIJob[]>([]);
	const [appID, setAppID] = useState("");
	const [secret, setSecret] = useState("");
	const [err, setErr] = useState("");
	const [busy, setBusy] = useState(false);
	const load = () => {
		fiApi
			.app()
			.then(setApp)
			.catch(() => setErr("加载失败"));
		fiApi
			.jobs()
			.then(setJobs)
			.catch(() => {});
	};
	useEffect(load, []);
	const save = async () => {
		setBusy(true);
		setErr("");
		try {
			await fiApi.setApp(appID, secret);
			setSecret("");
			load();
		} catch (e) {
			setErr(String(e));
		}
		setBusy(false);
	};
	const clear = async () => {
		if (!confirm("清除飞书应用凭据？")) return;
		await fiApi.clearApp();
		load();
	};
	return (
		<div>
			<h2>飞书应用</h2>
			{app?.configured ? (
				<p className="ok">
					已配置 App ID {app.app_id}（{app.base_url}）
				</p>
			) : (
				<p className="warn">未配置</p>
			)}
			<div className="fs-form">
				<input
					placeholder="App ID"
					value={appID}
					onChange={(e) => setAppID(e.target.value)}
				/>
				<input
					type="password"
					placeholder="App Secret（不显示已保存值）"
					value={secret}
					onChange={(e) => setSecret(e.target.value)}
				/>
				<div className="row">
					<button
						type="button"
						className="primary"
						disabled={busy || !appID || !secret}
						onClick={save}
					>
						保存
					</button>
					<button type="button" className="ghost danger" onClick={clear}>
						清除
					</button>
				</div>
			</div>
			{err && <p className="err">{err}</p>}
			<h3>迁移步骤</h3>
			<ol className="guide">
				{GUIDE.map((g) => (
					<li key={g}>{g}</li>
				))}
			</ol>
			<p className="muted">
				应用只能读到分享给它的内容；员工只能导入自己是所有者的文档。
			</p>
			<h3>导入任务</h3>
			<table>
				<thead>
					<tr>
						<th>任务</th>
						<th>创建者</th>
						<th>模式</th>
						<th>状态</th>
						<th>完成/跳过/失败/待</th>
					</tr>
				</thead>
				<tbody>
					{jobs.map((j) => (
						<tr key={j.id}>
							<td>{j.name}</td>
							<td>{j.created_by.slice(0, 8)}</td>
							<td>{j.mode}</td>
							<td>{j.status}</td>
							<td>
								{j.done}/{j.skipped}/{j.failed}/{j.pending}
							</td>
						</tr>
					))}
				</tbody>
			</table>
		</div>
	);
}
