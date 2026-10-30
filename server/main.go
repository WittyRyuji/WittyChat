// WittyChat server entrypoint.
// Commands: serve (default) | bootstrap | migrate
//
// WittyChat 服务端入口。
// 子命令：serve（默认）| bootstrap（首个管理员）| migrate（应用数据库迁移）。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	"wittychat/core/audit"
	"wittychat/core/auth"
	"wittychat/core/db"
	"wittychat/core/im"
	"wittychat/core/migrations"
	"wittychat/core/objects"
	"wittychat/core/settings"
	"wittychat/server/httpapi"
	"wittychat/server/realtime"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 && os.Args[1][0] != '-' {
		cmd = os.Args[1]
		os.Args = append(os.Args[:1], os.Args[2:]...)
	}

	switch cmd {
	case "bootstrap":
		cmdBootstrap()
	case "migrate":
		cmdMigrate()
	case "serve":
		cmdServe()
	default:
		fmt.Fprintln(os.Stderr, "usage: server [serve|bootstrap|migrate]")
		os.Exit(2)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func dbURL() string {
	return envOr("WITTYCHAT_DB_URL", "postgres://wittychat:wittychat@localhost:5432/wittychat?sslmode=disable")
}

func publicURL() string {
	return envOr("WITTYCHAT_PUBLIC_URL", "http://localhost:8080")
}

func openDB() *pgxpool.Pool {
	pool, err := db.Open(context.Background(), dbURL())
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	return pool
}

// ---------- serve ----------

func cmdServe() {
	pool := openDB()
	defer pool.Close()

	key, ok, err := auth.LoadSecretKey()
	if err != nil {
		log.Fatalf("secret key: %v", err)
	}
	if !ok {
		log.Println("WARN: WITTYCHAT_SECRET_KEY not set — ephemeral dev key in use; sealed secrets die on restart")
	}

	imStore := im.NewStore(pool)
	objStore, err := objects.NewStore(pool, envOr("WITTYCHAT_DATA_DIR", "./data"))
	if err != nil {
		log.Fatalf("objects: %v", err)
	}
	settingsStore := settings.New(pool)
	imStore.RecallWindow = time.Duration(
		settingsStore.Int(context.Background(), settings.KeyIMRecallTimeout, settings.DefRecallTimeout)) * time.Second
	d := &httpapi.Deps{
		Pool:      pool,
		Store:     auth.NewStore(pool),
		Audit:     audit.New(pool),
		Limiter:   auth.NewLimiter(),
		SecretKey: key,
		Issuer:    envOr("WITTYCHAT_ISSUER", "WittyChat"),
		PublicURL: publicURL(),
		IM:        imStore,
		Hub:       realtime.NewHub(imStore.IsMember),
		Objects:   objStore,
		Settings:  settingsStore,
	}

	addr := envOr("WITTYCHAT_ADDR", ":8080")
	srv := &http.Server{
		Addr:              addr,
		Handler:           httpapi.New(d),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("server listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// ---------- bootstrap ----------

func cmdBootstrap() {
	fs := flag.NewFlagSet("bootstrap", flag.ExitOnError)
	email := fs.String("email", "", "admin email (required) / 管理员邮箱（必填）")
	name := fs.String("name", "Admin", "display name / 显示名")
	pw := fs.String("password", "", "dev shortcut: set password directly, skip link / 开发捷径：直接设密码跳过激活链接")
	_ = fs.Parse(os.Args[1:])
	if *email == "" {
		fs.Usage()
		os.Exit(2)
	}

	pool := openDB()
	defer pool.Close()
	store := auth.NewStore(pool)
	ctx := context.Background()

	u, err := store.CreateUser(ctx, *email, *name, true)
	if err != nil {
		log.Fatalf("create admin: %v", err)
	}

	if *pw != "" {
		phc, err := auth.HashPassword(*pw)
		if err != nil {
			log.Fatalf("hash: %v", err)
		}
		if err := store.SetPassword(ctx, u.ID, phc); err != nil {
			log.Fatalf("set password: %v", err)
		}
		if err := store.ActivateUser(ctx, u.ID); err != nil {
			log.Fatalf("activate: %v", err)
		}
		fmt.Printf("admin %s created and activated with the given password (dev only!)\n", u.Email)
		return
	}

	token, expiresAt, err := store.CreateActivationLink(ctx, u.ID, auth.LinkKindActivation, u.ID)
	if err != nil {
		log.Fatalf("activation link: %v", err)
	}
	fmt.Printf("admin %s created. Activation link (expires %s):\n\n  %s/activate?token=%s\n\n",
		u.Email, expiresAt.Format(time.RFC3339), publicURL(), token)
}

// ---------- migrate ----------

func cmdMigrate() {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	dir := fs.String("dir", "up", "up|down N|goto N|version")
	steps := fs.Int("steps", 1, "steps for down")
	_ = fs.Parse(os.Args[1:])

	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		log.Fatalf("migration source: %v", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, dbURL())
	if err != nil {
		log.Fatalf("migrate: %v", err)
	}
	defer func() { _, _ = m.Close() }()

	switch *dir {
	case "up":
		err = m.Up()
	case "down":
		err = m.Steps(-*steps)
	case "version":
		var v uint
		var dirty bool
		v, dirty, err = m.Version()
		if err == nil {
			fmt.Printf("version %d (dirty=%v)\n", v, dirty)
		}
	default:
		log.Fatalf("unknown -dir %q", *dir)
	}
	if err != nil && !errors.Is(err, migrate.ErrNoChange) {
		log.Fatalf("migrate %s: %v", *dir, err)
	}
}
