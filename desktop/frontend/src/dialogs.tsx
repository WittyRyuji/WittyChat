// Group/contact dialogs: new group, join public, message search, members
// panel. Kept out of Chat.tsx so the main view stays readable.
//
// 群与通讯录弹窗：建群、加公开群、消息搜索、成员面板。
import { useEffect, useState } from "react";
import {
  api,
  api2,
  type ConversationInfo,
  type MemberInfo,
  type MessageSearchHit,
  type PublicGroupInfo,
  type UserPublic,
} from "./api";
import { GroupAvatar } from "./avatar";

export function Modal({
  title,
  onClose,
  children,
}: {
  title: React.ReactNode;
  onClose: () => void;
  children: React.ReactNode;
}) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);
  return (
    <div className="modal-mask" role="presentation">
      <div className="modal">
        <div className="modal-head">
          <b>{title}</b>
          <button type="button" className="ghost" onClick={onClose}>
            ✕
          </button>
        </div>
        {children}
      </div>
    </div>
  );
}

export function NewGroupDialog({
  me,
  onClose,
  onCreated,
}: {
  me: UserPublic;
  onClose: () => void;
  onCreated: (c: ConversationInfo) => void;
}) {
  const [title, setTitle] = useState("");
  const [pub, setPub] = useState(false);
  const [contacts, setContacts] = useState<UserPublic[]>([]);
  const [picked, setPicked] = useState<Set<string>>(new Set());
  const [err, setErr] = useState("");

  useEffect(() => {
    api
      .users()
      .then((d) => setContacts(d.users.filter((u) => u.id !== me.id)))
      .catch(() => {});
  }, [me.id]);

  const create = async () => {
    if (!title.trim()) return;
    try {
      const c = await api.createGroup(title.trim(), pub, [...picked]);
      onCreated(c);
      onClose();
    } catch (e) {
      setErr(e instanceof Error ? e.message : "failed");
    }
  };

  return (
    <Modal title="新建群聊" onClose={onClose}>
      <input
        className="modal-input"
        placeholder="群名称"
        value={title}
        onChange={(e) => setTitle(e.target.value)}
      />
      <label className="modal-row">
        <input type="checkbox" checked={pub} onChange={(e) => setPub(e.target.checked)} />
        公开群（可被搜索加入）
      </label>
      <div className="modal-list">
        {contacts.map((u) => (
          <label key={u.id} className="modal-row">
            <input
              type="checkbox"
              checked={picked.has(u.id)}
              onChange={(e) => {
                const s = new Set(picked);
                if (e.target.checked) s.add(u.id);
                else s.delete(u.id);
                setPicked(s);
              }}
            />
            {u.display_name} <span className="dim">{u.email}</span>
          </label>
        ))}
      </div>
      {err && <p className="modal-err">{err}</p>}
      <button type="button" className="primary" onClick={create} disabled={!title.trim()}>
        创建
      </button>
    </Modal>
  );
}

export function JoinGroupDialog({
  onClose,
  onJoined,
}: {
  onClose: () => void;
  onJoined: () => void;
}) {
  const [q, setQ] = useState("");
  const [results, setResults] = useState<PublicGroupInfo[]>([]);
  const [err, setErr] = useState("");

  const search = async (v: string) => {
    try {
      const d = await api.searchGroups(v);
      setResults(d.groups);
    } catch {
      /* transient */
    }
  };

  // biome-ignore lint/correctness/useExhaustiveDependencies: debounced search on q only
  useEffect(() => {
    const t = setTimeout(() => search(q), 200);
    return () => clearTimeout(t);
  }, [q]);

  const join = async (id: string) => {
    try {
      await api.joinGroup(id);
      onJoined();
      setResults((r) => r.map((g) => (g.id === id ? { ...g, joined: true } : g)));
    } catch (e) {
      setErr(e instanceof Error ? e.message : "join failed");
    }
  };

  return (
    <Modal title="加入公开群" onClose={onClose}>
      <input
        className="modal-input"
        placeholder="搜索群名称…"
        value={q}
        onChange={(e) => setQ(e.target.value)}
      />
      <div className="modal-list">
        {results.map((g) => (
          <div key={g.id} className="modal-row space-between">
            <span className="grow-name">
              <GroupAvatar className="avatar sm" id={g.id} title={g.title} /> {g.title}{" "}
              <span className="dim">{g.member_count}人</span>
            </span>
            {g.joined ? (
              <span className="dim">已加入</span>
            ) : (
              <button type="button" className="ghost" onClick={() => join(g.id)}>
                加入
              </button>
            )}
          </div>
        ))}
        {results.length === 0 && <p className="dim">没有匹配的公开群</p>}
      </div>
      {err && <p className="modal-err">{err}</p>}
    </Modal>
  );
}

export function SearchDialog({
  onClose,
  onPick,
  initialQ = "",
}: {
  onClose: () => void;
  onPick: (convId: string) => void;
  initialQ?: string;
}) {
  const [q, setQ] = useState(initialQ);
  const [hits, setHits] = useState<MessageSearchHit[]>([]);

  useEffect(() => {
    if (!q.trim()) {
      setHits([]);
      return;
    }
    const t = setTimeout(() => {
      api
        .searchMessages(q)
        .then((d) => setHits(d.hits))
        .catch(() => {});
    }, 250);
    return () => clearTimeout(t);
  }, [q]);

  return (
    <Modal title="搜索消息" onClose={onClose}>
      <input
        className="modal-input"
        placeholder="搜索所有会话…"
        value={q}
        onChange={(e) => setQ(e.target.value)}
      />
      <div className="modal-list">
        {hits.map((h) => (
          <button
            type="button"
            key={`${h.conv_id}:${h.op_seq}`}
            className="modal-hit"
            onClick={() => {
              onPick(h.conv_id);
              onClose();
            }}
          >
            <span className="dim">
              {h.conv_title || "私聊"} · {h.actor_name}
            </span>
            <span className="hit-body">{h.body.slice(0, 120)}</span>
          </button>
        ))}
        {q && hits.length === 0 && <p className="dim">无匹配结果</p>}
      </div>
    </Modal>
  );
}

export function MembersPanel({
  conv,
  members,
  me,
  contacts,
  onClose,
  onChanged,
}: {
  conv: ConversationInfo;
  members: MemberInfo[];
  me: UserPublic;
  contacts: UserPublic[];
  onClose: () => void;
  onChanged: () => void;
}) {
  const myRole = members.find((m) => m.user_id === me.id)?.role ?? "member";
  const canManage = myRole === "owner" || myRole === "admin";
  const [renaming, setRenaming] = useState(false);
  const [newTitle, setNewTitle] = useState(conv.title ?? "");
  const [adding, setAdding] = useState(false);
  const [err, setErr] = useState("");
  const memberIds = new Set(members.map((m) => m.user_id));

  const act = (fn: () => Promise<unknown>) => async () => {
    try {
      await fn();
      onChanged();
    } catch (e) {
      setErr(e instanceof Error ? e.message : "failed");
    }
  };

  return (
    <Modal
      title={
        <span className="grow-name">
          <GroupAvatar
            className="avatar sm"
            id={conv.id}
            title={conv.title || "群"}
            avatarId={conv.avatar_object_id}
          />{" "}
          成员管理 · {conv.title || "群聊"}
        </span>
      }
      onClose={onClose}
    >
      {canManage && (
        <div className="modal-row space-between">
          {renaming ? (
            <>
              <input
                className="modal-input slim"
                value={newTitle}
                onChange={(e) => setNewTitle(e.target.value)}
              />
              <button
                type="button"
                className="ghost"
                onClick={act(async () => {
                  await api.renameConv(conv.id, newTitle.trim());
                  setRenaming(false);
                })}
              >
                保存
              </button>
            </>
          ) : (
            <button type="button" className="ghost" onClick={() => setRenaming(true)}>
              修改群名
            </button>
          )}
          <button
            type="button"
            className="ghost"
            onClick={() => {
              const input = document.createElement("input");
              input.type = "file";
              input.accept = "image/png,image/jpeg,image/gif,image/webp";
              input.onchange = () => {
                const f = input.files?.[0];
                if (!f) return;
                void act(async () => {
                  const o = await api2.uploadObject(f, "avatar");
                  await api.setGroupAvatar(conv.id, o.id);
                })();
              };
              input.click();
            }}
          >
            更换群头像
          </button>
          <button type="button" className="ghost" onClick={() => setAdding(!adding)}>
            + 拉人
          </button>
        </div>
      )}
      {adding && (
        <div className="modal-list bordered">
          {contacts
            .filter((u) => !memberIds.has(u.id))
            .map((u) => (
              <button
                type="button"
                key={u.id}
                className="modal-row"
                onClick={act(() => api.addMembers(conv.id, [u.id]))}
              >
                + {u.display_name}
              </button>
            ))}
        </div>
      )}
      <div className="modal-list">
        {members.map((m) => (
          <div key={m.user_id} className="modal-row space-between">
            <span>
              {m.name}
              {m.role !== "member" && <em className="role-tag">{m.role}</em>}
            </span>
            {canManage && m.user_id !== me.id && (
              <span className="member-ops">
                {myRole === "owner" && (
                  <>
                    <button
                      type="button"
                      className="ghost"
                      onClick={act(() =>
                        api.setRole(conv.id, m.user_id, m.role === "admin" ? "member" : "admin")
                      )}
                    >
                      {m.role === "admin" ? "取消管理" : "设为管理"}
                    </button>
                    <button
                      type="button"
                      className="ghost"
                      onClick={act(() => api.transferConv(conv.id, m.user_id))}
                    >
                      转让群主
                    </button>
                  </>
                )}
                <button
                  type="button"
                  className="ghost danger"
                  onClick={act(() => api.removeMember(conv.id, m.user_id))}
                >
                  移出
                </button>
              </span>
            )}
          </div>
        ))}
      </div>
      {err && <p className="modal-err">{err}</p>}
    </Modal>
  );
}
