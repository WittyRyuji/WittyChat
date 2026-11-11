// Display-dim helpers for ObjImg: per-context fit caps plus a bounded
// localStorage memory for objects the server can't size yet. Deliberately
// React-free and api-free so the logic stays unit-testable under plain
// node (no localStorage there — every access is already best-effort).
// ObjImg 的显示尺寸助手：各场景适配上限 + 有界 localStorage 尺寸记忆。
// 刻意不依赖 React/api，便于在纯 node 环境单测（node 无 localStorage，
// 所有访问本就尽力而为）。

// Bounded display-size cache — one JSON map under a single localStorage
// key, capped at DIM_MAX entries (oldest evicted). Server-provided dims
// (upload sniff + backfill sweep) cover every format Go decodes; this
// only helps the rest (BMP/AVIF/HEIC/SVG…) from the second view on.
// Reads hit the in-memory map, not localStorage, after the first call.
// 有界显示尺寸缓存：单 key JSON map，超上限逐出最旧条目。Go 能解码的
// 格式都由服务端宽高覆盖（上传嗅探+后台回填）；本缓存只兜底其余格式
// （BMP/AVIF/HEIC/SVG…）的第二次查看。首次载入后只读内存 map。
const DIM_STORE = "objdim.v1";
export const DIM_MAX = 300;
let dimMap: Map<string, string> | null = null;

function dims(): Map<string, string> {
  if (dimMap) return dimMap;
  dimMap = new Map();
  try {
    const raw = JSON.parse(localStorage.getItem(DIM_STORE) ?? "{}");
    for (const [k, v] of Object.entries(raw)) dimMap.set(k, String(v));
  } catch {
    /* corrupt or unavailable — start fresh / 损坏或不可用时从零开始 */
  }
  // Sweep the pre-v1 per-image objdim:* keys — dead weight now.
  // 顺手清掉旧版逐图键。
  try {
    const dead: string[] = [];
    for (let i = 0; i < localStorage.length; i++) {
      const k = localStorage.key(i);
      if (k?.startsWith("objdim:")) dead.push(k);
    }
    for (const k of dead) localStorage.removeItem(k);
  } catch {
    /* best-effort / 尽力而为 */
  }
  return dimMap;
}

const DIM_KEY = (id: string, kind: string, ctx: string) => `${kind}:${ctx}:${id}`;

export function readDim(id: string, kind: string, ctx: string) {
  const v = dims().get(DIM_KEY(id, kind, ctx));
  const [w, h] = v?.split("x").map(Number) ?? [];
  return w > 0 && h > 0 ? { width: w, height: h } : undefined;
}

export function writeDim(id: string, kind: string, ctx: string, w: number, h: number) {
  const m = dims();
  const k = DIM_KEY(id, kind, ctx);
  const v = `${w}x${h}`;
  if (m.get(k) === v) return; // unchanged — skip the write / 值未变不落盘
  m.delete(k);
  m.set(k, v); // re-insert → newest position / 重插到最新位
  while (m.size > DIM_MAX) {
    const oldest = m.keys().next().value;
    if (oldest === undefined) break;
    m.delete(oldest);
  }
  try {
    localStorage.setItem(DIM_STORE, JSON.stringify(Object.fromEntries(m)));
  } catch {
    /* quota — cache is best-effort / 配额满则放弃 */
  }
}

// Display caps per context — MUST mirror the CSS (.att-img img 260×180,
// .atts.stk-msg 260×200, .mail-attach-img 320×240). Both thumbnail
// pipelines preserve aspect, so the ORIGINAL dims fit to the exact box
// the rendered thumb will occupy — the layout is final before any
// bytes arrive. Contexts NOT listed here (e.g. the stk picker cell) get
// no remembered/pinned box — their CSS sizes the img. / 各场景显示上限
// 必须与 CSS 同步。缩略图保比例，故用原图宽高即可算出最终显示盒——
// 首载也不再跳。未列出的场景（如 stk 表情格）不钉显示盒——由 CSS
// 决定 img 尺寸。
export const FIT: Record<string, readonly [number, number]> = {
  att: [260, 180],
  attstk: [260, 200],
  mail: [320, 240],
};

export function fitBox(
  natW: number | null | undefined,
  natH: number | null | undefined,
  ctx: string
) {
  const cap = FIT[ctx];
  if (!cap || !natW || !natH) return undefined;
  const s = Math.min(1, cap[0] / natW, cap[1] / natH);
  return { width: Math.round(natW * s), height: Math.round(natH * s) };
}
