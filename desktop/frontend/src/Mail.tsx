// Mail view: folders | message list | reading pane + compose modal.
// Internal mail delivers instantly; external queues for direct-to-MX.
//
// 邮件视图：文件夹 | 列表 | 阅读 + 写信弹窗。

import DOMPurify from "dompurify";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  api,
  api2,
  authBlob,
  downloadExternal,
  isOfficeFile,
  type MailFolder,
  type MailMessage,
  mailApi,
  officeOpenExternal,
  openExternal,
} from "./api";
import {
  IconArchive,
  IconDraft,
  IconInbox,
  IconSent,
  IconShield,
  IconSpam,
  IconTrash,
} from "./icons";
import { ObjImg } from "./objimg";
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
              <MailBody msg={open} />
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
                {open.attachments
                  .filter(
                    (a) =>
                      !(a.inline && a.content_id && open.html_body?.includes(`cid:${a.content_id}`))
                  )
                  .map((a) => (
                    <span key={a.object_id} className="mail-attach-item">
                      {a.mime.startsWith("image/") && showImages && (
                        <ObjImg
                          className="mail-attach-img"
                          id={a.object_id}
                          alt={a.name}
                          ctx="mail"
                          natW={a.w}
                          natH={a.h}
                        />
                      )}
                      <a
                        href={api2.downloadURL(a.object_id)}
                        rel="noreferrer"
                        download={a.name}
                        onClick={(e) => {
                          e.preventDefault();
                          downloadExternal(a.object_id).catch(() => {});
                        }}
                      >
                        📎 {a.name}
                      </a>
                      {officeOn && isOfficeFile(a.name) && (
                        <button
                          type="button"
                          className="att-edit"
                          title="邮件附件是不可变记录，只读打开"
                          onClick={() => officeOpenExternal(a.object_id).catch(() => {})}
                        >
                          在线查看
                        </button>
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
  const [uploading, setUploading] = useState<Record<string, number>>({});
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
        {Object.entries(uploading).map(([k, pct]) => (
          <span key={k} className="chip">
            {k.replace(/-\d+$/, "")} <em>上传中 {pct}%</em>
          </span>
        ))}
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
            // Same size routing as chat: ≤upload_max direct, else S3
            // multipart (mail purpose, no conv), else friendly refuse.
            // 与聊天一致的按大小分流：超限走 S3 分片（mail 用途）。
            const limits = await api2.storageLimits().catch(() => null);
            for (const f of Array.from(e.target.files ?? [])) {
              const key = `${f.name}-${f.size}`;
              try {
                let obj: { id: string; name: string };
                if (!limits || f.size <= limits.upload_max_bytes) {
                  obj = await mailApi.upload(f);
                } else if (limits.multipart && f.size <= limits.large_max_bytes) {
                  setUploading((x) => ({ ...x, [key]: 0 }));
                  obj = await api2.uploadLarge(f, { purpose: "mail" }, (pct) =>
                    setUploading((x) => ({ ...x, [key]: pct }))
                  );
                  setUploading((x) => {
                    const n = { ...x };
                    delete n[key];
                    return n;
                  });
                } else {
                  const lim = limits.multipart ? limits.large_max_bytes : limits.upload_max_bytes;
                  setErr(`${f.name} 超过上限（${Math.round(lim / 1048576)} MB）`);
                  continue;
                }
                files.push(obj);
                setFiles([...files]);
              } catch (err) {
                setUploading((x) => {
                  const n = { ...x };
                  delete n[key];
                  return n;
                });
                setErr(
                  `上传失败：${f.name}${err instanceof Error && err.message ? ` — ${err.message}` : ""}`
                );
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

// URL_RE for linkify — http(s) and mailto only. / 链接化：仅 http(s) 与 mailto。
const URL_RE = /(https?:\/\/[^\s<>"')]+|mailto:[^\s<>"')]+)/g;

function linkifyText(t: string) {
  const out: (string | React.JSX.Element)[] = [];
  let last = 0;
  let i = 0;
  for (const m of t.matchAll(URL_RE)) {
    const idx = m.index ?? 0;
    out.push(t.slice(last, idx));
    const u = m[0];
    out.push(
      <a
        key={i++}
        href={u}
        rel="noreferrer"
        onClick={(e) => {
          e.preventDefault();
          openExternal(u).catch(() => {});
        }}
      >
        {u}
      </a>
    );
    last = idx + u.length;
  }
  out.push(t.slice(last));
  return out;
}

// MailBody: plain text renders pre-wrap with linkified URLs; HTML mail
// renders inside a sandboxed iframe (no allow-scripts) after DOMPurify.
// Remote images are blocked by default (tracking pixels) behind a bar.
// 正文渲染：纯文本 pre-wrap + 链接化；HTML 邮件 DOMPurify 后进沙盒
// iframe。远程图片默认拦截（防追踪像素），点"显示"才加载。
function MailBody({ msg }: { msg: MailMessage }) {
  const ref = useRef<HTMLIFrameElement>(null);
  const [showRemote, setShowRemote] = useState(false);
  const roRef = useRef<ResizeObserver | null>(null);
  const blobRef = useRef<Map<string, string>>(new Map());

  // CSP does the real remote-blocking: nothing fetches http(s) until
  // showRemote widens img-src — covers CSS url(), srcset, background=,
  // <video poster>, everything the DOM rewrite could miss.
  // 远程拦截靠 CSP：showRemote 之前任何 http(s) 拉取都被拒——CSS url()、
  // srcset、background、video poster 一并堵死，不靠 DOM 改写兜底。
  const { html, remote } = useMemo(() => {
    if (!msg.html_body) return { html: "", remote: 0 };
    const clean = DOMPurify.sanitize(msg.html_body, {
      WHOLE_DOCUMENT: false,
      FORBID_TAGS: ["script", "iframe", "object", "embed", "form", "input", "base"],
    });
    const doc = new DOMParser().parseFromString(`<div>${clean}</div>`, "text/html");
    let remote = 0;
    doc.querySelectorAll("img").forEach((img) => {
      const src = img.getAttribute("src") || "";
      if (src.startsWith("cid:")) {
        img.setAttribute("data-cid", src.slice(4));
        img.removeAttribute("src");
      } else if (/^https?:/i.test(src)) {
        remote++;
        if (!showRemote) {
          img.setAttribute("data-remote-src", src);
          img.removeAttribute("src");
        }
      }
      if (!showRemote) {
        img.removeAttribute("srcset");
      }
    });
    // meta CSP goes in the rebuilt document head AFTER sanitize.
    const imgSrc = showRemote ? " http: https:" : "";
    return {
      html: `<!doctype html><html><head><meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'; font-src data:; img-src blob: data:${imgSrc}"><style>body{margin:8px;background:#fff;color:#111;font:14px/1.5 sans-serif}img{max-width:100%}</style></head><body>${doc.body.innerHTML}</body></html>`,
      remote,
    };
  }, [msg.html_body, showRemote]);

  // wire() runs ONLY on iframe load — each load is a fresh document, so
  // handlers bind exactly once. Delegated click listener; one ResizeObserver
  // per document; cid blobs cached per message.
  // 只在 iframe onLoad 时接线：每次载入是新文档，事件只绑一次。委托式
  // 点击监听；每文档一个 ResizeObserver；cid blob 按消息缓存。
  const wire = useCallback(() => {
    const d = ref.current?.contentDocument;
    // srcdoc iframes can fire load twice on the same document — wire once.
    // srcdoc iframe 同一文档可能触发两次 load——只接一次线。
    if (!d || (d as Document & { __wired?: boolean }).__wired) return;
    (d as Document & { __wired?: boolean }).__wired = true;
    d.querySelectorAll("img[data-cid]").forEach((img) => {
      const cid = img.getAttribute("data-cid");
      const att = msg.attachments?.find((a) => a.content_id === cid);
      if (!att) return;
      const cached = blobRef.current.get(att.object_id);
      if (cached) {
        img.setAttribute("src", cached);
        return;
      }
      authBlob(api2.downloadURL(att.object_id))
        .then((b) => {
          const url = URL.createObjectURL(b);
          blobRef.current.set(att.object_id, url);
          img.setAttribute("src", url);
        })
        .catch(() => {});
    });
    // Delegated click: one listener for every anchor. / 委托监听所有链接。
    d.addEventListener("click", (e) => {
      const a = (e.target as Element).closest("a[href]");
      if (!a) return;
      e.preventDefault();
      const href = a.getAttribute("href") || "";
      if (/^https?:/i.test(href) || href.startsWith("mailto:")) {
        openExternal(href).catch(() => {});
      }
    });
    const fitH = () => {
      if (ref.current?.contentDocument?.body) {
        ref.current.style.height = `${ref.current.contentDocument.body.scrollHeight + 8}px`;
      }
    };
    fitH();
    roRef.current?.disconnect();
    if (typeof ResizeObserver !== "undefined" && d.body) {
      roRef.current = new ResizeObserver(fitH);
      roRef.current.observe(d.body);
    }
  }, [msg.attachments]);

  // Cleanup on unmount — blob URLs are object-keyed so they stay valid
  // across messages. / 卸载时回收：blob URL 按对象 id 存，跨消息仍有效。
  useEffect(
    () => () => {
      roRef.current?.disconnect();
      roRef.current = null;
      blobRef.current.forEach((u) => {
        URL.revokeObjectURL(u);
      });
      blobRef.current.clear();
    },
    []
  );

  if (!msg.html_body) {
    return <div className="mail-text">{linkifyText(msg.text_body)}</div>;
  }
  return (
    <div className="mail-html-wrap">
      {remote > 0 && !showRemote && (
        <div className="mail-remote-bar">
          已阻止 {remote} 张远程图片 ·{" "}
          <button type="button" className="ghost sm" onClick={() => setShowRemote(true)}>
            显示
          </button>
        </div>
      )}
      <iframe
        ref={ref}
        className="mail-html"
        sandbox="allow-same-origin"
        srcDoc={html}
        title="mail body"
        onLoad={wire}
      />
    </div>
  );
}
