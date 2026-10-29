import { type FormEvent, useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  api,
  type ConversationInfo,
  connectWS,
  newClientMsgId,
  type OpEvent,
  setToken,
  type UserPublic,
  type WsFrame,
} from "./api";

const MSG_LIMIT = 2000;

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

export function Chat({ me, onLogout }: { me: UserPublic; onLogout: () => void }) {
  const [convs, setConvs] = useState<ConversationInfo[]>([]);
  const [activeId, setActiveId] = useState("");
  const [states, setStates] = useState<Record<string, ConvState>>({});
  const [pending, setPending] = useState<Pending[]>([]);
  const [text, setText] = useState("");
  const [quote, setQuote] = useState<OpEvent | null>(null);
  const [typing, setTyping] = useState<Record<string, string>>({});
  const [online, setOnline] = useState<Record<string, boolean>>({});
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
            [convId]: { ops: acc, watermark: wm, hasOlder: cur.hasOlder, loaded: true },
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

  const loadOlder = useCallback(
    async (convId: string) => {
      const cur = states[convId];
      if (!cur?.hasOlder || cur.ops.length === 0) return;
      const el = scrollRef.current;
      const prevHeight = el?.scrollHeight ?? 0;
      const oldest = cur.ops[0].op_seq;
      const page = await api.opsBefore(convId, oldest);
      setStates((p) => {
        const c = p[convId];
        if (!c) return p;
        const older = page.ops.filter((o) => !c.ops.some((x) => x.op_seq === o.op_seq));
        return {
          ...p,
          [convId]: { ...c, ops: [...older, ...c.ops], hasOlder: page.has_more },
        };
      });
      requestAnimationFrame(() => {
        if (el) el.scrollTop = el.scrollHeight - prevHeight;
      });
    },
    [states]
  );

  // ---------- ws ----------

  useEffect(() => {
    refreshDirectory();
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
              setActiveId((cur) => {
                if (f.conv_id === cur) fetchDelta(cur);
                else if (f.conv_id) fetchDelta(f.conv_id);
                return cur;
              });
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
  }, [refreshDirectory, fetchDelta]);

  // Load a conversation when selected.
  // Keyed on activeId only — load/reset on conversation switch.
  // biome-ignore lint/correctness/useExhaustiveDependencies: intentional switch-load
  useEffect(() => {
    if (!activeId) return;
    setQuote(null);
    setText("");
    if (!states[activeId]?.loaded) fetchDelta(activeId);
    const c = convs.find((x) => x.id === activeId);
    if (c && c.last_op_seq > 0) api.markRead(activeId, c.last_op_seq).catch(() => {});
  }, [activeId]);

  // Scroll to bottom on new messages (when near bottom) or conv switch.
  // biome-ignore lint/correctness/useExhaustiveDependencies: scroll triggers
  useEffect(() => {
    const el = scrollRef.current;
    if (!el) return;
    const nearBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 160;
    if (nearBottom) el.scrollTop = el.scrollHeight;
  }, [st?.ops.length, activeId]);

  const onScroll = () => {
    const el = scrollRef.current;
    if (el && el.scrollTop < 60 && st?.hasOlder) loadOlder(activeId);
  };

  // ---------- send ----------

  const send = async (e?: FormEvent) => {
    e?.preventDefault();
    const md = text.trim();
    if (!md || !activeId || md.length > MSG_LIMIT) return;
    const p: Pending = {
      id: newClientMsgId(),
      convId: activeId,
      markdown: md,
      quoteSeq: quote?.op_seq,
    };
    setPending((x) => [...x, p]);
    setText("");
    setQuote(null);
    try {
      const op = await api.send(activeId, p.id, md, p.quoteSeq);
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

  return (
    <div className="chat">
      <aside className="sidebar">
        <div className="side-head">
          <span className="me">{me.display_name}</span>
          <button
            type="button"
            className="ghost"
            onClick={() => {
              api.logout().catch(() => {});
              setToken("");
              onLogout();
            }}
          >
            登出
          </button>
        </div>
        <nav className="nav">
          <span className="nav-item active">消息</span>
          <span className="nav-item dim">邮件</span>
          <span className="nav-item dim">Zen</span>
        </nav>
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
                    </div>
                  );
                }
                const mine = o.actor_id === me.id;
                const q = quoteOf(o);
                return (
                  <div key={o.op_seq} className={`msg ${mine ? "mine" : ""}`}>
                    <div className="msg-meta">
                      {o.actor_name} · {new Date(o.created_at).toLocaleTimeString()}
                      {o.edited && " · 已编辑"}
                    </div>
                    {q && (
                      <div className="quote">
                        {q.actor_name}: {q.recalled ? "已撤回" : q.body?.slice(0, 80)}
                      </div>
                    )}
                    <div className={`msg-body ${o.recalled ? "recalled" : ""}`}>
                      {o.recalled ? "消息已撤回" : o.body}
                    </div>
                    {!o.recalled && (
                      <div className="msg-tools">
                        <button type="button" className="ghost" onClick={() => setQuote(o)}>
                          引用
                        </button>
                        {mine && (
                          <button type="button" className="ghost" onClick={() => recall(o.op_seq)}>
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
            <form className="composer" onSubmit={send}>
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
              <div className="composer-foot">
                <span className={`count ${text.length > MSG_LIMIT ? "over" : ""}`}>
                  {text.length}/{MSG_LIMIT}
                </span>
                <button type="submit" disabled={!text.trim() || text.length > MSG_LIMIT}>
                  发送
                </button>
              </div>
            </form>
          </>
        )}
      </section>
    </div>
  );
}
