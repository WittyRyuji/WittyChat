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
