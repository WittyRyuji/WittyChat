package feishuimport

// Feishu migration orchestrator. Directory sync and optional account
// provisioning are shared; the scope selects mail, docs or the original
// whole-tenant pipeline. While "continuous" is on, mail keeps catching up
// incrementally so the MX switch can happen at any time; Finish runs a last
// full pass.
// 飞书迁移编排：通讯录同步与可选建号是公共步骤，scope 区分邮件、文档或原整体
// 流程。持续同步期间邮件增量追平，随时可切 MX；完成时再跑一轮全量。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
)

// MailSyncEvery is the incremental catch-up interval while syncing.
const MailSyncEvery = 30 * time.Minute

// Migration is the singleton state plus roll-up counts for the wizard.
type Migration struct {
	Status      string     `json:"status"`
	Scope       string     `json:"scope"`
	Provision   bool       `json:"provision"`
	Continuous  bool       `json:"continuous"`
	StartedAt   *time.Time `json:"started_at"`
	DirSyncedAt *time.Time `json:"dir_synced_at"`
	FinishedAt  *time.Time `json:"finished_at"`
	WikiJob     *uuid.UUID `json:"wiki_job"`
	LastError   string     `json:"last_error"`
	People      int        `json:"people"`
	Linked      int        `json:"linked"`
	Provisioned int        `json:"provisioned"`
	Activated   int        `json:"activated"`
	MailDone    int        `json:"mail_done_people"`
	MailRunning int        `json:"mail_running_people"`
	MailErrors  int        `json:"mail_error_people"`
	Messages    int        `json:"messages"`
	DocsAuthed  int        `json:"docs_authorized"`
}

func (s *Store) ensureMigration(ctx context.Context) {
	_, _ = s.pool.Exec(ctx, `INSERT INTO feishu_migration (id) VALUES (true) ON CONFLICT (id) DO NOTHING`)
}

// GetMigration returns the state and counts. 迁移状态与汇总。
func (s *Store) GetMigration(ctx context.Context) (*Migration, error) {
	s.ensureMigration(ctx)
	m := &Migration{}
	err := s.pool.QueryRow(ctx, `
		SELECT status,scope,provision,continuous,started_at,dir_synced_at,finished_at,wiki_job,COALESCE(last_error,'')
		FROM feishu_migration WHERE id`).
		Scan(&m.Status, &m.Scope, &m.Provision, &m.Continuous, &m.StartedAt, &m.DirSyncedAt, &m.FinishedAt, &m.WikiJob, &m.LastError)
	if err != nil {
		return nil, err
	}
	err = s.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE p.feishu_status='active'),
		       count(*) FILTER (WHERE p.user_id IS NOT NULL AND p.feishu_status='active'),
		       count(*) FILTER (WHERE p.provisioned),
		       count(*) FILTER (WHERE p.provisioned AND u.status='active'),
		       count(*) FILTER (WHERE p.mail_status='done'),
		       count(*) FILTER (WHERE p.mail_status IN ('queued','running')),
		       count(*) FILTER (WHERE p.mail_status='error'),
		       COALESCE(sum(p.mail_done),0),
		       count(*) FILTER (WHERE EXISTS(SELECT 1 FROM feishu_user_tokens t WHERE t.user_id=p.user_id))
		FROM feishu_people p LEFT JOIN users u ON u.id=p.user_id`).
		Scan(&m.People, &m.Linked, &m.Provisioned, &m.Activated, &m.MailDone, &m.MailRunning, &m.MailErrors, &m.Messages, &m.DocsAuthed)
	return m, err
}

// StartMigration kicks off (or re-runs) the whole pipeline in the worker.
// 启动（或重跑）整体迁移，由 worker 执行。
func (s *Store) StartMigration(ctx context.Context, by uuid.UUID, provision, continuous bool) error {
	return s.StartMigrationScope(ctx, by, provision, continuous, "all")
}

// StartMigrationScope starts a mail-only, docs-only or whole-tenant pass.
// scope=docs cannot run continuous mail sync. scope=mail 不触碰文档任务。
func (s *Store) StartMigrationScope(ctx context.Context, by uuid.UUID, provision, continuous bool, scope string) error {
	if scope == "" {
		scope = "all"
	}
	switch scope {
	case "all", "mail":
	case "docs":
		continuous = false
	default:
		return errors.New("invalid migration scope")
	}
	if _, err := s.appCreds(ctx); err != nil {
		return err
	}
	s.ensureMigration(ctx)

	// Starting docs while mail is running widens the active run to "all"
	// instead of switching off continuous mail sync (and vice versa).
	var oldStatus, oldScope string
	var oldProvision, oldContinuous bool
	if err := s.pool.QueryRow(ctx,
		`SELECT status,scope,provision,continuous FROM feishu_migration WHERE id`).
		Scan(&oldStatus, &oldScope, &oldProvision, &oldContinuous); err != nil {
		return err
	}
	active := oldStatus == "running" || oldStatus == "syncing" || oldStatus == "finishing"
	force := active && oldScope != scope
	if force {
		scope = "all"
		if continuous || oldContinuous {
			continuous = true
		}
	}
	provision = provision || (force && oldProvision)
	if _, err := s.pool.Exec(ctx, `
		UPDATE feishu_migration SET status='running', scope=$1, provision=$2, continuous=$3, started_by=$4,
		  started_at=COALESCE(started_at, now()), finished_at=NULL, last_error=NULL WHERE id`,
		scope, provision, continuous, by); err != nil {
		return err
	}
	if force {
		_, err := s.pool.Exec(ctx, `INSERT INTO bg_tasks (kind,payload) VALUES ('feishu_migrate','{}')`)
		return err
	}
	return s.enqueueOnce(ctx, "feishu_migrate", map[string]any{})
}

// FinishMigration queues a last full mail pass for everyone and stops
// continuous sync once it drains. 收尾：每人最后一轮全量，跑完后停止同步。
func (s *Store) FinishMigration(ctx context.Context) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE feishu_migration SET status='finishing' WHERE id AND status IN ('running','syncing')`)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("迁移未在进行中")
	}
	return s.queueMail(ctx, true, `mail_status IN ('done','error','none')`)
}

func (s *Store) enqueueOnce(ctx context.Context, kind string, payload map[string]any) error {
	var n int
	_ = s.pool.QueryRow(ctx,
		`SELECT count(*) FROM bg_tasks WHERE kind=$1 AND status IN ('queued','running')`, kind).Scan(&n)
	if n > 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO bg_tasks (kind,payload) VALUES ($1,$2)`, kind, payload)
	return err
}

// queueMail starts a pass for every linked active person matching where.
func (s *Store) queueMail(ctx context.Context, full bool, where string) error {
	rows, err := s.pool.Query(ctx, `SELECT open_id FROM feishu_people
		WHERE user_id IS NOT NULL AND email IS NOT NULL AND feishu_status='active' AND `+where)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		if err := s.StartMailFor(ctx, id, full); err != nil {
			return err
		}
	}
	return nil
}

// RunMigrate is the worker step behind StartMigration. Every step is
// idempotent, so a re-run only picks up what is new or failed.
// worker 执行的迁移主流程；每步幂等，重跑只处理新增或失败的部分。
func (s *Store) RunMigrate(ctx context.Context) error {
	var status, scope string
	var provision, continuous bool
	var by *uuid.UUID
	var wiki *uuid.UUID
	if err := s.pool.QueryRow(ctx,
		`SELECT status,scope,provision,continuous,started_by,wiki_job FROM feishu_migration WHERE id`).
		Scan(&status, &scope, &provision, &continuous, &by, &wiki); err != nil {
		return err
	}
	if status != "running" && status != "syncing" {
		return nil
	}
	mailScope := scope == "all" || scope == "mail"
	docsScope := scope == "all" || scope == "docs"
	fail := func(err error) error {
		_, _ = s.pool.Exec(ctx, `UPDATE feishu_migration SET status='error', last_error=$1 WHERE id`, err.Error())
		return nil
	}
	if _, err := s.SyncDirectory(ctx); err != nil {
		return fail(err)
	}
	if provision && by != nil {
		if n, err := s.Provision(ctx, *by); err != nil {
			return fail(err)
		} else if n > 0 {
			log.Printf("feishu migrate: provisioned %d accounts", n)
		}
	}
	if mailScope {
		if err := s.queueMail(ctx, true, `mail_status IN ('none','error')`); err != nil {
			return fail(err)
		}
	}
	if docsScope {
		if err := s.importAllWiki(ctx, by, wiki); err != nil {
			// Wiki access is optional (the app may not be a member of any space).
			log.Printf("feishu migrate: wiki: %v", err)
		}
		s.notifyDocs(ctx)
	}
	if !mailScope {
		_, err := s.pool.Exec(ctx,
			`UPDATE feishu_migration SET status='finished', finished_at=COALESCE(finished_at, now()) WHERE id`)
		return err
	}
	next := "running"
	if continuous {
		next = "syncing"
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE feishu_migration SET status=$1 WHERE id AND status IN ('running','syncing')`, next)
	return err
}

// importAllWiki imports every wiki space visible to the app as an admin job
// (owners mapped by email); a re-run restarts the same job.
func (s *Store) importAllWiki(ctx context.Context, by, existing *uuid.UUID) error {
	if by == nil {
		return nil
	}
	if existing != nil {
		var st string
		if s.pool.QueryRow(ctx, `SELECT status FROM feishu_import_jobs WHERE id=$1`, *existing).Scan(&st) == nil {
			if st == "running" {
				return nil
			}
			return s.StartJob(ctx, *existing)
		}
	}
	c, err := s.appCreds(ctx)
	if err != nil {
		return err
	}
	spaces, err := s.ListSpaces(ctx, c.appID, c.secret, c.base)
	if err != nil {
		return err
	}
	if len(spaces) == 0 {
		return nil
	}
	roots := make([]Root, 0, len(spaces))
	for _, sp := range spaces {
		roots = append(roots, Root{Kind: "wiki_space", ID: sp["id"], Name: sp["name"]})
	}
	rb, _ := json.Marshal(roots)
	j, err := s.CreateJob(ctx, *by, true, "全部知识库", "admin", rb)
	if err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `UPDATE feishu_migration SET wiki_job=$1 WHERE id`, j.ID); err != nil {
		return err
	}
	return s.StartJob(ctx, j.ID)
}

// notifyDocs tells each linked employee (once) how to bring over their
// personal docs. 每位员工通知一次如何一键导入个人文档。
func (s *Store) notifyDocs(ctx context.Context) {
	rows, err := s.pool.Query(ctx, `SELECT p.open_id FROM feishu_people p
		WHERE p.user_id IS NOT NULL AND p.feishu_status='active' AND p.notified_docs_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM feishu_user_tokens t WHERE t.user_id=p.user_id)`)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		err := s.botText(ctx, id, "公司正在从飞书迁移到 WittyChat，你的邮件会自动搬过去。\n"+
			"个人文档需要你点一下：在 WittyChat 桌面端打开「文档」，点「一键导入我的飞书文档」，按提示同意授权即可。")
		if err != nil {
			log.Printf("feishu migrate: docs notice to %s: %v", id, err)
			continue
		}
		_, _ = s.pool.Exec(ctx, `UPDATE feishu_people SET notified_docs_at=now() WHERE open_id=$1`, id)
	}
}

// Tick runs from the worker scheduler: incremental mail catch-up while
// syncing, a daily directory refresh (new hires get accounts and mail),
// and completion once the finishing pass has drained.
// 定时：同步期增量追平邮件；每日刷新通讯录（新人建号、搬邮件）；收尾跑完即结束。
func (s *Store) Tick(ctx context.Context) error {
	var status, scope string
	var dir *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT status, dir_synced_at, scope FROM feishu_migration WHERE id`).Scan(&status, &dir, &scope); err != nil {
		return nil // no migration configured
	}
	mailScope := scope == "all" || scope == "mail"
	switch status {
	case "syncing":
		if !mailScope {
			return nil
		}
		if dir == nil || time.Since(*dir) > 24*time.Hour {
			return s.enqueueOnce(ctx, "feishu_migrate", map[string]any{})
		}
		return s.queueMail(ctx, false,
			fmt.Sprintf(`mail_status='done' AND (mail_synced_at IS NULL OR mail_synced_at < now() - interval '%d seconds')`,
				int(MailSyncEvery.Seconds())))
	case "finishing":
		var busy int
		_ = s.pool.QueryRow(ctx,
			`SELECT count(*) FROM feishu_people WHERE mail_status IN ('queued','running')`).Scan(&busy)
		if busy == 0 {
			_, err := s.pool.Exec(ctx, `UPDATE feishu_migration SET status='finished', finished_at=now() WHERE id`)
			return err
		}
	}
	return nil
}
