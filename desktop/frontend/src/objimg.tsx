// Object image rendering without session tokens in URLs: bytes are
// pulled with the Authorization header and shown through blob: URLs.
// <img src>?token=… would leak the token into access logs and any
// browser history — blob fetch keeps it in the request header only.
//
// 对象图片渲染：经 Authorization 头取字节成 blob: URL——img 直链
// ?token= 会把会话 token 写进访问日志/浏览器历史。
import { useEffect, useMemo, useState } from "react";
import { api2, authBlob } from "./api";

// useObjectBlob resolves an object to a blob: URL (thumb first, full
// bytes as fallback for not-yet-generated thumbnails). Revoked on
// unmount or when id/kind changes. retry() refetches after failure;
// `failed` tracks WHICH object failed so a retry keeps the error box
// up until bytes actually arrive (no loading-stub flash).
// thumb 未生成时回退完整字节；retry() 失败后重取。failed 记录是哪个
// 对象失败——重试期间保持失败占位不闪成加载占位。
export function useObjectBlob(
  id: string,
  kind: "thumb" | "full" = "thumb"
): { url: string | null; failed: boolean; retry: () => void; decodeFailed: () => void } {
  const [url, setUrl] = useState<string | null>(null);
  const [failedFor, setFailedFor] = useState<string | null>(null);
  const [tick, setTick] = useState(0);
  // biome-ignore lint/correctness/useExhaustiveDependencies: tick is the retry trigger — bumping it re-runs the fetch / tick 是重试触发器，变化即重取
  useEffect(() => {
    setUrl(null);
    let dead = false;
    let blob = "";
    const paths =
      kind === "thumb" ? [api2.thumbURL(id), api2.downloadURL(id)] : [api2.downloadURL(id)];
    (async () => {
      for (const p of paths) {
        try {
          const b = await authBlob(p);
          if (dead) return;
          blob = URL.createObjectURL(b);
          setUrl(blob);
          setFailedFor(null);
          return;
        } catch {
          /* next candidate / 下一路径重试 */
        }
      }
      if (!dead) setFailedFor(`${id}:${kind}`);
    })();
    return () => {
      dead = true;
      if (blob) URL.revokeObjectURL(blob);
    };
  }, [id, kind, tick]);
  return {
    url,
    failed: failedFor === `${id}:${kind}`,
    retry: () => setTick((t) => t + 1),
    // img onError — bytes fetched but undecodable (corrupt upload).
    // img onError——字节取回但解码失败（损坏的上传）。
    decodeFailed: () => setFailedFor(`${id}:${kind}`),
  };
}

// Bounded display-size cache — one JSON map under a single localStorage
// key, capped at DIM_MAX entries (oldest evicted). Server-provided dims
// cover new uploads; this still helps objects predating the
// objects.width/height columns. Reads hit the in-memory map, not
// localStorage, after the first call. / 有界显示尺寸缓存：单 key JSON
// map，超上限逐出最旧条目。新上传靠服务端宽高，本缓存兜底旧对象；
// 首次载入后只读内存 map。
const DIM_STORE = "objdim.v1";
const DIM_MAX = 300;
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

function readDim(id: string, kind: string, ctx: string) {
  const v = dims().get(DIM_KEY(id, kind, ctx));
  const [w, h] = v?.split("x").map(Number) ?? [];
  return w > 0 && h > 0 ? { width: w, height: h } : undefined;
}

function writeDim(id: string, kind: string, ctx: string, w: number, h: number) {
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
// bytes arrive. / 各场景显示上限必须与 CSS 同步。缩略图保比例，故用
// 原图宽高即可算出最终显示盒——首载也不再跳。
const FIT: Record<string, readonly [number, number]> = {
  att: [260, 180],
  attstk: [260, 200],
  mail: [320, 240],
};

function fitBox(natW: number | undefined, natH: number | undefined, ctx: string) {
  const cap = FIT[ctx];
  if (!cap || !natW || !natH) return undefined;
  const s = Math.min(1, cap[0] / natW, cap[1] / natH);
  return { width: Math.round(natW * s), height: Math.round(natH * s) };
}

export function ObjImg({
  id,
  kind = "thumb",
  className,
  alt,
  title,
  ctx = "",
  natW,
  natH,
}: {
  id: string;
  kind?: "thumb" | "full";
  className?: string;
  alt?: string;
  title?: string;
  ctx?: string;
  natW?: number;
  natH?: number;
}) {
  const { url, failed, retry, decodeFailed } = useObjectBlob(id, kind);
  // Server dims → fitted box → remembered dims → CSS fallback stub. The
  // same box lands on the <img> too, sealing the decode-frame gap.
  // 服务端宽高→适配盒→记忆尺寸→CSS 兜底；img 上同样钉住，解码帧也不跳。
  const box = useMemo(
    () => fitBox(natW, natH, ctx) ?? readDim(id, kind, ctx),
    [id, kind, ctx, natW, natH]
  );
  if (failed)
    return (
      // biome-ignore lint/a11y/noStaticElementInteractions lint/a11y/useKeyWithClickEvents: chat/sticker render inside interactive controls where a nested <button> is invalid HTML — retry is pointer-only there; in mail the attachment's own download link is the keyboard path / 嵌在按钮内不能用真 button——聊天/贴纸场景重试仅鼠标；邮件场景键盘用户用附件下载链接
      <span
        className={`obj-ph obj-ph-err${className ? ` ${className}` : ""}`}
        style={box}
        title={`${alt ?? "图片"} 加载失败 · 点击重试`}
        onClick={(e) => {
          e.stopPropagation();
          retry();
        }}
      >
        加载失败
      </span>
    );
  if (!url)
    return <span className={`obj-ph${className ? ` ${className}` : ""}`} style={box} aria-hidden />;
  return (
    <img
      src={url}
      className={className}
      alt={alt ?? ""}
      title={title}
      loading="lazy"
      style={box}
      onError={decodeFailed}
      onLoad={(e) => {
        const im = e.currentTarget;
        if (im.offsetWidth > 0 && im.offsetHeight > 0) {
          writeDim(id, kind, ctx, im.offsetWidth, im.offsetHeight);
        }
      }}
    />
  );
}
