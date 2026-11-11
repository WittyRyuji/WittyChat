-- 23: group avatars — a conversation can carry its own avatar object, and
-- changing it is recorded as an `avatar` op like a rename.
-- 23：群头像——会话可设置头像对象，更换记为 avatar 操作（同改群名）。
ALTER TABLE conversations ADD COLUMN avatar_object_id uuid REFERENCES objects(id) ON DELETE SET NULL;
ALTER TABLE conv_ops DROP CONSTRAINT conv_ops_kind_check;
ALTER TABLE conv_ops ADD CONSTRAINT conv_ops_kind_check CHECK (kind IN
    ('message', 'edit', 'recall', 'member_add', 'member_remove', 'role_change',
     'rename', 'transfer', 'reaction', 'avatar'));
