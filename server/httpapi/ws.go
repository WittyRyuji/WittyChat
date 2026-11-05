package httpapi

// WebSocket endpoint: /ws?token=...
// Pushes conv_update/presence/typing frames only — real data via HTTP ops API.
//
// WebSocket 端点：/ws?token=...
// 只推 conv_update/presence/typing 帧——真实数据走 HTTP ops 接口。

import (
	"net/http"
	"strings"

	"github.com/coder/websocket"
)

// wsToken extracts the session token WITHOUT putting it in the request
// URI — browsers can't set headers on a WebSocket handshake, so the
// client sends it as a Sec-WebSocket-Protocol value ("wt1.<token>");
// we never select the protocol, so the token doesn't echo back. The
// ?token= query stays supported for tools/tests.
// WebSocket 鉴权：握手无法带自定义头，token 经 Sec-WebSocket-Protocol
// 子协议传递（"wt1.<token>"），服务端不回选故不回显；?token= 查询
// 参数保留供工具/测试。
func wsToken(r *http.Request) string {
	if t := r.URL.Query().Get("token"); t != "" {
		return t
	}
	for _, h := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, p := range strings.Split(h, ",") {
			if t, ok := strings.CutPrefix(strings.TrimSpace(p), "wt1."); ok {
				return t
			}
		}
	}
	return ""
}

func registerWS(mux *http.ServeMux, d *Deps) {
	mux.HandleFunc("GET /ws", func(w http.ResponseWriter, r *http.Request) {
		sess, err := d.Store.ValidateSession(r.Context(), wsToken(r))
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		u, err := d.Store.UserByID(r.Context(), sess.UserID)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			// Same-origin desktop client; allow any origin for dev.
			InsecureSkipVerify: true,
			// Offered alongside wt1.<token>; selected so strict clients
			// see a negotiated protocol. / 与 wt1.<token> 一同提供，
			// 回选它以满足严格要求子协议协商的客户端。
			Subprotocols: []string{"wittychat"},
		})
		if err != nil {
			return
		}
		d.Hub.Serve(r.Context(), ws, u.ID, u.DisplayName)
	})
}
