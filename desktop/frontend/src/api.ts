// Typed client for the WittyChat HTTP API + WS notifications.
// WittyChat HTTP API + WS 提醒的类型化客户端。

export interface UserPublic {
  id: string;
  email: string;
  display_name: string;
  status: string;
  is_admin: boolean;
  totp_enabled: boolean;
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

async function req<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
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
  send: (convId: string, clientMsgId: string, markdown: string, quoteOpSeq?: number) =>
    req<OpEvent>("POST", `/api/v1/conversations/${convId}/messages`, {
      client_msg_id: clientMsgId,
      markdown,
      ...(quoteOpSeq ? { quote_op_seq: quoteOpSeq } : {}),
    }),
  recall: (convId: string, seq: number) =>
    req<OpEvent>("POST", `/api/v1/conversations/${convId}/messages/${seq}/recall`),
  markRead: (convId: string, seq: number) =>
    req("POST", `/api/v1/conversations/${convId}/read`, { op_seq: seq }),
};

// connectWS opens the notification socket; calls onFrame per frame.
// Auto-reconnect handled by the caller via onClose.
export function connectWS(onFrame: (f: WsFrame) => void, onClose: () => void): WebSocket {
  const proto = location.protocol === "https:" ? "wss" : "ws";
  const ws = new WebSocket(`${proto}://${location.host}/ws?token=${getToken()}`);
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
