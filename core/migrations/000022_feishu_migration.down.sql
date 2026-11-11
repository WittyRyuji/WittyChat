ALTER TABLE feishu_import_jobs DROP COLUMN IF EXISTS user_token;
DROP TABLE IF EXISTS feishu_oauth_states;
DROP TABLE IF EXISTS feishu_user_tokens;
DROP TABLE IF EXISTS feishu_migration;
DROP TABLE IF EXISTS feishu_mail_failures;
DROP TABLE IF EXISTS feishu_mail_cursors;
DROP TABLE IF EXISTS feishu_people;
