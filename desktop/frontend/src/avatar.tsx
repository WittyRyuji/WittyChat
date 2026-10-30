// Avatar: renders user's uploaded avatar image, falls back to the
// display-name initial. Object URLs are cached per avatar_object_id.
//
// 头像组件：有上传头像显示图片，否则显示昵称首字。
// 按 avatar_object_id 缓存 objectURL。
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
  const oid = user.avatar_object_id;
  const [url, setUrl] = useState(oid ? cache.get(oid) : undefined);

  useEffect(() => {
    if (!oid) {
      setUrl(undefined);
      return;
    }
    const hit = cache.get(oid);
    if (hit) {
      setUrl(hit);
      return;
    }
    let dead = false;
    fetch(api2.avatarURL(user.id), { headers: { Authorization: `Bearer ${getToken()}` } })
      .then((r) => (r.ok ? r.blob() : Promise.reject(new Error(String(r.status)))))
      .then((b) => {
        const u = URL.createObjectURL(b);
        cache.set(oid, u);
        if (!dead) setUrl(u);
      })
      .catch(() => {});
    return () => {
      dead = true;
    };
  }, [user.id, oid]);

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
