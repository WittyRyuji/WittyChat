// Avatar: renders user's uploaded avatar image, falls back to the
// display-name initial. Works with or without avatar_object_id — when the
// id is unknown (chat rows only carry user_id) it still fetches
// /users/{id}/avatar once and negative-caches 404s. Object URLs are cached
// per avatar_object_id ("u:"+userId otherwise) so GIF blobs keep animating.
//
// 头像组件：有上传头像显示图片，否则显示昵称首字。没有 avatar_object_id
// 时按 user.id 直接取一次，404 负缓存；blob URL 按对象 id 缓存。
import { useEffect, useState } from "react";
import { api2, authBlob, getToken, type UserPublic } from "./api";

const cache = new Map<string, string>();

export function Avatar({
  user,
  className = "avatar",
  onClick,
  title,
}: {
  user: Pick<UserPublic, "id" | "display_name" | "avatar_object_id">;
  className?: string;
  onClick?: () => void;
  title?: string;
}) {
  const key = user.avatar_object_id || `u:${user.id}`;
  const [url, setUrl] = useState(user.id ? cache.get(key) : "");

  useEffect(() => {
    if (!user.id) {
      setUrl("");
      return;
    }
    const hit = cache.get(key);
    if (hit !== undefined) {
      setUrl(hit);
      return;
    }
    let dead = false;
    fetch(api2.avatarURL(user.id, user.avatar_object_id), {
      headers: { Authorization: `Bearer ${getToken()}` },
    })
      .then((r) => {
        if (r.ok) return r.blob();
        // Only an unversioned 404 means "no avatar". A transient failure or
        // an outdated version must not poison the new image's cache.
        // 仅无版本请求的 404 可负缓存；瞬时失败/版本过期不污染新头像缓存。
        if (r.status === 404 && !user.avatar_object_id) cache.set(key, "");
        throw new Error(String(r.status));
      })
      .then((b) => {
        const u = URL.createObjectURL(b);
        cache.set(key, u);
        if (!dead) setUrl(u);
      })
      .catch(() => {
        if (!dead) setUrl("");
      });
    return () => {
      dead = true;
    };
  }, [user.id, user.avatar_object_id, key]);

  const inner = url ? (
    <img className="avatar-img" src={url} alt="" />
  ) : (
    user.display_name.slice(0, 1)
  );
  if (onClick) {
    return (
      <button type="button" className={`${className} clickable`} onClick={onClick} title={title}>
        {inner}
      </button>
    );
  }
  if (url) {
    return <img className={`${className} avatar-img`} src={url} alt="" title={title} />;
  }
  return <span className={className}>{inner}</span>;
}

// GroupAvatar: deterministic first-char tile for conversations — the
// background is hashed from the conversation id so each group keeps one
// color across sessions and devices ("random" but stable).
//
// 群头像：取群名首字，底色按会话 id 哈希固定在色板内，跨会话/设备不变。
const GROUP_COLORS = [
  "#d1615b",
  "#d98e32",
  "#3d9a50",
  "#2f9e8f",
  "#4a8ecd",
  "#6a7fdb",
  "#9a6fd0",
  "#c05299",
];

export function GroupAvatar({
  id,
  title,
  avatarId,
  className = "avatar",
}: {
  id: string;
  title: string;
  avatarId?: string; // uploaded group avatar; falls back to the color tile / 上传的群头像，否则用色块
  className?: string;
}) {
  const key = avatarId ? `g:${avatarId}` : "";
  const [url, setUrl] = useState(key ? (cache.get(key) ?? "") : "");
  useEffect(() => {
    if (!key) {
      setUrl("");
      return;
    }
    const hit = cache.get(key);
    if (hit !== undefined) {
      setUrl(hit);
      return;
    }
    let dead = false;
    authBlob(api2.groupAvatarPath(id, avatarId))
      .then((b) => {
        const u = URL.createObjectURL(b);
        cache.set(key, u);
        if (!dead) setUrl(u);
      })
      .catch(() => {
        if (!dead) setUrl("");
      });
    return () => {
      dead = true;
    };
  }, [id, key, avatarId]);
  if (url) return <img className={`${className} avatar-img`} src={url} alt="" />;
  let h = 0;
  for (const ch of id) h = (h * 31 + ch.charCodeAt(0)) >>> 0;
  return (
    <span className={className} style={{ background: GROUP_COLORS[h % GROUP_COLORS.length] }}>
      {(title || "群").slice(0, 1)}
    </span>
  );
}
