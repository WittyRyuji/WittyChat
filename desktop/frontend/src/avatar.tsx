// Avatar: renders user's uploaded avatar image, falls back to the
// display-name initial. Works with or without avatar_object_id — when the
// id is unknown (chat rows only carry user_id) it still fetches
// /users/{id}/avatar once and negative-caches 404s. Object URLs are cached
// per avatar_object_id ("u:"+userId otherwise) so GIF blobs keep animating.
//
// 头像组件：有上传头像显示图片，否则显示昵称首字。没有 avatar_object_id
// 时按 user.id 直接取一次，404 负缓存；blob URL 按对象 id 缓存。
import { useEffect, useState } from "react";
import { api2, getToken, type UserPublic } from "./api";

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
    fetch(api2.avatarURL(user.id), { headers: { Authorization: `Bearer ${getToken()}` } })
      .then((r) => (r.ok ? r.blob() : Promise.reject(new Error(String(r.status)))))
      .then((b) => {
        const u = URL.createObjectURL(b);
        cache.set(key, u);
        if (!dead) setUrl(u);
      })
      .catch(() => {
        cache.set(key, ""); // 负缓存：无头像不再重复请求
        if (!dead) setUrl("");
      });
    return () => {
      dead = true;
    };
  }, [user.id, key]);

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
