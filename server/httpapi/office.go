package httpapi

// OnlyOffice Document Server integration: signed edit pages, file feed
// for the docs container, and the save callback. The docs container has
// no user context, so browser↔docs trust is two different keys:
//   - browser → us: signed URLs (objects.Signer over "office|obj|uid")
//   - docs   → us: HS256 JWT shared secret (WITTYCHAT_ONLYOFFICE_JWT_SECRET)
//
// OnlyOffice 文档服务集成：签名编辑页、容器拉文件、保存回调三段。
// 浏览器侧用对象签名 URL（office|obj|uid），文档服务侧用共享 JWT 密钥。
//
// Flow: attachment "在线编辑" link → /office/open (session+ACL) → 302 to
// signed /office/edit page → DocsAPI loads file via /api/v1/office/file
// (URL signed for the docs container) → saves POST to
// /api/v1/office/callback → we pull the new bytes and ReplaceBytes the
// object in place so every reference sees the edit.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
	"wittychat/core/audit"

	"github.com/google/uuid"
)

// OfficeConfig carries the deployment wiring for the docs container.
type OfficeConfig struct {
	DocsURL      string // browser-facing base (api.js) e.g. http://localhost:8180
	JWTSecret    []byte // shared with the container's JWT_SECRET (optional)
	CallbackBase string // base URL the docs container uses to reach US (file+callback)
	FetchBase    string // where WE fetch saved output (default = DocsURL)
}

func (d *Deps) officeEnabled() bool {
	return d.Office != nil && d.Office.DocsURL != ""
}

func registerOffice(mux *http.ServeMux, d *Deps) {
	if !d.officeEnabled() {
		return
	}
	mux.HandleFunc("GET /office/open", d.officeOpen)
	mux.HandleFunc("GET /office/edit", d.officeEdit)
	mux.HandleFunc("GET /api/v1/objects/{id}/office-url", d.officeURL)
	mux.HandleFunc("GET /api/v1/office/file", d.officeFile)
	mux.HandleFunc("POST /api/v1/office/callback", d.officeCallback)
}

// officeType maps a file extension to OnlyOffice's documentType.
// fileType itself is always the extension. 文件后缀 → 文档类型。
var officeType = map[string]string{
	"doc": "word", "docx": "word", "docm": "word", "dot": "word", "dotx": "word",
	"dotm": "word", "odt": "word", "ott": "word", "rtf": "word", "txt": "word",
	"html": "word", "htm": "word", "pdf": "word", "epub": "word", "fb2": "word",
	"mht": "word", "djvu": "word", "xps": "word", "oxps": "word",
	"xls": "cell", "xlsx": "cell", "xlsm": "cell", "xlsb": "cell", "xlt": "cell",
	"xltx": "cell", "xltm": "cell", "ods": "cell", "ots": "cell", "csv": "cell",
	"ppt": "slide", "pptx": "slide", "pptm": "slide", "pps": "slide", "ppsx": "slide",
	"ppsm": "slide", "pot": "slide", "potx": "slide", "potm": "slide", "odp": "slide",
	"otp": "slide",
}

func officeExt(name string) (ext, docType string) {
	ext = strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))
	return ext, officeType[ext]
}

// officeACL mirrors downloadObject's access model: owner / conv member /
// referenced-by-mail / referenced-by-hr-request.
func (d *Deps) officeACL(ctx context.Context, objID, uid uuid.UUID) bool {
	if _, err := d.Objects.Get(ctx, objID, uid); err == nil {
		return true
	}
	return d.objectViaMail(ctx, objID, uid) || d.objectViaHR(ctx, objID, uid)
}

// signedEditURL mints the self-contained edit page URL for (obj,user) —
// capability is the signature itself, no cookie/session needed in the
// external browser tab. TTL covers a working day of editing.
func (d *Deps) signedEditURL(objID, uid uuid.UUID) string {
	sub := fmt.Sprintf("office|%s|%s", objID, uid)
	exp, sig := d.Signer.SignSubject(sub, 24*time.Hour)
	return fmt.Sprintf("%s/office/edit?obj=%s&u=%s&exp=%d&sig=%s",
		strings.TrimRight(d.PublicURL, "/"), objID, uid, exp, sig)
}

// GET /office/open?obj=<id> — session link target for the desktop app:
// validates the user + object ACL, then 302s to the signed edit page.
// 会话链接入口：校验登录态与对象权限后跳转到签名编辑页。
func (d *Deps) officeOpen(w http.ResponseWriter, r *http.Request) {
	uid, err := d.bearerUser(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	objID, err := uuid.Parse(r.URL.Query().Get("obj"))
	if err != nil {
		http.Error(w, "bad obj", http.StatusBadRequest)
		return
	}
	if !d.officeACL(r.Context(), objID, uid) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if obj, err := d.Objects.Peek(r.Context(), objID); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	} else if _, docType := officeExt(obj.Name); docType == "" {
		http.Error(w, "not an office document / 非可编辑文档", http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, d.signedEditURL(objID, uid), http.StatusFound)
}

// GET /api/v1/objects/{id}/office-url → {url} — programmatic variant of
// /office/open for clients that want the absolute URL up front.
func (d *Deps) officeURL(w http.ResponseWriter, r *http.Request) {
	uid, err := d.bearerUser(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	objID, err := uuid.Parse(r.PathValue("id"))
	if err != nil || !d.officeACL(r.Context(), objID, uid) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"url": d.signedEditURL(objID, uid)})
}

// GET /office/edit — the DocsAPI host page. Everything the editor needs
// (config + JWT) is embedded; the docs container then fetches the file
// itself. Signature covers office|obj|uid so the page is a capability.
func (d *Deps) officeEdit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	objID, err := uuid.Parse(q.Get("obj"))
	if err != nil {
		http.Error(w, "bad obj", http.StatusBadRequest)
		return
	}
	uid, err := uuid.Parse(q.Get("u"))
	if err != nil {
		http.Error(w, "bad user", http.StatusBadRequest)
		return
	}
	sub := fmt.Sprintf("office|%s|%s", objID, uid)
	if !d.Signer.VerifySubject(sub, q.Get("exp"), q.Get("sig")) {
		http.Error(w, "link expired or invalid / 链接过期或无效", http.StatusForbidden)
		return
	}
	obj, err := d.Objects.Peek(r.Context(), objID)
	if err != nil || obj.Status != "ready" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	ext, docType := officeExt(obj.Name)
	if docType == "" {
		http.Error(w, "not an office document / 非可编辑文档", http.StatusBadRequest)
		return
	}
	user, err := d.Store.UserByID(r.Context(), uid)
	if err != nil {
		http.Error(w, "user gone", http.StatusNotFound)
		return
	}

	// File feed + callback are scoped for the docs container, not the user.
	fexp, fsig := d.Signer.SignSubject("office-file|"+objID.String(), 24*time.Hour)
	cb := strings.TrimRight(d.Office.CallbackBase, "/")
	fileURL := fmt.Sprintf("%s/api/v1/office/file?obj=%s&exp=%d&sig=%s", cb, objID, fexp, fsig)
	callbackURL := fmt.Sprintf("%s/api/v1/office/callback?obj=%s", cb, objID)
	// key must change whenever bytes do — the docs server caches by key.
	key := fmt.Sprintf("%s-%x", objID, obj.SHA256[:6])

	cfg := map[string]any{
		"documentType": docType,
		"type":         "desktop",
		"width":        "100%",
		"height":       "100%",
		"document": map[string]any{
			"fileType": ext,
			"key":      key,
			"title":    obj.Name,
			"url":      fileURL,
			"permissions": map[string]any{
				"edit": true, "download": true, "print": true,
				"review": true, "comment": true, "copy": true,
			},
		},
		"editorConfig": map[string]any{
			"callbackUrl": callbackURL,
			"lang":        "zh-CN",
			"mode":        "edit",
			"user":        map[string]string{"id": uid.String(), "name": user.DisplayName},
			"customization": map[string]any{
				"autosave":      true,
				"forcesave":     true,
				"about":         false,
				"help":          false,
				"compactHeader": true,
				"customer":      map[string]string{"name": "WittyChat"},
			},
		},
	}
	if len(d.Office.JWTSecret) > 0 {
		cfg["token"] = jwtSign(cfg, d.Office.JWTSecret)
	}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		http.Error(w, "config failed", http.StatusInternalServerError)
		return
	}
	docsURL := strings.TrimRight(d.Office.DocsURL, "/")
	title := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(obj.Name)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><html lang="zh"><head><meta charset="utf-8">
<title>%s — WittyChat Office</title>
<style>html,body{margin:0;height:100%%;overflow:hidden}#placeholder{width:100%%;height:100vh}</style>
<script src="%s/web-apps/apps/api/documents/api.js"></script></head>
<body><div id="placeholder"></div>
<script>new DocsAPI.DocEditor("placeholder", %s);</script>
</body></html>`, title, docsURL, cfgJSON)
}

// GET /api/v1/office/file — docs-container file feed. The signature is
// the credential; no user session exists on that side.
func (d *Deps) officeFile(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	objID, err := uuid.Parse(q.Get("obj"))
	if err != nil {
		http.Error(w, "bad obj", http.StatusBadRequest)
		return
	}
	if !d.Signer.VerifySubject("office-file|"+objID.String(), q.Get("exp"), q.Get("sig")) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	obj, err := d.Objects.Peek(r.Context(), objID)
	if err != nil || obj.Status != "ready" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	rc, err := d.Objects.Open(r.Context(), obj)
	if err != nil {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	defer func() { _ = rc.Close() }()
	if obj.Mime != "" {
		w.Header().Set("Content-Type", obj.Mime)
	}
	_, _ = io.Copy(w, rc)
}

// officeCallbackBody is the OnlyOffice document-status POST shape
// (only the fields we act on). 回调体我们只取用得上的字段。
type officeCallbackBody struct {
	Status int      `json:"status"`
	URL    string   `json:"url"`
	Key    string   `json:"key"`
	Users  []string `json:"users"`
}

// POST /api/v1/office/callback — docs-server status push. Authenticated
// by the shared JWT (Authorization: Bearer) when a secret is configured;
// the JWT payload IS the body, so claims come from the token, not the
// raw POST. status 2 = MustSave, 6 = ForceSave — both carry a url to the
// converted output on the docs server.
func (d *Deps) officeCallback(w http.ResponseWriter, r *http.Request) {
	objID, err := uuid.Parse(r.URL.Query().Get("obj"))
	if err != nil {
		http.Error(w, "bad obj", http.StatusBadRequest)
		return
	}
	var body officeCallbackBody
	if len(d.Office.JWTSecret) > 0 {
		tok, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			http.Error(w, "missing token", http.StatusUnauthorized)
			return
		}
		payload, err := jwtVerify(tok, d.Office.JWTSecret)
		if err != nil {
			http.Error(w, "bad token", http.StatusForbidden)
			return
		}
		// OnlyOffice ≥7.2 wraps the real body under a "payload" claim.
		// 真实回调体嵌在 JWT 的 payload 声明里，顶层只有包装。
		if inner, ok := payload["payload"].(map[string]any); ok {
			payload = inner
		}
		raw, _ := json.Marshal(payload)
		if err := json.Unmarshal(raw, &body); err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
	} else {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
	}
	if body.Status == 2 || body.Status == 6 {
		if err := d.officePull(r.Context(), objID, body.URL); err != nil {
			log.Printf("office save obj=%s: %v", objID, err)
			// OnlyOffice expects {"error":N}; non-zero makes it show a
			// save-failed notice and keep the session's output.
			_ = json.NewEncoder(w).Encode(map[string]any{"error": 1})
			return
		}
		var actor *uuid.UUID
		var kind = audit.ActorSystem
		if len(body.Users) > 0 {
			if u, err := uuid.Parse(body.Users[0]); err == nil {
				actor = &u
				kind = audit.ActorUser
			}
		}
		d.Audit.Log(r.Context(), actor, kind, "office.save", objID.String(),
			map[string]string{"key": body.Key}, nil)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"error": 0})
}

// officePull fetches the converted output the docs server points at and
// swaps the object's bytes. The callback url may carry a host only the
// container can resolve, so scheme+host are rebuilt from FetchBase.
func (d *Deps) officePull(ctx context.Context, objID uuid.UUID, docURL string) error {
	if docURL == "" {
		return errors.New("callback carried no url")
	}
	u, err := url.Parse(docURL)
	if err != nil {
		return err
	}
	base, err := url.Parse(strings.TrimRight(d.Office.FetchBase, "/") + "/")
	if err != nil {
		return err
	}
	u.Scheme = base.Scheme
	u.Host = base.Host
	u.Path = path.Join("/", base.Path, u.Path) // keep cache path under base prefix
	u.User = nil

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	cli := &http.Client{Timeout: 60 * time.Second}
	res, err := cli.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("docs fetch: %s", res.Status)
	}
	maxBytes := int64(32 << 20)
	if d.Settings != nil {
		if v := d.Settings.Int(ctx, "upload_max_mb", 0) << 20; int64(v) > maxBytes {
			maxBytes = int64(v)
		}
	}
	mime := res.Header.Get("Content-Type")
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	_, err = d.Objects.ReplaceBytes(ctx, objID, res.Body, mime, maxBytes)
	return err
}

// ---------- HS256 JWT (docs-server shared secret) ----------
// OnlyOffice signs the editor config client-side-verified `token` and the
// callback's Authorization bearer with JWT_SECRET. A fixed HS256 profile
// is small enough to keep in-tree instead of importing a JWT library.
// OnlyOffice 用 JWT_SECRET 签编辑配置 token 与回调 Authorization。
// 固定 HS256 规格很小，内联实现免引第三方库。

func jwtSign(payload map[string]any, secret []byte) string {
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body, _ := json.Marshal(payload)
	b := base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(head + "." + b))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return head + "." + b + "." + sig
}

func jwtVerify(tok string, secret []byte) (map[string]any, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, errors.New("bad jwt shape")
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(parts[0] + "." + parts[1]))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(parts[2])) {
		return nil, errors.New("bad jwt signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	return payload, nil
}
