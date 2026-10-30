import { type FormEvent, useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  api,
  api2,
  type ConversationInfo,
  connectWS,
  type MemberInfo,
  type MessageSearchHit,
  newClientMsgId,
  type OpEvent,
  type UserPublic,
  type WsFrame,
} from "./api";
import { Contacts } from "./Contacts";
import { JoinGroupDialog, MembersPanel, NewGroupDialog } from "./dialogs";
import { IconChat, IconContacts, IconGear, IconMail, IconSearch, IconZen } from "./icons";
import { Mail } from "./Mail";
import { Md } from "./md";
import { Settings } from "./Settings";

const MSG_LIMIT = 2000;

interface Attachment {
  id: string;
  name: string;
  size: number;
  mime: string;
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

export function Chat({ me, onLogout }: { me: UserPublic; onLogout: () => void }) {
  const [convs, setConvs] = useState<ConversationInfo[]>([]);
  const [activeId, setActiveId] = useState("");
  const [states, setStates] = useState<Record<string, ConvState>>({});
  const [pending, setPending] = useState<Pending[]>([]);
  const [text, setText] = useState("");
  const [quote, setQuote] = useState<OpEvent | null>(null);
  const [files, setFiles] = useState<Attachment[]>([]);
  const fileInput = useRef<HTMLInputElement>(null);
  const [typing, setTyping] = useState<Record<string, string>>({});
  const [online, setOnline] = useState<Record<string, boolean>>({});
  const [members, setMembers] = useState<Record<string, MemberInfo[]>>({});
  const [contacts, setContacts] = useState<UserPublic[]>([]);
  const [modal, setModal] = useState<"" | "newGroup" | "join" | "members">("");
  const [msgLimit, setMsgLimit] = useState(MSG_LIMIT);
  const [wsName, setWsName] = useState("W");
  const [searchQ, setSearchQ] = useState("");
  const [searchHits, setSearchHits] = useState<MessageSearchHit[] | null>(null);

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
  const [view, setView] = useState<"chat" | "contacts" | "mail" | "zen" | "settings">("chat");
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
    needInitScroll.current = true; // 进会话跳到底
    if (!states[activeId]?.loaded) fetchDelta(activeId);
    loadMembers(activeId);
    const c = convs.find((x) => x.id === activeId);
    if (c && c.last_op_seq > 0) api.markRead(activeId, c.last_op_seq).catch(() => {});
  }, [activeId]);

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
    for (const f of Array.from(list)) {
      try {
        const obj = await api2.upload(activeId, f);
        setFiles((x) => [...x, { id: obj.id, name: obj.name, size: obj.size, mime: obj.mime }]);
      } catch {
        setErr(`上传失败：${f.name}`);
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

  const recall = async (seq: number) => {
    try {
      await api.recall(activeId, seq);
      fetchDelta(activeId);
    } catch (ex) {
      setErr(ex instanceof Error ? ex.message : "recall failed");
    }
  };

  const convTitle = (c: ConversationInfo) =>
    c.kind === "dm" ? c.peer_name || "私聊" : c.title || "群聊";

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
        <Rail view={view} setView={setView} logo={wsName} />
        {view === "mail" && <Mail />}
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
        {view === "settings" && <Settings me={me} onLogout={onLogout} />}
        {modal === "newGroup" && (
          <NewGroupDialog
            contacts={contacts}
            onClose={() => setModal("")}
            onCreated={refreshDirectory}
          />
        )}
        {modal === "join" && (
          <JoinGroupDialog onClose={() => setModal("")} onJoined={refreshDirectory} />
        )}
      </div>
    );
  }

  return (
    <div className="app">
      <Rail view={view} setView={setView} logo={wsName} />
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
                  <span className="conv-title">
                    {h.conv_title}
                    <i className="dim"> · {h.actor_name}</i>
                  </span>
                  <span className="conv-preview">{h.body}</span>
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
                  <span className="conv-title">
                    {c.kind === "dm" && online[c.peer_user_id ?? ""] && <i className="dot" />}
                    {convTitle(c)}
                  </span>
                  {c.unread > 0 && <span className="badge">{c.unread}</span>}
                  <span className="conv-preview">{c.preview}</span>
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
                <b>{convTitle(active)}</b>
                {typingHere.length > 0 && (
                  <span className="typing">{typingHere.join("、")} 正在输入…</span>
                )}
                {active.kind === "group" && (
                  <button type="button" className="ghost right" onClick={() => setModal("members")}>
                    成员
                  </button>
                )}
              </header>
              {err && (
                <button type="button" className="errbar" onClick={() => setErr("")}>
                  {err} ✕
                </button>
              )}
              <div className="ops" ref={scrollRef} onScroll={onScroll}>
                {st?.hasOlder && <div className="hint">上滑加载更早的消息</div>}
                {st?.ops.map((o) => {
                  if (o.kind !== "message") {
                    return (
                      <div key={o.op_seq} className="sys">
                        {o.kind === "member_add" && "加入了会话"}
                        {o.kind === "member_remove" && "离开了会话"}
                        {o.kind === "edit" && "编辑了消息"}
                        {o.kind === "recall" && "撤回了一条消息"}
                        {o.kind === "rename" && `群名改为「${o.payload?.title ?? ""}」`}
                        {o.kind === "transfer" && "转让了群主"}
                        {o.kind === "role_change" && `成员角色调整为 ${o.payload?.role ?? ""}`}
                      </div>
                    );
                  }
                  const mine = o.actor_id === me.id;
                  const q = quoteOf(o);
                  const atts = Array.isArray(o.payload?.attachments)
                    ? (o.payload?.attachments as Attachment[])
                    : [];
                  return (
                    <div key={o.op_seq} className={`msg ${mine ? "mine" : ""}`}>
                      <div className="msg-meta">
                        {o.actor_name || me.display_name} ·{" "}
                        {new Date(o.created_at).toLocaleTimeString()}
                        {o.edited && " · 已编辑"}
                        {o.payload?.mentions_all === true && (
                          <span className="at-all"> @所有人</span>
                        )}
                      </div>
                      {q && (
                        <div className="quote">
                          {q.actor_name}: {q.recalled ? "已撤回" : q.body?.slice(0, 80)}
                        </div>
                      )}
                      <div className="msg-body-wrap">
                        <div className={`msg-body ${o.recalled ? "recalled" : ""}`}>
                          {o.recalled ? "消息已撤回" : <Md text={o.body ?? ""} />}
                        </div>
                        {mine && !o.recalled && (
                          <ReadBadge
                            seq={o.op_seq}
                            members={members[activeId] ?? []}
                            meId={me.id}
                            isDm={active.kind === "dm"}
                          />
                        )}
                      </div>
                      {!o.recalled && atts.length > 0 && (
                        <div className="atts">
                          {atts.map((a) => (
                            <button
                              type="button"
                              key={a.id}
                              className="att"
                              title="下载"
                              onClick={() => saveBlob(a.id, a.name)}
                            >
                              📎 {a.name}
                              <span className="att-size">{fmtSize(a.size)}</span>
                            </button>
                          ))}
                        </div>
                      )}
                      {!o.recalled && (
                        <div className="msg-tools">
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
                  );
                })}
                {pendingHere.map((p) => (
                  <div key={p.id} className="msg mine pending">
                    <div className="msg-body">{p.markdown}</div>
                    <div className="msg-meta">{p.failed ? "发送失败" : "发送中…"}</div>
                  </div>
                ))}
              </div>
              {quote && (
                <div className="quote-bar">
                  引用 {quote.actor_name}: {quote.body?.slice(0, 60)}
                  <button type="button" className="ghost" onClick={() => setQuote(null)}>
                    ✕
                  </button>
                </div>
              )}
              {files.length > 0 && (
                <div className="file-chips">
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
                <button
                  type="button"
                  className="attach-btn"
                  title="发送附件"
                  onClick={() => fileInput.current?.click()}
                >
                  +
                </button>
                <textarea
                  value={text}
                  onChange={(e) => setText(e.target.value)}
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
      </div>
    </div>
  );
}

// Rail — Feishu-style icon column / 飞书式左侧图标列。
function Rail({
  view,
  setView,
  logo,
}: {
  view: string;
  setView: (v: "chat" | "contacts" | "mail" | "zen" | "settings") => void;
  logo: string;
}) {
  const items = [
    { key: "chat", title: "消息", icon: <IconChat /> },
    { key: "contacts", title: "通讯录", icon: <IconContacts /> },
    { key: "mail", title: "邮件", icon: <IconMail /> },
    { key: "zen", title: "Zen", icon: <IconZen /> },
  ] as const;
  return (
    <nav className="rail">
      <span className="rail-logo" title={logo}>
        {logo.slice(0, 1).toUpperCase()}
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
