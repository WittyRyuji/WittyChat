-- 22: whole-tenant Feishu migration — directory snapshot (people), automatic
-- account provisioning, per-person mail migration through the Feishu mail API
-- with resumable folder cursors and a failure journal, per-employee OAuth for
-- personal docs, and the singleton migration state.
-- 22：飞书整体迁移——通讯录快照、自动建号、按人邮件搬迁（游标可续、失败台账）、
-- 员工授权导入个人文档、迁移全局状态。
CREATE TABLE feishu_people (
  open_id text PRIMARY KEY,
  name text NOT NULL DEFAULT '',
  email text,
  feishu_status text NOT NULL DEFAULT 'active',
  user_id uuid REFERENCES users(id) ON DELETE SET NULL,
  provisioned boolean NOT NULL DEFAULT false,
  link_sent_at timestamptz,
  link_error text,
  mail_status text NOT NULL DEFAULT 'none',
  mail_error text,
  mail_done int NOT NULL DEFAULT 0,
  mail_skipped int NOT NULL DEFAULT 0,
  mail_failed int NOT NULL DEFAULT 0,
  mail_full boolean NOT NULL DEFAULT true,
  mail_synced_at timestamptz,
  notified_docs_at timestamptz,
  synced_at timestamptz NOT NULL DEFAULT now(),
  CHECK (feishu_status IN ('active','resigned','frozen','inactive','missing')),
  CHECK (mail_status IN ('none','queued','running','done','error'))
);
CREATE INDEX feishu_people_user ON feishu_people (user_id);
CREATE UNIQUE INDEX feishu_people_email ON feishu_people (lower(email)) WHERE email IS NOT NULL;

CREATE TABLE feishu_mail_cursors (
  open_id text NOT NULL REFERENCES feishu_people(open_id) ON DELETE CASCADE,
  folder_id text NOT NULL,
  page_token text NOT NULL DEFAULT '',
  finished boolean NOT NULL DEFAULT false,
  PRIMARY KEY (open_id, folder_id)
);

CREATE TABLE feishu_mail_failures (
  open_id text NOT NULL REFERENCES feishu_people(open_id) ON DELETE CASCADE,
  message_id text NOT NULL,
  folder_id text NOT NULL,
  error text NOT NULL,
  attempts int NOT NULL DEFAULT 1,
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (open_id, message_id)
);

CREATE TABLE feishu_migration (
  id boolean PRIMARY KEY DEFAULT true CHECK (id),
  status text NOT NULL DEFAULT 'idle',
  provision boolean NOT NULL DEFAULT true,
  continuous boolean NOT NULL DEFAULT true,
  started_by uuid REFERENCES users(id),
  started_at timestamptz,
  dir_synced_at timestamptz,
  finished_at timestamptz,
  wiki_job uuid,
  last_error text,
  CHECK (status IN ('idle','running','syncing','finishing','finished','error'))
);

CREATE TABLE feishu_user_tokens (
  user_id uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  open_id text NOT NULL,
  sealed_refresh bytea NOT NULL,
  refresh_expires_at timestamptz,
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE feishu_oauth_states (
  state text PRIMARY KEY,
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at timestamptz NOT NULL
);

ALTER TABLE feishu_import_jobs ADD COLUMN user_token boolean NOT NULL DEFAULT false;
