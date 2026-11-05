package httpapi

// Server-rendered activation page: the only browser-facing page in P0.
// Employee opens the one-time link, sets the initial password, done.
//
// 服务端渲染的激活页：P0 唯一的浏览器页面。
// 员工打开一次性链接 → 设置初始密码 → 完成。

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"wittychat/core/audit"
	"wittychat/core/auth"
	"wittychat/core/settings"
	"wittychat/server/adminui"

	"github.com/google/uuid"
)

const activateTpl = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>WittyChat · 激活账号</title>
<style>
  :root{color-scheme:light}
  *{box-sizing:border-box}
  body{margin:0;min-height:100vh;display:grid;place-items:center;
       font:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"PingFang SC","Microsoft YaHei",sans-serif;
       background:#f5f5f7;color:#1d1d1f}
  .card{width:min(420px,92vw);background:#fff;border-radius:18px;padding:40px;
        box-shadow:0 4px 24px rgba(0,0,0,.06)}
  h1{font-size:22px;font-weight:600;margin:0 0 6px}
  .sub{color:#6e6e73;font-size:14px;margin:0 0 28px}
  label{display:block;font-size:13px;color:#6e6e73;margin:0 0 6px}
  input[type=password]{width:100%;padding:12px 14px;font-size:15px;border:1px solid #d2d2d7;
        border-radius:10px;margin-bottom:18px;outline:none}
  input[type=password]:focus{border-color:#0071e3;box-shadow:0 0 0 3px rgba(0,113,227,.15)}
  button{width:100%;padding:12px;font-size:15px;font-weight:600;color:#fff;background:#0071e3;
         border:0;border-radius:10px;cursor:pointer}
  button:hover{background:#0077ed}
  .err{color:#d70015;font-size:14px;margin:0 0 14px}
  .ok{font-size:15px;line-height:1.7}
  .ok b{color:#1d1d1f}
</style>
</head>
<body><div class="card">
{{if .Done}}
  <h1>激活成功</h1>
  <p class="sub">WittyChat</p>
  <p class="ok">账号 <b>{{.Email}}</b> 已激活。<br>现在可以打开 WittyChat 桌面端登录了。</p>
{{else if .Invalid}}
  <h1>链接无效</h1>
  <p class="sub">WittyChat</p>
  <p class="err">这条链接已失效、被使用或已过期。请联系管理员重新签发。</p>
{{else}}
  <h1>设置初始密码</h1>
  <p class="sub">{{.Email}} · WittyChat</p>
  {{if .Error}}<p class="err">{{.Error}}</p>{{end}}
  <form method="post" action="/activate">
    <input type="hidden" name="token" value="{{.Token}}">
    <label for="pw">新密码（至少 8 位）</label>
    <input id="pw" name="password" type="password" minlength="8" required autocomplete="new-password">
    <label for="pw2">确认密码</label>
    <input id="pw2" name="password2" type="password" minlength="8" required autocomplete="new-password">
    <button type="submit">激活账号</button>
  </form>
{{end}}
</div></body></html>`

var tpl = template.Must(template.New("activate").Parse(activateTpl))

type pageData struct {
	Token   string
	Email   string
	Error   string
	Done    bool
	Invalid bool
}

func registerPages(mux *http.ServeMux, d *Deps) {
	// Admin SPA (embedded build) / 管理后台单页应用
	mux.Handle("/admin/", adminui.Handler())

	registerSetup(mux, d)

	mux.HandleFunc("GET /activate", func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		data := pageData{Token: token}
		if token == "" {
			data.Invalid = true
		} else if email, _, _, _, err := d.Store.PeekLink(r.Context(), token); err != nil {
			data.Invalid = true
		} else {
			data.Email = email
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = tpl.Execute(w, data)
	})

	mux.HandleFunc("POST /activate", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		token := r.Form.Get("token")
		pw, pw2 := r.Form.Get("password"), r.Form.Get("password2")
		data := pageData{Token: token}

		fail := func(msg string) {
			data.Error = msg
			if email, _, _, _, err := d.Store.PeekLink(r.Context(), token); err == nil {
				data.Email = email
			} else {
				data.Invalid = true
				data.Error = ""
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_ = tpl.Execute(w, data)
		}

		if pw != pw2 {
			fail("两次输入的密码不一致")
			return
		}
		if min := int(d.Settings.Int(r.Context(), settings.KeySecurityMinPass, settings.DefMinPassword)); len(pw) < min {
			fail(fmt.Sprintf("密码至少 %d 位", min))
			return
		}
		userID, kind, err := d.Store.ConsumeLink(r.Context(), token)
		if err != nil {
			data.Invalid = true
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_ = tpl.Execute(w, data)
			return
		}
		phc, err := auth.HashPassword(pw)
		if err != nil {
			http.Error(w, "internal", http.StatusInternalServerError)
			return
		}
		if err := d.Store.SetPassword(r.Context(), userID, phc); err != nil {
			http.Error(w, "internal", http.StatusInternalServerError)
			return
		}
		d.docRevalidateUser(r.Context(), userID) // reset link bumps cred version / 重置会作废旧会话
		if kind == auth.LinkKindActivation {
			if err := d.Store.ActivateUser(r.Context(), userID); err != nil && !errors.Is(err, auth.ErrUserNotActive) {
				http.Error(w, "internal", http.StatusInternalServerError)
				return
			}
		}
		u, err := d.Store.UserByID(r.Context(), userID)
		if err == nil {
			d.Audit.Log(r.Context(), &u.ID, "user", "auth.link.used:"+kind+":page", u.Email, nil, nil)
			data.Email = u.Email
		}
		data.Done = true
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = tpl.Execute(w, data)
	})
}

// ---------- first-run setup wizard / 首启初始化向导 ----------
//
// State machine (unauthenticated — only meaningful before setup_done):
//   no users        → admin create form
//   users && !done  → secret key shown + "saved it" confirm
//   done            → 已初始化 (locks out; /setup can no longer act)
// 首启状态机：无用户→建管理员；有用户未完成→展示密钥并要求确认已保存；完成后锁死。

const setupTpl = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>WittyChat · 初始化</title>
<style>
  *{box-sizing:border-box}
  body{margin:0;min-height:100vh;display:grid;place-items:center;
       font:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"PingFang SC","Microsoft YaHei",sans-serif;
       background:#f5f5f7;color:#1d1d1f}
  .card{width:min(480px,92vw);background:#fff;border-radius:18px;padding:40px;
        box-shadow:0 4px 24px rgba(0,0,0,.06)}
  h1{font-size:22px;font-weight:600;margin:0 0 6px}
  .sub{color:#6e6e73;font-size:14px;margin:0 0 28px}
  label{display:block;font-size:13px;color:#6e6e73;margin:0 0 6px}
  input[type=email],input[type=text],input[type=password]{width:100%;padding:12px 14px;font-size:15px;
        border:1px solid #d2d2d7;border-radius:10px;margin-bottom:16px;outline:none}
  input:focus{border-color:#0071e3;box-shadow:0 0 0 3px rgba(0,113,227,.15)}
  button{width:100%;padding:12px;font-size:15px;font-weight:600;color:#fff;background:#0071e3;
         border:0;border-radius:10px;cursor:pointer}
  button:hover{background:#0077ed}
  .err{color:#d70015;font-size:14px;margin:0 0 14px}
  .keybox{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:13px;
        word-break:break-all;background:#f5f5f7;border:1px solid #d2d2d7;border-radius:10px;
        padding:14px;margin:12px 0 8px;user-select:all}
  .warn{font-size:13px;color:#b25000;background:#fff4e5;border-radius:8px;padding:10px 12px;margin:0 0 18px;line-height:1.6}
  .ack{display:flex;gap:8px;align-items:flex-start;font-size:14px;margin:0 0 18px;color:#1d1d1f}
  .ack input{margin-top:3px}
  .ok{font-size:15px;line-height:1.8}
  code{background:#f5f5f7;padding:2px 6px;border-radius:5px}
</style></head>
<body><div class="card">
{{if .Done}}
  <h1>初始化完成</h1>
  <p class="sub">WittyChat</p>
  <p class="ok">管理员 <b>{{.Email}}</b> 已就绪，服务密钥已生成并保存。<br>
  进入 <a href="/admin/">管理后台</a>，或打开桌面端登录。</p>
{{else if .Locked}}
  <h1>已完成初始化</h1>
  <p class="sub">WittyChat</p>
  <p class="ok">本实例已完成初始化。进入 <a href="/admin/">管理后台</a>。</p>
{{else if .ShowKey}}
  <h1>保存服务密钥</h1>
  <p class="sub">WittyChat · 仅显示关键一步</p>
  <p class="warn">这是实例的<b>主密钥</b>（WITTYCHAT_SECRET_KEY，base64）。它用于加密 TOTP 种子等敏感数据。<br>
  已写入 <code>data/secret.key</code>（权限 600）。<b>请抄写并保存在安全位置</b>——丢失后已加密数据不可恢复。</p>
  <div class="keybox">{{.Secret}}</div>
  <form method="post" action="/setup/confirm">
    <label class="ack"><input type="checkbox" name="ack" value="1" required>
      <span>我已抄写并将此密钥保存在安全位置</span></label>
    <button type="submit">完成初始化</button>
  </form>
{{else}}
  <h1>初始化 WittyChat</h1>
  <p class="sub">创建第一个管理员账号</p>
  {{if .Error}}<p class="err">{{.Error}}</p>{{end}}
  <form method="post" action="/setup">
    <label for="em">管理员邮箱</label>
    <input id="em" name="email" type="email" required value="{{.Email}}">
    <label for="nm">显示名</label>
    <input id="nm" name="name" type="text" required value="{{.Name}}">
    <label for="pw">密码（至少 8 位）</label>
    <input id="pw" name="password" type="password" minlength="8" required autocomplete="new-password">
    <label for="pw2">确认密码</label>
    <input id="pw2" name="password2" type="password" minlength="8" required autocomplete="new-password">
    <button type="submit">创建并继续</button>
  </form>
{{end}}
</div></body></html>`

var setupT = template.Must(template.New("setup").Parse(setupTpl))

type setupData struct {
	Email   string
	Name    string
	Error   string
	Secret  string
	ShowKey bool
	Done    bool
	Locked  bool
}

func (d *Deps) setupDone(ctx context.Context) bool {
	return d.Settings.Bool(ctx, "setup_done", false)
}

func (d *Deps) hasUsers(ctx context.Context) bool {
	us, err := d.Store.ListUsers(ctx)
	return err == nil && len(us) > 0
}

func renderSetup(w http.ResponseWriter, data setupData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = setupT.Execute(w, data)
}

func registerSetup(mux *http.ServeMux, d *Deps) {
	mux.HandleFunc("GET /setup", func(w http.ResponseWriter, r *http.Request) {
		if d.setupDone(r.Context()) {
			renderSetup(w, setupData{Locked: true})
			return
		}
		if !d.hasUsers(r.Context()) {
			renderSetup(w, setupData{})
			return
		}
		// users exist but key-save wasn't confirmed — show the key again
		renderSetup(w, setupData{
			ShowKey: true,
			Secret:  base64.StdEncoding.EncodeToString(d.SecretKey),
		})
	})

	mux.HandleFunc("POST /setup", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if d.setupDone(r.Context()) || d.hasUsers(r.Context()) {
			renderSetup(w, setupData{Locked: true})
			return
		}
		email := strings.TrimSpace(r.Form.Get("email"))
		name := strings.TrimSpace(r.Form.Get("name"))
		pw, pw2 := r.Form.Get("password"), r.Form.Get("password2")
		data := setupData{Email: email, Name: name}
		if email == "" || name == "" {
			data.Error = "邮箱和显示名必填"
		} else if pw != pw2 {
			data.Error = "两次输入的密码不一致"
		} else if min := int(d.Settings.Int(r.Context(), settings.KeySecurityMinPass, settings.DefMinPassword)); len(pw) < min {
			data.Error = fmt.Sprintf("密码至少 %d 位", min)
		}
		if data.Error != "" {
			renderSetup(w, data)
			return
		}
		u, err := d.Store.CreateUser(r.Context(), email, name, true)
		if err != nil {
			data.Error = "创建失败：" + err.Error()
			renderSetup(w, data)
			return
		}
		phc, err := auth.HashPassword(pw)
		if err != nil || d.Store.SetPassword(r.Context(), u.ID, phc) != nil ||
			d.Store.ActivateUser(r.Context(), u.ID) != nil {
			http.Error(w, "internal", http.StatusInternalServerError)
			return
		}
		d.Audit.Log(r.Context(), &u.ID, audit.ActorUser, "setup.admin.create", u.Email, nil, nil)
		renderSetup(w, setupData{
			ShowKey: true,
			Secret:  base64.StdEncoding.EncodeToString(d.SecretKey),
		})
	})

	mux.HandleFunc("POST /setup/confirm", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if d.setupDone(r.Context()) {
			renderSetup(w, setupData{Locked: true})
			return
		}
		if r.Form.Get("ack") != "1" {
			renderSetup(w, setupData{
				ShowKey: true,
				Secret:  base64.StdEncoding.EncodeToString(d.SecretKey),
			})
			return
		}
		email := ""
		var adminID uuid.UUID
		if us, err := d.Store.ListUsers(r.Context()); err == nil && len(us) > 0 {
			email = us[0].Email
			adminID = us[0].ID
		}
		if err := d.Settings.Set(r.Context(), "setup_done", true, adminID); err != nil {
			http.Error(w, "internal", http.StatusInternalServerError)
			return
		}
		d.Audit.Log(r.Context(), nil, audit.ActorUser, "setup.completed", email, nil, nil)
		renderSetup(w, setupData{Done: true, Email: email})
	})
}
