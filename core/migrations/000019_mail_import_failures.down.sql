-- down: drop failure journal + processed counter / 回滚：删失败表与计数列
DROP TABLE IF EXISTS mail_import_failures;
ALTER TABLE mail_import_folders DROP COLUMN IF EXISTS msgs_processed;
