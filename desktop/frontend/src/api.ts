// Typed client for the WittyChat HTTP API + WS notifications.
// WittyChat HTTP API + WS 提醒的类型化客户端。

export interface UserPublic {
  id: string;
  email: string;
  display_name: string;
  status: string;
  is_admin: boolean;
  totp_enabled: boolean;
  avatar_object_id?: string;
}

export interface ConversationInfo {
  id: string;
  kind: string;
  title?: string;
  is_public?: boolean;
  last_op_seq: number;
  last_read_seq: number;
  unread: number;
  preview?: string;
  preview_at?: string;
  peer_user_id?: string;
  peer_name?: string;
  peer_read_seq?: number; // dm: peer's watermark → 已读回执
  member_count?: number;
}

export interface OpEvent {
  op_seq: number;
  op_id: string;
  kind: string;
  actor_id?: string;
  actor_name?: string;
  client_msg_id?: string;
  target_seq?: number;
  body?: string;
  payload?: Record<string, unknown>;
  edited?: boolean;
  recalled?: boolean;
  created_at: string;
}

export interface MemberInfo {
  user_id: string;
  name: string;
  role: string;
  last_read_seq: number;
}

export interface PublicGroupInfo {
  id: string;
  title: string;
  member_count: number;
  joined: boolean;
}

export interface MessageSearchHit {
  conv_id: string;
  op_seq: number;
  body: string;
  actor_name: string;
  conv_title: string;
  created_at: string;
}

export interface WsFrame {
  type: string;
  conv_id?: string;
  last_op_seq?: number;
  user_id?: string;
  name?: string;
  online?: boolean;
}

const TOKEN_KEY = "wittychat.token";

export function getToken(): string {
  return localStorage.getItem(TOKEN_KEY) ?? "";
}
export function setToken(t: string) {
  if (t) localStorage.setItem(TOKEN_KEY, t);
  else localStorage.removeItem(TOKEN_KEY);
}

// API base: "" in browser dev (vite proxy), backend origin inside Wails.
// Wails v3 serves assets via http(s)://wails.localhost — protocol alone can't
// distinguish the shell from a real browser, so detect it explicitly.
// localStorage "wittychat.server" overrides for remote/HTTPS instances.
// API 基址：浏览器开发环境走 vite 代理（""）；Wails 壳内资源也是 http(s)
// 来源（wails.localhost），需显式识别壳环境后直连服务端；
// localStorage "wittychat.server" 可覆盖指向远程/HTTPS 实例。
export const inWails = location.hostname === "wails.localhost" || location.protocol === "wails:";
export const API_BASE =
  localStorage.getItem("wittychat.server") ??
  (inWails || !(location.protocol === "http:" || location.protocol === "https:")
    ? "http://localhost:8080"
    : "");
// Absolute admin console URL — a bare "/admin/" resolves against the
// wails.localhost shell origin instead of the backend.
// 管理后台绝对地址——裸 "/admin/" 在壳内会解析到 wails.localhost 源。
export const adminURL = `${API_BASE || location.origin}/admin/`;

async function req<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(API_BASE + path, {
    method,
    headers: {
      "Content-Type": "application/json",
      ...(getToken() ? { Authorization: `Bearer ${getToken()}` } : {}),
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!res.ok) {
    let detail = `HTTP ${res.status}`;
    try {
      const err = await res.json();
      if (err.detail) detail = err.detail;
    } catch {
      /* keep default */
    }
    throw new Error(detail);
  }
  if (res.status === 204) return undefined as T;
  return res.json() as Promise<T>;
}

export const api = {
  login: (email: string, password: string) =>
    req<{ session_token: string; user: UserPublic; requires_totp?: boolean }>(
      "POST",
      "/api/v1/auth/login",
      { email, password, device_name: "web", device_kind: "web" }
    ),
  me: () => req<{ user: UserPublic }>("GET", "/api/v1/auth/me"),
  getPrefs: () => req<{ prefs: Record<string, unknown> }>("GET", "/api/v1/me/prefs"),
  mergePrefs: (patch: Record<string, unknown>) =>
    req<{ prefs: Record<string, unknown> }>("PATCH", "/api/v1/me/prefs", { prefs: patch }),
  updateMe: (body: { display_name?: string; avatar_object_id?: string }) =>
    req<{ user: UserPublic }>("PATCH", "/api/v1/users/me", body),
  logout: () => req("POST", "/api/v1/auth/logout"),

  directory: () => req<{ conversations: ConversationInfo[] }>("GET", "/api/v1/conversations"),
  createDM: (userId: string) =>
    req<ConversationInfo>("POST", "/api/v1/conversations/dm", { user_id: userId }),
  opsAfter: (convId: string, after: number, limit = 200) =>
    req<{ ops: OpEvent[]; has_more: boolean; latest_seq: number }>(
      "GET",
      `/api/v1/conversations/${convId}/ops?after=${after}&limit=${limit}`
    ),
  opsBefore: (convId: string, before: number, limit = 50) =>
    req<{ ops: OpEvent[]; has_more: boolean }>(
      "GET",
      `/api/v1/conversations/${convId}/ops?before=${before}&limit=${limit}`
    ),
  send: (
    convId: string,
    clientMsgId: string,
    markdown: string,
    quoteOpSeq?: number,
    objectIds?: string[]
  ) =>
    req<OpEvent>("POST", `/api/v1/conversations/${convId}/messages`, {
      client_msg_id: clientMsgId,
      markdown,
      ...(quoteOpSeq ? { quote_op_seq: quoteOpSeq } : {}),
      ...(/@所有人|@all\b/i.test(markdown) ? { mention_all: true } : {}),
      ...(objectIds?.length ? { object_ids: objectIds } : {}),
    }),
  recall: (convId: string, seq: number) =>
    req<OpEvent>("POST", `/api/v1/conversations/${convId}/messages/${seq}/recall`),
  react: (convId: string, seq: number, emoji: string, add: boolean) =>
    req<OpEvent>("POST", `/api/v1/conversations/${convId}/messages/${seq}/reaction`, {
      emoji,
      add,
    }),
  markRead: (convId: string, seq: number) =>
    req("POST", `/api/v1/conversations/${convId}/read`, { op_seq: seq }),
  members: (convId: string) =>
    req<{ members: MemberInfo[] }>("GET", `/api/v1/conversations/${convId}/members`),

  // phase-2: group manage / discovery / search / contacts / settings
  users: () => req<{ users: UserPublic[] }>("GET", "/api/v1/users"),
  createGroup: (title: string, isPublic: boolean, memberIds: string[]) =>
    req<ConversationInfo>("POST", "/api/v1/conversations/group", {
      title,
      is_public: isPublic,
      member_ids: memberIds,
    }),
  renameConv: (convId: string, title: string) =>
    req("POST", `/api/v1/conversations/${convId}/rename`, { title }),
  transferConv: (convId: string, userId: string) =>
    req("POST", `/api/v1/conversations/${convId}/transfer`, { user_id: userId }),
  setRole: (convId: string, userId: string, role: "admin" | "member") =>
    req("POST", `/api/v1/conversations/${convId}/members/${userId}/role`, { role }),
  addMembers: (convId: string, userIds: string[]) =>
    req("POST", `/api/v1/conversations/${convId}/members`, { user_ids: userIds }),
  removeMember: (convId: string, userId: string) =>
    req("DELETE", `/api/v1/conversations/${convId}/members/${userId}`),
  searchGroups: (q: string) =>
    req<{ groups: PublicGroupInfo[] }>("GET", `/api/v1/groups/public?q=${encodeURIComponent(q)}`),
  joinGroup: (id: string) => req("POST", `/api/v1/groups/${id}/join`),
  searchMessages: (q: string) =>
    req<{ hits: MessageSearchHit[] }>("GET", `/api/v1/search/messages?q=${encodeURIComponent(q)}`),
  assistants: () =>
    req<{
      assistants: {
        id: string;
        name: string;
        runtime: string;
        adapter: string;
        model: string;
        auto_send: boolean;
        status: string;
      }[];
    }>("GET", "/api/v1/assistants"),
  settings: () =>
    req<{
      workspace: { name: string; logo_object_id?: string };
      im: {
        message_max_chars: number;
        recall_timeout_sec: number;
        read_receipts: boolean;
        at_all: string;
      };
      office?: { enabled: boolean };
    }>("GET", "/api/v1/settings"),
};

let officeOn: Promise<boolean> | null = null;

export const api2 = {
  upload: async (convId: string, file: File) => {
    const fd = new FormData();
    fd.append("file", file, file.name);
    const res = await fetch(`${API_BASE}/api/v1/conversations/${convId}/attachments`, {
      method: "POST",
      headers: { Authorization: `Bearer ${getToken()}` },
      body: fd,
    });
    if (!res.ok) throw new Error(`upload failed: ${res.status}`);
    return res.json() as Promise<{ id: string; name: string; size: number; mime: string }>;
  },
  downloadURL: (objId: string) =>
    `${API_BASE}/api/v1/objects/${objId}/download?token=${getToken()}`,
  thumbURL: (objId: string) => `${API_BASE}/api/v1/objects/${objId}/thumb?token=${getToken()}`,
  // uploadThumb pushes a client-generated thumbnail for an object.
  // uploadThumb 上传客户端生成的缩略图（canvas 压缩，服务端不再解码）。
  uploadThumb: async (objId: string, blob: Blob) => {
    const fd = new FormData();
    fd.append("file", blob, "thumb.jpg");
    const res = await fetch(`${API_BASE}/api/v1/objects/${objId}/thumb`, {
      method: "POST",
      headers: { Authorization: `Bearer ${getToken()}` },
      body: fd,
    });
    if (!res.ok) throw new Error(`thumb ${res.status}`);
  },
  avatarURL: (userId: string) => `${API_BASE}/api/v1/users/${userId}/avatar`,
  workspaceLogoURL: () => `${API_BASE}/api/v1/workspace/logo?token=${getToken()}`,
  uploadObject: async (file: File, purpose: string) => {
    const fd = new FormData();
    fd.append("file", file, file.name);
    const res = await fetch(`${API_BASE}/api/v1/objects/upload?purpose=${purpose}`, {
      method: "POST",
      headers: { Authorization: `Bearer ${getToken()}` },
      body: fd,
    });
    if (!res.ok) throw new Error(`upload ${res.status}`);
    return res.json() as Promise<{ id: string; name: string; size: number; mime: string }>;
  },
  fetchObject: async (objId: string) => {
    const res = await fetch(api2.downloadURL(objId), {
      headers: { Authorization: `Bearer ${getToken()}` },
    });
    if (!res.ok) throw new Error("download failed");
    return res.blob();
  },
  // OnlyOffice edit entry — /office/open validates the session and 302s
  // to the signed editor page; the shell's link interceptor opens it in
  // the system browser. 在线编辑入口（服务端校验后跳签名编辑页）。
  officeEditURL: (objId: string) => `${API_BASE}/office/open?obj=${objId}&token=${getToken()}`,
  officeEnabled: () =>
    (officeOn ??= api
      .settings()
      .then((d) => !!d.office?.enabled)
      .catch(() => false)),
};

// Office extensions the "在线编辑" affordance appears for — mirrors
// officeType in server/httpapi/office.go (common editable set).
// 前端镜像的可编辑扩展名集合（与服务端 officeType 对齐的常用子集）。
export const OFFICE_EXTS = new Set([
  "doc",
  "docx",
  "docm",
  "dot",
  "dotx",
  "dotm",
  "odt",
  "ott",
  "rtf",
  "txt",
  "xls",
  "xlsx",
  "xlsm",
  "xlsb",
  "xlt",
  "xltx",
  "xltm",
  "ods",
  "ots",
  "csv",
  "ppt",
  "pptx",
  "pptm",
  "pps",
  "ppsx",
  "ppsm",
  "pot",
  "potx",
  "potm",
  "odp",
  "otp",
  "pdf",
  "epub",
]);

export function isOfficeFile(name: string): boolean {
  const i = name.lastIndexOf(".");
  return i >= 0 && OFFICE_EXTS.has(name.slice(i + 1).toLowerCase());
}

// ---- mail ----
export interface MailFolder {
  id: string;
  kind: string;
  name: string;
  unread: number;
}
export interface MailAttach {
  object_id: string;
  name: string;
  mime: string;
  size_bytes: number;
}
export interface MailMessage {
  id: string;
  folder_id: string;
  direction: "in" | "out";
  from_addr: string;
  to_addrs: string[];
  cc_addrs: string[];
  subject: string;
  text_body: string;
  preview: string;
  is_read: boolean;
  is_starred: boolean;
  rfc_msgid?: string;
  in_reply_to?: string;
  received_at: string;
  attachments?: MailAttach[];
}

export const mailApi = {
  folders: () => req<MailFolder[]>("GET", "/api/v1/mail/folders"),
  messages: (folderId: string, offset = 0) =>
    req<MailMessage[]>("GET", `/api/v1/mail/messages?folder=${folderId}&offset=${offset}`),
  get: (id: string) => req<MailMessage>("GET", `/api/v1/mail/messages/${id}`),
  search: (q: string) =>
    req<MailMessage[]>("GET", `/api/v1/mail/search?q=${encodeURIComponent(q)}`),
  send: (
    to: string[],
    cc: string[],
    subject: string,
    body: string,
    inReplyTo?: string,
    objectIds?: string[]
  ) =>
    req<{ id: string; external_queued: number }>("POST", "/api/v1/mail/send", {
      to,
      cc,
      subject,
      body,
      ...(inReplyTo ? { in_reply_to: inReplyTo } : {}),
      ...(objectIds?.length ? { object_ids: objectIds } : {}),
    }),
  flags: (id: string, read?: boolean, starred?: boolean) =>
    req("POST", `/api/v1/mail/messages/${id}/flags`, {
      ...(read !== undefined ? { read } : {}),
      ...(starred !== undefined ? { starred } : {}),
    }),
  readAll: (folderId: string) =>
    req<{ marked: number }>("POST", `/api/v1/mail/folders/${folderId}/read-all`),
  move: (id: string, folder: string) => req("POST", `/api/v1/mail/messages/${id}/move`, { folder }),
  del: (id: string) => req("DELETE", `/api/v1/mail/messages/${id}`),
  upload: async (file: File) => {
    const fd = new FormData();
    fd.append("file", file, file.name);
    const res = await fetch(`${API_BASE}/api/v1/objects/upload?purpose=mail`, {
      method: "POST",
      headers: { Authorization: `Bearer ${getToken()}` },
      body: fd,
    });
    if (!res.ok) throw new Error(`upload failed: ${res.status}`);
    return res.json() as Promise<{ id: string; name: string }>;
  },
};

// connectWS opens the notification socket; calls onFrame per frame.
// Auto-reconnect handled by the caller via onClose.
export function connectWS(onFrame: (f: WsFrame) => void, onClose: () => void): WebSocket {
  const proto = location.protocol === "https:" || API_BASE.startsWith("https") ? "wss" : "ws";
  const host = API_BASE ? new URL(API_BASE).host : location.host;
  const ws = new WebSocket(`${proto}://${host}/ws?token=${getToken()}`);
  ws.onmessage = (ev) => {
    try {
      onFrame(JSON.parse(ev.data));
    } catch {
      /* ignore bad frame */
    }
  };
  ws.onclose = onClose;
  ws.onerror = () => ws.close();
  return ws;
}

export function newClientMsgId(): string {
  return crypto.randomUUID();
}

// ---- Docs / 协同文档 ----
// In-app native documents (Feishu-style). REST below manages metadata;
// realtime editing rides GET /ws/doc/{id} speaking the y-websocket
// protocol — see docWsBase(). 原生协同文档：REST 管元数据，实时编辑走
// y-websocket 协议的 /ws/doc/{id}。
export interface DocInfo {
  id: string;
  title: string;
  kind: "doc" | "sheet" | "slide";
  owner: string;
  conv_id?: string | null;
  created_at: string;
  updated_at: string;
  editors: number; // live editor count, relay-provided
}

export const docsApi = {
  list: () => req<{ docs: DocInfo[] }>("GET", "/api/v1/docs"),
  create: (title: string, kind: "doc" | "sheet" | "slide" = "doc", convId?: string) =>
    req<DocInfo>("POST", "/api/v1/docs", {
      title,
      kind,
      ...(convId ? { conv_id: convId } : {}),
    }),
  get: (id: string) => req<DocInfo>("GET", `/api/v1/docs/${id}`),
  rename: (id: string, title: string) => req("POST", `/api/v1/docs/${id}/rename`, { title }),
  del: (id: string) => req("DELETE", `/api/v1/docs/${id}`),
  convDocs: (convId: string) =>
    req<{ docs: DocInfo[] }>("GET", `/api/v1/conversations/${convId}/docs`),
};

// docWsBase is the y-websocket serverUrl: the provider appends
// "/{roomname}" — our room name is the doc id → /ws/doc/{id}?token=.
// docWsBase 即 y-websocket 的 serverUrl：provider 拼 "/{room}"（doc id）。
export function docWsBase(): string {
  const proto = location.protocol === "https:" || API_BASE.startsWith("https") ? "wss" : "ws";
  const host = API_BASE ? new URL(API_BASE).host : location.host;
  return `${proto}://${host}/ws/doc`;
}

// ---- HR / 人力 ----
export interface HRRequest {
  id: string;
  kind: "leave" | "contract" | "expense";
  requester: string;
  requester_name?: string;
  status: "pending" | "approved" | "rejected" | "cancelled";
  payload: Record<string, unknown>;
  decider_name?: string;
  decided_at?: string;
  decision_note?: string;
  created_at: string;
}
export interface HRDay {
  work_date: string;
  check_in: string;
  check_out?: string;
  note?: string;
  minutes: number;
  late: boolean;
}
export interface HRSummary {
  user_id: string;
  user_name?: string;
  days: number;
  total_minutes: number;
  late_days: number;
  missing_checkout: number;
  leave_days: number;
}

export const hrApi = {
  create: (kind: string, payload: Record<string, unknown>) =>
    req<HRRequest>("POST", "/api/v1/hr/requests", { kind, payload }),
  list: (scope: "mine" | "pending" | "all" | "history") =>
    req<{ requests: HRRequest[] }>("GET", `/api/v1/hr/requests?scope=${scope}`),
  decide: (id: string, approve: boolean, note?: string) =>
    req<HRRequest>("POST", `/api/v1/hr/requests/${id}/decide`, { approve, note }),
  cancel: (id: string) => req("POST", `/api/v1/hr/requests/${id}/cancel`),
  checkIn: (note?: string) => req("POST", "/api/v1/hr/attendance/check-in", { note }),
  checkOut: () => req("POST", "/api/v1/hr/attendance/check-out"),
  myMonth: (month: string) =>
    req<{ days: HRDay[]; summary: HRSummary }>("GET", `/api/v1/hr/attendance/mine?month=${month}`),
  monthSummary: (month: string) =>
    req<{ rows: HRSummary[] }>("GET", `/api/v1/hr/attendance/summary?month=${month}`),
};
