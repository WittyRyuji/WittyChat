// Mail view: folders | message list | reading pane + compose modal.
// Internal mail delivers instantly; external queues for direct-to-MX.
//
// 邮件视图：文件夹 | 列表 | 阅读 + 写信弹窗。
import { useCallback, useEffect, useRef, useState } from "react";
import { api, api2, type MailFolder, type MailMessage, mailApi } from "./api";
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

const TRUST_KEY = "pref.mail.trusted";
const AUTOIMG_KEY = "pref.mail.autoimg";

function readTrusted(): string[] {
  try {
    return JSON.parse(localStorage.getItem(TRUST_KEY) ?? "[]");
  } catch {
    return [];
  }
}

export function trustSender(addr: string) {
  const a = addr.toLowerCase();
  const list = readTrusted();
  if (!list.includes(a)) {
    localStorage.setItem(TRUST_KEY, JSON.stringify([...list, a]));
  }
}
export function untrustSender(addr: string) {
  const a = addr.toLowerCase();
  localStorage.setItem(TRUST_KEY, JSON.stringify(readTrusted().filter((x) => x !== a)));
}
export function trustedSenders(): string[] {
  return readTrusted();
}
export function autoLoadImages(): boolean {
  return localStorage.getItem(AUTOIMG_KEY) !== "0";
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
  const [composing, setComposing] = useState<MailMessage | null | "new">(null);
  const [err, setErr] = useState("");
  const [contacts, setContacts] = useState<Set<string>>(new Set());
  const [trusted, setTrusted] = useState<string[]>(readTrusted());
  const [revealed, setRevealed] = useState<Set<string>>(new Set());

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

  const loadMsgs = useCallback(() => {
    if (!activeFolder) return;
    mailApi
      .messages(activeFolder)
      .then(setMsgs)
      .catch(() => setMsgs([]));
  }, [activeFolder]);

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
      loadMsgs();
      refreshFolders();
    });
  };

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
          <button type="button" className="ghost" onClick={() => setComposing("new")}>
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
            >
              {folderIcon(f.kind)}
              {f.unread > 0 && <span className="dot">{f.unread}</span>}
            </button>
          ))}
        </div>

        <div className="conv-list mail-list">
          {msgs.map((m) => (
            <button
              type="button"
              key={m.id}
              className={`mail-item ${m.is_read ? "" : "unread"} ${open?.id === m.id ? "active" : ""}`}
              onClick={() => openMsg(m)}
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
        </div>
      </aside>

      <section className="main mailmain">
        {err && <div className="errbar">{err}</div>}
        {!open && <p className="empty">选择左侧邮件查看</p>}
        {open && (
          <div className="mail-read">
            <div className="mail-read-head">
              <b>{open.subject || "(无主题)"}</b>
              <span className="mail-meta">
                {open.from_addr} → {open.to_addrs.join(", ")}
                {open.cc_addrs.length > 0 && ` (抄送 ${open.cc_addrs.join(", ")})`}
              </span>
              <div className="mail-actions">
                <button type="button" className="ghost" onClick={() => setComposing(open)}>
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
                  </span>
                ))}
              </div>
            )}
          </div>
        )}
      </section>

      {composing && (
        <Compose
          replyTo={composing === "new" ? null : composing}
          onClose={() => {
            setComposing(null);
            loadMsgs();
          }}
        />
      )}
    </div>
  );
}

function Compose({ replyTo, onClose }: { replyTo: MailMessage | null; onClose: () => void }) {
  const [to, setTo] = useState(replyTo ? replyTo.from_addr : "");
  const [subject, setSubject] = useState(replyTo ? `Re: ${replyTo.subject}` : "");
  const [body, setBody] = useState("");
  const [files, setFiles] = useState<{ id: string; name: string }[]>([]);
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
        replyTo?.rfc_msgid,
        files.map((f) => f.id)
      );
      onClose();
    } catch (e) {
      setErr(String(e));
      setSending(false);
    }
  };

  return (
    <div className="modal-mask" role="dialog" aria-modal="true">
      <div className="modal compose">
        <h3>{replyTo ? "回复" : "写信"}</h3>
        {err && <div className="errbar">{err}</div>}
        <input
          placeholder="收件人（逗号分隔）"
          value={to}
          onChange={(e) => setTo(e.target.value)}
        />
        <input placeholder="主题" value={subject} onChange={(e) => setSubject(e.target.value)} />
        <textarea
          placeholder="正文（支持 Markdown）"
          value={body}
          onChange={(e) => setBody(e.target.value)}
          rows={10}
        />
        <div className="compose-attach">
          <button type="button" className="ghost" onClick={() => fileRef.current?.click()}>
            + 附件
          </button>
          {files.map((f) => (
            <span key={f.id} className="chip">
              {f.name}
            </span>
          ))}
          <input
            ref={fileRef}
            type="file"
            hidden
            onChange={async (e) => {
              const f = e.target.files?.[0];
              if (!f) return;
              try {
                files.push(await mailApi.upload(f));
                setFiles([...files]);
              } catch (err) {
                setErr(String(err));
              }
              e.target.value = "";
            }}
          />
        </div>
        <div className="modal-actions">
          <button type="button" className="ghost" onClick={onClose}>
            取消
          </button>
          <button type="button" className="primary" onClick={send} disabled={sending || !to.trim()}>
            {sending ? "发送中…" : "发送"}
          </button>
        </div>
      </div>
    </div>
  );
}
