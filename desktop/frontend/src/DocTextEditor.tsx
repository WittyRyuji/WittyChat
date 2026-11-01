// Feishu-style rich-text doc editor: TipTap 2 + Yjs Collaboration/Cursor.
// Toolbar: font family/size, bold/italic/underline/strike, color/highlight,
// align, headings, lists, task list, link, image, table ops, undo/redo.
//
// 富文本编辑器：TipTap2 + Yjs 协同。工具栏含字体/字号/加粗斜体下划线/
// 颜色高亮/对齐/标题/列表/任务/链接/图片/表格/撤销重做。
import { Extension } from "@tiptap/core";
import Collaboration from "@tiptap/extension-collaboration";
import CollaborationCursor from "@tiptap/extension-collaboration-cursor";
import Color from "@tiptap/extension-color";
import FontFamily from "@tiptap/extension-font-family";
import Highlight from "@tiptap/extension-highlight";
import Image from "@tiptap/extension-image";
import Link from "@tiptap/extension-link";
import Table from "@tiptap/extension-table";
import TableCell from "@tiptap/extension-table-cell";
import TableHeader from "@tiptap/extension-table-header";
import TableRow from "@tiptap/extension-table-row";
import TaskItem from "@tiptap/extension-task-item";
import TaskList from "@tiptap/extension-task-list";
import TextAlign from "@tiptap/extension-text-align";
import TextStyle from "@tiptap/extension-text-style";
import Underline from "@tiptap/extension-underline";
import { EditorContent, useEditor } from "@tiptap/react";
import StarterKit from "@tiptap/starter-kit";
import { useEffect, useState } from "react";
import type { WebsocketProvider } from "y-websocket";
import type * as Y from "yjs";
import type { UserPublic } from "./api";

// FontSize via TextStyle global attribute (v2 has no official ext).
// 字号：挂到 TextStyle 上的自定义属性（v2 无官方字号扩展）。
const FontSize = Extension.create({
  name: "fontSize",
  addGlobalAttributes() {
    return [
      {
        types: ["textStyle"],
        attributes: {
          fontSize: {
            default: null,
            parseHTML: (el) => (el.style as CSSStyleDeclaration).fontSize || null,
            renderHTML: (attrs) => (attrs.fontSize ? { style: `font-size:${attrs.fontSize}` } : {}),
          },
        },
      },
    ];
  },
  addCommands() {
    return {
      setFontSize:
        (size: string) =>
        // biome-ignore lint/suspicious/noExplicitAny: tiptap chained-command ctx / 链式上下文
        ({ chain }: any) =>
          chain().setMark("textStyle", { fontSize: size }).run(),
      unsetFontSize:
        () =>
        // biome-ignore lint/suspicious/noExplicitAny: tiptap chained-command ctx / 链式上下文
        ({ chain }: any) =>
          chain().setMark("textStyle", { fontSize: null }).removeEmptyTextStyle().run(),
    };
  },
});

declare module "@tiptap/core" {
  interface Commands<ReturnType> {
    fontSize: {
      setFontSize: (size: string) => ReturnType;
      unsetFontSize: () => ReturnType;
    };
  }
}

const FONT_SIZES = ["12px", "14px", "15px", "16px", "18px", "20px", "24px", "28px", "32px"];
const FONT_FAMILIES = [
  ["默认", ""],
  ["宋体", "SimSun, serif"],
  ["黑体", "SimHei, sans-serif"],
  ["楷体", "KaiTi, serif"],
  ["微软雅黑", "'Microsoft YaHei', sans-serif"],
  ["Arial", "Arial, sans-serif"],
  ["Times", "'Times New Roman', serif"],
  ["Courier", "'Courier New', monospace"],
];
const COLORS = ["", "#d83931", "#e67e22", "#f1c40f", "#2f9e44", "#1c7ed6", "#7048e8", "#868e96"];

export default function DocTextEditor({
  ydoc,
  provider,
  me,
}: {
  ydoc: Y.Doc;
  provider: WebsocketProvider;
  me: UserPublic;
}) {
  const editor = useEditor(
    {
      extensions: [
        StarterKit.configure({ history: false }), // history off: Yjs handles undo across peers
        Collaboration.configure({ document: ydoc }),
        CollaborationCursor.configure({
          provider,
          user: { name: me.display_name || me.email, color: "#7048e8" },
        }),
        TextStyle,
        FontFamily,
        FontSize,
        Color,
        Underline,
        Highlight.configure({ multicolor: true }),
        TextAlign.configure({ types: ["heading", "paragraph"] }),
        Link.configure({ openOnClick: false }),
        Image,
        TaskList,
        TaskItem.configure({ nested: true }),
        Table.configure({ resizable: false }),
        TableRow,
        TableHeader,
        TableCell,
      ],
      editorProps: { attributes: { class: "doc-editor-prose" } },
    },
    [ydoc]
  );

  // refresh toolbar state on every transaction so toggles stay accurate
  useEffect(() => {
    if (!editor) return;
    const rerender = () => setTick((t) => t + 1);
    editor.on("transaction", rerender);
    return () => {
      editor.off("transaction", rerender);
    };
  }, [editor]);

  const [, setTick] = useState(0);
  if (!editor) return <div className="doc-editor-loading">加载中…</div>;
  const e = editor;
  const is = (n: string, a?: Record<string, unknown>) => e.isActive(n, a);
  const on = (fn: () => unknown) => () => {
    fn();
    setTick((t) => t + 1);
  };

  const setLink = () => {
    const prev = e.getAttributes("link").href as string | undefined;
    const url = window.prompt("链接地址 / Link URL", prev ?? "https://");
    if (url === null) return;
    if (url === "") e.chain().focus().unsetLink().run();
    else e.chain().focus().setLink({ href: url }).run();
  };

  // Toolbar button factory — plain function (not a component) so biome's
  // hook-factory rule doesn't fire. 工具栏按钮工厂（普通函数非组件）。
  const btn = (
    label: string,
    fn: () => void,
    opts?: { title?: string; active?: boolean; disabled?: boolean }
  ) => (
    <button
      type="button"
      key={label + (opts?.title ?? "")}
      className={`tb-btn${opts?.active ? " active" : ""}`}
      title={opts?.title ?? label}
      disabled={opts?.disabled}
      onClick={fn}
      onMouseDown={(ev) => ev.preventDefault()}
    >
      {label}
    </button>
  );

  return (
    <div className="doc-editor">
      <div className="doc-toolbar">
        <select
          className="tb-sel"
          title="字体"
          value={(e.getAttributes("textStyle").fontFamily as string) ?? ""}
          onChange={(ev) =>
            ev.target.value
              ? e.chain().focus().setFontFamily(ev.target.value).run()
              : e.chain().focus().unsetFontFamily().run()
          }
        >
          {FONT_FAMILIES.map(([n, v]) => (
            <option key={n} value={v}>
              {n}
            </option>
          ))}
        </select>
        <select
          className="tb-sel tb-size"
          title="字号"
          value={(e.getAttributes("textStyle").fontSize as string) ?? ""}
          onChange={(ev) =>
            ev.target.value
              ? e.chain().focus().setFontSize(ev.target.value).run()
              : e.chain().focus().unsetFontSize().run()
          }
        >
          <option value="">字号</option>
          {FONT_SIZES.map((s) => (
            <option key={s} value={s}>
              {s.replace("px", "")}
            </option>
          ))}
        </select>
        <span className="tb-sep" />
        {btn(
          "B",
          on(() => e.chain().focus().toggleBold().run()),
          { title: "加粗", active: is("bold") }
        )}
        {btn(
          "I",
          on(() => e.chain().focus().toggleItalic().run()),
          { title: "斜体", active: is("italic") }
        )}
        {btn(
          "U",
          on(() => e.chain().focus().toggleUnderline().run()),
          { title: "下划线", active: is("underline") }
        )}
        {btn(
          "S",
          on(() => e.chain().focus().toggleStrike().run()),
          { title: "删除线", active: is("strike") }
        )}
        <span className="tb-sep" />
        <select
          className="tb-sel tb-color"
          title="文字颜色"
          value={(e.getAttributes("textStyle").color as string) ?? ""}
          onChange={(ev) =>
            ev.target.value
              ? e.chain().focus().setColor(ev.target.value).run()
              : e.chain().focus().unsetColor().run()
          }
        >
          <option value="">字色</option>
          {COLORS.filter(Boolean).map((c) => (
            <option key={c} value={c} style={{ color: c }}>
              ●{" "}
              {c === "#d83931"
                ? "红"
                : c === "#e67e22"
                  ? "橙"
                  : c === "#f1c40f"
                    ? "黄"
                    : c === "#2f9e44"
                      ? "绿"
                      : c === "#1c7ed6"
                        ? "蓝"
                        : c === "#7048e8"
                          ? "紫"
                          : "灰"}
            </option>
          ))}
        </select>
        {btn(
          "HL",
          on(() => e.chain().focus().toggleHighlight().run()),
          { title: "高亮", active: is("highlight") }
        )}
        <span className="tb-sep" />
        {btn(
          "H1",
          on(() => e.chain().focus().toggleHeading({ level: 1 }).run()),
          { title: "标题1", active: is("heading", { level: 1 }) }
        )}
        {btn(
          "H2",
          on(() => e.chain().focus().toggleHeading({ level: 2 }).run()),
          { title: "标题2", active: is("heading", { level: 2 }) }
        )}
        {btn(
          "H3",
          on(() => e.chain().focus().toggleHeading({ level: 3 }).run()),
          { title: "标题3", active: is("heading", { level: 3 }) }
        )}
        <span className="tb-sep" />
        {btn(
          "≡L",
          on(() => e.chain().focus().setTextAlign("left").run()),
          { title: "左对齐", active: e.isActive({ textAlign: "left" }) }
        )}
        {btn(
          "≡C",
          on(() => e.chain().focus().setTextAlign("center").run()),
          { title: "居中", active: e.isActive({ textAlign: "center" }) }
        )}
        {btn(
          "≡R",
          on(() => e.chain().focus().setTextAlign("right").run()),
          { title: "右对齐", active: e.isActive({ textAlign: "right" }) }
        )}
        <span className="tb-sep" />
        {btn(
          "•",
          on(() => e.chain().focus().toggleBulletList().run()),
          { title: "无序列表", active: is("bulletList") }
        )}
        {btn(
          "1.",
          on(() => e.chain().focus().toggleOrderedList().run()),
          { title: "有序列表", active: is("orderedList") }
        )}
        {btn(
          "☐",
          on(() => e.chain().focus().toggleTaskList().run()),
          { title: "任务列表", active: is("taskList") }
        )}
        {btn(
          "❝",
          on(() => e.chain().focus().toggleBlockquote().run()),
          { title: "引用", active: is("blockquote") }
        )}
        <span className="tb-sep" />
        {btn("🔗", on(setLink), { title: "链接", active: is("link") })}
        {btn(
          "图",
          on(() => {
            const u = window.prompt("图片地址 / Image URL", "https://");
            if (u) e.chain().focus().setImage({ src: u }).run();
          }),
          { title: "图片" }
        )}
        {btn(
          "⊞",
          on(() => e.chain().focus().insertTable({ rows: 3, cols: 3, withHeaderRow: true }).run()),
          { title: "插入表格 3×3", active: is("table") }
        )}
        {is("table") && (
          <>
            {btn(
              "+行",
              on(() => e.chain().focus().addRowAfter().run()),
              { title: "下方插行" }
            )}
            {btn(
              "+列",
              on(() => e.chain().focus().addColumnAfter().run()),
              { title: "右方插列" }
            )}
            {btn(
              "−行",
              on(() => e.chain().focus().deleteRow().run()),
              { title: "删行" }
            )}
            {btn(
              "−列",
              on(() => e.chain().focus().deleteColumn().run()),
              { title: "删列" }
            )}
            {btn(
              "✕表",
              on(() => e.chain().focus().deleteTable().run()),
              { title: "删表" }
            )}
          </>
        )}
        <span className="tb-sep" />
        {btn(
          "↶",
          on(() => e.chain().focus().undo().run()),
          { title: "撤销", disabled: !e.can().undo() }
        )}
        {btn(
          "↷",
          on(() => e.chain().focus().redo().run()),
          { title: "重做", disabled: !e.can().redo() }
        )}
      </div>
      <EditorContent editor={editor} className="doc-editor-scroll" />
    </div>
  );
}
