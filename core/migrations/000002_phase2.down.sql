DROP TABLE IF EXISTS mail_attachments;
DROP TABLE IF EXISTS mail_messages;
DROP TABLE IF EXISTS mail_folders;
DROP TABLE IF EXISTS mail_addresses;
DROP TABLE IF EXISTS upload_sessions;

ALTER TABLE conv_ops DROP CONSTRAINT conv_ops_kind_check;
ALTER TABLE conv_ops ADD CONSTRAINT conv_ops_kind_check CHECK (kind IN
    ('message', 'edit', 'recall', 'member_add', 'member_remove', 'role_change'));

DROP INDEX IF EXISTS conv_ops_body_trgm;
DROP EXTENSION IF EXISTS pg_trgm;
