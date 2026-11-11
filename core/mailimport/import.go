// Package mailimport: IMAP mailbox import engine — job/account/folder
// bookkeeping plus the chunked RunAccount loop the worker's mail_import
// task drives. Secrets are sealed with the workspace key; Purge drops them.
//
// IMAP 邮箱导入引擎：任务/账号/文件夹三表 + 分块 RunAccount 主循环
// （worker 的 mail_import 任务驱动）。密码用工作区密钥加密，Purge 清除。

package mailimport

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/textproto"
	"os"
	"strings"
	"time"
	"wittychat/core/auth"
	"wittychat/core/mail"
	"wittychat/core/objects"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store holds the import engine's dependencies.
type Store struct {
	pool      *pgxpool.Pool
	objects   *objects.Store
	mail      *mail.Store
	secretKey []byte
}

func New(pool *pgxpool.Pool, obj *objects.Store, m *mail.Store, secretKey []byte) *Store {
	return &Store{pool: pool, objects: obj, mail: m, secretKey: secretKey}
}

// ---- public API ----

type JobParams struct {
	Name, Provider, Host string
	Port                 int
	TLSMode              string // tls|starttls|plain
	Since                *time.Time
	AddAlias             bool
	CreatedBy            uuid.UUID
}

// CreateJob validates + inserts a job. plain mode is loopback-only (dev/tests).
// 建任务：plain 只允许回环地址（开发/测试用）。
func (s *Store) CreateJob(ctx context.Context, p JobParams) (uuid.UUID, error) {
	if p.TLSMode == "plain" && !isLoopback(p.Host) {
		return uuid.Nil, errors.New("plain 模式仅允许本机回环地址 / plain only on loopback")
	}
	if p.TLSMode != "tls" && p.TLSMode != "starttls" && p.TLSMode != "plain" {
		return uuid.Nil, errors.New("bad tls_mode")
	}
	if p.Provider == "" {
		p.Provider = "custom"
	}
	id := uuid.Must(uuid.NewV7())
	_, err := s.pool.Exec(ctx,
		`INSERT INTO mail_import_jobs (id,name,provider,host,port,tls_mode,since,add_alias,created_by)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		id, p.Name, p.Provider, p.Host, p.Port, p.TLSMode, p.Since, p.AddAlias, p.CreatedBy)
	return id, err
}

// AddAccountsCSV upserts accounts from `source_email,password,target_email`
// rows (optional header). Re-uploading reseals the password.
// CSV 上行导入账号：可选表头；重复 source 重新加密覆盖密码。
func (s *Store) AddAccountsCSV(ctx context.Context, jobID uuid.UUID, csvText string) (added, updated int, errs []string) {
	var st string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM mail_import_jobs WHERE id=$1`, jobID).Scan(&st); err != nil {
		return 0, 0, []string{"job not found"}
	}
	if st == "running" {
		return 0, 0, []string{"job is running / 任务运行中"}
	}
	r := csv.NewReader(strings.NewReader(csvText))
	r.FieldsPerRecord = -1
	lines, _ := r.ReadAll()
	for i, row := range lines {
		if len(row) == 0 || (i == 0 && strings.EqualFold(strings.TrimSpace(row[0]), "source_email")) {
			continue
		}
		if len(row) < 3 {
			errs = append(errs, fmt.Sprintf("line %d: 列数不足", i+1))
			continue
		}
		src, pw, tgt := strings.TrimSpace(row[0]), row[1], strings.TrimSpace(row[2])
		var target uuid.UUID
		if err := s.pool.QueryRow(ctx,
			`SELECT COALESCE((SELECT user_id FROM mail_addresses WHERE lower(address)=$1 LIMIT 1),
			                 (SELECT id FROM users WHERE lower(email)=$1 AND status='active' LIMIT 1))`,
			strings.ToLower(tgt)).Scan(&target); err != nil {
			errs = append(errs, fmt.Sprintf("line %d: 目标用户不存在 %s", i+1, tgt))
			continue
		}
		sealed, err := auth.SealSecret(s.secretKey, []byte(pw))
		if err != nil {
			errs = append(errs, fmt.Sprintf("line %d: seal failed", i+1))
			continue
		}
		var prevID *uuid.UUID
		_ = s.pool.QueryRow(ctx,
			`SELECT id FROM mail_import_accounts WHERE job_id=$1 AND source_login=$2`, jobID, src).Scan(&prevID)
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO mail_import_accounts (id,job_id,source_login,sealed_secret,target_user)
			 VALUES ($1,$2,$3,$4,$5)
			 ON CONFLICT (job_id, source_login) DO UPDATE SET sealed_secret=EXCLUDED.sealed_secret`,
			uuid.Must(uuid.NewV7()), jobID, src, sealed, target); err != nil {
			errs = append(errs, fmt.Sprintf("line %d: %v", i+1, err))
			continue
		}
		if prevID == nil {
			added++
		} else {
			updated++
		}
	}
	return added, updated, errs
}

// StartJob: job→running, accounts pending/error/paused/done→queued,
// enqueue a mail_import task per account. Refuses when secrets are purged.
// 启动：可重复（done 任务再跑就是增量同步）。
func (s *Store) StartJob(ctx context.Context, jobID uuid.UUID) error {
	var purged bool
	var st string
	if err := s.pool.QueryRow(ctx,
		`SELECT status,secrets_purged FROM mail_import_jobs WHERE id=$1`, jobID).Scan(&st, &purged); err != nil {
		return err
	}
	if purged {
		return errors.New("secrets purged — re-upload passwords / 密码已清除，请重新上传")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE mail_import_jobs SET status='running',updated_at=now() WHERE id=$1`, jobID); err != nil {
		return err
	}
	rows, err := s.pool.Query(ctx,
		`UPDATE mail_import_accounts SET status='queued', last_error=NULL, updated_at=now()
		 WHERE job_id=$1 AND status IN ('pending','error','paused','done') RETURNING id`, jobID)
	if err != nil {
		return err
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO bg_tasks (kind,payload) VALUES ('mail_import', jsonb_build_object('account_id',$1::text))`,
			id.String()); err != nil {
			return err
		}
	}
	return nil
}

// PauseJob marks the job paused — RunAccount notices between batches.
func (s *Store) PauseJob(ctx context.Context, jobID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE mail_import_jobs SET status='paused',updated_at=now() WHERE id=$1`, jobID)
	return err
}

// CancelJob marks the job cancelled — accounts get flushed to cancelled.
func (s *Store) CancelJob(ctx context.Context, jobID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`UPDATE mail_import_jobs SET status='cancelled',updated_at=now() WHERE id=$1`, jobID); err != nil {
		return err
	}
	_, err = tx.Exec(ctx,
		`UPDATE mail_import_accounts SET status='cancelled',finished_at=now(),updated_at=now()
		 WHERE job_id=$1 AND status IN ('pending','queued','running','paused')`, jobID)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// FinishJob purges the secrets and marks the job done.
func (s *Store) FinishJob(ctx context.Context, jobID uuid.UUID) error {
	if err := s.Purge(ctx, jobID); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE mail_import_jobs SET status='done',updated_at=now() WHERE id=$1`, jobID)
	return err
}

// Purge drops every sealed secret under a job and flags it.
func (s *Store) Purge(ctx context.Context, jobID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`UPDATE mail_import_accounts SET sealed_secret=NULL WHERE job_id=$1`, jobID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE mail_import_jobs SET secrets_purged=true,updated_at=now() WHERE id=$1`, jobID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// DeleteJob purges secrets then removes the job rows (imported mail stays).
func (s *Store) DeleteJob(ctx context.Context, jobID uuid.UUID) error {
	var st string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM mail_import_jobs WHERE id=$1`, jobID).Scan(&st); err != nil {
		return err
	}
	if st == "running" {
		return errors.New("job is running / 任务运行中")
	}
	if err := s.Purge(ctx, jobID); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM mail_import_jobs WHERE id=$1`, jobID)
	return err
}

// TestAccounts synchronously logs in for up to 20 accounts (10s each).
// Never changes state. / 测试连接：不改状态。
type TestResult struct {
	AccountID uuid.UUID `json:"account_id"`
	OK        bool      `json:"ok"`
	Error     string    `json:"error,omitempty"`
	Folders   int       `json:"folders,omitempty"`
}

func (s *Store) TestAccounts(ctx context.Context, jobID uuid.UUID, ids []uuid.UUID) ([]TestResult, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT a.id,a.source_login,a.sealed_secret,j.host,j.port,j.tls_mode
		 FROM mail_import_accounts a JOIN mail_import_jobs j ON j.id=a.job_id
		 WHERE a.job_id=$1 AND (cardinality($2::uuid[])=0 OR a.id=ANY($2))
		 LIMIT 20`, jobID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TestResult
	for rows.Next() {
		var r TestResult
		var login, host, mode string
		var port int
		var sealed []byte
		if err := rows.Scan(&r.AccountID, &login, &sealed, &host, &port, &mode); err != nil {
			continue
		}
		r.OK, r.Error, r.Folders = s.tryLogin(ctx, host, port, mode, login, sealed)
		out = append(out, r)
	}
	return out, nil
}

func (s *Store) tryLogin(ctx context.Context, host string, port int, mode, login string, sealed []byte) (bool, string, int) {
	pw, err := auth.OpenSecret(s.secretKey, sealed)
	if err != nil {
		return false, "凭据无法解密", 0
	}
	c, err := s.dial(ctx, host, port, mode)
	if err != nil {
		return false, err.Error(), 0
	}
	defer func() { _ = c.Close() }()
	if err := c.Login(login, string(pw)).Wait(); err != nil {
		return false, authErrText(err), 0
	}
	lc := c.List("", "*", nil)
	n := 0
	for lc.Next() != nil {
		n++
	}
	_ = lc.Close()
	return true, "", n
}

// ---- engine ----

type acctRow struct {
	id     uuid.UUID
	jobID  uuid.UUID
	login  string
	target uuid.UUID
	sealed []byte
	status string
}

type jobRow struct {
	status, tlsMode, host, provider string
	port                            int
	since                           *time.Time
	addAlias, purged                bool
}

// RunAccount processes one chunk of work for an account; budget caps the
// wall time so the worker can service other tasks. Returns more=true when
// the account still has work left.
// 分块执行：budget 内尽量多导，超时就存进度返回 more=true 让 worker 续跑。
func (s *Store) RunAccount(ctx context.Context, accountID uuid.UUID, budget time.Duration) (bool, error) {
	deadline := time.Now().Add(budget)
	a, j, err := s.loadAccount(ctx, accountID)
	if err != nil {
		return false, err
	}
	if j.status != "running" {
		st := "paused"
		if j.status == "cancelled" {
			st = "cancelled"
		}
		_, _ = s.pool.Exec(ctx,
			`UPDATE mail_import_accounts SET status=$2,updated_at=now() WHERE id=$1 AND status IN ('queued','running','paused')`,
			a.id, st)
		return false, nil
	}
	if a.sealed == nil {
		_, _ = s.pool.Exec(ctx,
			`UPDATE mail_import_accounts SET status='error',last_error='凭据已清除，请重新上传密码',updated_at=now() WHERE id=$1`, a.id)
		return false, nil
	}
	if a.status != "running" {
		if _, err := s.pool.Exec(ctx,
			`UPDATE mail_import_accounts SET status='running',started_at=COALESCE(started_at,now()),updated_at=now() WHERE id=$1`,
			a.id); err != nil {
			return false, err
		}
		s.applyAlias(ctx, a, j)
	}

	pw, err := auth.OpenSecret(s.secretKey, a.sealed)
	if err != nil {
		return false, s.setErr(ctx, a.id, "凭据无法解密")
	}
	c, err := s.dial(ctx, j.host, j.port, j.tlsMode)
	if err != nil {
		return false, err // network error → task retries / 网络错交给任务重试
	}
	defer func() { _ = c.Close() }()
	if err := c.Login(a.login, string(pw)).Wait(); err != nil {
		if msg := loginErr(err); msg != "" {
			return false, s.setErr(ctx, a.id, msg)
		}
		return false, err
	}

	folders, err := s.mapFolders(ctx, c, a.target, a.id)
	if err != nil {
		return false, err
	}
	more := false
	for _, f := range folders {
		if time.Now().After(deadline) {
			more = true
			break
		}
		done, err := s.importFolder(ctx, c, a, j, f, deadline)
		if err != nil {
			return false, err
		}
		if !done {
			more = true
			break
		}
	}
	s.recountFailed(ctx, a.id)
	if more {
		return true, nil
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE mail_import_accounts SET status='done',finished_at=now(),updated_at=now() WHERE id=$1`, a.id); err != nil {
		return false, err
	}
	// Job done when nothing is left pending/queued/running. / 无在途账号即收工。
	_, _ = s.pool.Exec(ctx,
		`UPDATE mail_import_jobs SET status='done',updated_at=now()
		 WHERE id=$1 AND status='running'
		   AND NOT EXISTS (SELECT 1 FROM mail_import_accounts
		                   WHERE job_id=$1 AND status IN ('pending','queued','running'))`, a.jobID)
	return false, nil
}

func (s *Store) loadAccount(ctx context.Context, id uuid.UUID) (*acctRow, *jobRow, error) {
	var a acctRow
	var j jobRow
	err := s.pool.QueryRow(ctx,
		`SELECT a.id,a.job_id,a.source_login,a.target_user,a.sealed_secret,a.status,
		        j.status,j.tls_mode,j.host,j.port,j.since,j.add_alias,j.secrets_purged,j.provider
		 FROM mail_import_accounts a JOIN mail_import_jobs j ON j.id=a.job_id
		 WHERE a.id=$1`, id).
		Scan(&a.id, &a.jobID, &a.login, &a.target, &a.sealed, &a.status,
			&j.status, &j.tlsMode, &j.host, &j.port, &j.since, &j.addAlias, &j.purged, &j.provider)
	if err != nil {
		return nil, nil, err
	}
	return &a, &j, nil
}

// applyAlias: add the source address as a mail alias when it's on the local
// domain — replies to migrated mail then reach the user.
// 域名在本地域的源地址加成别名：回复迁移邮件能落到用户。
func (s *Store) applyAlias(ctx context.Context, a *acctRow, j *jobRow) {
	if !j.addAlias {
		return
	}
	if !s.mail.IsLocal(a.login) {
		_, _ = s.pool.Exec(ctx,
			`UPDATE mail_import_accounts SET note='来源域名非本地域，未添加别名' WHERE id=$1`, a.id)
		return
	}
	_ = s.mail.AddAddress(ctx, a.target, a.login, "alias")
}

func (s *Store) setErr(ctx context.Context, id uuid.UUID, msg string) error {
	_, _ = s.pool.Exec(ctx,
		`UPDATE mail_import_accounts SET status='error',last_error=$2,updated_at=now() WHERE id=$1`, id, msg)
	return nil
}

// ---- IMAP plumbing ----

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// dial connects per tls_mode; plain is re-checked as loopback-only here so
// a poisoned job row can't smuggle plaintext credentials to a remote host.
// 连接：plain 二次校验回环——防止篡改的任务行把明文密码发去远端。
func (s *Store) dial(ctx context.Context, host string, port int, mode string) (*imapclient.Client, error) {
	addr := fmt.Sprintf("%s:%d", host, port)
	opts := &imapclient.Options{Dialer: &net.Dialer{Timeout: 15 * time.Second}}
	switch mode {
	case "tls":
		return imapclient.DialTLS(addr, opts)
	case "starttls":
		return imapclient.DialStartTLS(addr, opts)
	case "plain":
		if !isLoopback(host) {
			return nil, errors.New("plain only on loopback / plain 仅限回环地址")
		}
		return imapclient.DialInsecure(addr, opts)
	}
	return nil, fmt.Errorf("bad tls_mode %q", mode)
}

// loginErr classifies a failed LOGIN by *imap.Error.Type:
// NO → auth error (专用密码 hint, no retry); BAD → config error (no retry);
// everything else → caller retries. Dial/TLS errors never reach here.
// 按 *imap.Error.Type 分类：NO=凭据错（不重试）、BAD=协议错（不重试），
// 其余（含拨号/TLS）交任务重试。
func loginErr(err error) (permanent string) {
	var ierr *imap.Error
	if !errors.As(err, &ierr) {
		return ""
	}
	switch ierr.Type {
	case imap.StatusResponseTypeNo:
		return "登录失败：请确认管理员已开启第三方邮箱客户端，且使用的是专用密码（非登录密码）"
	case imap.StatusResponseTypeBad:
		return "服务器拒绝登录命令：" + ierr.Text + "（可能需要 TLS/STARTTLS）"
	}
	return ""
}

// authErrText kept for tryLogin's report path. / 测试端点沿用同文案。
func authErrText(err error) string {
	if m := loginErr(err); m != "" {
		return m
	}
	return err.Error()
}

// folderMap is one source mailbox mapped onto a local folder.
type folderMap struct {
	SourceName, Kind string
	TargetID         uuid.UUID
}

// mapFolders: LIST → kind heuristics / EnsureCustomFolder → persist rows in
// mail_import_folders; re-runs reuse the stored mapping.
// 目录映射：SPECIAL-USE 优先，其次名称启发，其他进自定义文件夹；映射落库复用。
func (s *Store) mapFolders(ctx context.Context, c *imapclient.Client, target, accountID uuid.UUID) ([]folderMap, error) {
	lc := c.List("", "*", &imap.ListOptions{
		ReturnSpecialUse: true, // attrs only — SelectSpecialUse would filter
	})
	var boxes []imap.ListData
	for {
		d := lc.Next()
		if d == nil {
			break
		}
		boxes = append(boxes, *d)
	}
	if err := lc.Close(); err != nil {
		return nil, err
	}
	var out []folderMap
	for _, b := range boxes {
		if hasAttr(b.Attrs, `\Noselect`) || hasAttr(b.Attrs, `\NonExistent`) ||
			hasAttr(b.Attrs, `\All`) || hasAttr(b.Attrs, `\Flagged`) {
			continue
		}
		kind := specialKind(b.Attrs)
		name := strings.ReplaceAll(b.Mailbox, string(b.Delim), "/")
		if kind == "" {
			kind = nameKind(lastSeg(name))
		}
		if kind == "" {
			kind = "custom"
		}
		var fid uuid.UUID
		var err error
		if kind == "custom" {
			fid, err = s.mail.EnsureCustomFolder(ctx, target, name)
		} else {
			fid, err = s.mailFolderByKind(ctx, target, kind)
		}
		if err != nil {
			return nil, err
		}
		// persist mapping — re-runs reuse / 映射落库，重跑复用
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO mail_import_folders (account_id,source_name,target_folder_id)
			 VALUES ($1,$2,$3)
			 ON CONFLICT (account_id,source_name) DO UPDATE SET target_folder_id=EXCLUDED.target_folder_id`,
			accountID, b.Mailbox, fid); err != nil {
			return nil, err
		}
		out = append(out, folderMap{SourceName: b.Mailbox, Kind: kind, TargetID: fid})
	}
	// INBOX first
	for i, f := range out {
		if strings.EqualFold(f.SourceName, "INBOX") {
			out[0], out[i] = out[i], out[0]
			break
		}
	}
	return out, nil
}

func (s *Store) mailFolderByKind(ctx context.Context, user uuid.UUID, kind string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM mail_folders WHERE user_id=$1 AND kind=$2 LIMIT 1`, user, kind).Scan(&id)
	return id, err
}

func hasAttr(attrs []imap.MailboxAttr, want string) bool {
	for _, a := range attrs {
		if strings.EqualFold(string(a), want) {
			return true
		}
	}
	return false
}

func specialKind(attrs []imap.MailboxAttr) string {
	for k, v := range map[string]string{
		`\Sent`: "sent", `\Drafts`: "drafts", `\Trash`: "trash",
		`\Junk`: "spam", `\Archive`: "archive",
	} {
		if hasAttr(attrs, k) {
			return v
		}
	}
	return ""
}

func lastSeg(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[i+1:]
	}
	return name
}

func nameKind(seg string) string {
	switch strings.ToLower(seg) {
	case "inbox":
		return "inbox"
	case "sent", "sent messages", "sent items", "已发送", "已发送邮件":
		return "sent"
	case "drafts", "草稿", "草稿箱":
		return "drafts"
	case "trash", "deleted messages", "deleted items", "已删除", "已删除邮件":
		return "trash"
	case "junk", "spam", "垃圾邮件":
		return "spam"
	case "archive", "归档":
		return "archive"
	}
	return ""
}

// importFolder walks one source mailbox; returns done=true when exhausted.
// 单文件夹：UIDVALIDITY 变动则重置游标；UID SEARCH 增量、25 一批流式取件，
// import_key + Message-ID 双重去重。
func (s *Store) importFolder(ctx context.Context, c *imapclient.Client, a *acctRow, j *jobRow,
	f folderMap, deadline time.Time,
) (bool, error) {
	sd, err := c.Select(f.SourceName, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return false, err
	}
	uidv := int64(sd.UIDValidity)
	var st struct {
		uidv  int64
		last  int64
		total int
	}
	_ = s.pool.QueryRow(ctx,
		`SELECT uidvalidity,last_uid,msgs_total FROM mail_import_folders WHERE account_id=$1 AND source_name=$2`,
		a.id, f.SourceName).Scan(&st.uidv, &st.last, &st.total)
	if st.uidv != uidv {
		st.last = 0
		if _, err := s.pool.Exec(ctx,
			`UPDATE mail_import_folders SET uidvalidity=$3,last_uid=0 WHERE account_id=$1 AND source_name=$2`,
			a.id, f.SourceName, uidv); err != nil {
			return false, err
		}
	}
	nextUID := st.last + 1
	if nextUID > math.MaxUint32 {
		nextUID = math.MaxUint32 // saturating: UID space is uint32
	}
	crit := &imap.SearchCriteria{
		UID: []imap.UIDSet{{imap.UIDRange{Start: imap.UID(nextUID)}}},
	}
	if j.since != nil {
		crit.Since = *j.since
	}
	sr, err := c.UIDSearch(crit, nil).Wait()
	if err != nil {
		return false, err
	}
	uids := append([]imap.UID(nil), sr.AllUIDs()...)
	if len(uids) == 0 {
		return true, nil
	}
	// Exact totals: folder total = processed + this scan's uids; account
	// total = SUM(folder totals). Chunked re-scans can't inflate them.
	// 精确计数：文件夹总数=已处理+本轮 uid 数；账号=各文件夹之和，
	// 分块重扫不会重复计入。
	if _, err := s.pool.Exec(ctx,
		`UPDATE mail_import_folders SET msgs_total=msgs_processed+$3 WHERE account_id=$1 AND source_name=$2`,
		a.id, f.SourceName, len(uids)); err != nil {
		return false, err
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE mail_import_accounts a SET msgs_total=(
		   SELECT COALESCE(sum(msgs_total),0) FROM mail_import_folders WHERE account_id=a.id)
		 WHERE a.id=$1`, a.id); err != nil {
		return false, err
	}
	// retry this folder's recorded failures before the new scan
	// 先重试本目录的失败记录再扫新件
	if err := s.retryFailures(ctx, c, a, j, f, uidv); err != nil {
		return false, err
	}

	processed := 0
	for i := 0; i < len(uids); i += 25 {
		if time.Now().After(deadline) {
			return false, nil // budget out → checkpoint done above / 超时让位
		}
		if js := s.jobStatus(ctx, a.jobID); js != "running" {
			return false, nil // pause/cancel → stop cleanly / 暂停/取消即止
		}
		batch := uids[i:min(i+25, len(uids))]
		fcmd := c.Fetch(imap.UIDSetNum(batch...), &imap.FetchOptions{
			UID: true, Flags: true, InternalDate: true, RFC822Size: true,
			BodySection: []*imap.FetchItemBodySection{{Peek: true}},
		})
		for {
			msg := fcmd.Next()
			if msg == nil {
				break
			}
			var uid imap.UID
			var flags []imap.Flag
			var idate time.Time
			var size int64
			tmp, err := os.CreateTemp("", "imp-*") //nolint:gosec // self-created temp file
			if err != nil {
				_ = fcmd.Close()
				return false, err // abort chunk → task retries / 中止让任务重试
			}
			ok := false
			for {
				item := msg.Next()
				if item == nil {
					break
				}
				switch it := item.(type) {
				case imapclient.FetchItemDataUID:
					uid = it.UID
				case imapclient.FetchItemDataFlags:
					flags = it.Flags
				case imapclient.FetchItemDataInternalDate:
					idate = it.Time
				case imapclient.FetchItemDataRFC822Size:
					size = it.Size
				case imapclient.FetchItemDataBodySection:
					if it.Literal != nil {
						_, _ = io.Copy(tmp, it.Literal)
						ok = true
					}
				}
			}
			_ = tmp.Close()
			if ok && uid > 0 {
				processed++
				if ierr := s.importOne(ctx, a, j, f, uidv, uid, flags, idate, size, tmp.Name()); ierr != nil {
					if uerr := s.recordFailure(ctx, a.id, f.SourceName, uidv, int64(uid), ierr.Error()); uerr != nil {
						_ = os.Remove(tmp.Name())
						_ = fcmd.Close()
						return false, uerr // journal write failed → retry whole chunk
					}
				}
				if int64(uid) > st.last {
					st.last = int64(uid)
				}
			}
			_ = os.Remove(tmp.Name())
		}
		if err := fcmd.Close(); err != nil {
			return false, err
		}
		// flush progress each batch / 每批落进度
		_, _ = s.pool.Exec(ctx,
			`UPDATE mail_import_folders SET last_uid=$3,msgs_processed=msgs_processed+$4 WHERE account_id=$1 AND source_name=$2`,
			a.id, f.SourceName, st.last, processed)
		processed = 0
	}
	return true, nil
}

func (s *Store) jobStatus(ctx context.Context, jobID uuid.UUID) string {
	var st string
	_ = s.pool.QueryRow(ctx, `SELECT status FROM mail_import_jobs WHERE id=$1`, jobID).Scan(&st)
	return st
}

// importOne: dedup → raw object → parse → DeliverParsed. Per-message
// failures bump msgs_failed and continue.
// 单封：先查重（import_key + Message-ID），原文入对象库，解析投递。
// testFailUID: test-only hook — nil in production. When set, importOne
// returns this error before doing any work. / 测试钩子：先于一切返回错误。
var testFailUID func(folder string, uid imap.UID) error

// headerMsgID reads just the RFC822 headers for the cheap Message-ID dedup
// — before a full parse + attachment Puts produce orphans we can't un-make.
// 只读头部拿 Message-ID 先查重：完整解析+附件入库后再发现重件，附件已成孤儿。
func headerMsgID(path string) string {
	fp, err := os.Open(path) //nolint:gosec // own temp file
	if err != nil {
		return ""
	}
	defer func() { _ = fp.Close() }()
	tp := textproto.NewReader(bufio.NewReaderSize(fp, 64<<10))
	h, err := tp.ReadMIMEHeader()
	if err != nil {
		return ""
	}
	mid := strings.TrimSpace(h.Get("Message-ID"))
	return strings.Trim(mid, "<>")
}

// importOne: dedup → raw object → parse → DeliverParsed. An error return
// lands in the failure journal; last_uid still advances so nothing blocks.
// 单封：先查重（import_key + 头部 Message-ID），原文入库，解析投递。
// 出错由调用方记台账；last_uid 照常前进不阻塞。
func (s *Store) importOne(ctx context.Context, a *acctRow, j *jobRow, f folderMap,
	uidv int64, uid imap.UID, flags []imap.Flag, idate time.Time, size int64, tmpPath string,
) error {
	if testFailUID != nil {
		if err := testFailUID(f.SourceName, uid); err != nil {
			return err
		}
	}
	ik := importKey(a.login, f.SourceName, uidv, uid)
	var exists bool
	_ = s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM mail_messages WHERE owner_user_id=$1 AND import_key=$2)`,
		a.target, ik).Scan(&exists)
	bump := func(col string, n int64) {
		_, _ = s.pool.Exec(ctx,
			fmt.Sprintf(`UPDATE mail_import_accounts SET %s=%s+$2,updated_at=now() WHERE id=$1`, col, col), a.id, n)
	}
	if exists {
		bump("msgs_skipped", 1)
		return nil
	}
	// Message-ID dedup BEFORE the full parse: covers UIDVALIDITY resets and
	// self-delivered mail without leaving orphan attachment objects behind.
	// 先按 Message-ID 查重再解析：兜住 UIDVALIDITY 重置和自投递，不留孤儿附件。
	if mid := headerMsgID(tmpPath); mid != "" {
		var dup bool
		_ = s.pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM mail_messages WHERE owner_user_id=$1 AND folder_id=$2 AND rfc_msgid=$3)`,
			a.target, f.TargetID, mid).Scan(&dup)
		if dup {
			bump("msgs_skipped", 1)
			return nil
		}
	}
	parsed, dAtts, err := func() (*mail.Parsed, []mail.DeliveredAttachment, error) {
		fp, err := os.Open(tmpPath) //nolint:gosec // own temp file
		if err != nil {
			return nil, nil, err
		}
		defer func() { _ = fp.Close() }()
		var dAtts []mail.DeliveredAttachment
		p, err := mail.ParseMessage(fp, func(meta mail.AttachmentMeta, r io.Reader) error {
			o, _, err := s.objects.Put(ctx, a.target, uuid.Nil, "mail", meta.Filename, meta.Mime, r, 512<<20)
			if err != nil {
				return err
			}
			dAtts = append(dAtts, mail.DeliveredAttachment{
				ObjectID: o.ID, ContentID: meta.ContentID, Inline: meta.Inline,
			})
			return nil
		})
		return p, dAtts, err
	}()
	if err != nil {
		log.Printf("mailimport: %s %s uid %d parse: %v", a.login, f.SourceName, uid, err)
		return fmt.Errorf("parse: %w", err)
	}
	// raw .eml object / 原文入对象库
	fp, err := os.Open(tmpPath) //nolint:gosec // own temp file
	if err != nil {
		return err
	}
	raw, _, err := s.objects.Put(ctx, a.target, uuid.Nil, "mail_raw",
		fmt.Sprintf("import-%d.eml", uid), "message/rfc822", fp, size+1)
	_ = fp.Close()
	if err != nil {
		return fmt.Errorf("raw put: %w", err)
	}
	sentAt := parsed.Date
	if sentAt.IsZero() {
		sentAt = idate
	}
	dir := "in"
	if f.Kind == "sent" || f.Kind == "drafts" {
		dir = "out"
	}
	_, err = s.mail.DeliverParsed(ctx, a.target, "", &f.TargetID, parsed, mail.DeliverOpts{
		RawObjectID: &raw.ID,
		Attachments: dAtts,
		IsRead:      hasFlag(flags, "\\Seen"),
		IsStarred:   hasFlag(flags, "\\Flagged"),
		SentAt:      sentAt,
		ReceivedAt:  idate,
		Direction:   dir,
		SizeBytes:   size,
		ImportKey:   ik,
	})
	if err != nil {
		// import_key unique violation → raced import → skipped
		if strings.Contains(err.Error(), "mail_messages_import_key") {
			bump("msgs_skipped", 1)
			return nil
		}
		log.Printf("mailimport: %s %s uid %d deliver: %v", a.login, f.SourceName, uid, err)
		return fmt.Errorf("deliver: %w", err)
	}
	bump("msgs_done", 1)
	bump("bytes_done", size)
	_, _ = s.pool.Exec(ctx,
		`UPDATE mail_import_folders SET msgs_done=msgs_done+1 WHERE account_id=$1 AND source_name=$2`,
		a.id, f.SourceName)
	return nil
}

// recordFailure upserts one failure row; the row retries on the next pass.
// 失败台账：upsert 记录，下轮重试。
func (s *Store) recordFailure(ctx context.Context, accountID uuid.UUID, folder string,
	uidv, uid int64, msg string,
) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO mail_import_failures (account_id,source_name,uidvalidity,uid,error)
		 VALUES ($1,$2,$3,$4,$5)
		 ON CONFLICT (account_id,source_name,uidvalidity,uid)
		 DO UPDATE SET error=EXCLUDED.error,attempts=mail_import_failures.attempts+1,updated_at=now()`,
		accountID, folder, uidv, uid, msg)
	return err
}

// retryFailures: UID FETCH just the recorded UIDs for this folder and
// re-import them; success deletes the row, failure bumps attempts. Rows for
// a stale uidvalidity are dropped — the fresh rescan covers them.
// 重试失败：只抓台账里的 UID；成功删行，失败 attempts+1。旧 uidvalidity
// 的行由重扫覆盖，直接删。
func (s *Store) retryFailures(ctx context.Context, c *imapclient.Client, a *acctRow,
	j *jobRow, f folderMap, uidv int64,
) error {
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM mail_import_failures WHERE account_id=$1 AND source_name=$2 AND uidvalidity<>$3`,
		a.id, f.SourceName, uidv); err != nil {
		return err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT uid FROM mail_import_failures WHERE account_id=$1 AND source_name=$2 AND uidvalidity=$3`,
		a.id, f.SourceName, uidv)
	if err != nil {
		return err
	}
	var uids []imap.UID
	for rows.Next() {
		var u int64
		if rows.Scan(&u) == nil && u >= 0 && u <= math.MaxUint32 {
			uids = append(uids, imap.UID(u)) //nolint:gosec // bounded above
		}
	}
	rows.Close()
	if len(uids) == 0 {
		return nil
	}
	fcmd := c.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{
		UID: true, Flags: true, InternalDate: true, RFC822Size: true,
		BodySection: []*imap.FetchItemBodySection{{Peek: true}},
	})
	for {
		msg := fcmd.Next()
		if msg == nil {
			break
		}
		var uid imap.UID
		var flags []imap.Flag
		var idate time.Time
		var size int64
		tmp, err := os.CreateTemp("", "imp-*") //nolint:gosec // self-created temp file
		if err != nil {
			_ = fcmd.Close()
			return err
		}
		ok := false
		for {
			item := msg.Next()
			if item == nil {
				break
			}
			switch it := item.(type) {
			case imapclient.FetchItemDataUID:
				uid = it.UID
			case imapclient.FetchItemDataFlags:
				flags = it.Flags
			case imapclient.FetchItemDataInternalDate:
				idate = it.Time
			case imapclient.FetchItemDataRFC822Size:
				size = it.Size
			case imapclient.FetchItemDataBodySection:
				if it.Literal != nil {
					_, _ = io.Copy(tmp, it.Literal)
					ok = true
				}
			}
		}
		_ = tmp.Close()
		if ok && uid > 0 {
			if ierr := s.importOne(ctx, a, j, f, uidv, uid, flags, idate, size, tmp.Name()); ierr != nil {
				_, _ = s.pool.Exec(ctx,
					`UPDATE mail_import_failures SET error=$4,attempts=attempts+1,updated_at=now()
					 WHERE account_id=$1 AND source_name=$2 AND uidvalidity=$3 AND uid=$5`,
					a.id, f.SourceName, uidv, ierr.Error(), int64(uid))
			} else {
				_, _ = s.pool.Exec(ctx,
					`DELETE FROM mail_import_failures
					 WHERE account_id=$1 AND source_name=$2 AND uidvalidity=$3 AND uid=$4`,
					a.id, f.SourceName, uidv, int64(uid))
				// retried-and-delivered counts as processed too / 重试成功也算处理
				_, _ = s.pool.Exec(ctx,
					`UPDATE mail_import_folders SET msgs_processed=msgs_processed+1 WHERE account_id=$1 AND source_name=$2`,
					a.id, f.SourceName)
			}
		}
		_ = os.Remove(tmp.Name())
	}
	return fcmd.Close()
}

// recountFailed: msgs_failed = live failure-row count (recomputed, never
// incremented). / msgs_failed 按台账行数重算，不再自增。
func (s *Store) recountFailed(ctx context.Context, accountID uuid.UUID) {
	_, _ = s.pool.Exec(ctx,
		`UPDATE mail_import_accounts a SET msgs_failed=(
		   SELECT count(*) FROM mail_import_failures WHERE account_id=a.id)
		 WHERE a.id=$1`, accountID)
}

func hasFlag(flags []imap.Flag, want string) bool {
	for _, f := range flags {
		if strings.EqualFold(string(f), want) {
			return true
		}
	}
	return false
}

func importKey(login, folder string, uidv int64, uid imap.UID) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%d", login, folder, uidv, uid)))
	return hex.EncodeToString(h[:])
}
