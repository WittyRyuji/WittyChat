-- objects.dims_checked: "this row's dims were settled by the orientation-
-- aware sniffer". Every image row is re-sniffed once by the background
-- sweep — including rows already sized by the request-path sniff, which
-- was blind to EXIF orientation. The column is the negative marker the
-- sweep selects on, so images Go can't decode are marked checked (with
-- NULL dims) instead of being retried forever.
-- objects.dims_checked：标记"本行尺寸已由感知 EXIF 方向的嗅探器定案"。
-- 所有图片行都会被后台清扫重嗅一次——包括请求路径嗅探已填过尺寸的
-- 行（旧嗅探不看 EXIF 方向）。它是清扫器按"未检查"选取的负标记，Go
-- 解不出的图片会被标记为已查（尺寸留 NULL）而不是无限重试。
ALTER TABLE objects ADD COLUMN dims_checked boolean NOT NULL DEFAULT false;
UPDATE objects SET dims_checked = true WHERE mime IS NULL OR mime NOT LIKE 'image/%';
CREATE INDEX objects_dims_pending ON objects (id) WHERE NOT dims_checked;
