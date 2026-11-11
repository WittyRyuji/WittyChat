// WittyChat worker: claims bg_tasks and processes them.
// First pipeline task: image thumbnails (thumb kind).
//
// WittyChat worker：认领 bg_tasks 并处理。
// 首个流水线任务：图片缩略图（thumb 类型）。
package main

import (
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // register decoders / 注册解码器
	"image/jpeg"
	_ "image/png"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
	"wittychat/core/auth"
	"wittychat/core/db"
	"wittychat/core/docs"
	"wittychat/core/feishuimport"
	"wittychat/core/im"
	"wittychat/core/mail"
	"wittychat/core/mailimport"
	"wittychat/core/objects"
	"wittychat/core/settings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/image/draw"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

const thumbMax = 256 // px, longest side / 最长边

func main() {
	pool, err := db.Open(context.Background(),
		envOr("WITTYCHAT_DB_URL", "postgres://wittychat:wittychat@localhost:5432/wittychat?sslmode=disable"))
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer pool.Close()

	store, err := objects.NewStore(pool, envOr("WITTYCHAT_DATA_DIR", "./data"))
	if err != nil {
		log.Fatalf("objects: %v", err)
	}
	store.EnableS3(objects.NewS3(objects.S3ConfigFromEnv(), envOr("WITTYCHAT_DATA_DIR", "./data")))

	hostname, _ := os.Hostname()
	w := &worker{pool: pool, store: store, name: "worker-" + hostname, scanner: objects.NoopScanner{}}
	log.Printf("worker %s started", w.name)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go w.schedule(ctx)
	go w.runImportLanes(ctx)
	w.run(ctx)
}

// schedule ticks every 5 min (and once at start) to enqueue the periodic
// maintenance tasks — there is no external scheduler, so without this
// gc/migrate_cold/verify_all never run.
// 周期调度：每 5 分钟（并启动即触发一次）给维护任务入队——无外部调度器，
// 没有它 gc/migrate_cold/verify_all 永远不会执行。
func (w *worker) schedule(ctx context.Context) {
	fire := func() {
		// Requeue tasks whose claimant died (chunked tasks like mail_import
		// at 4min stay far under 60min). / 回收死认领者的任务。
		if n, err := w.store.RequeueStale(ctx, 60*time.Minute); err == nil && n > 0 {
			log.Printf("schedule: %d stale tasks requeued", n)
		}
		// Feishu migration: incremental mail catch-up / finishing check.
		// 飞书迁移：增量追平邮件、收尾检查。
		if fi, err := w.fimpStore(); err == nil {
			if err := fi.Tick(ctx); err != nil {
				log.Printf("schedule feishu: %v", err)
			}
		}
		for _, spec := range []struct {
			kind   string
			every  time.Duration
			s3Only bool
		}{
			{"gc", 24 * time.Hour, false},
			{"verify_all", 30 * 24 * time.Hour, false},
			{"migrate_cold", 24 * time.Hour, true},
		} {
			if spec.s3Only && !w.store.S3Enabled() {
				continue
			}
			fired, err := w.store.EnqueueIfDue(ctx, spec.kind, spec.every)
			if err != nil {
				log.Printf("schedule %s: %v", spec.kind, err)
			} else if fired {
				log.Printf("schedule: enqueued %s", spec.kind)
			}
		}
	}
	fire()
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fire()
		}
	}
}

type worker struct {
	pool    *pgxpool.Pool
	store   *objects.Store
	name    string
	scanner objects.Scanner   // AV plugin; NoopScanner default / 杀毒插件
	mail    *mail.Store       // lazily built / 惰性构造
	imp     *mailimport.Store // lazily built — needs the secret key / 惰性构造
	fimp    *feishuimport.Store
	docSt   *docs.Store
	impMu   sync.Mutex // guards imp/fimp/docSt lazy init across lanes / 惰性构造锁
}

// purgeStaleImportSecrets clears sealed_secret on jobs not running and
// idle for 30+ days. / 清掉非运行状态且 30 天未动的导入任务密码。
func (w *worker) purgeStaleImportSecrets(ctx context.Context) (int64, error) {
	tag, err := w.pool.Exec(ctx,
		`UPDATE mail_import_accounts SET sealed_secret=NULL
		 WHERE sealed_secret IS NOT NULL AND job_id IN (
		   SELECT id FROM mail_import_jobs WHERE status<>'running' AND updated_at < now()-interval '30 days')`)
	if err != nil {
		return 0, err
	}
	_, _ = w.pool.Exec(ctx,
		`UPDATE mail_import_jobs SET secrets_purged=true,updated_at=updated_at
		 WHERE id IN (SELECT DISTINCT job_id FROM mail_import_accounts WHERE sealed_secret IS NULL)
		   AND status<>'running' AND NOT secrets_purged
		   AND NOT EXISTS (SELECT 1 FROM mail_import_accounts a WHERE a.job_id=mail_import_jobs.id AND a.sealed_secret IS NOT NULL)`)
	return tag.RowsAffected(), nil
}

// impStore builds the import engine on demand — the secret key is
// load-only: a worker-created key would differ from the server's.
// Missing key → mail_import tasks fail loudly, the rest keeps working.
// 惰性构造导入引擎：密钥只读加载，缺失时 mail_import 报错重试，其他任务照跑。
func (w *worker) impStore() (*mailimport.Store, error) {
	w.impMu.Lock()
	defer w.impMu.Unlock()
	if w.imp != nil {
		return w.imp, nil
	}
	key, err := auth.LoadSecretKeyFile(envOr("WITTYCHAT_DATA_DIR", "./data"))
	if err != nil {
		return nil, fmt.Errorf("secret key for mail_import: %w", err)
	}
	w.imp = mailimport.New(w.pool, w.store, w.mailStore(), key)
	return w.imp, nil
}

// fimpStore lazily builds the Feishu import engine — same secret-key rule.
// 飞书导入引擎同样惰性构造（密钥只读）。
func (w *worker) fimpStore() (*feishuimport.Store, error) {
	w.impMu.Lock()
	defer w.impMu.Unlock()
	if w.fimp != nil {
		return w.fimp, nil
	}
	key, err := auth.LoadSecretKeyFile(envOr("WITTYCHAT_DATA_DIR", "./data"))
	if err != nil {
		return nil, fmt.Errorf("secret key for feishu_import: %w", err)
	}
	if w.docSt == nil {
		ims := im.NewStore(w.pool)
		w.docSt = docs.New(w.pool, ims.IsMember, ims.MemberRole)
	}
	w.fimp = feishuimport.New(w.pool, w.store, w.docSt, key).
		WithMigration(w.mailStore(), auth.NewStore(w.pool), envOr("WITTYCHAT_PUBLIC_URL", "http://localhost:8080"))
	return w.fimp, nil
}

// importKinds are long, budgeted chunks that run on their own lanes.
// 长耗时分块任务，走独立泳道。
var importKinds = []string{"mail_import", "feishu_import", "feishu_mail", "feishu_migrate"}

// run processes everything EXCEPT mail_import — a 4-minute import chunk
// must not delay thumbnails or outbound mail (head-of-line blocking).
// 主循环认领除 mail_import 外的一切：4 分钟的导入块不能卡住缩略图/发信。
func (w *worker) run(ctx context.Context) {
	w.loop(ctx, importKinds, false)
}

// runImportLanes spawns N goroutines that claim ONLY mail_import tasks.
// N comes from WITTYCHAT_IMPORT_CONCURRENCY, default 2, clamped 1..8.
// 导入专用泳道：只认领 mail_import，默认 2 个并发。
func (w *worker) runImportLanes(ctx context.Context) {
	n := 2
	if v, err := strconv.Atoi(envOr("WITTYCHAT_IMPORT_CONCURRENCY", "2")); err == nil {
		n = v
	}
	if n < 1 {
		n = 1
	} else if n > 8 {
		n = 8
	}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.loop(ctx, importKinds, true)
		}()
	}
	wg.Wait()
}

func (w *worker) loop(ctx context.Context, kinds []string, include bool) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		t, err := w.store.ClaimKinds(ctx, w.name, kinds, include)
		if err != nil {
			log.Printf("claim: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}
		if t == nil {
			time.Sleep(1500 * time.Millisecond)
			continue
		}
		if err := w.dispatch(ctx, t); err != nil {
			log.Printf("task %d (%s): %v", t.ID, t.Kind, err)
			_ = w.store.Fail(ctx, t.ID, t.Attempts, err.Error())
		} else {
			log.Printf("task %d (%s): done", t.ID, t.Kind)
			_ = w.store.Done(ctx, t.ID)
		}
	}
}

func (w *worker) dispatch(ctx context.Context, t *objects.Task) error {
	switch t.Kind {
	case "thumb":
		return w.thumb(ctx, t)
	case "gc":
		// Abort expired multipart sessions first — pending objects are
		// deleted there; OrphanGC then drops the files.
		// 先中止过期分片会话（pending 行即删），再跑孤儿清理。
		if ab, aerr := w.store.AbortExpiredUploads(ctx); aerr == nil && ab > 0 {
			log.Printf("gc: %d expired uploads aborted", ab)
		}
		n, err := w.store.OrphanGC(ctx)
		log.Printf("gc: %d orphans deleted", n)
		if err != nil {
			return err
		}
		// Retention sweeps past the audit window — the workspace setting
		// audit.retention_days (default 180d) governs both object version
		// history and archived doc-update rows.
		// 审计窗口保留期清扫——audit.retention_days（默认 180 天）同时约束
		// 对象历史版本与文档更新归档。
		window := time.Duration(settings.New(w.pool).Int(
			ctx, settings.KeyAuditRetention, settings.DefAuditRetention)) * 24 * time.Hour
		v, verr := w.store.PurgeOldVersions(ctx, window)
		if verr == nil {
			var a int64
			a, verr = docs.PurgeArchive(ctx, w.pool, window)
			if verr == nil {
				log.Printf("gc: %d versions + %d archived updates purged", v, a)
			}
			// Purge sealed IMAP passwords on idle jobs older than 30d — credentials
			// are a liability; a running job keeps its keys.
			// 闲置 30 天的导入任务清密码：凭据是负债，运行中的任务保留。
			if n, err := w.purgeStaleImportSecrets(ctx); err == nil && n > 0 {
				log.Printf("gc: %d idle import jobs purged", n)
			}
		}
		return verr
	case "verify":
		objID, err := uuid.Parse(str(t.Payload["object_id"]))
		if err != nil {
			return err
		}
		ok, err := w.store.VerifyHash(ctx, objID)
		if err == nil && !ok {
			return errors.New("hash mismatch / 哈希校验失败")
		}
		return err
	case "verify_all":
		n, err := w.store.EnqueueVerifyAll(ctx)
		log.Printf("verify_all: %d tasks enqueued", n)
		return err
	case "migrate_cold":
		days := settings.New(w.pool).Int(ctx, settings.KeyStorageColdDays, settings.DefColdDays)
		if days <= 0 {
			log.Printf("migrate_cold: storage.cold_days=0, skipped")
			return nil
		}
		age := time.Duration(days) * 24 * time.Hour
		budget := time.Now().Add(30 * time.Minute)
		total := 0
		for {
			n, err := w.store.MigrateCold(ctx, age, 100)
			total += n
			if errors.Is(err, objects.ErrS3Disabled) {
				return nil // local-only deployment — no-op
			}
			if err != nil {
				return err
			}
			if n < 100 || time.Now().After(budget) {
				break
			}
		}
		log.Printf("migrate_cold: %d objects moved to s3", total)
		return nil
	case "avscan":
		objID, err := uuid.Parse(str(t.Payload["object_id"]))
		if err != nil {
			return err
		}
		det, err := w.store.ScanObject(ctx, objID, w.scanner)
		if det != "" {
			return errors.New("av detection: " + det)
		}
		return err
	case "mail_send":
		// External delivery: render RFC822 from the sent message + MX send.
		msgID, err := uuid.Parse(str(t.Payload["message_id"]))
		if err != nil {
			return err
		}
		rcpt, _ := t.Payload["rcpt"].(string)
		return w.mailSend(ctx, msgID, rcpt)
	case "mail_parse":
		// Inbound .eml in objects → parse + deliver to local mailboxes.
		objID, err := uuid.Parse(str(t.Payload["object_id"]))
		if err != nil {
			return err
		}
		return w.mailParse(ctx, objID)
	case "mail_import":
		// IMAP import: 4-min chunk; re-enqueue while the account has work.
		// IMAP 导入：4 分钟一块，没完就续跑——让其他任务穿插执行。
		aid, err := uuid.Parse(str(t.Payload["account_id"]))
		if err != nil {
			return err
		}
		imp, err := w.impStore()
		if err != nil {
			return err
		}
		more, err := imp.RunAccount(ctx, aid, 4*time.Minute)
		if err != nil {
			if t.Attempts >= 5 {
				// give up after 5 tries — mark the account error / 终局置错
				_, _ = w.pool.Exec(ctx,
					`UPDATE mail_import_accounts SET status='error',last_error=$2,updated_at=now() WHERE id=$1`,
					aid, err.Error())
				return nil // no more retries / 不再重试
			}
			return err
		}
		if more {
			return w.store.Enqueue(ctx, "mail_import", map[string]any{"account_id": aid.String()})
		}
		return nil
	case "feishu_mail":
		openID := str(t.Payload["open_id"])
		fi, err := w.fimpStore()
		if err != nil {
			return err
		}
		more, err := fi.RunMail(ctx, openID, 4*time.Minute)
		if err != nil {
			return err
		}
		if more {
			return w.store.Enqueue(ctx, "feishu_mail", map[string]any{"open_id": openID})
		}
		return nil
	case "feishu_migrate":
		fi, err := w.fimpStore()
		if err != nil {
			return err
		}
		return fi.RunMigrate(ctx)
	case "feishu_import":
		jid, err := uuid.Parse(str(t.Payload["job_id"]))
		if err != nil {
			return err
		}
		fi, err := w.fimpStore()
		if err != nil {
			return err
		}
		more, err := fi.Run(ctx, jid, 4*time.Minute)
		if err != nil {
			return err
		}
		if more {
			return w.store.Enqueue(ctx, "feishu_import", map[string]any{"job_id": jid.String()})
		}
		return nil
	default:
		return errors.New("unknown task kind: " + t.Kind)
	}
}

// thumb decodes the source image, resizes to <=256px, writes thumbs/<id>.jpg.
func (w *worker) thumb(ctx context.Context, t *objects.Task) error {
	objIDStr, _ := t.Payload["object_id"].(string)
	objID, err := uuid.Parse(objIDStr)
	if err != nil {
		return err
	}
	owner, _ := uuid.Parse(str(t.Payload["owner"]))
	obj, err := w.store.Get(ctx, objID, owner)
	if err != nil {
		return err
	}
	rc, err := w.store.Open(ctx, obj)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()

	// Tee the head bytes for EXIF orientation — Go's decoder ignores it,
	// so a stored-landscape phone photo would render sideways.
	// 分流头部字节用于 EXIF 方向——Go 解码器忽略方向，竖拍横存的手机
	// 照片会缩略成横躺。
	hc := objects.NewHeadCapture(1 << 20)
	src, format, err := image.Decode(io.TeeReader(rc, hc))
	if err != nil {
		return err // non-image → task fails (payload error recorded)
	}
	b := src.Bounds()
	w0, h0 := b.Dx(), b.Dy()
	nw, nh := w0, h0
	if w0 > thumbMax || h0 > thumbMax {
		if w0 >= h0 {
			nw, nh = thumbMax, h0*thumbMax/w0
		} else {
			nh, nw = thumbMax, w0*thumbMax/h0
		}
	}
	scaled := image.NewRGBA(image.Rect(0, 0, nw, nh))
	draw.CatmullRom.Scale(scaled, scaled.Bounds(), src, b, draw.Over, nil)
	var dst image.Image = scaled
	if format == "jpeg" {
		dst = objects.ApplyOrientation(scaled, objects.JPEGOrientation(hc.Bytes()))
	}

	if err := os.MkdirAll(filepath.Dir(w.store.ThumbPath(objID)), 0o750); err != nil {
		return err
	}
	out, err := os.Create(w.store.ThumbPath(objID))
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	return jpeg.Encode(out, dst, &jpeg.Options{Quality: 82})
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
