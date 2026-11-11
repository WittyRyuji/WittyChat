-- 18: IMAP mailbox import — jobs/accounts/folder progress + per-owner
-- import dedup key. Secrets sealed with the workspace secret key.
-- 18：IMAP 邮箱导入——任务/账号/文件夹进度三表 + 按属主的导入去重键。
-- 密码用工作区密钥加密落库。
CREATE TABLE mail_import_jobs (
  id uuid PRIMARY KEY, name text NOT NULL, provider text NOT NULL DEFAULT 'custom',
  host text NOT NULL, port int NOT NULL CHECK (port BETWEEN 1 AND 65535),
  tls_mode text NOT NULL CHECK (tls_mode IN ('tls','starttls','plain')),
  since date, add_alias boolean NOT NULL DEFAULT true,
  status text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','running','paused','done','cancelled')),
  secrets_purged boolean NOT NULL DEFAULT false,
  created_by uuid NOT NULL REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now());
CREATE TABLE mail_import_accounts (
  id uuid PRIMARY KEY, job_id uuid NOT NULL REFERENCES mail_import_jobs(id) ON DELETE CASCADE,
  source_login text NOT NULL, sealed_secret bytea, target_user uuid NOT NULL REFERENCES users(id),
  status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','queued','running','done','error','paused','cancelled')),
  msgs_total int NOT NULL DEFAULT 0, msgs_done int NOT NULL DEFAULT 0, msgs_skipped int NOT NULL DEFAULT 0,
  msgs_failed int NOT NULL DEFAULT 0, bytes_done bigint NOT NULL DEFAULT 0,
  last_error text, note text, started_at timestamptz, finished_at timestamptz,
  updated_at timestamptz NOT NULL DEFAULT now(), UNIQUE (job_id, source_login));
CREATE TABLE mail_import_folders (
  account_id uuid NOT NULL REFERENCES mail_import_accounts(id) ON DELETE CASCADE,
  source_name text NOT NULL, target_folder_id uuid NOT NULL REFERENCES mail_folders(id),
  uidvalidity bigint NOT NULL DEFAULT 0, last_uid bigint NOT NULL DEFAULT 0,
  msgs_total int NOT NULL DEFAULT 0, msgs_done int NOT NULL DEFAULT 0,
  PRIMARY KEY (account_id, source_name));
ALTER TABLE mail_messages ADD COLUMN import_key text;
CREATE UNIQUE INDEX mail_messages_import_key ON mail_messages (owner_user_id, import_key) WHERE import_key IS NOT NULL;
