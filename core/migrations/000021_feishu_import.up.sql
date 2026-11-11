-- 21: Feishu cloud-doc import — a workspace-level self-built app credential
-- (set once by an admin), per-user self-import jobs (every employee imports
-- their own docs; admin mode maps owners by email).
-- 21：飞书云文档导入——工作区级自建应用凭据（管理员配置一次）；员工各自导入。
CREATE TABLE feishu_app (
  id boolean PRIMARY KEY DEFAULT true CHECK (id),
  app_id text NOT NULL, sealed_secret bytea NOT NULL,
  base_url text NOT NULL DEFAULT 'https://open.feishu.cn',
  updated_by uuid REFERENCES users(id), updated_at timestamptz NOT NULL DEFAULT now());
CREATE TABLE feishu_import_jobs (
  id uuid PRIMARY KEY, name text NOT NULL,
  mode text NOT NULL CHECK (mode IN ('self','admin')),
  roots jsonb NOT NULL, status text NOT NULL DEFAULT 'draft', last_error text,
  created_by uuid NOT NULL REFERENCES users(id),
  discovered boolean NOT NULL DEFAULT false, discover_done int[] NOT NULL DEFAULT '{}',
  created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
  CHECK (status IN ('draft','running','paused','done','cancelled','error')));
CREATE TABLE feishu_import_items (
  job_id uuid NOT NULL REFERENCES feishu_import_jobs(id) ON DELETE CASCADE,
  obj_type text NOT NULL, obj_token text NOT NULL, title text NOT NULL, path text NOT NULL DEFAULT '',
  owner_open_id text, status text NOT NULL DEFAULT 'pending', error text,
  doc_ids uuid[] NOT NULL DEFAULT '{}', updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (job_id, obj_type, obj_token),
  CHECK (status IN ('pending','done','skipped','failed')));
