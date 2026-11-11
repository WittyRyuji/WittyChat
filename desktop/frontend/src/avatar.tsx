// Avatar: renders user's uploaded avatar image, falls back to the
// display-name initial. Works with or without avatar_object_id — when the
// id is unknown (chat rows only carry user_id) it still fetches
// /users/{id}/avatar. Requests are shared per identity/version/session and
// failures cached for 30s; successful blob URLs keep GIFs animating.
//
// 头像组件：有上传头像显示图片，否则显示昵称首字。没有 avatar_object_id
// 时按 user.id 取；按身份/版本/会话合并请求，失败缓存 30 秒，成功 blob URL 稳定复用。
import { useEffect, useState } from "react";
import { API_BASE, api2, authBlob, getToken, type UserPublic } from "./api";
import { AVATAR_FAILURE_TTL, AvatarCache } from "./avatarcache";

const cache = new AvatarCache();

function useAvatar(path: string) {
  const key = path ? `${API_BASE}|${getToken()}|${path}` : "";
  const [image, setImage] = useState({ key, url: key ? (cache.get(key) ?? "") : "" });
  useEffect(() => {
    if (!key) return;
    let dead = false;
    let retry: ReturnType<typeof setTimeout>;
    const load = () => {
      cache
        .load(key, () => authBlob(path))
        .then((url) => {
          if (dead) return;
          setImage({ key, url });
          if (!url) retry = setTimeout(load, AVATAR_FAILURE_TTL);
        });
    };
    load();
    return () => {
      dead = true;
      clearTimeout(retry);
    };
  }, [key, path]);
  return image.key === key ? image.url : key ? (cache.get(key) ?? "") : "";
}

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
  const url = useAvatar(user.id ? api2.avatarURL(user.id, user.avatar_object_id) : "");

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
  const url = useAvatar(avatarId ? api2.groupAvatarPath(id, avatarId) : "");
  if (url) return <img className={`${className} avatar-img`} src={url} alt="" />;
  let h = 0;
  for (const ch of id) h = (h * 31 + ch.charCodeAt(0)) >>> 0;
  return (
    <span className={className} style={{ background: GROUP_COLORS[h % GROUP_COLORS.length] }}>
      {(title || "群").slice(0, 1)}
    </span>
  );
}
