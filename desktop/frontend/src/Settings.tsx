// Settings page (bottom gear): account card + workspace settings + logout.
// 设定页（底部齿轮）：账号卡片 + 工作区设置展示 + 登出。
import { useEffect, useState } from "react";
import { api, setToken, type UserPublic } from "./api";

interface WsSettings {
  im: {
    message_max_chars: number;
    recall_timeout_sec: number;
    read_receipts: boolean;
    at_all: string;
  };
}

export function Settings({ me, onLogout }: { me: UserPublic; onLogout: () => void }) {
  const [s, setS] = useState<WsSettings | null>(null);

  useEffect(() => {
    api
      .settings()
      .then(setS)
      .catch(() => {});
  }, []);

  return (
    <div className="pane settings">
      <h3>设定</h3>
      <section className="set-card">
        <h4>我的账号</h4>
        <div className="set-row">
          <span className="avatar lg">{me.display_name.slice(0, 1)}</span>
          <div>
            <b>{me.display_name}</b>
            <p className="dim">{me.email}</p>
          </div>
        </div>
        <div className="set-row">
          <span>角色</span>
          <b>{me.is_admin ? "管理员" : "成员"}</b>
        </div>
        {me.is_admin && (
          <div className="set-row">
            <span>管理后台</span>
            <a href="/admin/" target="_blank" rel="noreferrer">
              打开 admin/
            </a>
          </div>
        )}
      </section>

      {s && (
        <section className="set-card">
          <h4>工作区策略</h4>
          <div className="set-row">
            <span>消息长度上限</span>
            <b>{s.im.message_max_chars} 字</b>
          </div>
          <div className="set-row">
            <span>撤回时限</span>
            <b>{s.im.recall_timeout_sec}s</b>
          </div>
          <div className="set-row">
            <span>已读回执</span>
            <b>{s.im.read_receipts ? "开启" : "关闭"}</b>
          </div>
          <div className="set-row">
            <span>@所有人 权限</span>
            <b>{s.im.at_all === "all" ? "全员" : "仅管理员"}</b>
          </div>
        </section>
      )}

      <section className="set-card">
        <button
          type="button"
          className="ghost danger"
          onClick={() => {
            api.logout().catch(() => {});
            setToken("");
            onLogout();
          }}
        >
          退出登录
        </button>
      </section>
    </div>
  );
}
