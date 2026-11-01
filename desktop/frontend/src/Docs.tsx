// Native collaborative docs (Feishu-style): left list + TipTap editor
// synced over /ws/doc/{id} via Yjs. Awareness drives presence chips and
// peer carets; the Go relay persists every update for late joiners.
//
// 原生协同文档（飞书式）：左列表 + TipTap 编辑器，经 /ws/doc/{id} 走
// Yjs 同步；awareness 驱动在线徽章与对端光标；Go 中继逐条落库供回放。
import Collaboration from "@tiptap/extension-collaboration";
import CollaborationCursor from "@tiptap/extension-collaboration-cursor";
import { EditorContent, useEditor } from "@tiptap/react";
import StarterKit from "@tiptap/starter-kit";
import { useEffect, useMemo, useState } from "react";
import { WebsocketProvider } from "y-websocket";
import * as Y from "yjs";
import { type DocInfo, docsApi, docWsBase, getToken, type UserPublic } from "./api";

// Peer caret color — stable per user id, shared palette with group avatars.
// 对端光标颜色：按用户 id 定 hash 取色，同人同色。
const CURSOR_COLORS = [
  "#7c5cfc",
  "#1f9cf0",
  "#00b96b",
  "#f0a41f",
  "#f0524f",
  "#c94fd6",
  "#0fb5ba",
  "#8a6d3b",
];

export function cursorColor(id: string): string {
  let h = 0;
  for (let i = 0; i < id.length; i++) h = (h * 31 + id.charCodeAt(i)) >>> 0;
  return CURSOR_COLORS[h % CURSOR_COLORS.length];
}

interface PeerUser {
  name?: string;
  color?: string;
}

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
  const [err, setErr] = useState("");

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

  const active = docs?.find((d) => d.id === activeId);

  const createDoc = () => {
    docsApi
      .create("未命名文档")
      .then((d) => {
        setDocs((l) => (l ? [d, ...l] : [d]));
        setActiveId(d.id);
      })
      .catch(() => setErr("新建文档失败"));
  };

  return (
    <div className="main docs-page">
      <aside className="doc-list">
        <div className="doc-list-head">
          <b>文档</b>
          <button type="button" className="ghost sm" onClick={createDoc}>
            ＋新建
          </button>
        </div>
        {err && (
          <button type="button" className="errbar" onClick={() => setErr("")}>
            {err} ✕
          </button>
        )}
        {docs === null ? (
          <div className="empty">加载中…</div>
        ) : docs.length === 0 ? (
          <div className="empty">还没有文档，点「＋新建」开始</div>
        ) : (
          docs.map((d) => (
            <button
              type="button"
              key={d.id}
              className={`doc-row ${d.id === activeId ? "active" : ""}`}
              onClick={() => setActiveId(d.id)}
            >
              <span className="doc-row-title">{d.title || "未命名文档"}</span>
              <span className="doc-row-meta">
                {d.conv_id && <i className="doc-tag">群文档</i>}
                {d.editors > 0 && <i className="doc-live">{d.editors} 人在线</i>}
                <time>{d.updated_at?.slice(0, 10)}</time>
              </span>
            </button>
          ))
        )}
      </aside>
      {active ? (
        <DocEditor
          key={active.id}
          me={me}
          doc={active}
          onRenamed={(t) =>
            setDocs((l) => l?.map((x) => (x.id === active.id ? { ...x, title: t } : x)) ?? l)
          }
        />
      ) : (
        <div className="pane">
          <div className="empty">从左侧选择一篇文档，或新建一篇与成员实时协作</div>
        </div>
      )}
    </div>
  );
}

function DocEditor({
  me,
  doc,
  onRenamed,
}: {
  me: UserPublic;
  doc: DocInfo;
  onRenamed: (title: string) => void;
}) {
  const [title, setTitle] = useState(doc.title);
  const [peers, setPeers] = useState<PeerUser[]>([]);
  const [connected, setConnected] = useState(false);

  // One Y.Doc + one provider per mounted editor (parent keys by doc.id).
  const [{ ydoc, provider }] = useState(() => {
    const y = new Y.Doc();
    const p = new WebsocketProvider(docWsBase(), doc.id, y, {
      params: { token: getToken() },
    });
    return { ydoc: y, provider: p };
  });

  useEffect(
    () => () => {
      provider.destroy();
      ydoc.destroy();
    },
    [provider, ydoc]
  );

  useEffect(() => {
    const onSync = (s: boolean) => setConnected(s);
    const onStatus = ({ status }: { status: string }) => setConnected(status === "connected");
    provider.on("sync", onSync);
    provider.on("status", onStatus);
    return () => {
      provider.off("sync", onSync);
      provider.off("status", onStatus);
    };
  }, [provider]);

  useEffect(() => {
    const aw = provider.awareness;
    const onChange = () => {
      const states = [...aw.getStates().values()] as { user?: PeerUser }[];
      setPeers(states.map((s) => s.user ?? {}).filter((u) => u.name));
    };
    aw.on("change", onChange);
    onChange();
    return () => aw.off("change", onChange);
  }, [provider]);

  const user = useMemo(
    () => ({ name: me.display_name || me.email, color: cursorColor(me.id) }),
    [me]
  );

  const editor = useEditor(
    {
      extensions: [
        StarterKit.configure({ history: false }),
        Collaboration.configure({ document: ydoc }),
        CollaborationCursor.configure({ provider, user }),
      ],
    },
    []
  );

  useEffect(() => () => editor?.destroy(), [editor]);

  const commitTitle = () => {
    const t = title.trim() || "未命名文档";
    setTitle(t);
    if (t !== doc.title) {
      docsApi
        .rename(doc.id, t)
        .then(() => onRenamed(t))
        .catch(() => {});
    }
  };

  return (
    <div className="doc-editor-pane">
      <header className="doc-editor-head">
        <input
          className="doc-title"
          value={title}
          onChange={(e) => setTitle(e.target.value)}
          onBlur={commitTitle}
          onKeyDown={(e) => e.key === "Enter" && (e.target as HTMLInputElement).blur()}
          placeholder="未命名文档"
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
      </header>
      <Toolbar editor={editor} />
      <div className="doc-editor-scroll">
        <EditorContent editor={editor} className="doc-editor" />
      </div>
    </div>
  );
}

// Minimal formatting toolbar — 常用排版按钮。
function Toolbar({ editor }: { editor: ReturnType<typeof useEditor> }) {
  if (!editor) return null;
  const btn = (label: string, active: boolean, onClick: () => void, title?: string) => (
    <button
      type="button"
      title={title ?? label}
      className={`doc-tb ${active ? "on" : ""}`}
      onMouseDown={(e) => {
        e.preventDefault(); // keep editor selection
        onClick();
      }}
    >
      {label}
    </button>
  );
  return (
    <div className="doc-toolbar">
      {btn("B", editor.isActive("bold"), () => editor.chain().focus().toggleBold().run(), "加粗")}
      {btn(
        "I",
        editor.isActive("italic"),
        () => editor.chain().focus().toggleItalic().run(),
        "斜体"
      )}
      {btn(
        "S",
        editor.isActive("strike"),
        () => editor.chain().focus().toggleStrike().run(),
        "删除线"
      )}
      <span className="doc-tb-sep" />
      {btn("H1", editor.isActive("heading", { level: 1 }), () =>
        editor.chain().focus().toggleHeading({ level: 1 }).run()
      )}
      {btn("H2", editor.isActive("heading", { level: 2 }), () =>
        editor.chain().focus().toggleHeading({ level: 2 }).run()
      )}
      <span className="doc-tb-sep" />
      {btn(
        "•",
        editor.isActive("bulletList"),
        () => editor.chain().focus().toggleBulletList().run(),
        "无序列表"
      )}
      {btn(
        "1.",
        editor.isActive("orderedList"),
        () => editor.chain().focus().toggleOrderedList().run(),
        "有序列表"
      )}
      {btn(
        "❝",
        editor.isActive("blockquote"),
        () => editor.chain().focus().toggleBlockquote().run(),
        "引用"
      )}
      {btn(
        "</>",
        editor.isActive("codeBlock"),
        () => editor.chain().focus().toggleCodeBlock().run(),
        "代码块"
      )}
    </div>
  );
}
