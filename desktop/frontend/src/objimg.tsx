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
// unmount or when id/kind changes. thumb 未生成时回退完整字节。
export function useObjectBlob(id: string, kind: "thumb" | "full" = "thumb"): string | null {
  const [url, setUrl] = useState<string | null>(null);
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
          return;
        } catch {
          /* next candidate / 下一路径重试 */
        }
      }
    })();
    return () => {
      dead = true;
      if (blob) URL.revokeObjectURL(blob);
    };
  }, [id, kind]);
  return url;
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
  const url = useObjectBlob(id, kind);
  if (!url) return <span className={className} aria-hidden />; // layout stub while loading
  return <img src={url} className={className} alt={alt ?? ""} title={title} loading="lazy" />;
}
