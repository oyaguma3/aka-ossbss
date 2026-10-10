// eapaka-ossbss は外部 OSS/BSS のサンプル実装としての統合BFF（eapaka-node-provisioner の管理クライアント）。
package main

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	// distroless のイメージにはタイムゾーンのデータがないので、埋め込んで環境変数 TZ を効かせる。
	_ "time/tzdata"

	"github.com/oyaguma3/eapaka-ossbss-sample/internal/config"
	"github.com/oyaguma3/eapaka-ossbss-sample/internal/db"
	"github.com/oyaguma3/eapaka-ossbss-sample/internal/server"
	"github.com/oyaguma3/eapaka-ossbss-sample/internal/web"
)

// version はビルド時に -ldflags "-X main.version=..." で埋め込む。
var version = "dev"

const usage = `usage: eapaka-ossbss <command>

commands:
  serve              サーバーを起動する（コマンド省略時の既定）。起動時に DB のスキーマを移行する
  migrate            DB のスキーマを移行して終わる
  healthcheck        動いている本BFF の /healthz を確かめる（compose の healthcheck 用）

設定は環境変数（接頭辞 OSSBSS_）で与える。
`

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		args = []string{"serve"}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch args[0] {
	case "serve":
		err = serve(ctx)
	case "migrate":
		err = migrate(ctx)
	case "healthcheck":
		err = healthcheck(ctx)
	case "-h", "-help", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", args[0], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// openDB は設定を読み、DB に接続してスキーマを移行する。
func openDB(ctx context.Context) (config.Config, *slog.Logger, *db.DB, error) {
	cfg, err := config.Load()
	if err != nil {
		return cfg, nil, nil, err
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	log.Info("starting", "version", version)

	// DB のコンテナの起動を待つため、接続は少しの間やり直す。
	var d *db.DB
	deadline := time.Now().Add(30 * time.Second)
	for {
		openCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		d, err = db.Open(openCtx, db.Options{Addr: cfg.DB.Addr, Name: cfg.DB.Name, User: cfg.DB.User,
			Password: cfg.DB.Password, MaxConns: cfg.DB.MaxConns})
		cancel()
		if err == nil || time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		log.Warn("db is not reachable; retrying", "addr", cfg.DB.Addr, "error", err)
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
	if err != nil {
		return cfg, log, nil, err
	}
	migCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	applied, err := d.Migrate(migCtx)
	if err != nil {
		d.Close()
		return cfg, log, nil, err
	}
	v, err := d.SchemaVersion(migCtx)
	if err != nil {
		d.Close()
		return cfg, log, nil, err
	}
	log.Info("database", "addr", cfg.DB.Addr, "name", cfg.DB.Name, "schema_version", v, "applied", applied)
	return cfg, log, d, nil
}

func serve(ctx context.Context) error {
	cfg, log, d, err := openDB(ctx)
	if err != nil {
		return err
	}
	defer d.Close()

	h, err := web.New(web.Options{Log: log, Version: version, DB: d})
	if err != nil {
		return err
	}
	return server.Run(ctx, server.Options{Addr: cfg.Addr, Handler: h.Routes(), Log: log})
}

func migrate(ctx context.Context) error {
	_, _, d, err := openDB(ctx)
	if err != nil {
		return err
	}
	d.Close()
	return nil
}

// healthcheck は、同じコンテナで動いている本BFF の /healthz（DB に接続できるか）を確かめる。
// distroless のイメージには curl がないので、compose の healthcheck はこれを使う。
func healthcheck(ctx context.Context) error {
	_, port, err := net.SplitHostPort(cmp.Or(os.Getenv("OSSBSS_ADDR"), ":8446"))
	if err != nil {
		return fmt.Errorf("OSSBSS_ADDR: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort("127.0.0.1", port)+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz: %s", resp.Status)
	}
	return nil
}
