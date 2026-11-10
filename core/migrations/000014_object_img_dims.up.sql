-- objects.width/height: decoded image pixel dims captured at upload and
-- lazily backfilled on first download. Attachment payloads carry them so
-- the client can reserve the exact display box BEFORE bytes arrive —
-- kills the first-view layout jump that the localStorage dim memory
-- could only fix on remount.
-- objects.width/height：上传时解析、首次下载惰性回填的图片像素尺寸。
-- 附件负载携带它们，客户端在字节到达前就能预留精确显示盒——根治
-- localStorage 尺寸记忆只能管"看过一次"的首载跳动。
ALTER TABLE objects ADD COLUMN width int;
ALTER TABLE objects ADD COLUMN height int;
