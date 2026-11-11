package httpapi

// Attachment endpoints: direct upload (<=5MB), member-authorized download,
// thumbnail passthrough. Upload enqueues a thumb task for images.
//
// 附件端点：直传（≤5MB）、会话成员鉴权下载、缩略图。
// 图片上传后入队缩略图任务。

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"wittychat/core/audit"
	"wittychat/core/objects"
	"wittychat/core/settings"

	"github.com/aws/aws-sdk-go-v2/aws"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/google/uuid"
)

func registerObjects(mux *http.ServeMux, d *Deps) {
	mux.HandleFunc("POST /api/v1/conversations/{id}/attachments", d.uploadAttachment)
	mux.HandleFunc("POST /api/v1/objects/upload", d.uploadObject) // purpose=mail etc, no conv
	mux.HandleFunc("GET /api/v1/objects/{id}/download", d.downloadObject)
	mux.HandleFunc("GET /api/v1/objects/{id}/thumb", d.thumbObject)
	mux.HandleFunc("POST /api/v1/objects/{id}/thumb", d.uploadThumb) // client-side thumbs / 客户端缩略图
	mux.HandleFunc("GET /api/v1/admin/objects", d.adminObjects)
	mux.HandleFunc("POST /api/v1/admin/objects/purge", d.adminObjectsPurge)
	mux.HandleFunc("GET /api/v1/objects/{id}/url", d.signedObjectURL)
	mux.HandleFunc("POST /api/v1/objects/multipart/init", d.multipartInit)
	mux.HandleFunc("POST /api/v1/objects/multipart/complete", d.multipartComplete)
	mux.HandleFunc("GET /api/v1/objects/quota", d.storageQuota)
	mux.HandleFunc("GET /api/v1/workspace/logo", d.workspaceLogo)
}

// objectUser authorizes by session bearer OR a valid exp+sig signature
// (signed URLs for <img>/mail contexts / 短时效签名 URL 场景).
func (d *Deps) objectUser(r *http.Request, objID uuid.UUID) (uuid.UUID, error) {
	if sig := r.URL.Query().Get("sig"); sig != "" && d.Signer != nil {
		if d.Signer.Verify(objID, r.URL.Query().Get("exp"), sig) {
			// Possession of a valid signature is itself the authorization
			// (like s3 presigned URLs). Serve as the owner so Get() passes.
			obj, err := d.Objects.Peek(r.Context(), objID)
			if err == nil {
				return obj.Owner, nil
			}
		}
	}
	return d.bearerUser(r)
}

func (d *Deps) bearerUser(r *http.Request) (uuid.UUID, error) {
	header := r.Header.Get("Authorization")
	if header == "" {
		if tok := r.URL.Query().Get("token"); tok != "" {
			header = "Bearer " + tok // <img>/<a> 不能带 header 的场景
		}
	}
	_, u, err := d.requireUser(r.Context(), header)
	if err != nil {
		return uuid.Nil, err
	}
	return u.ID, nil
}

func (d *Deps) uploadAttachment(w http.ResponseWriter, r *http.Request) {
	uid, err := d.bearerUser(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	convID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad conv id", http.StatusBadRequest)
		return
	}
	if !d.IM.IsMember(r.Context(), convID, uid) {
		http.Error(w, "not a member", http.StatusForbidden)
		return
	}
	var maxBytes int64 = objects.MaxDirectUpload
	quotaBytes := int64(0)
	if d.Settings != nil {
		maxBytes = d.Settings.Int(r.Context(), settings.KeyStorageUploadMax, settings.DefUploadMaxMB) << 20
		quotaBytes = d.Settings.Int(r.Context(), settings.KeyStorageQuota, 0)
	}
	if quotaBytes > 0 {
		used, err := d.Objects.UsedBytes(r.Context())
		if err == nil && used >= quotaBytes {
			http.Error(w, "storage quota exceeded / 存储配额已满", http.StatusInsufficientStorage)
			return
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+1<<20)
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file / 缺少文件", http.StatusBadRequest)
		return
	}
	defer func() { _ = file.Close() }()

	obj, dedup, err := d.Objects.Put(r.Context(), uid, convID, "attachment",
		header.Filename, header.Header.Get("Content-Type"), file, maxBytes)
	if err != nil {
		if errors.Is(err, objects.ErrTooLarge) {
			http.Error(w, "file too large / 文件过大", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "upload failed", http.StatusInternalServerError)
		return
	}
	// Antivirus scan hook for every new object (noop scanner by default).
	if !dedup {
		_ = d.Objects.Enqueue(r.Context(), "avscan", map[string]any{
			"object_id": obj.ID.String(),
		})
	}
	// Image → enqueue thumbnail (worker picks it up).
	if !dedup && strings.HasPrefix(obj.Mime, "image/") {
		_ = d.Objects.Enqueue(r.Context(), "thumb", map[string]any{
			"object_id": obj.ID.String(), "owner": uid.String(),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": obj.ID.String(), "name": obj.Name, "size": obj.Size,
		"mime": obj.Mime, "deduplicated": dedup, "w": obj.Width, "h": obj.Height,
	})
}

func (d *Deps) downloadObject(w http.ResponseWriter, r *http.Request) {
	objID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	uid, err := d.objectUser(r, objID)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	obj, err := d.canReadObject(r.Context(), objID, uid)
	if err != nil {
		if errors.Is(err, objects.ErrForbidden) {
			http.Error(w, "forbidden", http.StatusForbidden)
		} else {
			http.Error(w, "not found", http.StatusNotFound)
		}
		return
	}
	rc, err := d.Objects.Open(r.Context(), obj)
	if err != nil {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	defer func() { _ = rc.Close() }()
	if obj.Mime != "" {
		w.Header().Set("Content-Type", obj.Mime)
	}
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Header().Set("Content-Disposition", `attachment; filename*=UTF-8''`+escapeName(obj.Name))
	// Dims are backfilled by the store sweep (dims_checked), never on the
	// request path — a DecodeConfig here consumed header bytes the copy
	// then lost, and each retry re-served the same truncated body from
	// the private cache. / 尺寸由存储层后台清扫回填（dims_checked），
	// 请求路径不嗅探——此前的 DecodeConfig 会吃掉头部字节导致响应截断，
	// 且私有缓存让重试反复拿到同一份残体。
	_, _ = io.Copy(w, rc) // stream / 流式
}

// canReadObject is THE object read check: owner / object-conv member, or
// a reference the user legitimately holds (mail copy, HR request, op in a
// conv they belong to). References are only trusted because every write
// path that creates one validates the writer could read the object first
// (requireReadableObjects) — keep those two in lockstep.
// canReadObject 是唯一的对象读权限判定：属主/对象会话成员，或用户合法
// 持有的引用（邮件副本、HR 申请、所在会话的消息）。引用之所以可信，是
// 因为所有产生引用的写入口都先校验写入者可读（requireReadableObjects）。
func (d *Deps) canReadObject(ctx context.Context, objID, uid uuid.UUID) (*objects.Object, error) {
	obj, err := d.Objects.Get(ctx, objID, uid)
	if err == nil {
		if obj.Owner == uid {
			return obj, nil
		}
		// Conv-member path: once every message referencing the object was
		// recalled, the attachment must stop being readable ("撤回消息附件
		// 留审计不可见"). An object never sent (no refs) stays readable by
		// its conv like any other pending upload.
		// 成员路径：引用该对象的消息全部被撤回后不再可读（撤回附件留审计
		// 不可见）。从未被引用的对象（待发送）沿用对象会话权限。
		if has, live := d.objectRefState(ctx, objID); !has || live {
			return obj, nil
		}
	} else if !errors.Is(err, objects.ErrForbidden) {
		return obj, err
	}
	if d.objectViaMail(ctx, objID, uid) || d.objectViaHR(ctx, objID, uid) ||
		d.objectViaAttachmentRef(ctx, objID, uid) {
		return d.Objects.Peek(ctx, objID)
	}
	return nil, objects.ErrForbidden
}

// objectRefState reports (has attachment_refs, has ≥1 live ref): a ref is
// dead once its message op was recalled. 返回（存在引用，存在未撤回引用）。
func (d *Deps) objectRefState(ctx context.Context, objID uuid.UUID) (has, live bool) {
	err := d.Pool.QueryRow(ctx, `
		SELECT count(*) > 0,
		       COALESCE(bool_or(NOT EXISTS (
		         SELECT 1 FROM conv_ops x
		         WHERE x.conv_id = r.conv_id AND x.kind='recall' AND x.target_op_seq = r.op_seq
		       )), false)
		FROM attachment_refs r WHERE r.object_id = $1`, objID).Scan(&has, &live)
	if err != nil {
		return false, true // unknown → don't withhold / 查询失败不收紧
	}
	return has, live
}

// errObjectNotReadable flags a write that references an object the writer
// cannot read — attaching an id must never grant access by itself.
// 写入引用了写入者不可读的对象——附上 id 本身绝不能授予访问权。
var errObjectNotReadable = errors.New("object not readable by sender / 无权引用该附件")

// requireReadableObjects validates every object id a user attaches (IM
// message, mail, HR request) before a reference row is created.
// 附件引用写入前逐个校验写入者可读（消息/邮件/HR 申请）。
func (d *Deps) requireReadableObjects(ctx context.Context, uid uuid.UUID, ids []uuid.UUID) error {
	for _, id := range ids {
		if _, err := d.canReadObject(ctx, id, uid); err != nil {
			return errObjectNotReadable
		}
	}
	return nil
}

// objectViaMail: requester owns a mail message that references this object
// (mail attachments are readable by the mailbox holder, not just the uploader).
// objectViaMail：请求者持有引用此对象的邮件（邮件附件对收发件人可读）。
func (d *Deps) objectViaMail(ctx context.Context, objID, uid uuid.UUID) bool {
	var ok bool
	err := d.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM mail_attachments a
		 JOIN mail_messages m ON m.id = a.message_id
		 WHERE a.object_id=$1 AND m.owner_user_id=$2)`, objID, uid).Scan(&ok)
	return err == nil && ok
}

// objectViaAttachmentRef: object is referenced by an op in a conv the user
// belongs to (stickers + reshared attachments carry no object-level conv_id).
// objectViaAttachmentRef：对象被用户所在会话的某条消息引用（sticker/转发
// 附件对象本身不绑 conv）。
func (d *Deps) objectViaAttachmentRef(ctx context.Context, objID, uid uuid.UUID) bool {
	var ok bool
	err := d.Pool.QueryRow(ctx,
		`SELECT EXISTS(
			SELECT 1 FROM attachment_refs r
			JOIN conv_members m ON m.conv_id = r.conv_id AND m.user_id = $2
			 AND m.left_at IS NULL
			WHERE r.object_id = $1
			  AND NOT EXISTS (
			    SELECT 1 FROM conv_ops x
			    WHERE x.conv_id = r.conv_id AND x.kind='recall'
			      AND x.target_op_seq = r.op_seq))`, objID, uid).Scan(&ok)
	return err == nil && ok
}

// objectViaHR: object is referenced by an hr_requests payload and the caller
// is the requester or an admin (approver must read attached receipts).
// objectViaHR：对象被某 HR 申请引用且请求人是申请人或管理员（审批人需看附件）。
func (d *Deps) objectViaHR(ctx context.Context, objID, uid uuid.UUID) bool {
	var ok bool
	err := d.Pool.QueryRow(ctx,
		`SELECT EXISTS(
			SELECT 1 FROM hr_requests hr
			JOIN users u ON u.id = $2
			WHERE hr.payload->'attachment_object_ids' ? $1::text
			  AND (hr.requester = $2 OR u.is_admin))`, objID.String(), uid).Scan(&ok)
	return err == nil && ok
}

func (d *Deps) thumbObject(w http.ResponseWriter, r *http.Request) {
	objID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	uid, err := d.objectUser(r, objID)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if _, err := d.canReadObject(r.Context(), objID, uid); err != nil {
		if errors.Is(err, objects.ErrForbidden) {
			http.Error(w, "forbidden", http.StatusForbidden)
		} else {
			http.Error(w, "not found", http.StatusNotFound)
		}
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=300")
	// Path derived from a parsed uuid under a fixed data dir — safe.
	//nolint:gosec // no user-controlled path component
	p := d.Objects.ThumbPath(objID)
	//nolint:gosec // path derived from parsed uuid under fixed data dir
	if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
		p = d.Objects.ThumbPathLegacy(objID) // pre-partition thumbs / 旧布局兜底
	}
	//nolint:gosec // same constraint as above — no user-controlled component
	http.ServeFile(w, r, p)
}

// POST /objects/{id}/thumb — client-generated thumbnail upload. Owner only:
// the uploader produces it at send time, cheaper than the worker path.
// 客户端生成的缩略图上传：仅对象属主（发送时顺带压缩，比 worker 解码轻）。
func (d *Deps) uploadThumb(w http.ResponseWriter, r *http.Request) {
	uid, err := d.bearerUser(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	objID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	obj, err := d.Objects.Peek(r.Context(), objID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if obj.Owner != uid {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20) // thumbs must stay tiny / 缩略图限 2MB
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file", http.StatusBadRequest)
		return
	}
	defer func() { _ = file.Close() }()
	if mime := header.Header.Get("Content-Type"); !strings.HasPrefix(mime, "image/") {
		http.Error(w, "thumb must be an image", http.StatusBadRequest)
		return
	}
	if err := d.Objects.WriteThumb(objID, file); err != nil {
		http.Error(w, "write failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// GET /admin/objects?owner=<email>&from=<YYYY-MM-DD>&to=<YYYY-MM-DD>
// Storage governance listing — every attachment has an owner; admins can
// scan and purge by owner+day. 附件归属管理：按属主/日期列出与清理。
func (d *Deps) adminObjects(w http.ResponseWriter, r *http.Request) {
	if _, _, err := d.requireAdmin(r.Context(), r.Header.Get("Authorization")); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	owner, from, to, err := d.objectFilter(w, r)
	if err != nil {
		return
	}
	list, err := d.Objects.AdminList(r.Context(), owner, from, to)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"objects": list})
}

// POST /admin/objects/purge {owner_email, day} — soft-delete that owner's
// objects created on the day, remove files/thumbs, detach references.
// 按 属主+日期 清理附件：软删对象、删文件与缩略图、解除引用，留审计。
func (d *Deps) adminObjectsPurge(w http.ResponseWriter, r *http.Request) {
	_, admin, err := d.requireAdmin(r.Context(), r.Header.Get("Authorization"))
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		Owner string `json:"owner_email"` // empty = all owners / 空 = 全部属主
		Day   string `json:"day"`         // YYYY-MM-DD
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	day, err := time.Parse("2006-01-02", req.Day)
	if err != nil {
		http.Error(w, "bad day (YYYY-MM-DD)", http.StatusBadRequest)
		return
	}
	owner := uuid.Nil
	if req.Owner != "" {
		u, err := d.Store.UserByEmail(r.Context(), req.Owner)
		if err != nil {
			http.Error(w, "owner not found", http.StatusNotFound)
			return
		}
		owner = u.ID
	}
	deleted, err := d.Objects.AdminPurge(r.Context(), owner, day, day.AddDate(0, 0, 1))
	if err != nil {
		http.Error(w, "purge failed", http.StatusInternalServerError)
		return
	}
	for i := range deleted {
		// Reference-aware cleanup — another live object may share the
		// same storage key (identical content, same day). / 引用感知
		// 清理——同内容的在用对象可能共享同一存储 key。
		d.Objects.RemoveFiles(r.Context(), &deleted[i])
	}
	d.Audit.Log(r.Context(), &admin.ID, audit.ActorAdmin, "admin.objects.purge", req.Owner,
		map[string]string{"day": req.Day, "count": fmt.Sprint(len(deleted))}, clientIPCtx(r.Context()))
	_ = json.NewEncoder(w).Encode(map[string]any{"deleted": len(deleted)})
}

func (d *Deps) objectFilter(w http.ResponseWriter, r *http.Request) (uuid.UUID, time.Time, time.Time, error) {
	q := r.URL.Query()
	owner := uuid.Nil
	if em := q.Get("owner"); em != "" {
		u, err := d.Store.UserByEmail(r.Context(), em)
		if err != nil {
			http.Error(w, "owner not found", http.StatusNotFound)
			return owner, time.Time{}, time.Time{}, err
		}
		owner = u.ID
	}
	from, err := time.Parse("2006-01-02", q.Get("from"))
	if err != nil {
		from = time.Now().UTC().AddDate(0, 0, -30)
	}
	to, err := time.Parse("2006-01-02", q.Get("to"))
	if err != nil {
		to = time.Now().UTC().AddDate(0, 0, 1) // default: through today / 缺省含今天
	}
	return owner, from.UTC(), to.UTC(), nil
}

// POST /objects/upload — direct upload not tied to a conversation
// (mail attachments, future purposes). Same streaming/quota/dedup path.
func (d *Deps) uploadObject(w http.ResponseWriter, r *http.Request) {
	uid, err := d.bearerUser(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var maxBytes int64 = objects.MaxDirectUpload
	quotaBytes := int64(0)
	if d.Settings != nil {
		maxBytes = d.Settings.Int(r.Context(), settings.KeyStorageUploadMax, settings.DefUploadMaxMB) << 20
		quotaBytes = d.Settings.Int(r.Context(), settings.KeyStorageQuota, 0)
	}
	if quotaBytes > 0 {
		if used, err := d.Objects.UsedBytes(r.Context()); err == nil && used >= quotaBytes {
			http.Error(w, "storage quota exceeded / 存储配额已满", http.StatusInsufficientStorage)
			return
		}
	}
	purpose := r.URL.Query().Get("purpose")
	if purpose == "" {
		purpose = "mail"
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+1<<20)
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file / 缺少文件", http.StatusBadRequest)
		return
	}
	defer func() { _ = file.Close() }()
	obj, dedup, err := d.Objects.Put(r.Context(), uid, uuid.Nil, purpose,
		header.Filename, header.Header.Get("Content-Type"), file, maxBytes)
	if err != nil {
		if errors.Is(err, objects.ErrTooLarge) {
			http.Error(w, "file too large / 文件过大", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "upload failed", http.StatusInternalServerError)
		return
	}
	if !dedup {
		_ = d.Objects.Enqueue(r.Context(), "avscan", map[string]any{"object_id": obj.ID.String()})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": obj.ID.String(), "name": obj.Name, "size": obj.Size,
		"mime": obj.Mime, "deduplicated": dedup,
	})
}

// GET /objects/{id}/url → {url} a short-lived signed download URL.
func (d *Deps) signedObjectURL(w http.ResponseWriter, r *http.Request) {
	uid, err := d.bearerUser(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	objID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if _, err := d.canReadObject(r.Context(), objID, uid); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if d.Signer == nil {
		http.Error(w, "signing disabled", http.StatusServiceUnavailable)
		return
	}
	exp, sig := d.Signer.Params(objID, 15*time.Minute)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"url": fmt.Sprintf("/api/v1/objects/%s/download?exp=%d&sig=%s", objID, exp, sig),
	})
}

// POST /objects/multipart/init {conv_id,name,size,mime,sha256}
// → {object_id,upload_id,part_size,urls[],deduplicated}
func (d *Deps) multipartInit(w http.ResponseWriter, r *http.Request) {
	uid, err := d.bearerUser(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		ConvID uuid.UUID `json:"conv_id"`
		Name   string    `json:"name"`
		Size   int64     `json:"size"`
		Mime   string    `json:"mime"`
		SHA256 string    `json:"sha256"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" || req.Size <= 0 {
		http.Error(w, "bad request / 请求无效", http.StatusBadRequest)
		return
	}
	if !d.IM.IsMember(r.Context(), req.ConvID, uid) {
		http.Error(w, "not a member", http.StatusForbidden)
		return
	}
	sha, err := objects.ParseSHA(req.SHA256)
	if err != nil {
		http.Error(w, "sha256 must be 64-hex / sha256 需为64位十六进制", http.StatusBadRequest)
		return
	}
	up, urls, err := d.Objects.BeginMultipart(r.Context(), uid, req.ConvID,
		"attachment", req.Name, req.Mime, req.Size, sha)
	if errors.Is(err, objects.ErrS3Disabled) {
		http.Error(w, "multipart disabled: use direct upload / 未启用S3,请用直传", http.StatusNotImplemented)
		return
	}
	if err != nil {
		http.Error(w, "init failed", http.StatusInternalServerError)
		return
	}
	if len(urls) == 0 {
		// Dedup hit: object already exists, nothing to upload.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object_id": up.ObjectID.String(), "deduplicated": true,
		})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"object_id": up.ObjectID.String(), "upload_id": up.UploadID,
		"part_size": up.PartSize, "urls": urls, "expires_at": up.ExpiresAt,
	})
}

// POST /objects/multipart/complete {object_id, parts:[{part_number,etag}]}
func (d *Deps) multipartComplete(w http.ResponseWriter, r *http.Request) {
	uid, err := d.bearerUser(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		ObjectID uuid.UUID `json:"object_id"`
		Parts    []struct {
			PartNumber int32  `json:"part_number"`
			ETag       string `json:"etag"`
		} `json:"parts"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ObjectID == uuid.Nil {
		http.Error(w, "bad request / 请求无效", http.StatusBadRequest)
		return
	}
	etags := make([]s3types.CompletedPart, len(req.Parts))
	for i, p := range req.Parts {
		etags[i] = s3types.CompletedPart{
			PartNumber: aws.Int32(p.PartNumber), ETag: aws.String(p.ETag),
		}
	}
	obj, err := d.Objects.CompleteMultipart(r.Context(), req.ObjectID, uid, etags)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, objects.ErrForbidden) {
			status = http.StatusForbidden
		} else if errors.Is(err, objects.ErrNotFound) {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}
	if strings.HasPrefix(obj.Mime, "image/") {
		_ = d.Objects.Enqueue(r.Context(), "thumb", map[string]any{
			"object_id": obj.ID.String(), "owner": uid.String(),
		})
	}
	_ = d.Objects.Enqueue(r.Context(), "avscan", map[string]any{"object_id": obj.ID.String()})
	_ = json.NewEncoder(w).Encode(map[string]any{"id": obj.ID.String(), "status": obj.Status})
}

// GET /objects/quota → {used_bytes, quota_bytes}
func (d *Deps) storageQuota(w http.ResponseWriter, r *http.Request) {
	if _, err := d.bearerUser(r); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	used, err := d.Objects.UsedBytes(r.Context())
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	quota := int64(0)
	if d.Settings != nil {
		quota = d.Settings.Int(r.Context(), settings.KeyStorageQuota, 0)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"used_bytes": used, "quota_bytes": quota})
}

func escapeName(s string) string {
	// RFC 5987 minimal: percent-encode non-ASCII for the header.
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 0x20 && c < 0x7f && c != '"' && c != '%' {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			const hexd = "0123456789ABCDEF"
			b.WriteByte(hexd[c>>4])
			b.WriteByte(hexd[c&0xf])
		}
	}
	return b.String()
}

// GET /workspace/logo — the org logo set via admin settings
// (workspace.logo_object_id), or the built-in dragon mark when unset.
// Any workspace member may read it; logo is org-shared rather than
// owner-scoped, so normal object ACL doesn't apply.
// 组织 Logo：管理员绑定的对象，未配置时返回内置默认图；任意成员可读。
//
//go:embed default_logo.png
var defaultLogoPNG []byte

func (d *Deps) workspaceLogo(w http.ResponseWriter, r *http.Request) {
	if _, err := d.bearerUser(r); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	raw := d.Settings.Str(r.Context(), "workspace.logo_object_id", "")
	objID, err := uuid.Parse(raw)
	if raw == "" || err != nil {
		// No custom logo configured — serve the built-in default so every
		// workspace has a recognizable mark out of the box.
		// 未配置自定义 logo 时返回内置默认图，开箱即有标识。
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(defaultLogoPNG)
		return
	}
	obj, err := d.Objects.Peek(r.Context(), objID)
	if err != nil {
		http.Error(w, "no logo", http.StatusNotFound)
		return
	}
	rc, err := d.Objects.Open(r.Context(), obj)
	if err != nil {
		http.Error(w, "read failed", http.StatusInternalServerError)
		return
	}
	defer func() { _ = rc.Close() }()
	mime := obj.Mime
	if !strings.HasPrefix(mime, "image/") {
		mime = "image/png"
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "private, max-age=300")
	_, _ = io.Copy(w, rc)
}
