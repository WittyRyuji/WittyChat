// Minimal safe markdown render: marked → DOMPurify → innerHTML.
// 最小安全 Markdown 渲染：marked → DOMPurify → innerHTML。

import DOMPurify from "dompurify";
import { marked } from "marked";
import { useMemo } from "react";

marked.setOptions({ gfm: true, breaks: true });
// Links open in a new browsing context — inside the Wails shell the
// global interceptor in main.tsx routes them to the system browser.
// 链接一律新上下文打开——Wails 壳内由 main.tsx 的全局拦截交给系统浏览器。
marked.use({
  renderer: {
    link({ href, title, tokens }) {
      const text = this.parser.parseInline(tokens);
      const esc = (s: string) => s.replace(/"/g, "&quot;");
      const t = title ? ` title="${esc(title)}"` : "";
      return `<a href="${esc(href)}"${t} target="_blank" rel="noreferrer">${text}</a>`;
    },
  },
});

const PURIFY_OPTS = {
  // Chat-safe subset: no images/iframes/scripts, links open externally.
  ALLOWED_TAGS: [
    "b",
    "strong",
    "i",
    "em",
    "u",
    "s",
    "del",
    "code",
    "pre",
    "blockquote",
    "ul",
    "ol",
    "li",
    "a",
    "p",
    "br",
    "hr",
    "h1",
    "h2",
    "h3",
    "h4",
    "table",
    "thead",
    "tbody",
    "tr",
    "th",
    "td",
    "span",
  ],
  ALLOWED_ATTR: ["href", "title", "target", "rel", "class"],
  ALLOW_DATA_ATTR: false,
};

export function renderMd(src: string): string {
  const html = marked.parse(src, { async: false }) as string;
  return DOMPurify.sanitize(html, PURIFY_OPTS) as string;
}

export function Md({ text }: { text: string }) {
  const html = useMemo(() => renderMd(text), [text]);
  // biome-ignore lint/security/noDangerouslySetInnerHtml: sanitized above
  return <div className="md" dangerouslySetInnerHTML={{ __html: html }} />;
}
