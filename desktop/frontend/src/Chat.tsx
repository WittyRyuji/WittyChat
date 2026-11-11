import { type FormEvent, Fragment, useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { DocInfo } from "./api";
import {
  api,
  api2,
  authBlob,
  type ConversationInfo,
  connectWS,
  docsApi,
  isOfficeFile,
  type MemberInfo,
  type MessageSearchHit,
  newClientMsgId,
  type OpEvent,
  openExternal,
  type UserPublic,
  type WsFrame,
} from "./api";
import { Avatar, GroupAvatar } from "./avatar";
import { Contacts } from "./Contacts";
import { Docs, KIND_ICON, KIND_LABEL } from "./Docs";
import { JoinGroupDialog, MembersPanel, NewGroupDialog } from "./dialogs";
import { EMOJI_GROUPS } from "./emoji";
import { HR } from "./HR";
import {
  IconChat,
  IconContacts,
  IconDoc,
  IconGear,
  IconHR,
  IconMail,
  IconSearch,
  IconZen,
} from "./icons";
import { Mail } from "./Mail";
import { Md } from "./md";
import { ObjImg } from "./objimg";
import { Settings } from "./Settings";

const MSG_LIMIT = 2000;

interface Attachment {
  id: string;
  name: string;
  size: number;
  mime: string;
  w?: number | null; // decoded px dims from objects.width/height (null when unknown) / 服务端解析的图片像素宽（未知为 null）
  h?: number | null;
}

// Reaction aggregate attached to message payloads (server-side applyEffectiveView
// and reaction ops' "reactions_full" both carry this shape).
// 消息 payload.reactions 与 reaction op 的 reactions_full 共用此结构。
interface RxGroup {
  emoji: string;
  users: { id: string; name: string }[];
}

function rxGroupsOf(o: OpEvent): RxGroup[] {
  return Array.isArray(o.payload?.reactions) ? (o.payload.reactions as RxGroup[]) : [];
}

// applyReaction — SET the target message's reactions from a reaction op's
// "reactions_full" aggregate (authoritative, includes out-of-page ops).
// 用 reaction op 携带的 reactions_full 聚合直接覆盖目标消息状态（非重放）。
function applyReaction(ops: OpEvent[], ro: OpEvent) {
  const i = ops.findIndex((x) => x.op_seq === ro.target_seq);
  if (i < 0) return;
  const full = ro.payload?.reactions_full;
  const t = ops[i];
  ops[i] = {
    ...t,
    payload: { ...(t.payload ?? {}), reactions: Array.isArray(full) ? full : [] },
  };
}

// makeThumb: client-side thumbnail (canvas, <=320px jpeg) — cheap, avoids the
// server re-decode. Uploaded right after the object itself.
// makeThumb：客户端生成缩略图（canvas ≤320px jpeg）——便宜，省服务端解码。
const THUMB_MAX = 320;
async function makeThumb(file: File): Promise<Blob | null> {
  if (!file.type.startsWith("image/") || file.type === "image/svg+xml") return null;
  try {
    const bmp = await createImageBitmap(file);
    const s = Math.min(1, THUMB_MAX / Math.max(bmp.width, bmp.height));
    const c = document.createElement("canvas");
    c.width = Math.max(1, Math.round(bmp.width * s));
    c.height = Math.max(1, Math.round(bmp.height * s));
    c.getContext("2d")?.drawImage(bmp, 0, 0, c.width, c.height);
    bmp.close();
    return await new Promise<Blob | null>((r) => c.toBlob((b) => r(b), "image/jpeg", 0.82));
  } catch {
    return null;
  }
}

// Local-calendar YYYY-MM-DD for date separators — deliberately not
// toISOString() (UTC would shift late-evening messages to the next day).
function dayKey(iso: string): string {
  const d = new Date(iso);
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
}

function fmtSize(n: number): string {
  if (n < 1024) return `${n}B`;
  if (n < 1 << 20) return `${(n / 1024).toFixed(1)}KB`;
  return `${(n / (1 << 20)).toFixed(1)}MB`;
}

async function saveBlob(objId: string, name: string) {
  const blob = await api2.fetchObject(objId);
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  a.click();
  URL.revokeObjectURL(url);
}

interface ConvState {
  ops: OpEvent[];
  watermark: number;
  hasOlder: boolean;
  loaded: boolean;
}

interface Pending {
  id: string; // client_msg_id
  convId: string;
  markdown: string;
  quoteSeq?: number;
  failed?: boolean;
}

// ReadBadge — Feishu-style read receipt circle / 已读回执圈。
// dm: solid gray (unread) → blue (read); group: pie fill by read ratio.
function ReadBadge({
  seq,
  members,
  meId,
  isDm,
}: {
  seq: number;
  members: MemberInfo[];
  meId: string;
  isDm: boolean;
}) {
  const others = members.filter((m) => m.user_id !== meId);
  if (others.length === 0) return null;
  const readers = others.filter((m) => m.last_read_seq >= seq);
  const read = readers.length === others.length;
  const tip = isDm
    ? read
      ? "已读"
      : "未读"
    : `已读 ${readers.length}/${others.length}` +
      (readers.length > 0 ? `：${readers.map((m) => m.name).join("、")}` : "");
  return (
    <span
      className={`read-badge ${isDm ? (read ? "read" : "unread") : "group"}`}
      style={
        isDm
          ? undefined
          : {
              background: `conic-gradient(var(--accent) ${Math.round(
                (readers.length / others.length) * 100
              )}%, var(--line) ${Math.round((readers.length / others.length) * 100)}%)`,
            }
      }
    >
      <span className="rb-tip">{tip}</span>
    </span>
  );
}

export function Chat({
  me,
  onLogout,
  onMeUpdated,
}: {
  me: UserPublic;
  onLogout: () => void;
  onMeUpdated: () => void;
}) {
  const [convs, setConvs] = useState<ConversationInfo[]>([]);
  const [activeId, setActiveId] = useState("");
  const [states, setStates] = useState<Record<string, ConvState>>({});
  const [pending, setPending] = useState<Pending[]>([]);
  const [text, setText] = useState("");
  const [quote, setQuote] = useState<OpEvent | null>(null);
  const [files, setFiles] = useState<Attachment[]>([]);
  const fileInput = useRef<HTMLInputElement>(null);
  const [uploading, setUploading] = useState<Record<string, number>>({});
  const [typing, setTyping] = useState<Record<string, string>>({});
  const [online, setOnline] = useState<Record<string, boolean>>({});
  const [members, setMembers] = useState<Record<string, MemberInfo[]>>({});
  const [contacts, setContacts] = useState<UserPublic[]>([]);
  // Live identity by user id: seeded from the directory and patched by
  // user_update frames, so a rename/avatar change shows everywhere at once —
  // stored ops only carry the uid; their actor_name is just a fallback.
  // 按 uid 的实时身份：通讯录初始化，user_update 帧增量更新；改名/换头像即时全局生效，
  // 消息里存的只是 uid，actor_name 仅作兜底。
  const [live, setLive] = useState<
    Record<string, { display_name: string; avatar_object_id?: string }>
  >({});
  useEffect(() => {
    setLive((prev) => {
      const next = { ...prev };
      for (const u of contacts) {
        next[u.id] = { display_name: u.display_name, avatar_object_id: u.avatar_object_id };
      }
      return next;
    });
  }, [contacts]);
  const who = (id?: string, fallback?: string) => {
    if (id && id === me.id) return me;
    const u = id ? live[id] : undefined;
    return {
      id: id ?? "",
      display_name: u?.display_name || fallback || "?",
      avatar_object_id: u?.avatar_object_id,
    };
  };
  const [modal, setModal] = useState<"" | "newGroup" | "join" | "members">("");
  const [viewer, setViewer] = useState<{ url: string; name: string } | null>(null);
  const [msgLimit, setMsgLimit] = useState(MSG_LIMIT);
  const [wsName, setWsName] = useState("W");
  const [wsLogo, setWsLogo] = useState("");
  const [searchQ, setSearchQ] = useState("");
  const [searchHits, setSearchHits] = useState<MessageSearchHit[] | null>(null);
  const [emojiOpen, setEmojiOpen] = useState(false);
  const [emojiTab, setEmojiTab] = useState<"emoji" | "stk">("emoji");
  const stickerInput = useRef<HTMLInputElement>(null);
  // Custom sticker shelf — per-device (same precedent as mail trust prefs);
  // each entry is an uploaded object, so any device can render it once synced.
  const [stickers, setStickers] = useState<{ id: string; name: string; mime: string }[]>(() => {
    try {
      return JSON.parse(localStorage.getItem("wittychat.stickers") ?? "[]");
    } catch {
      return [];
    }
  });
  const taRef = useRef<HTMLTextAreaElement>(null);
  const caretRef = useRef(0); // last textarea caret — emoji inserts land there

  // Inline message search (Feishu-style): typing swaps the conv list for hits.
  useEffect(() => {
    if (!searchQ.trim()) {
      setSearchHits(null);
      return;
    }
    const t = setTimeout(() => {
      api
        .searchMessages(searchQ)
        .then((d) => setSearchHits(d.hits))
        .catch(() => setSearchHits([]));
    }, 250);
    return () => clearTimeout(t);
  }, [searchQ]);
  const [view, setView] = useState<
    "chat" | "contacts" | "mail" | "docs" | "zen" | "hr" | "settings"
  >("chat");
  // Doc to open when entering the docs view (set by conv "文档" panel /
  // doc link clicks). 进入文档页时要打开的目标文档。
  const [docsFocus, setDocsFocus] = useState("");
  const [docsPanel, setDocsPanel] = useState(false); // 群文档侧栏
  const [convDocs, setConvDocs] = useState<DocInfo[] | null>(null);
  const [panelW, setPanelW] = useState(() => {
    const v = parseInt(localStorage.getItem("docsPanelW") ?? "", 10);
    return v >= 200 && v <= 520 ? v : 260;
  });
  const [plusMenu, setPlusMenu] = useState(false); // 输入框 ＋ 菜单
  const [docPick, setDocPick] = useState<DocInfo[] | null>(null); // 发送文档选择器
  const plusRef = useRef<HTMLDivElement>(null);
  const [officeOn, setOfficeOn] = useState(false);
  useEffect(() => {
    api2.officeEnabled().then(setOfficeOn);
  }, []);
  const [err, setErr] = useState("");
  const scrollRef = useRef<HTMLDivElement>(null);
  const typingTimers = useRef<Record<string, number>>({});
  const wsRef = useRef<WebSocket | null>(null);

  const active = convs.find((c) => c.id === activeId);
  const st = states[activeId];

  // ---------- data ----------

  const refreshDirectory = useCallback(async () => {
    try {
      const d = await api.directory();
      setConvs(d.conversations);
    } catch {
      /* transient */
    }
  }, []);

  const fetchDelta = useCallback(async (convId: string) => {
    setStates((prev) => {
      const cur = prev[convId] ?? { ops: [], watermark: 0, hasOlder: true, loaded: false };
      void (async () => {
        try {
          let wm = cur.watermark;
          const acc = [...cur.ops];
          for (;;) {
            const page = await api.opsAfter(convId, wm);
            for (const o of page.ops) {
              if (o.op_seq > wm) {
                if (o.kind === "reaction") {
                  applyReaction(acc, o);
                }
                acc.push(o);
              }
            }
            if (page.ops.length > 0) {
              wm = page.ops[page.ops.length - 1].op_seq;
            }
            if (!page.has_more) break;
          }
          acc.sort((a, b) => a.op_seq - b.op_seq);
          setStates((p) => ({
            ...p,
            // Full fetch from seq 0 → nothing older exists.
            [convId]: {
              ops: acc,
              watermark: wm,
              hasOlder: cur.watermark === 0 ? false : cur.hasOlder,
              loaded: true,
            },
          }));
          setPending((p) => {
            const got = new Set(acc.map((o) => o.client_msg_id).filter(Boolean));
            return p.filter((x) => x.convId !== convId || !got.has(x.id));
          });
          // Mark read when viewing this conv.
          if (wm > 0) {
            api.markRead(convId, wm).catch(() => {});
          }
        } catch {
          /* transient */
        }
      })();
      return prev;
    });
  }, []);

  const loadingOlder = useRef<Record<string, boolean>>({});
  const needInitScroll = useRef(false);

  const loadOlder = useCallback(
    async (convId: string) => {
      const cur = states[convId];
      if (!cur?.hasOlder || cur.ops.length === 0 || loadingOlder.current[convId]) return;
      loadingOlder.current[convId] = true;
      try {
        const el = scrollRef.current;
        const prevHeight = el?.scrollHeight ?? 0;
        const page = await api.opsBefore(convId, cur.ops[0].op_seq);
        setStates((p) => {
          const c = p[convId];
          if (!c) return p;
          const older = page.ops.filter((o) => !c.ops.some((x) => x.op_seq === o.op_seq));
          return {
            ...p,
            [convId]: { ...c, ops: [...older, ...c.ops], hasOlder: page.has_more },
          };
        });
        // Only restore viewport when older ops actually prepended —
        // an empty page must not snap scrollTop to 0.
        if (page.ops.length > 0) {
          requestAnimationFrame(() => {
            if (el) el.scrollTop = el.scrollHeight - prevHeight;
          });
        }
      } finally {
        loadingOlder.current[convId] = false;
      }
    },
    [states]
  );

  const loadMembers = useCallback(async (convId: string) => {
    try {
      const d = await api.members(convId);
      setMembers((prev) => ({ ...prev, [convId]: d.members }));
    } catch {
      /* transient */
    }
  }, []);

  // ---------- ws ----------

  // The socket is opened once; it reads the current user through refs.
  // 连接只建立一次，通过 ref 读取当前用户。
  const meIdRef = useRef(me.id);
  meIdRef.current = me.id;
  const onMeUpdatedRef = useRef(onMeUpdated);
  onMeUpdatedRef.current = onMeUpdated;

  useEffect(() => {
    refreshDirectory();
    api
      .users()
      .then((d) => setContacts(d.users))
      .catch(() => {});
    api
      .settings()
      .then((s) => {
        setMsgLimit(s.im.message_max_chars);
        setWsName(s.workspace.name);
        // Logo via header-authed blob — keeps the session token out of
        // image URLs. logo 经鉴权 blob 取回，URL 不携带会话 token。
        authBlob(api2.workspaceLogoPath())
          .then((b) =>
            setWsLogo((prev) => {
              if (prev.startsWith("blob:")) URL.revokeObjectURL(prev);
              return URL.createObjectURL(b);
            })
          )
          .catch(() => {});
      })
      .catch(() => {});
    let dead = false;
    let retry = 0;
    const open = () => {
      if (dead) return;
      wsRef.current = connectWS(
        (f: WsFrame) => {
          retry = 0;
          switch (f.type) {
            case "conv_update":
              refreshDirectory();
              if (f.conv_id) loadMembers(f.conv_id);
              setActiveId((cur) => {
                if (f.conv_id === cur) fetchDelta(cur);
                else if (f.conv_id) fetchDelta(f.conv_id);
                return cur;
              });
              break;
            case "read":
              // Someone advanced their watermark — update in place, no refetch.
              if (f.conv_id && f.user_id) {
                setMembers((prev) => ({
                  ...prev,
                  [f.conv_id as string]: (prev[f.conv_id as string] ?? []).map((m) =>
                    m.user_id === f.user_id
                      ? { ...m, last_read_seq: Math.max(m.last_read_seq, f.last_op_seq ?? 0) }
                      : m
                  ),
                }));
              }
              break;
            case "presence":
              if (f.user_id) setOnline((o) => ({ ...o, [f.user_id as string]: !!f.online }));
              break;
            case "user_update":
              if (f.user_id) {
                const uid = f.user_id;
                setLive((l) => ({
                  ...l,
                  [uid]: {
                    display_name: f.name ?? l[uid]?.display_name ?? "",
                    avatar_object_id: f.avatar_object_id || undefined,
                  },
                }));
                setContacts((cs) =>
                  cs.map((u) =>
                    u.id === uid
                      ? {
                          ...u,
                          display_name: f.name ?? u.display_name,
                          avatar_object_id: f.avatar_object_id || undefined,
                        }
                      : u
                  )
                );
                if (uid === meIdRef.current) onMeUpdatedRef.current();
                setActiveId((cur) => {
                  if (cur) loadMembers(cur);
                  return cur;
                });
              }
              break;
            case "typing":
              if (f.conv_id && f.user_id) {
                const key = `${f.conv_id}:${f.user_id}`;
                setTyping((t) => ({ ...t, [key]: f.name ?? "…" }));
                clearTimeout(typingTimers.current[key]);
                typingTimers.current[key] = window.setTimeout(
                  () => setTyping((t) => ({ ...t, [key]: "" })),
                  3000
                );
              }
              break;
          }
        },
        () => {
          if (!dead) {
            retry += 1;
            setTimeout(open, Math.min(1000 * retry, 10000));
          }
        }
      );
    };
    open();
    return () => {
      dead = true;
      wsRef.current?.close();
      setWsLogo((prev) => {
        if (prev.startsWith("blob:")) URL.revokeObjectURL(prev);
        return prev;
      });
    };
  }, [refreshDirectory, fetchDelta, loadMembers]);

  // Load a conversation when selected.
  // Keyed on activeId only — load/reset on conversation switch.
  // biome-ignore lint/correctness/useExhaustiveDependencies: intentional switch-load
  useEffect(() => {
    if (!activeId) return;
    setQuote(null);
    setText("");
    setFiles([]);
    setDocsPanel(false);
    setConvDocs(null);
    setPlusMenu(false);
    needInitScroll.current = true; // 进会话跳到底
    if (!states[activeId]?.loaded) fetchDelta(activeId);
    loadMembers(activeId);
    const c = convs.find((x) => x.id === activeId);
    if (c && c.last_op_seq > 0) api.markRead(activeId, c.last_op_seq).catch(() => {});
  }, [activeId]);

  // Click-outside closes the composer + menu. 外点关闭 ＋ 菜单。
  useEffect(() => {
    if (!plusMenu) return;
    const close = (e: MouseEvent) => {
      if (!plusRef.current?.contains(e.target as Node)) setPlusMenu(false);
    };
    document.addEventListener("mousedown", close);
    return () => document.removeEventListener("mousedown", close);
  }, [plusMenu]);

  // Conv switch → jump to latest; new ops while near bottom → keep pinned.
  // biome-ignore lint/correctness/useExhaustiveDependencies: scroll triggers
  useEffect(() => {
    const el = scrollRef.current;
    if (!el || !st?.loaded) return;
    if (needInitScroll.current) {
      needInitScroll.current = false;
      el.scrollTop = el.scrollHeight;
      return;
    }
    const nearBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 160;
    if (nearBottom) el.scrollTop = el.scrollHeight;
  }, [st?.ops.length, st?.loaded, activeId]);

  const onScroll = () => {
    const el = scrollRef.current;
    if (el && el.scrollTop < 60 && st?.hasOlder) loadOlder(activeId);
  };

  // ---------- send ----------

  const pickFile = async (list: FileList | null) => {
    if (!list || !activeId) return;
    // Size-aware routing: ≤upload_max direct POST; over it → S3 multipart
    // when the server supports it; over large_max → friendly refuse.
    // 按大小分流：≤直传上限走原直传；超过则走 S3 分片（服务端支持时）；
    // 再超大文件上限则友好拒绝。
    const limits = await api2.storageLimits().catch(() => null);
    for (const f of Array.from(list)) {
      const key = `${f.name}-${f.size}`;
      try {
        let obj: { id: string; name: string; size: number; mime: string };
        if (!limits || f.size <= limits.upload_max_bytes) {
          obj = await api2.upload(activeId, f);
        } else if (limits.multipart && f.size <= limits.large_max_bytes) {
          setUploading((x) => ({ ...x, [key]: 0 }));
          obj = await api2.uploadLarge(f, { convId: activeId, purpose: "attachment" }, (pct) =>
            setUploading((x) => ({ ...x, [key]: pct }))
          );
          setUploading((x) => {
            const n = { ...x };
            delete n[key];
            return n;
          });
        } else {
          const lim = limits.multipart ? limits.large_max_bytes : limits.upload_max_bytes;
          setErr(`${f.name} 超过上限（${fmtSize(lim)}）`);
          continue;
        }
        setFiles((x) => [...x, { id: obj.id, name: obj.name, size: obj.size, mime: obj.mime }]);
        const tb = await makeThumb(f);
        if (tb) api2.uploadThumb(obj.id, tb).catch(() => {});
      } catch (e) {
        setUploading((x) => {
          const n = { ...x };
          delete n[key];
          return n;
        });
        setErr(`上传失败：${f.name}${e instanceof Error && e.message ? ` — ${e.message}` : ""}`);
      }
    }
    if (fileInput.current) fileInput.current.value = "";
  };

  const send = async (e?: FormEvent) => {
    e?.preventDefault();
    const md = text.trim();
    if ((!md && files.length === 0) || !activeId || md.length > msgLimit) return;
    const objIds = files.map((f) => f.id);
    const p: Pending = {
      id: newClientMsgId(),
      convId: activeId,
      markdown: md,
      quoteSeq: quote?.op_seq,
    };
    setPending((x) => [...x, p]);
    setText("");
    setQuote(null);
    setFiles([]);
    try {
      const op = await api.send(activeId, p.id, md, p.quoteSeq, objIds);
      setPending((x) => x.filter((y) => y.id !== p.id));
      setStates((prev) => {
        const c = prev[activeId] ?? { ops: [], watermark: 0, hasOlder: true, loaded: true };
        if (c.ops.some((o) => o.op_seq === op.op_seq)) return prev;
        return {
          ...prev,
          [activeId]: {
            ...c,
            ops: [...c.ops, op].sort((a, b) => a.op_seq - b.op_seq),
            watermark: Math.max(c.watermark, op.op_seq),
          },
        };
      });
      refreshDirectory();
    } catch (ex) {
      setPending((x) => x.map((y) => (y.id === p.id ? { ...y, failed: true } : y)));
      setErr(ex instanceof Error ? ex.message : "send failed");
    }
  };

  // Feishu-style group docs: create a doc bound to this conv → it shows
  // up in the group's docs panel (no more auto link-spam in the feed).
  // Sending a doc link to the feed stays an explicit composer action.
  // 飞书式群文档：建绑定文档→出现在群文档面板（不再自动刷屏发链接）。
  // 发文档链接进消息流改为输入框 ＋ 菜单里的显式动作。
  const openDoc = (id: string) => {
    setDocsFocus(id);
    setDocsPanel(false);
    setView("docs");
  };

  const loadConvDocs = () => {
    if (!activeId) return;
    docsApi
      .convDocs(activeId)
      .then((r) => setConvDocs(r.docs))
      .catch(() => setConvDocs([]));
  };

  const toggleDocsPanel = () => {
    setDocsPanel((v) => {
      if (!v) loadConvDocs();
      return !v;
    });
  };

  const createConvDoc = async (kind: "doc" | "sheet" | "slide") => {
    if (!activeId) return;
    setPlusMenu(false);
    try {
      const d = await docsApi.create(`未命名${KIND_LABEL[kind]}`, kind, activeId);
      setConvDocs((l) => (l ? [d, ...l] : l));
      openDoc(d.id);
    } catch {
      setErr("新建失败");
    }
  };

  // Share a doc into this conv (grants members access) then post its link
  // into the feed. 先把文档分享给本会话（授予成员访问权），再发链接。
  const sendDocLink = async (d: DocInfo) => {
    if (!activeId) return;
    setDocPick(null);
    setPlusMenu(false);
    try {
      await docsApi.share(d.id, activeId);
    } catch {
      setErr("无法分享该文档：仅属主或群管理员可分享到其它会话");
      return;
    }
    // Escape markdown link-text metacharacters in the title.
    // 转义标题中的 Markdown 链接元字符。
    const title = (d.title || "未命名").replace(/[\\[\]()*_`]/g, "\\$&");
    try {
      const op = await api.send(
        activeId,
        newClientMsgId(),
        `${KIND_ICON[d.kind] ?? "📄"} [${title}](#doc/${d.id})`
      );
      if (docsPanel) loadConvDocs();
      setStates((prev) => {
        const c = prev[activeId] ?? { ops: [], watermark: 0, hasOlder: true, loaded: true };
        if (c.ops.some((o) => o.op_seq === op.op_seq)) return prev;
        return {
          ...prev,
          [activeId]: {
            ...c,
            ops: [...c.ops, op].sort((a, b) => a.op_seq - b.op_seq),
            watermark: Math.max(c.watermark, op.op_seq),
          },
        };
      });
      refreshDirectory();
    } catch {
      setErr("发送失败");
    }
  };

  // Office edit + big-image viewer: session token never leaves the app —
  // office goes through the header-authed office-url endpoint (signed
  // capability for the system browser), the viewer fetches to a blob.
  // 在线编辑与大图预览：token 不出应用——编辑链接走鉴权端点取签名能力
  // URL 交系统浏览器，预览图经 Authorization 头取成 blob。
  const officeOpen = (objId: string) => {
    api2
      .officeURL(objId)
      .then((r) => openExternal(r.url))
      .catch(() => setErr("无法打开在线编辑"));
  };

  const openViewerBlob = (objId: string, name: string) => {
    authBlob(api2.downloadURL(objId))
      .then((b) =>
        setViewer((v) => {
          if (v?.url.startsWith("blob:")) URL.revokeObjectURL(v.url);
          return { url: URL.createObjectURL(b), name };
        })
      )
      .catch(() => setErr("预览失败"));
  };

  const closeViewer = () =>
    setViewer((v) => {
      if (v?.url.startsWith("blob:")) URL.revokeObjectURL(v.url);
      return null;
    });

  const delConvDoc = async (d: DocInfo) => {
    if (!window.confirm(`删除「${d.title || "未命名"}」？`)) return;
    try {
      await docsApi.del(d.id);
      setConvDocs((l) => l?.filter((x) => x.id !== d.id) ?? l);
    } catch {
      setErr("删除失败：只有属主或群主可删");
    }
  };

  // Remove a shared doc from this conv's list (the doc itself survives).
  // 把分享进来的文档移出本群列表（文档本体不受影响）。
  const unshareConvDoc = async (d: DocInfo) => {
    if (!activeId) return;
    if (!window.confirm(`把「${d.title || "未命名"}」移出本群？文档本身不会删除。`)) return;
    try {
      await docsApi.unshare(d.id, activeId);
      setConvDocs((l) => l?.filter((x) => x.id !== d.id) ?? l);
    } catch {
      setErr("移出失败：只有文档属主或其群的群主/管理员可撤销分享");
    }
  };

  // In-message doc links render as <a href="#doc/{id}"> — intercept and
  // open in-app instead of navigating the WebView.
  // 消息内文档链接是 #doc/{id}——拦截后在应用内打开，不做 WebView 导航。
  const onOpsClick = (e: React.MouseEvent) => {
    const a = (e.target as HTMLElement).closest?.("a[href^='#doc/']");
    if (!a) return;
    e.preventDefault();
    const id = (a.getAttribute("href") ?? "").slice(5);
    if (id) {
      setDocsFocus(id);
      setView("docs");
    }
  };

  const recall = async (seq: number) => {
    try {
      await api.recall(activeId, seq);
      fetchDelta(activeId);
    } catch (ex) {
      setErr(ex instanceof Error ? ex.message : "recall failed");
    }
  };

  // Toggle my reaction on a message: click 👍 (or an existing pill's emoji).
  // The response carries reactions_full → SET the target's state right away;
  // the same op then arriving via WS delta re-applies the same value.
  const reactEmoji = async (o: OpEvent, emoji: string) => {
    const groups = rxGroupsOf(o);
    const mine = groups.find((g) => g.emoji === emoji)?.users.some((u) => u.id === me.id);
    try {
      const ev = await api.react(activeId, o.op_seq, emoji, !mine);
      setStates((p) => {
        const c = p[activeId];
        if (!c) return p;
        const ops = [...c.ops];
        applyReaction(ops, ev);
        return { ...p, [activeId]: { ...c, ops } };
      });
    } catch (ex) {
      setErr(ex instanceof Error ? ex.message : "reaction failed");
    }
  };

  // Insert emoji at the remembered textarea caret.
  const insertEmoji = (e: string) => {
    const pos = caretRef.current;
    setText((t) => t.slice(0, pos) + e + t.slice(pos));
    caretRef.current = pos + e.length;
    requestAnimationFrame(() => {
      const ta = taRef.current;
      if (ta) {
        ta.focus();
        ta.setSelectionRange(caretRef.current, caretRef.current);
      }
    });
  };

  // ---------- custom stickers (GIF/图片表情) ----------

  const saveStickers = (nx: { id: string; name: string; mime: string }[]) => {
    setStickers(nx);
    localStorage.setItem("wittychat.stickers", JSON.stringify(nx));
  };

  const addSticker = async (f: File | undefined) => {
    if (!f) return;
    try {
      const o = await api2.uploadObject(f, "sticker");
      saveStickers([{ id: o.id, name: f.name, mime: o.mime || f.type }, ...stickers].slice(0, 60));
    } catch (ex) {
      setErr(ex instanceof Error ? ex.message : "sticker upload failed");
    }
  };

  const delSticker = (id: string) => saveStickers(stickers.filter((x) => x.id !== id));

  // Send a sticker as a single-image message — reuses the whole attachment
  // pipeline (thumbnails, ACL, preview "[图片]").
  const sendSticker = async (id: string) => {
    setEmojiOpen(false);
    try {
      const op = await api.send(activeId, newClientMsgId(), "", 0, [id]);
      setStates((prev) => {
        const c = prev[activeId] ?? { ops: [], watermark: 0, hasOlder: true, loaded: true };
        if (c.ops.some((o) => o.op_seq === op.op_seq)) return prev;
        return {
          ...prev,
          [activeId]: {
            ...c,
            ops: [...c.ops, op].sort((a, b) => a.op_seq - b.op_seq),
            watermark: Math.max(c.watermark, op.op_seq),
          },
        };
      });
      refreshDirectory();
    } catch (ex) {
      setErr(ex instanceof Error ? ex.message : "sticker send failed");
    }
  };

  const convTitle = (c: ConversationInfo) =>
    c.kind === "dm" ? who(c.peer_user_id, c.peer_name || "私聊").display_name : c.title || "群聊";

  const pendingHere = useMemo(
    () => pending.filter((p) => p.convId === activeId),
    [pending, activeId]
  );

  const typingHere = useMemo(
    () =>
      Object.entries(typing)
        .filter(([k, v]) => v && k.startsWith(`${activeId}:`))
        .map(([, v]) => v),
    [typing, activeId]
  );

  const quoteOf = (o: OpEvent): OpEvent | undefined =>
    o.payload?.quote_op_seq
      ? st?.ops.find((x) => x.op_seq === Number(o.payload?.quote_op_seq))
      : undefined;

  // Feishu-style read receipt: 见顶部 ReadBadge 组件。

  // Directory → DM shortcut for the contacts pane.
  const openDM = (u: UserPublic) => {
    api
      .createDM(u.id)
      .then((c) => {
        setActiveId(c.id);
        refreshDirectory();
        setView("chat");
      })
      .catch(() => {});
  };

  if (view !== "chat") {
    return (
      <div className="app">
        <Rail view={view} setView={setView} logo={wsName} logoUrl={wsLogo} />
        {view === "mail" && <Mail />}
        {view === "docs" && <Docs me={me} focusId={docsFocus} onFocused={() => setDocsFocus("")} />}
        {view === "contacts" && (
          <Contacts
            me={me}
            onOpenDM={openDM}
            onNewGroup={() => setModal("newGroup")}
            onJoin={() => setModal("join")}
          />
        )}
        {view === "zen" && (
          <div className="pane">
            <h3>Zen</h3>
            <p className="empty">Zen 会话（P1）</p>
          </div>
        )}
        {view === "hr" && <HR me={me} />}
        {view === "settings" && <Settings me={me} onLogout={onLogout} onMeUpdated={onMeUpdated} />}
        {modal === "newGroup" && (
          <NewGroupDialog me={me} onClose={() => setModal("")} onCreated={refreshDirectory} />
        )}
        {modal === "join" && (
          <JoinGroupDialog onClose={() => setModal("")} onJoined={refreshDirectory} />
        )}
      </div>
    );
  }

  return (
    <div className="app">
      <Rail view={view} setView={setView} logo={wsName} logoUrl={wsLogo} />
      <div className="chat">
        <aside className="sidebar">
          <div className="side-head">
            <span className="me">{me.display_name}</span>
          </div>
          <div className="conv-search">
            <IconSearch size={16} />
            <input
              placeholder="搜索消息"
              value={searchQ}
              onChange={(e) => setSearchQ(e.target.value)}
            />
            {searchQ && (
              <button type="button" className="ghost sm" onClick={() => setSearchQ("")}>
                ✕
              </button>
            )}
          </div>
          {searchHits !== null ? (
            <div className="conv-list">
              {searchHits.map((h) => (
                <button
                  type="button"
                  key={`${h.conv_id}-${h.op_seq}`}
                  className="conv"
                  onClick={() => {
                    setActiveId(h.conv_id);
                    setSearchQ("");
                  }}
                >
                  <span className="conv-col">
                    <span className="conv-title">
                      {h.conv_title}
                      <i className="dim"> · {h.actor_name}</i>
                    </span>
                    <span className="conv-preview">{h.body}</span>
                  </span>
                </button>
              ))}
              {searchHits.length === 0 && <p className="empty">没有匹配的消息</p>}
            </div>
          ) : (
            <div className="conv-list">
              {convs.map((c) => (
                <button
                  type="button"
                  key={c.id}
                  className={`conv ${c.id === activeId ? "active" : ""}`}
                  onClick={() => setActiveId(c.id)}
                >
                  {c.kind === "dm" && c.peer_user_id ? (
                    <Avatar className="avatar sm conv-av" user={who(c.peer_user_id, c.peer_name)} />
                  ) : (
                    <GroupAvatar
                      className="avatar sm conv-av"
                      id={c.id}
                      title={convTitle(c)}
                      avatarId={c.avatar_object_id}
                    />
                  )}
                  <span className="conv-col">
                    <span className="conv-title">
                      {c.kind === "dm" && online[c.peer_user_id ?? ""] && <i className="dot" />}
                      {convTitle(c)}
                    </span>
                    <span className="conv-preview">{c.preview}</span>
                  </span>
                  {c.unread > 0 && <span className="badge">{c.unread}</span>}
                </button>
              ))}
              {convs.length === 0 && <p className="empty">还没有会话</p>}
            </div>
          )}
        </aside>

        <section className="main">
          {!active ? (
            <div className="placeholder">选择一个会话开始</div>
          ) : (
            <>
              <header className="conv-head">
                {active.kind === "group" && (
                  <GroupAvatar
                    className="avatar sm"
                    id={active.id}
                    title={convTitle(active)}
                    avatarId={active.avatar_object_id}
                  />
                )}
                <b>{convTitle(active)}</b>
                {typingHere.length > 0 && (
                  <span className="typing">{typingHere.join("、")} 正在输入…</span>
                )}
                <button
                  type="button"
                  className={`ghost right docbtn${docsPanel ? " on" : ""}`}
                  title="群文档 / 共享文件"
                  onClick={toggleDocsPanel}
                >
                  文档
                </button>
                {active.kind === "group" && (
                  <button type="button" className="ghost" onClick={() => setModal("members")}>
                    成员
                  </button>
                )}
              </header>
              {err && (
                <button type="button" className="errbar" onClick={() => setErr("")}>
                  {err} ✕
                </button>
              )}
              {/* biome-ignore lint/a11y/noStaticElementInteractions lint/a11y/useKeyWithClickEvents: delegated #doc/ link interception inside rendered messages / 委托拦截消息内文档链接 */}
              <div className="ops" ref={scrollRef} onScroll={onScroll} onClick={onOpsClick}>
                {st?.hasOlder && <div className="hint">上滑加载更早的消息</div>}
                {st?.ops.map((o, i) => {
                  if (o.kind === "reaction") return null; // folded into its target bubble
                  // Day separator: first op and whenever the local calendar
                  // date changes between consecutive ops (any visible kind).
                  const prev = i > 0 ? st.ops[i - 1] : null;
                  const dayChanged = !prev || dayKey(prev.created_at) !== dayKey(o.created_at);
                  const sep = dayChanged && (
                    <div className="date-sep">
                      <span>{dayKey(o.created_at)}</span>
                    </div>
                  );
                  if (o.kind !== "message") {
                    return (
                      <Fragment key={o.op_seq}>
                        {sep}
                        <div className="sys">
                          {who(o.actor_id, o.actor_name).display_name}{" "}
                          {o.kind === "avatar" && "更换了群头像"}
                          {o.kind === "member_add" && "加入了会话"}
                          {o.kind === "member_remove" && "离开了会话"}
                          {o.kind === "edit" && "编辑了消息"}
                          {o.kind === "recall" && "撤回了一条消息"}
                          {o.kind === "rename" && `群名改为「${o.payload?.title ?? ""}」`}
                          {o.kind === "transfer" && "转让了群主"}
                          {o.kind === "role_change" && `成员角色调整为 ${o.payload?.role ?? ""}`}
                        </div>
                      </Fragment>
                    );
                  }
                  const mine = o.actor_id === me.id;
                  const q = quoteOf(o);
                  const atts = Array.isArray(o.payload?.attachments)
                    ? (o.payload?.attachments as Attachment[])
                    : [];
                  // Attachment-placeholder body ("[附件]"/"[图片]") is for the
                  // sidebar preview — don't render it as a text bubble.
                  const isPlaceholder =
                    atts.length > 0 && (o.body === "[附件]" || o.body === "[图片]");
                  return (
                    <Fragment key={o.op_seq}>
                      {sep}
                      <div className={`msg ${mine ? "mine" : ""}`}>
                        <Avatar className="avatar sm msg-av" user={who(o.actor_id, o.actor_name)} />
                        <div className="msg-col">
                          <div className="msg-meta">
                            {who(o.actor_id, o.actor_name).display_name} ·{" "}
                            {new Date(o.created_at).toLocaleTimeString()}
                            {o.edited && " · 已编辑"}
                            {o.payload?.mentions_all === true && (
                              <span className="at-all"> @所有人</span>
                            )}
                          </div>
                          {q && (
                            <div className="quote">
                              {who(q.actor_id, q.actor_name).display_name}:{" "}
                              {q.recalled ? "已撤回" : q.body?.slice(0, 80)}
                            </div>
                          )}
                          <div className="msg-body-wrap">
                            <div className="msg-bubble">
                              {(o.recalled || !isPlaceholder) && (
                                <div className={`msg-body ${o.recalled ? "recalled" : ""}`}>
                                  {o.recalled ? "消息已撤回" : <Md text={o.body ?? ""} />}
                                </div>
                              )}
                              {!o.recalled && atts.length > 0 && (
                                <div
                                  className={`atts${
                                    atts.length === 1 && isPlaceholder ? " stk-msg" : ""
                                  }`}
                                >
                                  {atts.map((a) =>
                                    a.mime?.startsWith("image/") ? (
                                      <button
                                        type="button"
                                        key={a.id}
                                        className="att-img"
                                        title={`${a.name} · 点击查看大图`}
                                        onClick={() => openViewerBlob(a.id, a.name)}
                                      >
                                        <ObjImg
                                          id={a.id}
                                          kind={a.mime === "image/gif" ? "full" : "thumb"}
                                          alt={a.name}
                                          // solo sticker-style messages get a taller cap
                                          // — separate ctx keeps dim memory from reusing
                                          // the wrong box. / 单图消息上限更高，独立 ctx
                                          // 防止尺寸记忆串场景。
                                          ctx={
                                            atts.length === 1 && isPlaceholder ? "attstk" : "att"
                                          }
                                          natW={a.w}
                                          natH={a.h}
                                        />
                                      </button>
                                    ) : (
                                      <span key={a.id} className="att-file">
                                        <button
                                          type="button"
                                          className="att"
                                          title="下载"
                                          onClick={() => saveBlob(a.id, a.name)}
                                        >
                                          📎 {a.name}
                                          <span className="att-size">{fmtSize(a.size)}</span>
                                        </button>
                                        {officeOn && isOfficeFile(a.name) && (
                                          <button
                                            type="button"
                                            className="att-edit"
                                            onClick={() => officeOpen(a.id)}
                                          >
                                            在线编辑
                                          </button>
                                        )}
                                      </span>
                                    )
                                  )}
                                </div>
                              )}
                              {mine && !o.recalled && (
                                <ReadBadge
                                  seq={o.op_seq}
                                  members={members[activeId] ?? []}
                                  meId={me.id}
                                  isDm={active.kind === "dm"}
                                />
                              )}
                            </div>
                            {!o.recalled && (
                              <div className="rx-zone">
                                {rxGroupsOf(o).map((r) => (
                                  <button
                                    type="button"
                                    key={r.emoji}
                                    className={`rx-pill${
                                      r.users.some((u) => u.id === me.id) ? " mine" : ""
                                    }`}
                                    title={r.users
                                      .map((u) => who(u.id, u.name).display_name)
                                      .join("、")}
                                    onClick={() => reactEmoji(o, r.emoji)}
                                  >
                                    {r.emoji}
                                    <span className="rx-n">{r.users.length}</span>
                                  </button>
                                ))}
                              </div>
                            )}
                          </div>
                          {!o.recalled && (
                            <div className="msg-tools">
                              <button
                                type="button"
                                className="rx-add"
                                title="点赞"
                                onClick={() => reactEmoji(o, "👍")}
                              >
                                👍
                              </button>
                              <button type="button" className="ghost" onClick={() => setQuote(o)}>
                                引用
                              </button>
                              {mine && (
                                <button
                                  type="button"
                                  className="ghost"
                                  onClick={() => recall(o.op_seq)}
                                >
                                  撤回
                                </button>
                              )}
                            </div>
                          )}
                        </div>
                      </div>
                    </Fragment>
                  );
                })}
                {pendingHere.map((p) => (
                  <div key={p.id} className="msg mine pending">
                    <Avatar className="avatar sm msg-av" user={me} />
                    <div className="msg-col">
                      <div className="msg-body">{p.markdown}</div>
                      <div className="msg-meta">{p.failed ? "发送失败" : "发送中…"}</div>
                    </div>
                  </div>
                ))}
              </div>
              {quote && (
                <div className="quote-bar">
                  引用 {who(quote.actor_id, quote.actor_name).display_name}:{" "}
                  {quote.body?.slice(0, 60)}
                  <button type="button" className="ghost" onClick={() => setQuote(null)}>
                    ✕
                  </button>
                </div>
              )}
              {(files.length > 0 || Object.keys(uploading).length > 0) && (
                <div className="file-chips">
                  {Object.entries(uploading).map(([k, pct]) => (
                    <span key={k} className="chip">
                      📎 {k.replace(/-\d+$/, "")} <em>上传中 {pct}%</em>
                    </span>
                  ))}
                  {files.map((f) => (
                    <span key={f.id} className="chip">
                      📎 {f.name} <em>{fmtSize(f.size)}</em>
                      <button
                        type="button"
                        className="ghost"
                        onClick={() => setFiles((x) => x.filter((y) => y.id !== f.id))}
                      >
                        ✕
                      </button>
                    </span>
                  ))}
                </div>
              )}
              <form className="composer" onSubmit={send}>
                <input
                  ref={fileInput}
                  type="file"
                  multiple
                  hidden
                  onChange={(e) => pickFile(e.target.files)}
                />
                <input
                  ref={stickerInput}
                  type="file"
                  accept="image/gif,image/png,image/jpeg,image/webp"
                  hidden
                  onChange={(e) => {
                    addSticker(e.target.files?.[0]);
                    e.target.value = "";
                  }}
                />
                <div className="rel" ref={plusRef}>
                  <button
                    type="button"
                    className={`attach-btn${plusMenu ? " on" : ""}`}
                    title="更多：附件 / 文档"
                    onClick={() => setPlusMenu((v) => !v)}
                  >
                    +
                  </button>
                  {plusMenu && (
                    <div className="plus-menu">
                      <button
                        type="button"
                        onClick={() => {
                          setPlusMenu(false);
                          fileInput.current?.click();
                        }}
                      >
                        上传附件
                      </button>
                      <button
                        type="button"
                        onClick={() => {
                          setPlusMenu(false);
                          setDocPick([]);
                          docsApi
                            .list()
                            .then((r) => setDocPick(r.docs))
                            .catch(() => setDocPick([]));
                        }}
                      >
                        发送文档
                      </button>
                    </div>
                  )}
                </div>
                <span className="emoji-anchor">
                  <button
                    type="button"
                    className="attach-btn emoji-btn"
                    title="表情"
                    onClick={() => setEmojiOpen((o) => !o)}
                  >
                    😊
                  </button>
                  {emojiOpen && (
                    <>
                      {/* biome-ignore lint/a11y/noStaticElementInteractions: click-away dismiss layer / 点空白处关面板 */}
                      <div
                        className="ctxmask"
                        role="presentation"
                        onClick={() => setEmojiOpen(false)}
                      />
                      <div className="emoji-pop">
                        <div className="emoji-tabs">
                          <button
                            type="button"
                            className={emojiTab === "emoji" ? "on" : ""}
                            onClick={() => setEmojiTab("emoji")}
                          >
                            表情
                          </button>
                          <button
                            type="button"
                            className={emojiTab === "stk" ? "on" : ""}
                            onClick={() => setEmojiTab("stk")}
                          >
                            贴纸
                          </button>
                        </div>
                        {emojiTab === "emoji" ? (
                          EMOJI_GROUPS.map((g) => (
                            <div key={g.label}>
                              <div className="emoji-grp">{g.label}</div>
                              <div className="emoji-grid">
                                {g.items.map((e) => (
                                  <button type="button" key={e} onClick={() => insertEmoji(e)}>
                                    {e}
                                  </button>
                                ))}
                              </div>
                            </div>
                          ))
                        ) : (
                          <div className="stk-grid">
                            <button
                              type="button"
                              className="stk-add"
                              title="上传 GIF/图片为表情"
                              onClick={() => stickerInput.current?.click()}
                            >
                              +
                            </button>
                            {stickers.map((s) => (
                              <span key={s.id} className="stk-cell">
                                <button
                                  type="button"
                                  className="stk-img"
                                  title={`${s.name} · 点击发送`}
                                  onClick={() => sendSticker(s.id)}
                                >
                                  <ObjImg
                                    id={s.id}
                                    kind={s.mime === "image/gif" ? "full" : "thumb"}
                                    alt={s.name}
                                    ctx="stk"
                                  />
                                </button>
                                <button
                                  type="button"
                                  className="stk-del"
                                  title="移除"
                                  onClick={() => delSticker(s.id)}
                                >
                                  ✕
                                </button>
                              </span>
                            ))}
                            {stickers.length === 0 && (
                              <div className="stk-empty">还没有自定义表情 — 点 + 上传 GIF/图片</div>
                            )}
                          </div>
                        )}
                      </div>
                    </>
                  )}
                </span>
                <textarea
                  ref={taRef}
                  value={text}
                  onChange={(e) => setText(e.target.value)}
                  onSelect={(e) => {
                    caretRef.current = e.currentTarget.selectionStart;
                  }}
                  onKeyDown={(e) => {
                    if (e.key === "Enter" && !e.shiftKey) {
                      e.preventDefault();
                      send();
                    }
                  }}
                  placeholder="输入消息…"
                  rows={Math.min(6, Math.max(1, text.split("\n").length))}
                />
                <span className={`count ${text.length > msgLimit ? "over" : ""}`}>
                  {text.length}/{msgLimit}
                </span>
                <button
                  type="submit"
                  disabled={(!text.trim() && files.length === 0) || text.length > msgLimit}
                >
                  发送
                </button>
              </form>
              {/* Feishu-style group docs drawer — shared docs bound to this
                  conv. 群文档侧栏：本群共享的文档/表格/演示。 */}
              {docsPanel && (
                <aside className="docs-panel" style={{ width: panelW }}>
                  {/* drag-resize handle on the left edge / 左缘拖拽调宽 */}
                  <hr
                    className="dp-resize"
                    aria-orientation="vertical"
                    onMouseDown={(e) => {
                      e.preventDefault();
                      const startX = e.clientX;
                      const startW = panelW;
                      const move = (ev: MouseEvent) => {
                        const w = Math.max(200, Math.min(520, startW + (startX - ev.clientX)));
                        setPanelW(w);
                      };
                      const up = (ev: MouseEvent) => {
                        document.removeEventListener("mousemove", move);
                        document.removeEventListener("mouseup", up);
                        const w = Math.max(200, Math.min(520, startW + (startX - ev.clientX)));
                        localStorage.setItem("docsPanelW", String(w));
                      };
                      document.addEventListener("mousemove", move);
                      document.addEventListener("mouseup", up);
                    }}
                  />
                  <div className="dp-head">
                    <b>群文档</b>
                    <button
                      type="button"
                      className="ghost sm"
                      onClick={() => loadConvDocs()}
                      title="刷新"
                    >
                      ⟳
                    </button>
                    <button
                      type="button"
                      className="ghost sm"
                      onClick={() => setDocsPanel(false)}
                      title="关闭"
                    >
                      ✕
                    </button>
                  </div>
                  <div className="dp-new">
                    <button type="button" onClick={() => createConvDoc("doc")}>
                      📄 文档
                    </button>
                    <button type="button" onClick={() => createConvDoc("sheet")}>
                      📊 表格
                    </button>
                    <button type="button" onClick={() => createConvDoc("slide")}>
                      🖼 演示
                    </button>
                  </div>
                  {convDocs === null ? (
                    <div className="empty">加载中…</div>
                  ) : convDocs.length === 0 ? (
                    <div className="empty">还没有共享文档，点上方新建</div>
                  ) : (
                    convDocs.map((d) => (
                      <div key={d.id} className="dp-row">
                        <button type="button" className="dp-main" onClick={() => openDoc(d.id)}>
                          <i className="doc-kind-ic">{KIND_ICON[d.kind] ?? "📄"}</i>
                          <span className="dp-title">{d.title || "未命名"}</span>
                          {d.editors > 0 && <i className="doc-live">{d.editors}</i>}
                          {d.shared && <i className="dp-shared">分享</i>}
                          <time>{d.updated_at?.slice(0, 10)}</time>
                        </button>
                        {/* Unshare needs doc-owner rights — hide it for
                            plain members instead of letting them 403. */}
                        {d.shared ? (
                          d.owner === me.id && (
                            <button
                              type="button"
                              className="dp-del"
                              title="移出本群（不删除文档）"
                              onClick={() => unshareConvDoc(d)}
                            >
                              ⊘
                            </button>
                          )
                        ) : (
                          <button
                            type="button"
                            className="dp-del"
                            title="删除"
                            onClick={() => delConvDoc(d)}
                          >
                            ✕
                          </button>
                        )}
                      </div>
                    ))
                  )}
                </aside>
              )}
            </>
          )}
        </section>

        {modal === "newGroup" && (
          <NewGroupDialog
            me={me}
            onClose={() => setModal("")}
            onCreated={(c) => {
              refreshDirectory();
              setActiveId(c.id);
            }}
          />
        )}
        {modal === "join" && (
          <JoinGroupDialog onClose={() => setModal("")} onJoined={refreshDirectory} />
        )}
        {modal === "members" && active && (
          <MembersPanel
            conv={active}
            members={members[activeId] ?? []}
            me={me}
            contacts={contacts}
            onClose={() => setModal("")}
            onChanged={() => {
              loadMembers(activeId);
              refreshDirectory();
            }}
          />
        )}
        {docPick && (
          // biome-ignore lint/a11y/useKeyWithClickEvents lint/a11y/noStaticElementInteractions: modal dismiss layer / 点遮罩关闭
          <div className="modal-mask" onClick={() => setDocPick(null)}>
            {/* biome-ignore lint/a11y/useKeyWithClickEvents lint/a11y/noStaticElementInteractions: stop click-through / 阻止冒泡 */}
            <div className="modal doc-pick" onClick={(e) => e.stopPropagation()}>
              <h3>发送文档到会话</h3>
              {docPick.length === 0 ? (
                <div className="empty">没有可发送的文档</div>
              ) : (
                <div className="doc-pick-list">
                  {docPick.map((d) => (
                    <button
                      type="button"
                      key={d.id}
                      className="dp-row"
                      onClick={() => sendDocLink(d)}
                    >
                      <i className="doc-kind-ic">{KIND_ICON[d.kind] ?? "📄"}</i>
                      <span className="dp-title">{d.title || "未命名"}</span>
                      <time>{d.updated_at?.slice(0, 10)}</time>
                    </button>
                  ))}
                </div>
              )}
              <button type="button" className="ghost" onClick={() => setDocPick(null)}>
                取消
              </button>
            </div>
          </div>
        )}
        {viewer && (
          <div
            className="lightbox"
            role="dialog"
            aria-modal="true"
            onClick={closeViewer}
            onKeyDown={(e) => e.key === "Escape" && closeViewer()}
          >
            <img src={viewer.url} alt={viewer.name} />
            <span className="lightbox-name">{viewer.name}</span>
            <a
              className="lightbox-dl"
              href={viewer.url}
              download={viewer.name}
              onClick={(e) => e.stopPropagation()}
            >
              下载原图
            </a>
          </div>
        )}
      </div>
    </div>
  );
}

// Rail — Feishu-style icon column / 飞书式左侧图标列。
function Rail({
  view,
  setView,
  logo,
  logoUrl,
}: {
  view: string;
  setView: (v: "chat" | "contacts" | "mail" | "docs" | "zen" | "hr" | "settings") => void;
  logo: string;
  logoUrl?: string;
}) {
  const items = [
    { key: "chat", title: "消息", icon: <IconChat /> },
    { key: "contacts", title: "通讯录", icon: <IconContacts /> },
    { key: "mail", title: "邮件", icon: <IconMail /> },
    { key: "docs", title: "文档", icon: <IconDoc /> },
    { key: "hr", title: "人力", icon: <IconHR /> },
    { key: "zen", title: "Zen", icon: <IconZen /> },
  ] as const;
  return (
    <nav className="rail">
      <span className="rail-logo" title={logo}>
        {logoUrl ? <img src={logoUrl} alt={logo} /> : logo.slice(0, 1).toUpperCase()}
      </span>
      {items.map((i) => (
        <button
          type="button"
          key={i.key}
          title={i.title}
          className={`rail-item ${view === i.key ? "active" : ""}`}
          onClick={() => setView(i.key)}
        >
          {i.icon}
        </button>
      ))}
      <div className="rail-spacer" />
      <button
        type="button"
        title="设定"
        className={`rail-item ${view === "settings" ? "active" : ""}`}
        onClick={() => setView("settings")}
      >
        <IconGear />
      </button>
    </nav>
  );
}
