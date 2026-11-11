package feishuimport

// One-click personal-docs import: the employee authorizes the workspace's
// Feishu app once (OAuth, offline_access); the server keeps only the sealed
// refresh token and runs a self-mode import of their "我的空间" and
// "我的文档库" as them. The Feishu account's email must match the
// WittyChat account, so nobody can attach someone else's Feishu drive.
// 一键导入个人文档：员工授权一次，服务端只存加密的刷新令牌，以员工身份导入
// 「我的空间」「我的文档库」；飞书账号邮箱须与 WittyChat 账号一致。

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"wittychat/core/auth"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// OAuthScopes are the user-level permissions the personal import needs.
const OAuthScopes = "offline_access drive:drive:readonly wiki:wiki:readonly docs:document:export contact:user.email:readonly"

// ErrOAuthMismatch: the Feishu account authorized is not this employee's.
var ErrOAuthMismatch = errors.New("飞书账号与当前 WittyChat 账号的邮箱不一致")

// accountsBase maps the OpenAPI base to the OAuth host.
func accountsBase(apiBase string) string {
	switch apiBase {
	case "https://open.feishu.cn":
		return "https://accounts.feishu.cn"
	case "https://open.larksuite.com":
		return "https://accounts.larksuite.com"
	}
	return apiBase // loopback mock serves both
}

// OAuthRedirectURL is what the admin registers in the Feishu app.
func (s *Store) OAuthRedirectURL() string {
	return s.publicURL + "/api/v1/feishu/oauth/callback"
}

// OAuthStart returns the Feishu consent URL for this user (state valid 10 min).
// 生成授权地址（state 10 分钟有效、一次性）。
func (s *Store) OAuthStart(ctx context.Context, uid uuid.UUID) (string, error) {
	c, err := s.appCreds(ctx)
	if err != nil {
		return "", err
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	state := hex.EncodeToString(b)
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM feishu_oauth_states WHERE expires_at < now()`); err != nil {
		return "", err
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO feishu_oauth_states (state,user_id,expires_at) VALUES ($1,$2,now()+interval '10 minutes')`,
		state, uid); err != nil {
		return "", err
	}
	q := url.Values{
		"client_id": {c.appID}, "response_type": {"code"}, "redirect_uri": {s.OAuthRedirectURL()},
		"scope": {OAuthScopes}, "state": {state},
	}
	return accountsBase(c.base) + "/open-apis/authen/v1/authorize?" + q.Encode(), nil
}

type oauthTokens struct {
	Code             int    `json:"code"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	RefreshExpiresIn int    `json:"refresh_token_expires_in"`
}

func (s *Store) tokenEndpoint(ctx context.Context, c *fc, form map[string]string) (*oauthTokens, error) {
	form["client_id"], form["client_secret"] = c.appID, c.secret
	body, _ := json.Marshal(form)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, accountsBase(c.base)+"/oauth/v3/token", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var t oauthTokens
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("飞书授权响应无法解析（HTTP %d）", resp.StatusCode)
	}
	if t.Code != 0 || t.AccessToken == "" {
		msg := t.ErrorDescription
		if msg == "" {
			msg = t.Error
		}
		return nil, fmt.Errorf("飞书授权失败：%s（%d）", msg, t.Code)
	}
	return &t, nil
}

// OAuthCallback finishes the consent: one-shot state → token exchange →
// email check → sealed refresh token → personal import job started.
// 回调：消费 state → 换令牌 → 校验邮箱 → 存加密刷新令牌 → 启动个人导入。
func (s *Store) OAuthCallback(ctx context.Context, code, state string) (uuid.UUID, error) {
	var uid uuid.UUID
	if err := s.pool.QueryRow(ctx,
		`DELETE FROM feishu_oauth_states WHERE state=$1 AND expires_at > now() RETURNING user_id`, state).
		Scan(&uid); err != nil {
		return uuid.Nil, errors.New("授权链接已失效，请回到 WittyChat 重新点击导入")
	}
	c, err := s.appCreds(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	t, err := s.tokenEndpoint(ctx, c, map[string]string{
		"grant_type": "authorization_code", "code": code, "redirect_uri": s.OAuthRedirectURL(),
	})
	if err != nil {
		return uuid.Nil, err
	}
	uc := &fc{base: c.base, http: s.http, accessToken: t.AccessToken, exp: time.Now().Add(time.Hour)}
	var info struct {
		OpenID          string `json:"open_id"`
		Email           string `json:"email"`
		EnterpriseEmail string `json:"enterprise_email"`
	}
	if err := uc.get(ctx, "/open-apis/authen/v1/user_info", &info); err != nil {
		return uuid.Nil, err
	}
	j, err := s.creatorOf(ctx, uid)
	if err != nil {
		return uuid.Nil, err
	}
	if !j.creatorOwns(strings.ToLower(info.EnterpriseEmail)) && !j.creatorOwns(strings.ToLower(info.Email)) {
		return uuid.Nil, fmt.Errorf("%w（飞书：%s）", ErrOAuthMismatch, firstNonEmpty(info.EnterpriseEmail, info.Email, "未提供邮箱"))
	}
	if err := s.saveRefresh(ctx, uid, info.OpenID, t); err != nil {
		return uuid.Nil, err
	}
	// Re-authorizing restarts the same personal job: finished items are not
	// imported twice. 重新授权沿用原个人任务，已完成条目不重复导入。
	var job uuid.UUID
	if err := s.pool.QueryRow(ctx,
		`SELECT id FROM feishu_import_jobs WHERE created_by=$1 AND user_token ORDER BY created_at LIMIT 1`, uid).
		Scan(&job); err == nil {
		var st string
		_ = s.pool.QueryRow(ctx, `SELECT status FROM feishu_import_jobs WHERE id=$1`, job).Scan(&st)
		if st == "running" {
			return job, nil
		}
		if st == "cancelled" {
			_, _ = s.pool.Exec(ctx, `UPDATE feishu_import_jobs SET status='paused' WHERE id=$1`, job)
		}
		return job, s.StartJob(ctx, job)
	}
	roots, _ := json.Marshal([]Root{
		{Kind: "my_space", Name: "我的空间"},
		{Kind: "wiki_space", ID: "my_library", Name: "我的文档库"},
	})
	job = uuid.Must(uuid.NewV7())
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO feishu_import_jobs (id,name,mode,roots,created_by,user_token) VALUES ($1,'我的飞书文档','self',$2,$3,true)`,
		job, roots, uid); err != nil {
		return uuid.Nil, err
	}
	return job, s.StartJob(ctx, job)
}

func firstNonEmpty(v ...string) string {
	for _, x := range v {
		if x != "" {
			return x
		}
	}
	return ""
}

// creatorOf loads a user's own addresses for the ownership/email checks.
func (s *Store) creatorOf(ctx context.Context, uid uuid.UUID) (*Job, error) {
	j := &Job{CreatedBy: uid}
	if err := s.pool.QueryRow(ctx, `SELECT email FROM users WHERE id=$1`, uid).Scan(&j.CreatorEmail); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT lower(address) FROM mail_addresses WHERE user_id=$1`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a string
		if rows.Scan(&a) == nil {
			j.CreatorAlts = append(j.CreatorAlts, a)
		}
	}
	return j, rows.Err()
}

func (s *Store) saveRefresh(ctx context.Context, uid uuid.UUID, openID string, t *oauthTokens) error {
	sealed, err := auth.SealSecret(s.key, []byte(t.RefreshToken))
	if err != nil {
		return err
	}
	var exp *time.Time
	if t.RefreshExpiresIn > 0 {
		e := time.Now().Add(time.Duration(t.RefreshExpiresIn) * time.Second)
		exp = &e
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO feishu_user_tokens (user_id,open_id,sealed_refresh,refresh_expires_at) VALUES ($1,$2,$3,$4)
		ON CONFLICT (user_id) DO UPDATE SET open_id=$2, sealed_refresh=$3, refresh_expires_at=$4, updated_at=now()`,
		uid, openID, sealed, exp)
	return err
}

// userTokenFor returns a token source that refreshes (and rotates) the
// employee's refresh token under a row lock, so parallel lanes never use a
// refresh token twice. 刷新令牌轮换在行锁内完成，并发泳道不会重复使用。
func (s *Store) userTokenFor(uid uuid.UUID) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		c, err := s.appCreds(ctx)
		if err != nil {
			return "", err
		}
		var access string
		err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			var openID string
			var sealed []byte
			if err := tx.QueryRow(ctx,
				`SELECT open_id, sealed_refresh FROM feishu_user_tokens WHERE user_id=$1 FOR UPDATE`, uid).
				Scan(&openID, &sealed); err != nil {
				return errors.New("飞书授权已失效，请重新点击导入并授权")
			}
			rt, err := auth.OpenSecret(s.key, sealed)
			if err != nil {
				return err
			}
			t, err := s.tokenEndpoint(ctx, c, map[string]string{"grant_type": "refresh_token", "refresh_token": string(rt)})
			if err != nil {
				return fmt.Errorf("%w：%v", errRefreshRevoked, err)
			}
			newSealed, err := auth.SealSecret(s.key, []byte(t.RefreshToken))
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx,
				`UPDATE feishu_user_tokens SET sealed_refresh=$2, updated_at=now() WHERE user_id=$1`, uid, newSealed); err != nil {
				return err
			}
			access = t.AccessToken
			return nil
		})
		if errors.Is(err, errRefreshRevoked) {
			// Outside the rolled-back tx: drop the dead token so the UI asks
			// the employee to authorize again. 失效令牌在事务外删除，界面会提示重新授权。
			_, _ = s.pool.Exec(ctx, `DELETE FROM feishu_user_tokens WHERE user_id=$1`, uid)
			return "", fmt.Errorf("%w：飞书授权已失效，请重新点击导入并授权（%v）", ErrBadCreds, err)
		}
		return access, err
	}
}

var errRefreshRevoked = errors.New("refresh token rejected")

// Authorized reports whether the user has a stored Feishu authorization.
func (s *Store) Authorized(ctx context.Context, uid uuid.UUID) bool {
	var ok bool
	_ = s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM feishu_user_tokens WHERE user_id=$1)`, uid).Scan(&ok)
	return ok
}
