package httpapi

// WebSocket endpoint: /ws?token=...
// Pushes conv_update/presence/typing frames only — real data via HTTP ops API.
//
// WebSocket 端点：/ws?token=...
// 只推 conv_update/presence/typing 帧——真实数据走 HTTP ops 接口。

import (
	"net/http"

	"github.com/coder/websocket"
)

func registerWS(mux *http.ServeMux, d *Deps) {
	mux.HandleFunc("GET /ws", func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		sess, err := d.Store.ValidateSession(r.Context(), token)
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
		})
		if err != nil {
			return
		}
		d.Hub.Serve(r.Context(), ws, u.ID, u.DisplayName)
	})
}
