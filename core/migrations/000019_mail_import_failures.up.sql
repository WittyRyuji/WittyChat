-- 19: import failure journal — a per-message failure must never silently
-- drop mail; failures retry on the next pass while last_uid advances past
-- them so nothing blocks. msgs_processed makes totals exact under chunking.
-- 19：导入失败台账——单封失败不能丢邮件；last_uid 照常前进不阻塞，下一轮
-- 重试失败记录。msgs_processed 让分块运行时计数不膨胀。
CREATE TABLE mail_import_failures (
  account_id uuid NOT NULL REFERENCES mail_import_accounts(id) ON DELETE CASCADE,
  source_name text NOT NULL, uidvalidity bigint NOT NULL, uid bigint NOT NULL,
  error text NOT NULL, attempts int NOT NULL DEFAULT 1, updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (account_id, source_name, uidvalidity, uid));
ALTER TABLE mail_import_folders ADD COLUMN msgs_processed int NOT NULL DEFAULT 0;
