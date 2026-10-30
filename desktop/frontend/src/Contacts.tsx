// Contacts view: fixed list column (search + users + group actions)
// + right detail pane with "发消息". Group actions live here.
//
// 通讯录：固定列（搜索+用户+建群入口）+ 右侧详情。+群/公开群在此。
import { useEffect, useState } from "react";
import { api, type UserPublic } from "./api";
import { IconPlus, IconSearch } from "./icons";

export function Contacts({
  me,
  onOpenDM,
  onNewGroup,
  onJoin,
}: {
  me: UserPublic;
  onOpenDM: (u: UserPublic) => void;
  onNewGroup: () => void;
  onJoin: () => void;
}) {
  const [users, setUsers] = useState<UserPublic[]>([]);
  const [q, setQ] = useState("");
  const [sel, setSel] = useState<UserPublic | null>(null);

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
    <div className="chat">
      <aside className="sidebar">
        <div className="side-head">
          <span className="me">通讯录</span>
          <span className="head-actions">
            <button type="button" className="ghost" title="新建群聊" onClick={onNewGroup}>
              <IconPlus size={16} /> 群
            </button>
            <button type="button" className="ghost" title="加入公开群" onClick={onJoin}>
              公开群
            </button>
          </span>
        </div>
        <div className="conv-search">
          <IconSearch size={16} />
          <input placeholder="搜索姓名或邮箱" value={q} onChange={(e) => setQ(e.target.value)} />
        </div>
        <div className="conv-list">
          {list.map((u) => (
            <button
              type="button"
              key={u.id}
              className={`conv contact-row ${sel?.id === u.id ? "active" : ""}`}
              onClick={() => setSel(u)}
            >
              <span className="avatar sm">{u.display_name.slice(0, 1)}</span>
              <span className="conv-title">{u.display_name}</span>
              {u.is_admin && <span className="tag">管理员</span>}
            </button>
          ))}
          {list.length === 0 && <p className="empty">没有匹配的联系人</p>}
        </div>
      </aside>

      <section className="main">
        {!sel ? (
          <div className="placeholder">选择联系人查看详情</div>
        ) : (
          <div className="pane contact-detail">
            <span className="avatar lg">{sel.display_name.slice(0, 1)}</span>
            <h3>{sel.display_name}</h3>
            <p className="dim">{sel.email}</p>
            <button type="button" className="primary" onClick={() => onOpenDM(sel)}>
              发消息
            </button>
          </div>
        )}
      </section>
    </div>
  );
}
