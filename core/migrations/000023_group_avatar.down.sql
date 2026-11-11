DELETE FROM conv_ops WHERE kind = 'avatar';
ALTER TABLE conv_ops DROP CONSTRAINT conv_ops_kind_check;
ALTER TABLE conv_ops ADD CONSTRAINT conv_ops_kind_check CHECK (kind IN
    ('message', 'edit', 'recall', 'member_add', 'member_remove', 'role_change',
     'rename', 'transfer', 'reaction'));
ALTER TABLE conversations DROP COLUMN IF EXISTS avatar_object_id;
