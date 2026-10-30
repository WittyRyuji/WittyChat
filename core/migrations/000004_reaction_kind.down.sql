DROP INDEX conv_ops_reaction_target;
ALTER TABLE conv_ops DROP CONSTRAINT conv_ops_kind_check;
ALTER TABLE conv_ops ADD CONSTRAINT conv_ops_kind_check CHECK (kind IN
    ('message', 'edit', 'recall', 'member_add', 'member_remove', 'role_change',
     'rename', 'transfer'));
