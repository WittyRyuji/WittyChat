-- widen op-kind set: reaction ops (emoji like/react toggles on messages)
ALTER TABLE conv_ops DROP CONSTRAINT conv_ops_kind_check;
ALTER TABLE conv_ops ADD CONSTRAINT conv_ops_kind_check CHECK (kind IN
    ('message', 'edit', 'recall', 'member_add', 'member_remove', 'role_change',
     'rename', 'transfer', 'reaction'));

-- reaction lookups: aggregate per (target) + dedupe per (target, actor, emoji)
CREATE INDEX conv_ops_reaction_target ON conv_ops (conv_id, target_op_seq)
    WHERE kind = 'reaction';
