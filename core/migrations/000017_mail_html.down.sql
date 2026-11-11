-- down: drop mail html/inline columns / 回滚：删除邮件 HTML 与内联列
ALTER TABLE mail_messages DROP COLUMN html_body;
ALTER TABLE mail_attachments DROP COLUMN content_id;
ALTER TABLE mail_attachments DROP COLUMN inline;
