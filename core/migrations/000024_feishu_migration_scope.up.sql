-- 24: Feishu migration scope — mail and document migrations are separate
-- features; 'all' preserves the original whole-tenant pipeline.
-- 24：飞书迁移范围——邮件搬家与文档迁移是两类功能；all 保留原整体流程。
ALTER TABLE feishu_migration ADD COLUMN scope text NOT NULL DEFAULT 'all';
ALTER TABLE feishu_migration ADD CONSTRAINT feishu_migration_scope_check
  CHECK (scope IN ('all','mail','docs'));
