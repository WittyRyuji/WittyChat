import { type FormEvent, useState } from "react";
import { api, setToken, type UserPublic } from "./api";

export function Login({ onLogin }: { onLogin: (u: UserPublic) => void }) {
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [totp, setTotp] = useState("");
  const [needTotp, setNeedTotp] = useState(false);
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setErr("");
    try {
      const r = await api.login(email, password);
      if (r.requires_totp) {
        setNeedTotp(true);
        return;
      }
      setToken(r.session_token);
      onLogin(r.user);
    } catch (ex) {
      setErr(ex instanceof Error ? ex.message : "login failed");
    } finally {
      setBusy(false);
    }
  };

  const submitTotp = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setErr("");
    try {
      const res = await fetch("/api/v1/auth/login", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          email,
          password,
          totp,
          device_name: "web",
          device_kind: "web",
        }),
      });
      const d = await res.json();
      if (!res.ok) throw new Error(d.detail ?? "bad code");
      setToken(d.session_token);
      onLogin(d.user);
    } catch (ex) {
      setErr(ex instanceof Error ? ex.message : "failed");
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="login-wrap">
      <form className="login-card" onSubmit={needTotp ? submitTotp : submit}>
        <h1>WittyChat</h1>
        <p className="sub">{needTotp ? "输入二次验证码" : "登录你的工作区"}</p>
        {err && <p className="err">{err}</p>}
        {needTotp ? (
          <input
            value={totp}
            onChange={(e) => setTotp(e.target.value)}
            placeholder="6 位验证码"
            maxLength={6}
          />
        ) : (
          <>
            <input
              type="email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              placeholder="邮箱"
              required
            />
            <input
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder="密码"
              required
            />
          </>
        )}
        <button type="submit" disabled={busy}>
          {busy ? "…" : needTotp ? "验证" : "登录"}
        </button>
      </form>
    </div>
  );
}
