// Mail view: folders | message list | reading pane + compose modal.
// Internal mail delivers instantly; external queues for direct-to-MX.
//
// 邮件视图：文件夹 | 列表 | 阅读 + 写信弹窗。
import { useCallback, useEffect, useRef, useState } from "react";
import { api, api2, isOfficeFile, type MailFolder, type MailMessage, mailApi } from "./api";
import {
  IconArchive,
  IconDraft,
  IconInbox,
  IconSent,
  IconShield,
  IconSpam,
  IconTrash,
} from "./icons";
import { Md } from "./md";
import { prefGet, prefSet } from "./prefs";

const TRUST_KEY = "pref.mail.trusted";
const AUTOIMG_KEY = "pref.mail.autoimg";

// Prefs are server-synced via prefs.ts mirror — same keys, same semantics,
// but they now follow the user across devices.
// 偏好经 prefs.ts 镜像到服务端——键名语义不变，但跨设备跟随用户。

function readTrusted(): string[] {
  return prefGet<string[]>(TRUST_KEY, []);
}

export function trustSender(addr: string) {
  const a = addr.toLowerCase();
  const list = readTrusted();
  if (!list.includes(a)) {
    prefSet(TRUST_KEY, [...list, a]);
  }
}
export function untrustSender(addr: string) {
  const a = addr.toLowerCase();
  prefSet(
    TRUST_KEY,
    readTrusted().filter((x) => x !== a)
  );
}
export function trustedSenders(): string[] {
  return readTrusted();
}
export function autoLoadImages(): boolean {
  return prefGet<boolean>(AUTOIMG_KEY, true);
}

function fmtDate(s: string): string {
  const d = new Date(s);
  const today = new Date().toDateString();
  return d.toDateString() === today
    ? d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })
    : d.toLocaleDateString([], { month: "short", day: "numeric" });
}

function folderIcon(kind: string) {
  switch (kind) {
    case "inbox":
      return <IconInbox size={18} />;
    case "sent":
      return <IconSent size={18} />;
    case "drafts":
      return <IconDraft size={18} />;
    case "spam":
      return <IconSpam size={18} />;
    case "archive":
      return <IconArchive size={18} />;
    case "trash":
      return <IconTrash size={18} />;
    default:
      return <IconInbox size={18} />;
  }
}

export function Mail() {
  const [folders, setFolders] = useState<MailFolder[]>([]);
  const [activeFolder, setActiveFolder] = useState<string>("");
  const [msgs, setMsgs] = useState<MailMessage[]>([]);
  const [open, setOpen] = useState<MailMessage | null>(null);
  // composing in the right pane: new / reply / forward
  // 右栏写信：新邮件 / 回复 / 转发
  const [composing, setComposing] = useState<
    { mode: "new" } | { mode: "reply" | "fwd"; msg: MailMessage } | null
  >(null);
  const [menu, setMenu] = useState<{ x: number; y: number; m: MailMessage } | null>(null);
  // Folder context menu (mark-all-read) + multi-select for bulk actions.
  // 文件夹右键（全部已读）+ Ctrl+A 多选批量操作。
  const [fmenu, setFmenu] = useState<{ x: number; y: number; f: MailFolder } | null>(null);
  const [sel, setSel] = useState<Set<string>>(new Set());
  const [err, setErr] = useState("");
  const [contacts, setContacts] = useState<Set<string>>(new Set());
  const [trusted, setTrusted] = useState<string[]>(readTrusted());
  const [revealed, setRevealed] = useState<Set<string>>(new Set());
  const [officeOn, setOfficeOn] = useState(false);
  useEffect(() => {
    api2.officeEnabled().then(setOfficeOn);
  }, []);

  const refreshFolders = useCallback(() => {
    mailApi
      .folders()
      .then((fs) => {
        setFolders(fs);
        if (!activeFolder) {
          const inbox = fs.find((f) => f.kind === "inbox");
          if (inbox) setActiveFolder(inbox.id);
        }
      })
      .catch((e) => setErr(String(e)));
  }, [activeFolder]);

  const [hasMore, setHasMore] = useState(false);
  const moreRef = useRef(false); // pagination in-flight guard / 翻页请求去重
  // Folder the list currently shows — late responses for another folder
  // are dropped. 列表当前对应的文件夹；其它文件夹的迟到响应直接丢弃。
  const folderRef = useRef(activeFolder);
  folderRef.current = activeFolder;

  const loadMsgs = useCallback(() => {
    if (!activeFolder) return;
    const fid = activeFolder;
    mailApi
      .messages(fid, 0)
      .then((ms) => {
        if (folderRef.current !== fid) return;
        setMsgs(ms);
        setHasMore(ms.length === 50); // backend page size / 后端每页 50
      })
      .catch(() => {
        if (folderRef.current !== fid) return;
        setMsgs([]);
        setHasMore(false);
      });
  }, [activeFolder]);

  // Infinite scroll: fetch the next offset page when the list nears its
  // bottom (unread badge can exceed one page — 未读数可能超过单页 50)。
  const loadMore = useCallback(() => {
    if (!activeFolder || moreRef.current) return;
    const fid = activeFolder;
    moreRef.current = true;
    mailApi
      .messages(fid, msgs.length)
      .then((ms) => {
        if (folderRef.current !== fid) return;
        setMsgs((prev) => [...prev, ...ms.filter((m) => !prev.some((p) => p.id === m.id))]);
        setHasMore(ms.length === 50);
      })
      .catch(() => {})
      .finally(() => {
        moreRef.current = false;
      });
  }, [activeFolder, msgs.length]);

  const onListScroll = (e: React.UIEvent<HTMLDivElement>) => {
    const el = e.currentTarget;
    if (hasMore && el.scrollTop + el.clientHeight >= el.scrollHeight - 80) loadMore();
  };

  // Mount-only: seed folders + pick inbox. activeFolder lives in the callback
  // for the "folders empty → default inbox" path, refire would loop.
  // biome-ignore lint/correctness/useExhaustiveDependencies: mount-only
  useEffect(() => {
    refreshFolders();
    api
      .users()
      .then((r) => setContacts(new Set(r.users.map((u) => u.email.toLowerCase()))))
      .catch(() => {});
  }, []);
  useEffect(() => {
    loadMsgs();
  }, [loadMsgs]);

  const openMsg = (m: MailMessage) => {
    mailApi.get(m.id).then((full) => {
      setOpen(full);
      setComposing(null);
      loadMsgs();
      refreshFolders();
    });
  };

  // Ctrl+A selects all messages in the list (skip when typing in an
  // input/textarea/contenteditable). Escape or any click clears.
  // Ctrl+A 全选列表邮件（输入框内不劫持），Esc/点击清除。
  const isEditable = (t: EventTarget | null) =>
    t instanceof HTMLElement &&
    (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.isContentEditable);

  // biome-ignore lint/correctness/useExhaustiveDependencies: stable callbacks enough
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (sel.size > 0 && e.key === "Escape") setSel(new Set());
      if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "a" && !isEditable(e.target)) {
        e.preventDefault();
        setSel(new Set(msgs.map((m) => m.id)));
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [msgs, sel.size]);

  const bulk = async (action: "read" | "trash" | "del") => {
    const ids = [...sel];
    setSel(new Set());
    if (action === "read") {
      await Promise.all(ids.map((id) => mailApi.flags(id, true)));
    } else if (action === "trash") {
      const fid = folderId("trash");
      if (!fid) return;
      await Promise.all(ids.map((id) => mailApi.move(id, fid)));
      if (open && sel.has(open.id)) setOpen(null);
    } else {
      if (!window.confirm(`彻底删除 ${ids.length} 封邮件？不可恢复。`)) return;
      await Promise.all(ids.map((id) => mailApi.del(id)));
      setOpen(null);
    }
    loadMsgs();
    refreshFolders();
  };

  const folderId = (kind: string) => folders.find((f) => f.kind === kind)?.id;
  const activeKind = folders.find((f) => f.id === activeFolder)?.kind ?? "inbox";

  const moveTo = (m: MailMessage, kind: string) => {
    const fid = folderId(kind);
    if (!fid) return;
    mailApi.move(m.id, fid).then(() => {
      if (open?.id === m.id) setOpen(null);
      loadMsgs();
      refreshFolders();
    });
  };

  const forward = (m: MailMessage) =>
    mailApi.get(m.id).then((full) => {
      setComposing({ mode: "fwd", msg: full });
      setOpen(null);
    });

  const fromTrusted = (m: MailMessage) =>
    m.direction === "out" ||
    contacts.has(m.from_addr.toLowerCase()) ||
    trusted.includes(m.from_addr.toLowerCase());

  const imgCount = open?.attachments?.filter((a) => a.mime.startsWith("image/")).length ?? 0;
  const showImages =
    !!open && imgCount > 0 && (revealed.has(open.id) || (autoLoadImages() && fromTrusted(open)));

  return (
    <div className="chat mailview">
      <aside className="sidebar">
        <div className="side-head">
          <span className="me">邮件</span>
          <button
            type="button"
            className="ghost"
            onClick={() => {
              setComposing({ mode: "new" });
              setOpen(null);
            }}
          >
            写信
          </button>
        </div>

        <div className="mail-foldbar">
          {folders.map((f) => (
            <button
              type="button"
              key={f.id}
              className={`fold ${f.id === activeFolder ? "active" : ""}`}
              title={f.name}
              onClick={() => {
                setActiveFolder(f.id);
                setOpen(null);
              }}
              onContextMenu={(e) => {
                e.preventDefault();
                setFmenu({ x: e.clientX, y: e.clientY, f });
              }}
            >
              {folderIcon(f.kind)}
              {f.unread > 0 && <span className="dot">{f.unread}</span>}
            </button>
          ))}
        </div>

        {sel.size > 0 && (
          <div className="bulkbar">
            <span>已选 {sel.size} 封</span>
            <button type="button" className="ghost sm" onClick={() => bulk("read")}>
              标为已读
            </button>
            {activeKind === "trash" ? (
              <button type="button" className="ghost sm" onClick={() => bulk("del")}>
                彻底删除
              </button>
            ) : (
              <button type="button" className="ghost sm" onClick={() => bulk("trash")}>
                删除
              </button>
            )}
            <button type="button" className="ghost sm" onClick={() => setSel(new Set())}>
              取消
            </button>
          </div>
        )}
        <div className="conv-list mail-list" onScroll={onListScroll}>
          {msgs.map((m) => (
            <button
              type="button"
              key={m.id}
              className={`mail-item ${m.is_read ? "" : "unread"} ${open?.id === m.id ? "active" : ""} ${sel.has(m.id) ? "sel" : ""}`}
              onClick={(e) => {
                if (e.ctrlKey || e.metaKey) {
                  setSel((s) => {
                    const n = new Set(s);
                    if (n.has(m.id)) n.delete(m.id);
                    else n.add(m.id);
                    return n;
                  });
                  return;
                }
                setSel(new Set());
                openMsg(m);
              }}
              onContextMenu={(e) => {
                e.preventDefault();
                setMenu({ x: e.clientX, y: e.clientY, m });
              }}
            >
              <span className="mail-line1">
                <span className="mail-from">
                  {m.direction === "out" ? `→ ${m.to_addrs.join(",")}` : m.from_addr}
                </span>
                <span className="mail-date">{fmtDate(m.received_at)}</span>
              </span>
              <span className="mail-subj">
                {m.subject || "(无主题)"}
                {m.is_starred && " ★"}
              </span>
              <span className="mail-prev">{m.preview}</span>
            </button>
          ))}
          {msgs.length === 0 && <p className="empty">此文件夹为空</p>}
          {hasMore && <div className="hint">下拉加载更多…</div>}
        </div>
      </aside>

      <section className="main mailmain">
        {err && <div className="errbar">{err}</div>}
        {composing ? (
          <Compose
            mode={composing.mode}
            msg={"msg" in composing ? composing.msg : null}
            onClose={() => {
              setComposing(null);
              loadMsgs();
            }}
          />
        ) : !open ? (
          <p className="empty">选择左侧邮件查看</p>
        ) : (
          <div className="mail-read">
            <div className="mail-read-head">
              <b>{open.subject || "(无主题)"}</b>
              <span className="mail-meta">
                {open.from_addr} → {open.to_addrs.join(", ")}
                {open.cc_addrs.length > 0 && ` (抄送 ${open.cc_addrs.join(", ")})`}
              </span>
              <div className="mail-actions">
                <button
                  type="button"
                  className="ghost"
                  onClick={() => setComposing({ mode: "reply", msg: open })}
                >
                  回复
                </button>
                <button
                  type="button"
                  className="ghost"
                  onClick={() =>
                    mailApi
                      .flags(open.id, undefined, !open.is_starred)
                      .then(() => setOpen({ ...open, is_starred: !open.is_starred }))
                  }
                >
                  {open.is_starred ? "取消星标" : "星标"}
                </button>
                <button type="button" className="ghost" onClick={() => setOpen(null)}>
                  关闭
                </button>
              </div>
            </div>
            {!fromTrusted(open) && (
              <div className="mail-trustbar">
                <IconShield size={15} />
                <span>该发件人不在你的通讯录/信任列表中，请留意钓鱼风险。</span>
                <button
                  type="button"
                  className="ghost sm"
                  onClick={() => {
                    trustSender(open.from_addr);
                    setTrusted(readTrusted());
                  }}
                >
                  信任该发件人
                </button>
              </div>
            )}
            <div className="mail-body">
              <Md text={open.text_body} />
            </div>
            {open.attachments && open.attachments.length > 0 && (
              <div className="mail-attach">
                {imgCount > 0 && !showImages && (
                  <div className="mail-imgblock">
                    已隐藏 {imgCount} 张图片
                    {fromTrusted(open) ? "（设定中关闭了图片自动显示）" : "（发件人不受信任）"}
                    <button
                      type="button"
                      className="ghost sm"
                      onClick={() => setRevealed(new Set(revealed).add(open.id))}
                    >
                      显示图片
                    </button>
                  </div>
                )}
                {open.attachments.map((a) => (
                  <span key={a.object_id} className="mail-attach-item">
                    {a.mime.startsWith("image/") && showImages && (
                      <img
                        className="mail-attach-img"
                        src={api2.downloadURL(a.object_id)}
                        alt={a.name}
                      />
                    )}
                    <a
                      href={api2.downloadURL(a.object_id)}
                      target="_blank"
                      rel="noreferrer"
                      download={a.name}
                    >
                      📎 {a.name}
                    </a>
                    {officeOn && isOfficeFile(a.name) && (
                      <a
                        className="att-edit"
                        href={api2.officeEditURL(a.object_id)}
                        target="_blank"
                        rel="noreferrer"
                        title="邮件附件是不可变记录，只读打开"
                      >
                        在线查看
                      </a>
                    )}
                  </span>
                ))}
              </div>
            )}
          </div>
        )}
      </section>

      {fmenu && (
        /* biome-ignore lint/a11y/noStaticElementInteractions: click-away dismiss layer / 点空白处关菜单 */
        <div
          className="ctxmask"
          role="presentation"
          onClick={() => setFmenu(null)}
          onContextMenu={(e) => {
            e.preventDefault();
            setFmenu(null);
          }}
        >
          <div className="ctxmenu" style={{ left: fmenu.x, top: fmenu.y }}>
            <button
              type="button"
              onClick={() => {
                const f = fmenu.f;
                setFmenu(null);
                mailApi.readAll(f.id).then(() => {
                  loadMsgs();
                  refreshFolders();
                });
              }}
            >
              全部标为已读{fmenu.f.unread > 0 ? `（${fmenu.f.unread}）` : ""}
            </button>
          </div>
        </div>
      )}

      {menu && (
        /* biome-ignore lint/a11y/noStaticElementInteractions: click-away dismiss layer / 点空白处关菜单 */
        <div
          className="ctxmask"
          role="presentation"
          onClick={() => setMenu(null)}
          onContextMenu={(e) => {
            e.preventDefault();
            setMenu(null);
          }}
        >
          <div className="ctxmenu" style={{ left: menu.x, top: menu.y }}>
            <button
              type="button"
              onClick={() => {
                setMenu(null);
                setComposing({ mode: "reply", msg: menu.m });
                setOpen(null);
              }}
            >
              回复
            </button>
            <button
              type="button"
              onClick={() => {
                setMenu(null);
                forward(menu.m);
              }}
            >
              转发
            </button>
            <button
              type="button"
              onClick={() => {
                setMenu(null);
                mailApi.flags(menu.m.id, undefined, !menu.m.is_starred).then(loadMsgs);
              }}
            >
              {menu.m.is_starred ? "取消星标" : "星标"}
            </button>
            <hr />
            {activeKind === "spam" ? (
              <button
                type="button"
                onClick={() => {
                  setMenu(null);
                  moveTo(menu.m, "inbox");
                }}
              >
                这不是垃圾邮件
              </button>
            ) : (
              <button
                type="button"
                onClick={() => {
                  setMenu(null);
                  moveTo(menu.m, "spam");
                }}
              >
                这是垃圾邮件
              </button>
            )}
            {activeKind === "trash" ? (
              <button
                type="button"
                className="danger"
                onClick={() => {
                  setMenu(null);
                  if (window.confirm("彻底删除这封邮件？不可恢复。")) {
                    mailApi.del(menu.m.id).then(() => {
                      if (open?.id === menu.m.id) setOpen(null);
                      loadMsgs();
                    });
                  }
                }}
              >
                彻底删除
              </button>
            ) : (
              <button
                type="button"
                className="danger"
                onClick={() => {
                  setMenu(null);
                  moveTo(menu.m, "trash");
                }}
              >
                删除
              </button>
            )}
          </div>
        </div>
      )}
    </div>
  );
}

// Compose lives in the right pane — new / reply / forward.
// Compose 占右侧主区：新邮件 / 回复 / 转发（转发预填附件与引用正文）。
function Compose({
  mode,
  msg,
  onClose,
}: {
  mode: "new" | "reply" | "fwd";
  msg: MailMessage | null;
  onClose: () => void;
}) {
  const quoted = msg
    ? `\n\n---------- ${mode === "fwd" ? "转发" : "原始"}邮件 ----------\n发件人: ${msg.from_addr}\n日期: ${new Date(msg.received_at).toLocaleString()}\n主题: ${msg.subject}\n\n${msg.text_body}`
    : "";
  const [to, setTo] = useState(mode === "reply" && msg ? msg.from_addr : "");
  const [subject, setSubject] = useState(
    msg ? `${mode === "fwd" ? "Fwd: " : "Re: "}${msg.subject}` : ""
  );
  const [body, setBody] = useState(quoted);
  const [files, setFiles] = useState<{ id: string; name: string }[]>(
    mode === "fwd" && msg?.attachments
      ? msg.attachments.map((a) => ({ id: a.object_id, name: a.name }))
      : []
  );
  const [sending, setSending] = useState(false);
  const [err, setErr] = useState("");
  const fileRef = useRef<HTMLInputElement>(null);

  const send = async () => {
    setSending(true);
    setErr("");
    try {
      await mailApi.send(
        to
          .split(/[,;]/)
          .map((s) => s.trim())
          .filter(Boolean),
        [],
        subject,
        body,
        mode === "reply" ? msg?.rfc_msgid : undefined,
        files.map((f) => f.id)
      );
      onClose();
    } catch (e) {
      setErr(String(e));
      setSending(false);
    }
  };

  const title = mode === "reply" ? "回复" : mode === "fwd" ? "转发" : "写信";
  return (
    <div className="mail-compose">
      <div className="mail-compose-head">
        <h3>{title}</h3>
        <button type="button" className="ghost" onClick={onClose}>
          ✕ 关闭
        </button>
      </div>
      {err && <div className="errbar">{err}</div>}
      <input
        className="mc-input"
        placeholder="收件人（逗号分隔）"
        value={to}
        onChange={(e) => setTo(e.target.value)}
      />
      <input
        className="mc-input"
        placeholder="主题"
        value={subject}
        onChange={(e) => setSubject(e.target.value)}
      />
      <textarea
        className="mc-body"
        placeholder="正文（支持 Markdown）"
        value={body}
        onChange={(e) => setBody(e.target.value)}
      />
      <div className="compose-attach">
        <button type="button" className="ghost" onClick={() => fileRef.current?.click()}>
          + 附件
        </button>
        {files.map((f) => (
          <span key={f.id} className="chip">
            {f.name}
            <button
              type="button"
              className="ghost"
              onClick={() => setFiles(files.filter((x) => x.id !== f.id))}
            >
              ✕
            </button>
          </span>
        ))}
        <input
          ref={fileRef}
          type="file"
          multiple
          hidden
          onChange={async (e) => {
            for (const f of Array.from(e.target.files ?? [])) {
              try {
                files.push(await mailApi.upload(f));
                setFiles([...files]);
              } catch (err) {
                setErr(String(err));
              }
            }
            e.target.value = "";
          }}
        />
        <span className="mc-spacer" />
        <button type="button" className="primary" onClick={send} disabled={sending || !to.trim()}>
          {sending ? "发送中…" : "发送"}
        </button>
      </div>
    </div>
  );
}
