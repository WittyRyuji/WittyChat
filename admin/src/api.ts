// Thin admin API client — same token model as the desktop client.
// 管理端 API 客户端——与桌面端同一套会话令牌。
export interface User {
	id: string;
	email: string;
	display_name: string;
	status: string;
	is_admin: boolean;
	totp_enabled: boolean;
	created_at: string;
}

export interface AuditEvent {
	id: number;
	actor_id?: string;
	actor_kind: string;
	action: string;
	target: string;
	detail: Record<string, unknown>;
	created_at: string;
}

const KEY = "wittychat.admin.token";
export const getToken = () => localStorage.getItem(KEY) ?? "";
export const setToken = (t: string) =>
	t ? localStorage.setItem(KEY, t) : localStorage.removeItem(KEY);

async function req<T>(
	method: string,
	path: string,
	body?: unknown,
): Promise<T> {
	const res = await fetch(path, {
		method,
		headers: {
			"Content-Type": "application/json",
			...(getToken() ? { Authorization: `Bearer ${getToken()}` } : {}),
		},
		body: body === undefined ? undefined : JSON.stringify(body),
	});
	if (!res.ok) {
		const txt = await res.text().catch(() => "");
		throw new Error(`${res.status} ${txt.slice(0, 120)}`);
	}
	return res.status === 204 ? ({} as T) : ((await res.json()) as T);
}

export const api = {
	login: (email: string, password: string) =>
		req<{ session_token: string; user: User }>("POST", "/api/v1/auth/login", {
			email,
			password,
			device_name: "admin-web",
			device_kind: "web",
		}),
	me: () => req<{ user: User }>("GET", "/api/v1/auth/me"),
	logout: () => req("POST", "/api/v1/auth/logout"),

	users: () => req<{ users: User[] }>("GET", "/api/v1/admin/users"),
	createUser: (email: string, displayName: string, isAdmin: boolean) =>
		req<{ user: User; activation_url: string; expires_at: string }>(
			"POST",
			"/api/v1/admin/users",
			{ email, display_name: displayName, is_admin: isAdmin },
		),
	newLink: (id: string, kind: "activation" | "reset") =>
		req<{ activation_url: string; expires_at: string }>(
			"POST",
			`/api/v1/admin/users/${id}/links`,
			{ kind },
		),
	deactivate: (id: string) =>
		req("POST", `/api/v1/admin/users/${id}/deactivate`),
	reactivate: (id: string) =>
		req("POST", `/api/v1/admin/users/${id}/reactivate`),
	offboard: (id: string, transferTo: string) =>
		req("POST", `/api/v1/admin/users/${id}/offboard`, {
			transfer_to: transferTo,
		}),

	audit: (action: string, actor: string) =>
		req<{ events: AuditEvent[] }>(
			"GET",
			`/api/v1/admin/audit?limit=200&action=${encodeURIComponent(action)}&actor=${encodeURIComponent(actor)}`,
		),
	settingsGet: () =>
		req<{ settings: Record<string, unknown> }>("GET", "/api/v1/admin/settings"),
	settingsSet: (settings: Record<string, unknown>) =>
		req("PUT", "/api/v1/admin/settings", { settings }),

	uploadLogo: async (file: File) => {
		const fd = new FormData();
		fd.append("file", file, file.name);
		const res = await fetch("/api/v1/objects/upload?purpose=workspace_logo", {
			method: "POST",
			headers: { Authorization: `Bearer ${getToken()}` },
			body: fd,
		});
		if (!res.ok) throw new Error(`upload ${res.status}`);
		return res.json() as Promise<{ id: string; name: string }>;
	},
	logoUrl: () => `/api/v1/workspace/logo?token=${getToken()}`,

	objects: (owner: string, from: string, to: string) =>
		req<{ objects: AdminObject[] }>(
			"GET",
			`/api/v1/admin/objects?owner=${encodeURIComponent(owner)}&from=${from}&to=${to}`,
		),
	purgeObjects: (ownerEmail: string, day: string) =>
		req<{ deleted: number }>("POST", "/api/v1/admin/objects/purge", {
			owner_email: ownerEmail,
			day,
		}),
};

export interface AdminObject {
	id: string;
	owner: string;
	owner_email: string;
	name: string;
	purpose: string;
	mime: string;
	size_bytes: number;
	conv_id?: string;
	status: string;
	created_at: string;
}

// --- mail import (邮件搬家) ------------------------------------------------
export interface MIJob {
	id: string;
	name: string;
	provider: string;
	host: string;
	port: number;
	tls_mode: string;
	since: string | null;
	add_alias: boolean;
	status: string;
	secrets_purged: boolean;
	accounts: number;
	msgs_done: number;
	msgs_total: number;
	created_at: string;
}
export interface MIAccount {
	id: string;
	source_login: string;
	target_user: string;
	status: string;
	failures_count: number;
	msgs_total: number;
	msgs_done: number;
	msgs_skipped: number;
	msgs_failed: number;
	bytes_done: number;
	last_error: string;
	note: string;
}
export interface MIFailure {
	folder: string;
	uid: number;
	error: string;
	attempts: number;
}
export interface MIFolderRow {
	account_id: string;
	source_name: string;
	target_folder_id: string;
	uidvalidity: number;
	last_uid: number;
	msgs_total: number;
	msgs_done: number;
}
export interface MIPreset {
	key: string;
	name: string;
	host: string;
	port: number;
	tls_mode: string;
	help: string;
}
export interface MITestResult {
	account_id: string;
	ok: boolean;
	error: string;
	folders: number;
}

export const miApi = {
	presets: () => req<MIPreset[]>("GET", "/api/v1/admin/mail-import/presets"),
	jobs: () => req<MIJob[]>("GET", "/api/v1/admin/mail-import/jobs"),
	job: (id: string) =>
		req<{
			job: MIJob;
			accounts: MIAccount[];
			folders: MIFolderRow[];
			failures: MIFailure[];
		}>("GET", `/api/v1/admin/mail-import/jobs/${id}`),
	createJob: (b: Record<string, unknown>) =>
		req<{ id: string }>("POST", "/api/v1/admin/mail-import/jobs", b),
	accounts: (id: string, csv: string) =>
		req<{
			added: number;
			updated: number;
			errors: { line: number; error: string }[];
		}>("POST", `/api/v1/admin/mail-import/jobs/${id}/accounts`, { csv }),
	accountDel: (id: string, aid: string) =>
		req("DELETE", `/api/v1/admin/mail-import/jobs/${id}/accounts/${aid}`),
	test: (id: string, accountIds?: string[]) =>
		req<MITestResult[]>("POST", `/api/v1/admin/mail-import/jobs/${id}/test`, {
			account_ids: accountIds,
		}),
	start: (id: string) =>
		req("POST", `/api/v1/admin/mail-import/jobs/${id}/start`),
	pause: (id: string) =>
		req("POST", `/api/v1/admin/mail-import/jobs/${id}/pause`),
	cancel: (id: string) =>
		req("POST", `/api/v1/admin/mail-import/jobs/${id}/cancel`),
	finish: (id: string) =>
		req("POST", `/api/v1/admin/mail-import/jobs/${id}/finish`),
	del: (id: string) => req("DELETE", `/api/v1/admin/mail-import/jobs/${id}`),
};

export interface FIJob {
	id: string;
	name: string;
	mode: string;
	status: string;
	created_by: string;
	total: number;
	done: number;
	skipped: number;
	failed: number;
	pending: number;
}
export const fiApi = {
	app: () =>
		req<{ configured: boolean; app_id: string; base_url: string }>(
			"GET",
			"/api/v1/admin/feishu/app",
		),
	setApp: (app_id: string, app_secret: string, base_url?: string) =>
		req("PUT", "/api/v1/admin/feishu/app", { app_id, app_secret, base_url }),
	clearApp: () => req("DELETE", "/api/v1/admin/feishu/app"),
	jobs: () => req<FIJob[]>("GET", "/api/v1/feishu/jobs?all=1"),
};
