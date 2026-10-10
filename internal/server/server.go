// Package server は HTTP のリスナーを起動する。
//
// 本BFF は HTTPS を自分で終端しない。ブラウザとの HTTPS は Cloudflare が終端し、cloudflared が同じホストの
// compose のネットワークの中から HTTP で本BFF に届ける（設計概要 §5.4）。
package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

const (
	shutdownTimeout = 10 * time.Second
	// defaultWriteTimeout は、要求を受け取ってから応答を書き終えるまでの上限の既定。provisioner の 1 回の
	// 呼び出しの上限（60 秒）より長くする（既存の BFF の設計概要 §12.6 で分かったこと。短いと、時間のかかった
	// 応答を書き出せずに接続が切れる）。
	defaultWriteTimeout = 90 * time.Second
)

// Options はリスナーの設定。
type Options struct {
	// Addr は待ち受けアドレス。
	Addr    string
	Handler http.Handler
	Log     *slog.Logger
	// WriteTimeout は、要求を受け取ってから応答を書き終えるまでの上限（ハンドラーの処理の時間を含む）。0 なら 90 秒。
	WriteTimeout time.Duration
}

// Run はリスナーを起動し、ctx が終了するかリスナーが失敗するまで待つ。
func Run(ctx context.Context, opts Options) error {
	ln, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", opts.Addr, err)
	}
	return serve(ctx, ln, opts)
}

func serve(ctx context.Context, ln net.Listener, opts Options) error {
	srv := &http.Server{
		Handler:           opts.Handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      cmp.Or(opts.WriteTimeout, defaultWriteTimeout),
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(opts.Log.Handler(), slog.LevelWarn),
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	opts.Log.Info("listening", "addr", ln.Addr().String())

	select {
	case <-ctx.Done():
		opts.Log.Info("shutting down")
	case err := <-errc:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		opts.Log.Warn("shutdown", "error", err)
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
