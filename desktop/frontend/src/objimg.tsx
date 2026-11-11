// Object image rendering without session tokens in URLs: bytes are
// pulled with the Authorization header and shown through blob: URLs.
// <img src>?token=… would leak the token into access logs and any
// browser history — blob fetch keeps it in the request header only.
//
// 对象图片渲染：经 Authorization 头取字节成 blob: URL——img 直链
// ?token= 会把会话 token 写进访问日志/浏览器历史。
import { useEffect, useMemo, useState } from "react";
import { api2, authBlob } from "./api";
import { FIT, fitBox, readDim, writeDim } from "./objdims";

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
  // tick is the retry trigger AND a fetch input (cache bypass) — it's a
  // real dep now, no suppression needed. / tick 既是重试触发器也是
  // 请求输入（绕过缓存）——是真依赖，无需豁免。
  useEffect(() => {
    setUrl(null);
    let dead = false;
    let blob = "";
    const paths =
      kind === "thumb" ? [api2.thumbURL(id), api2.downloadURL(id)] : [api2.downloadURL(id)];
    (async () => {
      for (const p of paths) {
        try {
          // A user retry must bypass the private HTTP cache — a
          // cached broken response would be served straight back.
          // 用户重试必须绕过私有缓存——否则拿到的是缓存的坏响应。
          const b = await authBlob(p, tick > 0 ? { cache: "reload" } : undefined);
          if (dead) return;
          blob = URL.createObjectURL(b);
          // Decode-probe before swapping the URL in: a corrupt-but-200
          // body goes straight to the error stub instead of flashing the
          // browser's broken-image icon for a frame (first load AND
          // retry). / 换 URL 前先探活解码——坏字节直接进错误占位，
          // 绝不闪一帧浏览器裂图（首载与重试同规则）。
          try {
            const probe = new Image();
            probe.src = blob;
            await probe.decode();
          } catch {
            URL.revokeObjectURL(blob);
            blob = "";
            continue; // corrupt bytes: next path, else fail / 坏字节换下一路径
          }
          if (dead) return;
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
  natW?: number | null;
  natH?: number | null;
}) {
  const { url, failed, retry, decodeFailed } = useObjectBlob(id, kind);
  // Server dims → fitted box → remembered dims → CSS fallback stub. The
  // same box lands on the <img> too, sealing the decode-frame gap.
  // Only FIT contexts pin a box at all — e.g. the stk picker cell sizes
  // its img entirely by CSS. / 服务端宽高→适配盒→记忆尺寸→CSS 兜底；
  // img 上同样钉住，解码帧也不跳。仅 FIT 场景钉显示盒——stk 表情格
  // 的 img 尺寸完全交给 CSS。
  const box = useMemo(
    () => (FIT[ctx] ? (fitBox(natW, natH, ctx) ?? readDim(id, kind, ctx)) : undefined),
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
    return (
      // The known box overrides the 48×36 CSS minimum so a tiny image
      // doesn't jump from a too-big stub down to its real size. The
      // error stub keeps the minimum (its label must stay legible).
      // 已知显示盒覆盖 CSS 的 48×36 最小值，小图不会先占大坑再缩回
      // 真实尺寸。错误占位保留最小值（"加载失败"文字要可读）。
      <span
        className={`obj-ph${className ? ` ${className}` : ""}`}
        style={box ? { ...box, minWidth: 0, minHeight: 0 } : undefined}
        aria-hidden
      />
    );
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
        if (FIT[ctx] && im.offsetWidth > 0 && im.offsetHeight > 0) {
          writeDim(id, kind, ctx, im.offsetWidth, im.offsetHeight);
        }
      }}
    />
  );
}
