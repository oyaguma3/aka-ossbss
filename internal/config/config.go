// Package config は環境変数（接頭辞 OSSBSS_）から設定を読む。
package config

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
)

// Config は本BFF の設定。
type Config struct {
	// Addr は HTTP の待ち受けアドレス（cloudflared からだけ受ける。設計概要 §5.4）。
	Addr     string
	LogLevel slog.Level

	// DB は PostgreSQL への接続。
	DB DB
}

// DB は PostgreSQL への接続の設定。
type DB struct {
	// Addr は「ホスト:ポート」。
	Addr     string
	Name     string
	User     string
	Password string
	// MaxConns は接続プールの上限。
	MaxConns int32
}

// Load は環境変数から設定を読む。誤りはまとめて返す。
func Load() (Config, error) {
	c := Config{
		Addr: cmp.Or(os.Getenv("OSSBSS_ADDR"), ":8446"),
		DB: DB{
			Addr:     cmp.Or(os.Getenv("OSSBSS_DB_ADDR"), "postgres:5432"),
			Name:     cmp.Or(os.Getenv("OSSBSS_DB_NAME"), "ossbss"),
			User:     cmp.Or(os.Getenv("OSSBSS_DB_USER"), "ossbss"),
			Password: os.Getenv("OSSBSS_DB_PASSWORD"),
		},
	}
	var errs []error
	if v := os.Getenv("OSSBSS_LOG_LEVEL"); v != "" {
		if err := c.LogLevel.UnmarshalText([]byte(v)); err != nil {
			errs = append(errs, fmt.Errorf("OSSBSS_LOG_LEVEL: %w", err))
		}
	}
	if _, _, err := net.SplitHostPort(c.Addr); err != nil {
		errs = append(errs, fmt.Errorf("OSSBSS_ADDR: %w", err))
	}
	if _, _, err := net.SplitHostPort(c.DB.Addr); err != nil {
		errs = append(errs, fmt.Errorf("OSSBSS_DB_ADDR: %w", err))
	}
	c.DB.MaxConns = 10
	if v := os.Getenv("OSSBSS_DB_MAX_CONNS"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n < 1 || n > 100 {
			errs = append(errs, errors.New("OSSBSS_DB_MAX_CONNS: must be an integer between 1 and 100"))
		}
		c.DB.MaxConns = int32(n)
	}
	if c.DB.Password == "" {
		errs = append(errs, errors.New("OSSBSS_DB_PASSWORD is not set"))
	}
	return c, errors.Join(errs...)
}
