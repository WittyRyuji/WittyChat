// Settings page: left sub-nav column (账号/通用/助理/关于) + full-width
// content. Local prefs persist in localStorage; workspace policy is
// read-only display (admin edits via /admin/).
//
// 设定页：左列子导航 + 右侧全宽内容。个人偏好存 localStorage；
// 工作区策略只读（管理员经 /admin/ 改）。
import { useEffect, useRef, useState } from "react";
import { api, api2, setToken, type UserPublic } from "./api";
import { Avatar } from "./avatar";

interface WsSettings {
  workspace: { name: string };
  im: {
    message_max_chars: number;
    recall_timeout_sec: number;
    read_receipts: boolean;
    at_all: string;
  };
}

interface Assistant {
  id: string;
  name: string;
  runtime: string;
  adapter: string;
  model: string;
  auto_send: boolean;
  status: string;
}

// localStorage-backed personal preference (placeholder until server prefs land)
function useLocalToggle(key: string, def: boolean): [boolean, (v: boolean) => void] {
  const [v, setV] = useState(() => (localStorage.getItem(`pref.${key}`) !== "0" ? def : false));
  return [
    v,
    (nv) => {
      setV(nv);
      localStorage.setItem(`pref.${key}`, nv ? "1" : "0");
    },
  ];
}

function Toggle({
  on,
  onChange,
  label,
  hint,
}: {
  on: boolean;
  onChange: (v: boolean) => void;
  label: string;
  hint?: string;
}) {
  return (
    <div className="set-row">
      <span className="lbl">
        {label}
        {hint && <i className="dim"> {hint}</i>}
      </span>
      <button
        type="button"
        className={`switch ${on ? "on" : ""}`}
        role="switch"
        aria-checked={on}
        onClick={() => onChange(!on)}
      >
        <i />
      </button>
    </div>
  );
}

export function Settings({
  me,
  onLogout,
  onMeUpdated,
}: {
  me: UserPublic;
  onLogout: () => void;
  onMeUpdated: () => void;
}) {
  const [tab, setTab] = useState<"account" | "general" | "agent" | "about">("account");
  const [s, setS] = useState<WsSettings | null>(null);
  const [assistants, setAssistants] = useState<Assistant[]>([]);
  const [notify, setNotify] = useLocalToggle("notify", true);
  const [name, setName] = useState(me.display_name);
  const [saving, setSaving] = useState(false);
  const [profileErr, setProfileErr] = useState("");
  const avatarFile = useRef<HTMLInputElement>(null);
  const [sendRead, setSendRead] = useLocalToggle("send_read", true);
  const [friendVerify, setFriendVerify] = useLocalToggle("friend_verify", true);

  useEffect(() => {
    api
      .settings()
      .then(setS)
      .catch(() => {});
    api
      .assistants()
      .then((r) => setAssistants(r.assistants ?? []))
      .catch(() => {});
  }, []);

  const tabs = [
    { key: "account", name: "账号" },
    { key: "general", name: "通用" },
    { key: "agent", name: "助理" },
    { key: "about", name: "关于" },
  ] as const;

  return (
    <div className="chat">
      <aside className="sidebar">
        <div className="side-head">
          <span className="me">设定</span>
        </div>
        <div className="conv-list">
          {tabs.map((t) => (
            <button
              type="button"
              key={t.key}
              className={`conv ${tab === t.key ? "active" : ""}`}
              onClick={() => setTab(t.key)}
            >
              <span className="conv-title">{t.name}</span>
            </button>
          ))}
        </div>
      </aside>

      <section className="main settings-body">
        {tab === "account" && (
          <>
            <section className="set-card">
              <h4>我的账号</h4>
              <div className="set-row">
                <Avatar
                  user={me}
                  className="avatar lg clickable"
                  title="点击更换头像"
                  onClick={() => avatarFile.current?.click()}
                />
                <input
                  ref={avatarFile}
                  type="file"
                  accept="image/*"
                  hidden
                  onChange={async (e) => {
                    const f = e.target.files?.[0];
                    e.target.value = "";
                    if (!f) return;
                    try {
                      const obj = await api2.uploadObject(f, "avatar");
                      await api.updateMe({ avatar_object_id: obj.id });
                      onMeUpdated();
                    } catch (err) {
                      setProfileErr(`头像上传失败：${err}`);
                    }
                  }}
                />
                <span className="lbl">
                  <b>{me.display_name}</b>
                  <p className="dim">{me.email}</p>
                </span>
              </div>
              <div className="set-row">
                <span className="lbl">昵称</span>
                <input
                  className="set-input"
                  value={name}
                  maxLength={64}
                  onChange={(e) => setName(e.target.value)}
                />
                <button
                  type="button"
                  className="primary sm"
                  disabled={!name.trim() || name.trim() === me.display_name || saving}
                  onClick={async () => {
                    setSaving(true);
                    setProfileErr("");
                    try {
                      await api.updateMe({ display_name: name.trim() });
                      onMeUpdated();
                    } catch (err) {
                      setProfileErr(`改名失败：${err}`);
                    } finally {
                      setSaving(false);
                    }
                  }}
                >
                  {saving ? "保存中…" : "保存"}
                </button>
              </div>
              {profileErr && <div className="errbar">{profileErr}</div>}
              <div className="set-row">
                <span className="lbl">角色</span>
                <b>{me.is_admin ? "管理员" : "成员"}</b>
              </div>
              <div className="set-row">
                <span className="lbl">两步验证</span>
                <b className="dim">TOTP（启用流程见管理后台）</b>
              </div>
              {me.is_admin && (
                <div className="set-row">
                  <span className="lbl">管理后台</span>
                  <a href="/admin/" target="_blank" rel="noreferrer">
                    打开 admin/
                  </a>
                </div>
              )}
            </section>
            {s && (
              <section className="set-card">
                <h4>工作区策略（管理员配置）</h4>
                <div className="set-row">
                  <span className="lbl">消息长度上限</span>
                  <b>{s.im.message_max_chars} 字</b>
                </div>
                <div className="set-row">
                  <span className="lbl">撤回时限</span>
                  <b>{s.im.recall_timeout_sec}s</b>
                </div>
                <div className="set-row">
                  <span className="lbl">已读回执</span>
                  <b>{s.im.read_receipts ? "开启" : "关闭"}</b>
                </div>
                <div className="set-row">
                  <span className="lbl">@所有人 权限</span>
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
          </>
        )}

        {tab === "general" && (
          <>
            <section className="set-card">
              <h4>通知</h4>
              <Toggle on={notify} onChange={setNotify} label="桌面通知" hint="新消息与@提及" />
              <div className="set-row">
                <span className="lbl">通知声音</span>
                <b className="dim">跟随系统（P1）</b>
              </div>
            </section>
            <section className="set-card">
              <h4>隐私</h4>
              <Toggle
                on={sendRead}
                onChange={setSendRead}
                label="发送已读回执"
                hint="关闭后不向对方显示已读"
              />
              <Toggle
                on={friendVerify}
                onChange={setFriendVerify}
                label="加我需要验证"
                hint="好友与私聊发起"
              />
              <div className="set-row">
                <span className="lbl">谁能搜到我</span>
                <b className="dim">工作区成员（P1 细化）</b>
              </div>
            </section>
            <section className="set-card">
              <h4>外观</h4>
              <div className="set-row">
                <span className="lbl">主题</span>
                <b className="dim">深色（浅色 P1）</b>
              </div>
              <div className="set-row">
                <span className="lbl">语言</span>
                <b className="dim">简体中文</b>
              </div>
            </section>
          </>
        )}

        {tab === "agent" && (
          <>
            <section className="set-card">
              <h4>我的助理</h4>
              {assistants.length === 0 ? (
                <p className="dim">
                  还没有助理。助理运行时 P1 开放，届时可在此绑定 ACP/API 适配器。
                </p>
              ) : (
                assistants.map((a) => (
                  <div className="set-row" key={a.id}>
                    <span className="lbl">
                      <b>{a.name}</b>
                      <i className="dim">
                        {" "}
                        {a.runtime}/{a.adapter || "default"} {a.model}
                      </i>
                    </span>
                    <b className="dim">{a.status}</b>
                  </div>
                ))
              )}
            </section>
            <section className="set-card">
              <h4>接入与订阅</h4>
              <div className="set-row">
                <span className="lbl">运行时接入</span>
                <b className="dim">ACP 桥接（P1 开放）</b>
              </div>
              <div className="set-row">
                <span className="lbl">代发确认</span>
                <b className="dim">逐条审批（auto_send 默认关）</b>
              </div>
              <div className="set-row">
                <span className="lbl">模型额度</span>
                <b className="dim">未启用（订阅/网关 P1）</b>
              </div>
              <div className="set-row">
                <span className="lbl">能力范围 scopes</span>
                <b className="dim">im.read / mail.read …（契约已冻结）</b>
              </div>
            </section>
          </>
        )}

        {tab === "about" && (
          <section className="set-card">
            <h4>关于</h4>
            <div className="set-row">
              <span className="lbl">工作区</span>
              <b>{s?.workspace.name ?? "WittyChat"}</b>
            </div>
            <div className="set-row">
              <span className="lbl">版本</span>
              <b>P0 · dev build</b>
            </div>
            <div className="set-row">
              <span className="lbl">许可</span>
              <b>AGPL-3.0（核心）+ 商业模块可选</b>
            </div>
          </section>
        )}
      </section>
    </div>
  );
}
