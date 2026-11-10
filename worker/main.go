// WittyChat worker: claims bg_tasks and processes them.
// First pipeline task: image thumbnails (thumb kind).
//
// WittyChat worker：认领 bg_tasks 并处理。
// 首个流水线任务：图片缩略图（thumb 类型）。
package main

import (
	"context"
	"errors"
	"image"
	_ "image/gif" // register decoders / 注册解码器
	"image/jpeg"
	_ "image/png"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
	"wittychat/core/db"
	"wittychat/core/docs"
	"wittychat/core/mail"
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
	w.run(ctx)
}

type worker struct {
	pool    *pgxpool.Pool
	store   *objects.Store
	name    string
	scanner objects.Scanner // AV plugin; NoopScanner default / 杀毒插件
	mail    *mail.Store     // lazily built / 惰性构造
}

func (w *worker) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		t, err := w.store.Claim(ctx, w.name)
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
		n, err := w.store.MigrateCold(ctx, 90*24*time.Hour, 100)
		log.Printf("migrate_cold: %d objects moved to s3", n)
		if errors.Is(err, objects.ErrS3Disabled) {
			return nil // local-only deployment — no-op
		}
		return err
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

	src, _, err := image.Decode(rc)
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
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)

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
