// Native collaborative docs hub (Feishu-style): left list + kind-aware
// editor (rich text / sheet / slide), all synced over /ws/doc/{id} via
// Yjs. Awareness drives presence chips; the Go relay persists updates.
//
// 原生协同文档（飞书式）：左列表 + 按类型分发编辑器（富文本/表格/演示），
// 统一走 /ws/doc/{id} Yjs 同步；awareness 驱动在线徽章；中继逐条落库。
import { useEffect, useMemo, useRef, useState } from "react";
import { type DocInfo, docsApi, type UserPublic } from "./api";
import { cursorColor } from "./colors";
import DocTextEditor from "./DocTextEditor";
import SheetEditor from "./SheetEditor";
import SlideEditor from "./SlideEditor";
import { useDocConn } from "./useDocConn";

export const KIND_LABEL: Record<string, string> = {
  doc: "文档",
  sheet: "表格",
  slide: "演示",
};
export const KIND_ICON: Record<string, string> = {
  doc: "📄",
  sheet: "📊",
  slide: "🖼",
};

export function Docs({
  me,
  focusId,
  onFocused,
}: {
  me: UserPublic;
  focusId?: string;
  onFocused?: () => void;
}) {
  const [docs, setDocs] = useState<DocInfo[] | null>(null);
  const [activeId, setActiveId] = useState(focusId ?? "");
  const [kindFilter, setKindFilter] = useState("");
  const [err, setErr] = useState("");
  const [createMenu, setCreateMenu] = useState(false);
  const menuRef = useRef<HTMLDivElement>(null);

  const refresh = () =>
    docsApi
      .list()
      .then((r) => setDocs(r.docs))
      .catch(() => setErr("加载文档列表失败"));

  // biome-ignore lint/correctness/useExhaustiveDependencies: mount-only load / 仅挂载时加载
  useEffect(() => {
    void refresh();
  }, []);

  useEffect(() => {
    if (focusId) {
      setActiveId(focusId);
      onFocused?.();
    }
  }, [focusId, onFocused]);

  // click-outside closes the create menu
  useEffect(() => {
    if (!createMenu) return;
    const close = (e: MouseEvent) => {
      if (!menuRef.current?.contains(e.target as Node)) setCreateMenu(false);
    };
    document.addEventListener("mousedown", close);
    return () => document.removeEventListener("mousedown", close);
  }, [createMenu]);

  const active = docs?.find((d) => d.id === activeId);
  const shown = useMemo(
    () => (docs ?? []).filter((d) => !kindFilter || d.kind === kindFilter),
    [docs, kindFilter]
  );

  const createDoc = (kind: "doc" | "sheet" | "slide") => {
    setCreateMenu(false);
    docsApi
      .create(`未命名${KIND_LABEL[kind]}`, kind)
      .then((d) => {
        setDocs((l) => (l ? [d, ...l] : [d]));
        setActiveId(d.id);
      })
      .catch(() => setErr("新建失败"));
  };

  const delDoc = (d: DocInfo) => {
    if (!window.confirm(`删除「${d.title || "未命名"}」？删除后所有成员都将无法再访问。`)) return;
    docsApi
      .del(d.id)
      .then(() => {
        setDocs((l) => l?.filter((x) => x.id !== d.id) ?? l);
        if (activeId === d.id) setActiveId("");
      })
      .catch(() => setErr("删除失败：只有属主或群主可删"));
  };

  return (
    <div className="main docs-page">
      <aside className="doc-list">
        <div className="doc-list-head">
          <b>文档</b>
          <div className="rel" ref={menuRef}>
            <button type="button" className="ghost sm" onClick={() => setCreateMenu((v) => !v)}>
              ＋新建 ▾
            </button>
            {createMenu && (
              <div className="menu">
                <button type="button" onClick={() => createDoc("doc")}>
                  📄 文档
                </button>
                <button type="button" onClick={() => createDoc("sheet")}>
                  📊 表格
                </button>
                <button type="button" onClick={() => createDoc("slide")}>
                  🖼 演示
                </button>
              </div>
            )}
          </div>
        </div>
        <div className="doc-kind-tabs">
          {(["", "doc", "sheet", "slide"] as const).map((k) => (
            <button
              type="button"
              key={k || "all"}
              className={`kind-tab${kindFilter === k ? " active" : ""}`}
              onClick={() => setKindFilter(k)}
            >
              {k === "" ? "全部" : KIND_LABEL[k]}
            </button>
          ))}
        </div>
        {err && (
          <button type="button" className="errbar" onClick={() => setErr("")}>
            {err} ✕
          </button>
        )}
        {docs === null ? (
          <div className="empty">加载中…</div>
        ) : shown.length === 0 ? (
          <div className="empty">还没有文档，点「＋新建」开始</div>
        ) : (
          shown.map((d) => (
            <div key={d.id} className={`doc-row ${d.id === activeId ? "active" : ""}`}>
              <button type="button" className="doc-row-main" onClick={() => setActiveId(d.id)}>
                <span className="doc-row-title">
                  <i className="doc-kind-ic">{KIND_ICON[d.kind] ?? "📄"}</i>
                  {d.title || "未命名"}
                </span>
                <span className="doc-row-meta">
                  {d.conv_id && <i className="doc-tag">群</i>}
                  {d.editors > 0 && <i className="doc-live">{d.editors} 人在线</i>}
                  <time>{d.updated_at?.slice(0, 10)}</time>
                </span>
              </button>
              <button type="button" className="doc-row-del" title="删除" onClick={() => delDoc(d)}>
                ✕
              </button>
            </div>
          ))
        )}
      </aside>
      {active ? (
        <DocEditorPane
          key={active.id}
          me={me}
          doc={active}
          onRenamed={(t) =>
            setDocs((l) => l?.map((x) => (x.id === active.id ? { ...x, title: t } : x)) ?? l)
          }
          onDeleted={() => {
            setDocs((l) => l?.filter((x) => x.id !== active.id) ?? l);
            setActiveId("");
          }}
        />
      ) : (
        <div className="pane">
          <div className="empty">
            {activeId && docs !== null
              ? "该文档不存在、已被删除，或你没有访问权限。可请文档属主或群管理员分享给你。"
              : "从左侧选择一个文件，或新建文档 / 表格 / 演示与成员实时协作"}
          </div>
        </div>
      )}
    </div>
  );
}

function DocEditorPane({
  me,
  doc,
  onRenamed,
  onDeleted,
}: {
  me: UserPublic;
  doc: DocInfo;
  onRenamed: (title: string) => void;
  onDeleted: () => void;
}) {
  const [title, setTitle] = useState(doc.title);
  const { ydoc, provider, peers, connected, synced, docFull } = useDocConn(doc.id);
  const user = useMemo(
    () => ({ name: me.display_name || me.email || me.id, color: cursorColor(me.id) }),
    [me]
  );

  // publish our cursor identity into awareness (editors refine with cell/slide)
  useEffect(() => {
    provider.awareness.setLocalStateField("user", user);
  }, [provider, user]);

  const commitTitle = () => {
    const t = title.trim() || "未命名";
    setTitle(t);
    if (t !== doc.title) {
      docsApi
        .rename(doc.id, t)
        .then(() => onRenamed(t))
        .catch(() => {});
    }
  };

  const del = () => {
    if (!window.confirm(`删除「${title || "未命名"}」？删除后所有成员都将无法再访问。`)) return;
    docsApi
      .del(doc.id)
      .then(onDeleted)
      .catch(() => {});
  };

  return (
    <div className="doc-editor-pane">
      <header className="doc-editor-head">
        <i className="doc-kind-ic">{KIND_ICON[doc.kind] ?? "📄"}</i>
        <input
          className="doc-title"
          value={title}
          onChange={(e) => setTitle(e.target.value)}
          onBlur={commitTitle}
          onKeyDown={(e) => e.key === "Enter" && (e.target as HTMLInputElement).blur()}
          placeholder="未命名"
        />
        <span className={`doc-conn ${connected ? "on" : ""}`}>
          {connected ? "已连接" : "连接中…"}
        </span>
        {peers.length > 0 && (
          <span className="doc-peers">
            {peers.map((p, i) => (
              <i key={p.name ?? i} style={{ background: p.color }}>
                {(p.name ?? "?").slice(0, 1)}
              </i>
            ))}
          </span>
        )}
        <button type="button" className="ghost danger sm" title="删除" onClick={del}>
          删除
        </button>
      </header>
      {docFull && (
        <div className="doc-full-banner">
          文档存储已达上限，新的编辑不会被保存。请文档属主上线一次以自动压缩历史，或联系管理员调整上限。
        </div>
      )}
      {doc.kind === "sheet" ? (
        <SheetEditor ydoc={ydoc} provider={provider} me={me} />
      ) : doc.kind === "slide" ? (
        <SlideEditor
          ydoc={ydoc}
          provider={provider}
          me={me}
          synced={synced}
          isOwner={doc.owner === me.id}
        />
      ) : (
        <DocTextEditor ydoc={ydoc} provider={provider} me={me} />
      )}
    </div>
  );
}
