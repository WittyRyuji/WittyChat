DROP TABLE IF EXISTS object_versions;
DROP TABLE IF EXISTS doc_shares;
ALTER TABLE docs DROP COLUMN IF EXISTS deleted_by;
ALTER TABLE docs DROP COLUMN IF EXISTS deleted_at;
-- mail is_read fix is a one-way data correction / 邮件已读修正为单向数据修复
