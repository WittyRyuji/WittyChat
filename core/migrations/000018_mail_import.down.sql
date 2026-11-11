-- down: drop mail import tables + dedup column / 回滚：删除导入表与去重列
DROP INDEX IF EXISTS mail_messages_import_key;
ALTER TABLE mail_messages DROP COLUMN IF EXISTS import_key;
DROP TABLE IF EXISTS mail_import_folders;
DROP TABLE IF EXISTS mail_import_accounts;
DROP TABLE IF EXISTS mail_import_jobs;
