// Object image rendering without session tokens in URLs: bytes are
// pulled with the Authorization header and shown through blob: URLs.
// <img src>?token=… would leak the token into access logs and any
// browser history — blob fetch keeps it in the request header only.
//
// 对象图片渲染：经 Authorization 头取字节成 blob: URL——img 直链
// ?token= 会把会话 token 写进访问日志/浏览器历史。
import { useEffect, useState } from "react";
import { api2, authBlob } from "./api";

// useObjectBlob resolves an object to a blob: URL (thumb first, full
// bytes as fallback for not-yet-generated thumbnails). Revoked on
// unmount or when id/kind changes. retry() refetches after failure.
// thumb 未生成时回退完整字节；retry() 失败后重取。
export function useObjectBlob(
  id: string,
  kind: "thumb" | "full" = "thumb"
): { url: string | null; failed: boolean; retry: () => void } {
  const [url, setUrl] = useState<string | null>(null);
  const [failed, setFailed] = useState(false);
  const [tick, setTick] = useState(0);
  // biome-ignore lint/correctness/useExhaustiveDependencies: tick is the retry trigger — bumping it re-runs the fetch / tick 是重试触发器，变化即重取
  useEffect(() => {
    setUrl(null);
    setFailed(false);
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
          return;
        } catch {
          /* next candidate / 下一路径重试 */
        }
      }
      if (!dead) setFailed(true);
    })();
    return () => {
      dead = true;
      if (blob) URL.revokeObjectURL(blob);
    };
  }, [id, kind, tick]);
  return { url, failed, retry: () => setTick((t) => t + 1) };
}

export function ObjImg({
  id,
  kind = "thumb",
  className,
  alt,
  title,
}: {
  id: string;
  kind?: "thumb" | "full";
  className?: string;
  alt?: string;
  title?: string;
}) {
  const { url, failed, retry } = useObjectBlob(id, kind);
  if (failed)
    return (
      // biome-ignore lint/a11y/noStaticElementInteractions lint/a11y/useKeyWithClickEvents: rendered inside interactive wrappers (.att-img/.stk-img) where a real <button> is invalid HTML; pointer-only retry, keyboard users still have the parent control / 嵌在按钮内不能用真 button，点击重试
      <span
        className={`obj-ph obj-ph-err${className ? ` ${className}` : ""}`}
        title={`${alt ?? "图片"} 加载失败 · 点击重试`}
        onClick={(e) => {
          e.stopPropagation();
          retry();
        }}
      >
        加载失败
      </span>
    );
  if (!url) return <span className={`obj-ph${className ? ` ${className}` : ""}`} aria-hidden />;
  return <img src={url} className={className} alt={alt ?? ""} title={title} loading="lazy" />;
}
