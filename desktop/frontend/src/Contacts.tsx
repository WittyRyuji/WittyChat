// Contacts view: directory of workspace users; click opens a DM.
// 通讯录视图：工作区用户列表，点人直接开私聊。
import { useEffect, useState } from "react";
import { api, type UserPublic } from "./api";

export function Contacts({ me, onOpenDM }: { me: UserPublic; onOpenDM: (u: UserPublic) => void }) {
  const [users, setUsers] = useState<UserPublic[]>([]);
  const [q, setQ] = useState("");

  useEffect(() => {
    api
      .users()
      .then((r) => setUsers(r.users))
      .catch(() => {});
  }, []);

  const list = users.filter(
    (u) =>
      u.id !== me.id &&
      (q === "" ||
        u.display_name.toLowerCase().includes(q.toLowerCase()) ||
        u.email.toLowerCase().includes(q.toLowerCase()))
  );

  return (
    <div className="pane contacts">
      <div className="pane-head">
        <h3>通讯录</h3>
        <input
          className="modal-input"
          placeholder="搜索姓名或邮箱"
          value={q}
          onChange={(e) => setQ(e.target.value)}
        />
      </div>
      <div className="contact-list">
        {list.map((u) => (
          <button type="button" key={u.id} className="contact" onClick={() => onOpenDM(u)}>
            <span className="avatar">{u.display_name.slice(0, 1)}</span>
            <span className="contact-info">
              <b>{u.display_name}</b>
              <i>{u.email}</i>
            </span>
            {u.is_admin && <span className="tag">管理员</span>}
          </button>
        ))}
        {list.length === 0 && <p className="empty">没有匹配的联系人</p>}
      </div>
    </div>
  );
}
