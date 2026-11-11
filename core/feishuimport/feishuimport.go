// Package feishuimport imports Feishu cloud docs (wiki spaces, drive folders)
// into WittyChat docs via the staged-import payload. 飞书云文档导入：发现→暂存。
// Calls use raw OpenAPI endpoints with a cached tenant_access_token so a mock
// base_url exercises the whole path in tests.
package feishuimport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"wittychat/core/auth"
	"wittychat/core/docs"
	"wittychat/core/objects"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const maxFileBytes = 512 << 20 // 512MiB export cap

// Job + item row shapes. / 行模型。
type Job struct {
	ID           uuid.UUID       `json:"id"`
	Name         string          `json:"name"`
	Mode         string          `json:"mode"` // self | admin
	Status       string          `json:"status"`
	Roots        json.RawMessage `json:"roots"`
	LastError    string          `json:"last_error"`
	CreatedBy    uuid.UUID       `json:"created_by"`
	CreatedAt    time.Time       `json:"created_at"`
	CreatorEmail string          `json:"-"`
	CreatorAlts  []string        `json:"-"`
	Discovered   bool            `json:"-"`
	DiscoverDone []int           `json:"-"`
}
type Item struct {
	ObjType  string      `json:"obj_type"`
	ObjToken string      `json:"obj_token"`
	Title    string      `json:"title"`
	Path     string      `json:"path"`
	Owner    string      `json:"owner_open_id"`
	Status   string      `json:"status"`
	Error    string      `json:"error"`
	DocIDs   []uuid.UUID `json:"doc_ids"`
}

var (
	ErrBaseURL   = errors.New("base_url not allowed / 非法服务地址")
	ErrBadCreds  = errors.New("应用凭据无效")
	ErrForbidden = errors.New("应用无权读取，请将文档或所在文件夹分享给应用")
)

type Store struct {
	pool    *pgxpool.Pool
	objects *objects.Store
	docs    *docs.Store
	key     []byte
	http    *http.Client
}

func New(pool *pgxpool.Pool, objects *objects.Store, ds *docs.Store, secretKey []byte) *Store {
	return &Store{
		pool: pool, objects: objects, docs: ds, key: secretKey,
		http: &http.Client{Timeout: 60 * time.Second},
	}
}

// validBaseURL enforces the same loopback-only rule as plain IMAP.
// 仅允许飞书/Lark 正式域名或回环（测试/开发）。
func validBaseURL(u string) bool {
	if u == "https://open.feishu.cn" || u == "https://open.larksuite.com" {
		return true
	}
	p, err := url.Parse(u)
	if err != nil || p.Scheme != "http" {
		return false
	}
	host := p.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// ---- workspace app credential (admin-set, singleton) ----

// SetApp stores the Feishu app credentials after verifying them (token fetch).
// 管理员配置应用：先验证凭据（取租户令牌）再保存。
func (s *Store) SetApp(ctx context.Context, appID, secret, baseURL string, by uuid.UUID) error {
	if !validBaseURL(baseURL) {
		return ErrBaseURL
	}
	c := &fc{base: strings.TrimRight(baseURL, "/"), appID: appID, secret: secret, http: s.http}
	if _, err := c.token(ctx); err != nil {
		return err
	}
	sealed, err := auth.SealSecret(s.key, []byte(secret))
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO feishu_app (id,app_id,sealed_secret,base_url,updated_by) VALUES (true,$1,$2,$3,$4)
		 ON CONFLICT (id) DO UPDATE SET app_id=$1,sealed_secret=$2,base_url=$3,updated_by=$4,updated_at=now()`,
		appID, sealed, baseURL, by)
	return err
}

// GetApp returns app_id + base_url (never the secret). 读取配置（不含密钥）。
func (s *Store) GetApp(ctx context.Context) (string, string, bool) {
	var appID, baseURL string
	err := s.pool.QueryRow(ctx,
		`SELECT app_id, base_url FROM feishu_app WHERE id`).Scan(&appID, &baseURL)
	return appID, baseURL, err == nil
}

func (s *Store) ClearApp(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM feishu_app`)
	return err
}

// appCreds loads the workspace credentials for runtime use. 运行时取凭据。
func (s *Store) appCreds(ctx context.Context) (*fc, error) {
	var appID, baseURL string
	var sealed []byte
	err := s.pool.QueryRow(ctx,
		`SELECT app_id, sealed_secret, base_url FROM feishu_app WHERE id`).Scan(&appID, &sealed, &baseURL)
	if err != nil || len(sealed) == 0 {
		return nil, fmt.Errorf("管理员尚未配置飞书应用")
	}
	secret, err := auth.OpenSecret(s.key, sealed)
	if err != nil {
		return nil, err
	}
	return &fc{
		base: strings.TrimRight(baseURL, "/"), appID: appID, secret: string(secret),
		http: s.http, emailCache: map[string]string{},
	}, nil
}

// CreateJob stores a self/admin import job (no credentials — workspace-level).
// 新建任务：mode self 为本人导入，admin 为管理员批量导入（仅管理员）。
func (s *Store) CreateJob(ctx context.Context, by uuid.UUID, isAdmin bool, name, mode string, roots json.RawMessage) (*Job, error) {
	if mode != "self" && mode != "admin" {
		return nil, fmt.Errorf("mode must be self|admin")
	}
	if mode == "admin" && !isAdmin {
		return nil, fmt.Errorf("仅管理员可创建批量导入任务")
	}
	j := &Job{ID: uuid.Must(uuid.NewV7()), Name: name, Mode: mode, Status: "draft", Roots: roots, CreatedBy: by}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO feishu_import_jobs (id,name,mode,roots,created_by) VALUES ($1,$2,$3,$4,$5)`,
		j.ID, name, mode, roots, by)
	if err != nil {
		return nil, err
	}
	return j, nil
}

func (s *Store) setStatus(ctx context.Context, id uuid.UUID, st string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE feishu_import_jobs SET status=$2, updated_at=now() WHERE id=$1`, id, st)
	return err
}

// StartJob: draft/paused/done/error → running; clears last_error, resets
// failed items → pending (re-run retries them), clears discovery, enqueues once.
// 开始/再跑一次：失败条目重试，重新发现，任务入队一次。
func (s *Store) StartJob(ctx context.Context, id uuid.UUID) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var st string
		if err := tx.QueryRow(ctx,
			`SELECT status FROM feishu_import_jobs WHERE id=$1 FOR UPDATE`, id).Scan(&st); err != nil {
			return err
		}
		if st == "running" || st == "cancelled" {
			return fmt.Errorf("任务状态不允许开始")
		}
		if _, err := tx.Exec(ctx,
			`UPDATE feishu_import_jobs SET status='running', last_error=NULL,
			   discovered=false, discover_done='{}', updated_at=now() WHERE id=$1`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE feishu_import_items SET status='pending',error=NULL,updated_at=now()
			 WHERE job_id=$1 AND status='failed'`, id); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM bg_tasks WHERE kind='feishu_import'
			   AND status IN ('queued','running') AND payload->>'job_id'=$1`, id).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := tx.Exec(ctx,
				`INSERT INTO bg_tasks (kind,payload) VALUES ('feishu_import',$1)`,
				map[string]any{"job_id": id.String()}); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) PauseJob(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE feishu_import_jobs SET status='paused',updated_at=now() WHERE id=$1 AND status='running'`, id)
	if err == nil && tag.RowsAffected() == 0 {
		return fmt.Errorf("只能暂停运行中的任务")
	}
	return err
}

func (s *Store) CancelJob(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE feishu_import_jobs SET status='cancelled',updated_at=now() WHERE id=$1 AND status NOT IN ('done','cancelled')`, id)
	if err == nil && tag.RowsAffected() == 0 {
		return fmt.Errorf("不能取消已完成/已取消的任务")
	}
	return err
}

func (s *Store) DeleteJob(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM feishu_import_jobs WHERE id=$1 AND status<>'running'`, id)
	if err == nil && tag.RowsAffected() == 0 {
		return fmt.Errorf("运行中的任务不能删除")
	}
	return err
}

func (s *Store) loadJob(ctx context.Context, id uuid.UUID) (*Job, error) {
	j := &Job{ID: id}
	err := s.pool.QueryRow(ctx,
		`SELECT name,mode,roots,status,COALESCE(last_error,''),created_by,discovered,COALESCE(discover_done,'{}')
		 FROM feishu_import_jobs WHERE id=$1`, id).
		Scan(&j.Name, &j.Mode, &j.Roots, &j.Status, &j.LastError, &j.CreatedBy, &j.Discovered, &j.DiscoverDone)
	if err != nil {
		return nil, err
	}
	// creator's own emails for the self-mode ownership check
	_ = s.pool.QueryRow(ctx, `SELECT email FROM users WHERE id=$1`, j.CreatedBy).Scan(&j.CreatorEmail)
	rows, err2 := s.pool.Query(ctx,
		`SELECT lower(address) FROM mail_addresses WHERE user_id=$1`, j.CreatedBy)
	if err2 == nil {
		defer rows.Close()
		for rows.Next() {
			var a string
			_ = rows.Scan(&a)
			j.CreatorAlts = append(j.CreatorAlts, a)
		}
	}
	return j, nil
}

// creatorOwns: self-mode check — the Feishu owner's email must be one of the
// creator's addresses (fail closed on unresolvable owner).
// 本人导入校验：文档属主邮箱须与创建者一致（无法确认则不导入）。
func (j *Job) creatorOwns(email string) bool {
	if email == "" {
		return false
	}
	if strings.EqualFold(email, j.CreatorEmail) {
		return true
	}
	for _, a := range j.CreatorAlts {
		if strings.EqualFold(email, a) {
			return true
		}
	}
	return false
}

// ---- Feishu OpenAPI client ----
type fc struct {
	base        string
	appID       string
	secret      string
	accessToken string
	exp         time.Time
	http        *http.Client
	emailCache  map[string]string // open_id → email ("!" prefix = not found)
}

// token fetches + caches a tenant_access_token. 取租户令牌（缓存）。
func (c *fc) token(ctx context.Context) (string, error) {
	if c.accessToken != "" && time.Now().Before(c.exp) {
		return c.accessToken, nil
	}
	body, _ := json.Marshal(map[string]string{"app_id": c.appID, "app_secret": c.secret})
	resp, err := c.do(ctx, http.MethodPost, "/open-apis/auth/v3/tenant_access_token/internal", body, false)
	if err != nil {
		return "", err
	}
	var out struct {
		Code  int    `json:"code"`
		Msg   string `json:"msg"`
		Token string `json:"tenant_access_token"`
		Exp   int    `json:"expire"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		return "", err
	}
	if out.Code != 0 {
		return "", fmt.Errorf("%w：%s", ErrBadCreds, out.Msg)
	}
	c.accessToken = out.Token
	c.exp = time.Now().Add(time.Duration(out.Exp-120) * time.Second)
	return c.accessToken, nil
}

// do performs one OpenAPI call; set auth=true to inject the token.
func (c *fc) do(ctx context.Context, method, path string, body []byte, authz bool) ([]byte, error) {
	if authz {
		if _, err := c.token(ctx); err != nil {
			return nil, err
		}
	}
	var resp *http.Response
	var err error
	for try := 0; try < 6; try++ {
		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		var req *http.Request
		req, err = http.NewRequestWithContext(ctx, method, c.base+path, rdr)
		if err != nil {
			return nil, err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if authz {
			req.Header.Set("Authorization", "Bearer "+c.accessToken)
		}
		resp, err = c.http.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == 429 && try < 5 {
			wait := 2 * time.Second
			if r := resp.Header.Get("x-ogw-ratelimit-reset"); r != "" {
				if sec, err2 := time.ParseDuration(r + "s"); err2 == nil {
					wait = sec
				}
			}
			_ = resp.Body.Close()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		break
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == 403 {
		return nil, ErrForbidden
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("feishu %s %s → %d: %s", method, path, resp.StatusCode, b)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}

func (c *fc) get(ctx context.Context, path string, out any) error {
	b, err := c.do(ctx, http.MethodGet, path, nil, true)
	if err != nil {
		return err
	}
	return unwrap(b, out)
}

// post shares the envelope + error handling with get. POST 统一解包。
func (c *fc) post(ctx context.Context, path string, body []byte, out any) error {
	b, err := c.do(ctx, http.MethodPost, path, body, true)
	if err != nil {
		return err
	}
	return unwrap(b, out)
}

func unwrap(b []byte, out any) error {
	var env struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return err
	}
	if env.Code == 1069902 { // export permission code
		return ErrForbidden
	}
	if env.Code == 99991400 {
		return fmt.Errorf("rate-limited")
	}
	if env.Code != 0 {
		return fmt.Errorf("feishu code %d: %s", env.Code, env.Msg)
	}
	return json.Unmarshal(env.Data, out)
}

// emailFor maps an owner open_id → email via contact API (cached). 属主邮箱。
func (c *fc) emailFor(ctx context.Context, openID string) string {
	if openID == "" {
		return ""
	}
	if e, ok := c.emailCache[openID]; ok {
		if strings.HasPrefix(e, "!") {
			return ""
		}
		return e
	}
	var out struct {
		User struct {
			EnterpriseEmail string `json:"enterprise_email"`
			Email           string `json:"email"`
		} `json:"user"`
	}
	email := ""
	if err := c.get(ctx, "/open-apis/contact/v3/users/"+openID+"?user_id_type=open_id", &out); err == nil {
		email = out.User.EnterpriseEmail
		if email == "" {
			email = out.User.Email
		}
	}
	if email == "" {
		c.emailCache[openID] = "!" + openID
	} else {
		c.emailCache[openID] = email
	}
	return email
}

// ListSpaces returns the wiki spaces the app can see (for the UI). 列举空间。
func (s *Store) ListSpaces(ctx context.Context, appID, secret, baseURL string) ([]map[string]string, error) {
	if !validBaseURL(baseURL) {
		return nil, ErrBaseURL
	}
	c := &fc{base: strings.TrimRight(baseURL, "/"), appID: appID, secret: secret, http: s.http}
	var out struct {
		Items []struct {
			Name    string `json:"name"`
			SpaceID string `json:"space_id"`
		} `json:"items"`
		PageToken string `json:"page_token"`
		HasMore   bool   `json:"has_more"`
	}
	var res []map[string]string
	token := ""
	for {
		if err := c.get(ctx, "/open-apis/wiki/v2/spaces?page_size=50&page_token="+token, &out); err != nil {
			return nil, err
		}
		for _, it := range out.Items {
			res = append(res, map[string]string{"id": it.SpaceID, "name": it.Name})
		}
		if !out.HasMore {
			break
		}
		token = out.PageToken
	}
	return res, nil
}
