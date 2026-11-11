package feishuimport

// Whole-tenant directory: snapshot Feishu contacts, match them to WittyChat
// accounts by email, create the missing accounts and send each new person an
// activation link through the Feishu bot.
// 通讯录快照：按邮箱匹配 WittyChat 账号，缺失的自动建号，并由飞书机器人私信激活链接。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"wittychat/core/auth"

	"github.com/google/uuid"
)

// Person is one directory row as shown in the admin wizard. 通讯录一行。
type Person struct {
	OpenID       string     `json:"open_id"`
	Name         string     `json:"name"`
	Email        string     `json:"email"`
	FeishuStatus string     `json:"feishu_status"`
	UserID       *uuid.UUID `json:"user_id"`
	UserStatus   string     `json:"user_status"`
	Provisioned  bool       `json:"provisioned"`
	LinkSentAt   *time.Time `json:"link_sent_at"`
	LinkError    string     `json:"link_error"`
	MailStatus   string     `json:"mail_status"`
	MailError    string     `json:"mail_error"`
	MailDone     int        `json:"mail_done"`
	MailSkipped  int        `json:"mail_skipped"`
	MailFailed   int        `json:"mail_failed"`
	MailSyncedAt *time.Time `json:"mail_synced_at"`
	DocsLinked   bool       `json:"docs_authorized"`
}

// DirStats summarises one directory sync. 同步统计。
type DirStats struct {
	Seen, Active, Matched, Unmatched, NoEmail int
}

// ErrNoContactScope means the app lacks whole-tenant contact access.
var ErrNoContactScope = errors.New("应用缺少通讯录全员读取权限：在开发者后台把通讯录权限范围设为全部成员，并开通读取用户邮箱")

type fsUser struct {
	OpenID          string `json:"open_id"`
	Name            string `json:"name"`
	Email           string `json:"email"`
	EnterpriseEmail string `json:"enterprise_email"`
	Status          struct {
		IsFrozen    bool `json:"is_frozen"`
		IsResigned  bool `json:"is_resigned"`
		IsActivated bool `json:"is_activated"`
		IsExited    bool `json:"is_exited"`
	} `json:"status"`
}

func (u fsUser) email() string {
	if e := strings.TrimSpace(u.EnterpriseEmail); e != "" {
		return strings.ToLower(e)
	}
	return strings.ToLower(strings.TrimSpace(u.Email))
}

func (u fsUser) status() string {
	switch {
	case u.Status.IsResigned || u.Status.IsExited:
		return "resigned"
	case u.Status.IsFrozen:
		return "frozen"
	}
	return "active"
}

// listDepartments returns the root plus every descendant department.
// 根部门加全部子孙部门。
func (c *fc) listDepartments(ctx context.Context) ([]string, error) {
	ids := []string{"0"}
	page := ""
	for {
		var out struct {
			Items []struct {
				ID string `json:"open_department_id"`
			} `json:"items"`
			HasMore   bool   `json:"has_more"`
			PageToken string `json:"page_token"`
		}
		q := url.Values{
			"department_id_type": {"open_department_id"}, "fetch_child": {"true"},
			"page_size": {"50"}, "page_token": {page},
		}
		if err := c.get(ctx, "/open-apis/contact/v3/departments/0/children?"+q.Encode(), &out); err != nil {
			return nil, err
		}
		for _, d := range out.Items {
			ids = append(ids, d.ID)
		}
		if !out.HasMore || out.PageToken == "" {
			return ids, nil
		}
		page = out.PageToken
	}
}

func (c *fc) usersIn(ctx context.Context, dept string, each func(fsUser)) error {
	page := ""
	for {
		var out struct {
			Items     []fsUser `json:"items"`
			HasMore   bool     `json:"has_more"`
			PageToken string   `json:"page_token"`
		}
		q := url.Values{
			"department_id": {dept}, "department_id_type": {"open_department_id"},
			"user_id_type": {"open_id"}, "page_size": {"50"}, "page_token": {page},
		}
		if err := c.get(ctx, "/open-apis/contact/v3/users/find_by_department?"+q.Encode(), &out); err != nil {
			return err
		}
		for _, u := range out.Items {
			each(u)
		}
		if !out.HasMore || out.PageToken == "" {
			return nil
		}
		page = out.PageToken
	}
}

// SyncDirectory snapshots the whole Feishu directory into feishu_people and
// links rows to WittyChat accounts by email (login email or any address).
// People who disappeared are marked missing; nobody is ever deleted.
// 全量同步通讯录并按邮箱关联账号；消失的人标记 missing，不删除。
func (s *Store) SyncDirectory(ctx context.Context) (DirStats, error) {
	var st DirStats
	c, err := s.appCreds(ctx)
	if err != nil {
		return st, err
	}
	depts, err := c.listDepartments(ctx)
	if err != nil {
		if errors.Is(err, ErrForbidden) || strings.Contains(err.Error(), "code 400") || strings.Contains(err.Error(), "code 4000") {
			return st, fmt.Errorf("%w（%v）", ErrNoContactScope, err)
		}
		return st, err
	}
	seen := map[string]fsUser{}
	for _, d := range depts {
		if err := c.usersIn(ctx, d, func(u fsUser) { seen[u.OpenID] = u }); err != nil {
			if errors.Is(err, ErrForbidden) {
				return st, fmt.Errorf("%w（%v）", ErrNoContactScope, err)
			}
			return st, err
		}
	}
	now := time.Now()
	for _, u := range seen {
		st.Seen++
		email := u.email()
		status := u.status()
		if status == "active" {
			st.Active++
		}
		var emailArg any
		if email != "" {
			emailArg = email
		} else {
			st.NoEmail++
		}
		// A second Feishu account carrying an address already held by another
		// row would violate the unique email index — keep the first, drop the
		// address on the newcomer. 同邮箱重复的飞书账号：后来者不记邮箱。
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO feishu_people (open_id,name,email,feishu_status,synced_at) VALUES ($1,$2,$3,$4,$5)
			 ON CONFLICT (open_id) DO UPDATE SET name=$2,email=$3,feishu_status=$4,synced_at=$5`,
			u.OpenID, u.Name, emailArg, status, now); err != nil {
			if !strings.Contains(err.Error(), "feishu_people_email") {
				return st, err
			}
			if _, err := s.pool.Exec(ctx,
				`INSERT INTO feishu_people (open_id,name,feishu_status,synced_at) VALUES ($1,$2,$3,$4)
				 ON CONFLICT (open_id) DO UPDATE SET name=$2,feishu_status=$3,synced_at=$4`,
				u.OpenID, u.Name, status, now); err != nil {
				return st, err
			}
		}
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE feishu_people SET feishu_status='missing' WHERE synced_at < $1`, now); err != nil {
		return st, err
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE feishu_people p SET user_id = m.id FROM (
		  SELECT p2.open_id, COALESCE(
		    (SELECT u.id FROM users u WHERE lower(u.email)=lower(p2.email) AND u.status IN ('active','pending') LIMIT 1),
		    (SELECT a.user_id FROM mail_addresses a JOIN users u ON u.id=a.user_id
		      WHERE lower(a.address)=lower(p2.email) AND u.status IN ('active','pending') LIMIT 1)) AS id
		  FROM feishu_people p2 WHERE p2.email IS NOT NULL) m
		WHERE p.open_id=m.open_id AND m.id IS NOT NULL AND p.user_id IS DISTINCT FROM m.id`); err != nil {
		return st, err
	}
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE user_id IS NOT NULL),
	        count(*) FILTER (WHERE user_id IS NULL AND email IS NOT NULL AND feishu_status='active')
	        FROM feishu_people WHERE feishu_status<>'missing'`).Scan(&st.Matched, &st.Unmatched)
	_, _ = s.pool.Exec(ctx, `
		INSERT INTO feishu_migration (id) VALUES (true) ON CONFLICT (id) DO NOTHING;`)
	_, _ = s.pool.Exec(ctx, `UPDATE feishu_migration SET dir_synced_at=now() WHERE id`)
	return st, nil
}

// Provision creates a pending account (+ primary address, default folders)
// for every active, unmatched person with an email, then sends each an
// activation link through the Feishu bot. Send failures are recorded and do
// not undo the account — the admin can resend.
// 为在职、未匹配且有邮箱的人建 pending 账号并私信激活链接；发送失败只记录。
func (s *Store) Provision(ctx context.Context, by uuid.UUID) (created int, err error) {
	if s.auth == nil || s.mail == nil {
		return 0, errors.New("provisioning not wired / 未接入建号")
	}
	rows, err := s.pool.Query(ctx,
		`SELECT open_id,name,email FROM feishu_people
		 WHERE user_id IS NULL AND email IS NOT NULL AND feishu_status='active' ORDER BY name`)
	if err != nil {
		return 0, err
	}
	type cand struct{ openID, name, email string }
	var todo []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.openID, &c.name, &c.email); err == nil {
			todo = append(todo, c)
		}
	}
	rows.Close()
	for _, c := range todo {
		u, cerr := s.auth.CreateUser(ctx, c.email, c.name, false)
		if errors.Is(cerr, auth.ErrEmailTaken) {
			// Raced with a manual create — just link it. 并发已建：直接关联。
			_, _ = s.pool.Exec(ctx,
				`UPDATE feishu_people SET user_id=(SELECT id FROM users WHERE lower(email)=lower($2)) WHERE open_id=$1`,
				c.openID, c.email)
			continue
		}
		if cerr != nil {
			_, _ = s.pool.Exec(ctx, `UPDATE feishu_people SET link_error=$2 WHERE open_id=$1`, c.openID, "建号失败："+cerr.Error())
			continue
		}
		_ = s.mail.AddAddress(ctx, u.ID, u.Email, "primary")
		_ = s.mail.EnsureFolders(ctx, u.ID)
		if _, err := s.pool.Exec(ctx,
			`UPDATE feishu_people SET user_id=$2, provisioned=true, link_error=NULL WHERE open_id=$1`,
			c.openID, u.ID); err != nil {
			return created, err
		}
		created++
		s.sendActivation(ctx, c.openID, u.ID, u.Email, by)
	}
	return created, nil
}

// ResendLinks issues fresh activation links to provisioned people who have
// not activated yet (all of them when openIDs is empty).
// 给尚未激活的自动建号用户重发激活链接（不传则全部）。
func (s *Store) ResendLinks(ctx context.Context, by uuid.UUID, openIDs []string) (int, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT p.open_id, u.id, u.email FROM feishu_people p JOIN users u ON u.id=p.user_id
		 WHERE p.provisioned AND u.status='pending' AND (cardinality($1::text[])=0 OR p.open_id=ANY($1))`,
		openIDs)
	if err != nil {
		return 0, err
	}
	type t struct {
		openID string
		uid    uuid.UUID
		email  string
	}
	var todo []t
	for rows.Next() {
		var x t
		if err := rows.Scan(&x.openID, &x.uid, &x.email); err == nil {
			todo = append(todo, x)
		}
	}
	rows.Close()
	for _, x := range todo {
		s.sendActivation(ctx, x.openID, x.uid, x.email, by)
	}
	return len(todo), nil
}

func (s *Store) sendActivation(ctx context.Context, openID string, uid uuid.UUID, email string, by uuid.UUID) {
	token, exp, err := s.auth.CreateActivationLink(ctx, uid, auth.LinkKindActivation, by)
	if err == nil {
		link := s.publicURL + "/activate?token=" + url.QueryEscape(token)
		err = s.botText(ctx, openID, fmt.Sprintf(
			"你的 WittyChat 账号已开通（%s）。\n点击设置密码并激活：%s\n链接 %s 前有效。激活后可在 WittyChat 里看到从飞书搬过来的邮件和文档。",
			email, link, exp.Local().Format("01-02 15:04")))
	}
	if err != nil {
		_, _ = s.pool.Exec(ctx, `UPDATE feishu_people SET link_error=$2 WHERE open_id=$1`, openID, "激活链接发送失败："+err.Error())
		return
	}
	_, _ = s.pool.Exec(ctx, `UPDATE feishu_people SET link_sent_at=now(), link_error=NULL WHERE open_id=$1`, openID)
}

// botText sends a plain-text Feishu message from the app's bot to one person.
// 以应用机器人身份给某人发一条文本消息。
func (s *Store) botText(ctx context.Context, openID, text string) error {
	c, err := s.appCreds(ctx)
	if err != nil {
		return err
	}
	content, _ := json.Marshal(map[string]string{"text": text})
	body, _ := json.Marshal(map[string]string{"receive_id": openID, "msg_type": "text", "content": string(content)})
	var out struct {
		MessageID string `json:"message_id"`
	}
	return c.post(ctx, "/open-apis/im/v1/messages?receive_id_type=open_id", body, &out)
}

// People lists the directory snapshot with each person's progress.
// 通讯录快照与每人进度。
func (s *Store) People(ctx context.Context) ([]Person, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.open_id,p.name,COALESCE(p.email,''),p.feishu_status,p.user_id,COALESCE(u.status,''),
		       p.provisioned,p.link_sent_at,COALESCE(p.link_error,''),p.mail_status,COALESCE(p.mail_error,''),
		       p.mail_done,p.mail_skipped,p.mail_failed,p.mail_synced_at,
		       EXISTS(SELECT 1 FROM feishu_user_tokens t WHERE t.user_id=p.user_id)
		FROM feishu_people p LEFT JOIN users u ON u.id=p.user_id
		ORDER BY (p.feishu_status='active') DESC, p.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Person{}
	for rows.Next() {
		var p Person
		if err := rows.Scan(&p.OpenID, &p.Name, &p.Email, &p.FeishuStatus, &p.UserID, &p.UserStatus,
			&p.Provisioned, &p.LinkSentAt, &p.LinkError, &p.MailStatus, &p.MailError,
			&p.MailDone, &p.MailSkipped, &p.MailFailed, &p.MailSyncedAt, &p.DocsLinked); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
