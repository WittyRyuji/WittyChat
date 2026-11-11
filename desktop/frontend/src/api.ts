// Typed client for the WittyChat HTTP API + WS notifications.
// WittyChat HTTP API + WS 提醒的类型化客户端。

import { partPlan } from "./partplan";

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
const DEVICE_KEY = "wittychat.device";

export function getToken(): string {
  return localStorage.getItem(TOKEN_KEY) ?? "";
}
export function setToken(t: string) {
  if (t) localStorage.setItem(TOKEN_KEY, t);
  else localStorage.removeItem(TOKEN_KEY);
}
// Device token: server-issued marker that skips the account login queue.
// Not a credential — dies with any password change/reset.
// 设备令牌：服务端签发的标记，只豁免账号登录排队。非凭证——改密/重置即失效。
export function getDeviceToken(): string {
  return localStorage.getItem(DEVICE_KEY) ?? "";
}
export function setDeviceToken(t: string) {
  if (t) localStorage.setItem(DEVICE_KEY, t);
  else localStorage.removeItem(DEVICE_KEY);
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
  login: async (email: string, password: string) => {
    const res = await fetch(`${API_BASE}/api/v1/auth/login`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        ...(getDeviceToken() ? { "X-WC-Device": getDeviceToken() } : {}),
      },
      body: JSON.stringify({ email, password, device_name: "web", device_kind: "web" }),
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
    const body = (await res.json()) as {
      session_token: string;
      device_token?: string;
      user: UserPublic;
      requires_totp?: boolean;
    };
    if (body.device_token) setDeviceToken(body.device_token);
    return body;
  },
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

export interface StorageLimits {
  upload_max_bytes: number;
  large_max_bytes: number;
  multipart: boolean;
  used_bytes: number;
  quota_bytes: number;
}
export interface UploadDone {
  id: string;
  name: string;
  size: number;
  mime: string;
  w: number | null;
  h: number | null;
  status: string;
  deduplicated: boolean;
}
// MultipartUnavailableError: server has no S3 — the caller must fall back
// or report "not supported". / 服务端无 S3——调用方须降级或报不支持。
export class MultipartUnavailableError extends Error {
  constructor() {
    super("server has no S3 backend / 服务端未启用 S3");
    this.name = "MultipartUnavailableError";
  }
}
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
  // storageLimits fetches /api/v1/objects/quota every call — one GET per
  // pick, so admin changes (upload_max/large_max/S3 toggle) apply
  // immediately. / 每次调用都拉取：上限/S3 开关改动即时生效。
  storageLimits: () => req<StorageLimits>("GET", "/api/v1/objects/quota"),
  // uploadLarge: S3 multipart upload for files over upload_max_bytes —
  // init → PUT parts (concurrency 3, 3 retries each) → complete.
  // Presigned part PUTs carry NO Authorization header; the returned ETag
  // must be readable, which requires bucket CORS ExposeHeaders: ETag.
  // 大文件分片上传：init → 分片 PUT（并发3、各重试3次）→ complete。
  // 预签名分片 PUT 不带 Authorization；读不到 ETag 说明桶 CORS 未放行。
  uploadLarge: async (
    file: File,
    opts: { convId?: string; purpose: "attachment" | "mail" },
    onProgress?: (pct: number) => void
  ) => {
    const initRes = await fetch(`${API_BASE}/api/v1/objects/multipart/init`, {
      method: "POST",
      headers: { "Content-Type": "application/json", Authorization: `Bearer ${getToken()}` },
      body: JSON.stringify({
        conv_id: opts.convId,
        purpose: opts.purpose,
        name: file.name,
        size: file.size,
        mime: file.type || "application/octet-stream",
      }),
    });
    if (initRes.status === 501) throw new MultipartUnavailableError();
    if (initRes.status === 413) throw new Error(`文件过大，超过上限 / file too large`);
    if (initRes.status === 507) throw new Error("存储配额已满 / storage quota exceeded");
    if (!initRes.ok) throw new Error(`multipart init failed: ${initRes.status}`);
    const init = (await initRes.json()) as {
      object_id: string;
      part_size: number;
      urls: string[];
      deduplicated?: boolean;
      id?: string;
    };
    if (!init.urls || init.urls.length === 0) {
      throw new Error("multipart init returned no part URLs");
    }
    const plan = partPlan(file.size, init.part_size);
    const etags: { part_number: number; etag: string }[] = new Array(plan.length);
    let doneBytes = 0;
    const putPart = async (idx: number) => {
      const p = plan[idx];
      const url = init.urls[p.n - 1];
      let lastErr: unknown;
      for (let attempt = 0; attempt < 3; attempt++) {
        try {
          const res = await fetch(url, { method: "PUT", body: file.slice(p.start, p.end) });
          if (!res.ok) throw new Error(`part ${p.n} put ${res.status}`);
          const etag = res.headers.get("ETag");
          if (!etag) {
            throw new Error(
              "分片响应读不到 ETag——S3 桶 CORS 需配置 ExposeHeaders: ETag / " +
                "missing ETag: bucket CORS must expose ETag"
            );
          }
          etags[idx] = { part_number: p.n, etag };
          doneBytes += p.end - p.start;
          onProgress?.(Math.round((doneBytes / file.size) * 100));
          return;
        } catch (e) {
          lastErr = e;
          await new Promise((r) => setTimeout(r, 300 * 2 ** attempt));
        }
      }
      throw lastErr instanceof Error ? lastErr : new Error(`part ${p.n} failed`);
    };
    // Fixed-3 worker pool / 并发 3 的简易池
    let next = 0;
    await Promise.all(
      Array.from({ length: Math.min(3, plan.length) }, async () => {
        while (next < plan.length) {
          const i = next++;
          await putPart(i);
        }
      })
    );
    const doneRes = await fetch(`${API_BASE}/api/v1/objects/multipart/complete`, {
      method: "POST",
      headers: { "Content-Type": "application/json", Authorization: `Bearer ${getToken()}` },
      body: JSON.stringify({
        object_id: init.object_id,
        parts: etags.sort((a, b) => a.part_number - b.part_number),
      }),
    });
    if (!doneRes.ok) throw new Error(`multipart complete failed: ${doneRes.status}`);
    return doneRes.json() as Promise<UploadDone>;
  },
  // Object fetch paths carry NO token — bytes are pulled via authBlob
  // (Authorization header) and shown through blob: URLs; browser/system
  // contexts get a short-lived signed URL instead (objectSignedURL).
  // 对象路径不带 token——字节经 authBlob（Authorization 头）取回后用
  // blob: URL 展示；系统浏览器场景换 15 分钟签名链接。
  downloadURL: (objId: string) => `${API_BASE}/api/v1/objects/${objId}/download`,
  thumbURL: (objId: string) => `${API_BASE}/api/v1/objects/${objId}/thumb`,
  // objectSignedURL mints a 15-min capability URL for the object —
  // safe to hand to the system browser / history (no session token).
  objectSignedURL: async (objId: string) => {
    const r = await req<{ url: string }>("GET", `/api/v1/objects/${objId}/url`);
    return API_BASE + r.url;
  },
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
  workspaceLogoPath: () => "/api/v1/workspace/logo",
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
  // OnlyOffice edit URL — header-authed endpoint returns a signed
  // capability link for the system browser. Session tokens never leave
  // the app (the old ?token= office link leaked into browser history).
  // 在线编辑地址：鉴权接口返回签名能力链接交系统浏览器——会话 token
  // 不出应用（旧 ?token= 链接会落浏览器历史）。
  officeURL: (objId: string) => req<{ url: string }>("GET", `/api/v1/objects/${objId}/office-url`),
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
  w?: number | null; // decoded image px when the object has dims (server sends null) / 图片像素宽（服务端已知时，否则为 null）
  h?: number | null;
  content_id?: string | null; // cid: reference inside the HTML body / HTML 内 cid 引用
  inline?: boolean;
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
  html_body?: string;
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
  // Session token rides Sec-WebSocket-Protocol (wt1.<token>) — never the
  // URI, so proxies/access logs never record it. 会话 token 走
  // Sec-WebSocket-Protocol 子协议而非 URI——代理/访问日志都记不到。
  const ws = new WebSocket(
    `${proto}://${host}/ws`,
    getToken() ? ["wittychat", `wt1.${getToken()}`] : []
  );
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

// authBlob fetches bytes with the session header (no ?token= URL) —
// feed <img> with the returned Blob via URL.createObjectURL.
// 带会话头取字节（URL 不含 token）；返回 Blob 供 createObjectURL 给 <img>。
export async function authBlob(path: string, init?: { cache?: RequestCache }): Promise<Blob> {
  // Callers pass api2.*URL() values which are already absolute inside the
  // Wails shell — re-prefixing produced an unparsable doubled origin and
  // every chat/sticker/mail image failed. / 调用方传入的 api2.*URL 已含
  // API_BASE——再拼一次会得到非法双源地址，壳内图片全部加载失败。
  const res = await fetch(/^https?:\/\//i.test(path) ? path : API_BASE + path, {
    headers: { Authorization: `Bearer ${getToken()}` },
    cache: init?.cache,
  });
  if (!res.ok) throw new Error(`fetch ${res.status}`);
  return res.blob();
}

// openExternal hands an absolute URL to the system browser — the Wails
// shell API inside the app, window.open elsewhere.
// 交给系统浏览器：壳内走 Wails runtime，浏览器开发环境走 window.open。
export async function openExternal(url: string): Promise<void> {
  if (inWails) {
    const { Browser } = await import("@wailsio/runtime");
    await Browser.OpenURL(url).catch(() => {});
    return;
  }
  window.open(url, "_blank", "noopener");
}

// downloadExternal: mint a 15-min signed URL and open it in the system
// browser — session token never reaches history/logs.
// 下载外链：签发 15 分钟能力 URL 交系统浏览器——token 不进历史/日志。
export async function downloadExternal(objId: string): Promise<void> {
  const u = await api2.objectSignedURL(objId);
  await openExternal(u);
}

// officeOpenExternal: signed editor page in the system browser.
// 在线编辑：取签名编辑页地址交系统浏览器。
export async function officeOpenExternal(objId: string): Promise<void> {
  const r = await api2.officeURL(objId);
  await openExternal(r.url);
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
  shared?: boolean; // convDocs: present via doc_shares, not bound / 经分享而非绑定
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
  // Grant a conversation's members access (sent as a link there).
  share: (id: string, convId: string) =>
    req("POST", `/api/v1/docs/${id}/share`, { conv_id: convId }),
  unshare: (id: string, convId: string) =>
    req("POST", `/api/v1/docs/${id}/unshare`, { conv_id: convId }),
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
