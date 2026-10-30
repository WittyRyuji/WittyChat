package httpapi

// Server-rendered activation page: the only browser-facing page in P0.
// Employee opens the one-time link, sets the initial password, done.
//
// 服务端渲染的激活页：P0 唯一的浏览器页面。
// 员工打开一次性链接 → 设置初始密码 → 完成。

import (
	"errors"
	"html/template"
	"net/http"
	"wittychat/core/auth"
	"wittychat/server/adminui"
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
		if len(pw) < 8 {
			fail("密码至少 8 位")
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
