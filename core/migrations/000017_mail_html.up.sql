-- 17: HTML mail + inline attachments. Migrated/IMAP mail carries real
-- HTML bodies and cid-referenced inline images — store them.
-- 17：HTML 邮件与内联附件：迁移/IMAP 邮件有真 HTML 正文和 cid 引用内联
-- 图片——入库保存。
ALTER TABLE mail_messages ADD COLUMN html_body text NOT NULL DEFAULT '';
ALTER TABLE mail_attachments ADD COLUMN content_id text;
ALTER TABLE mail_attachments ADD COLUMN inline boolean NOT NULL DEFAULT false;
