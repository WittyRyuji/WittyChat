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
	"syscall"
	"time"
	"wittychat/core/db"
	"wittychat/core/objects"

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

	hostname, _ := os.Hostname()
	w := &worker{pool: pool, store: store, name: "worker-" + hostname}
	log.Printf("worker %s started", w.name)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	w.run(ctx)
}

type worker struct {
	pool  *pgxpool.Pool
	store *objects.Store
	name  string
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
	owner, _ := uuid.Parse(fmt(t.Payload["owner"]))
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

	out, err := os.Create(w.store.ThumbPath(objID))
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	return jpeg.Encode(out, dst, &jpeg.Options{Quality: 82})
}

func fmt(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
