package config

import (
	"log/slog"
	"strings"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("OSSBSS_DB_PASSWORD", "pw")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != ":8446" || c.LogLevel != slog.LevelInfo || c.DB != (DB{Addr: "postgres:5432", Name: "ossbss", User: "ossbss", Password: "pw", MaxConns: 10}) {
		t.Errorf("config = %+v", c)
	}
}

func TestLoadValues(t *testing.T) {
	for k, v := range map[string]string{
		"OSSBSS_ADDR": "127.0.0.1:18446", "OSSBSS_LOG_LEVEL": "debug", "OSSBSS_DB_ADDR": "127.0.0.1:25432",
		"OSSBSS_DB_NAME": "x", "OSSBSS_DB_USER": "y", "OSSBSS_DB_PASSWORD": "z", "OSSBSS_DB_MAX_CONNS": "3",
	} {
		t.Setenv(k, v)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != "127.0.0.1:18446" || c.LogLevel != slog.LevelDebug || c.DB != (DB{Addr: "127.0.0.1:25432", Name: "x", User: "y", Password: "z", MaxConns: 3}) {
		t.Errorf("config = %+v", c)
	}
}

// 誤りはまとめて返す。
func TestLoadErrors(t *testing.T) {
	t.Setenv("OSSBSS_ADDR", "8446")
	t.Setenv("OSSBSS_LOG_LEVEL", "verbose")
	t.Setenv("OSSBSS_DB_ADDR", "postgres")
	t.Setenv("OSSBSS_DB_MAX_CONNS", "0")
	t.Setenv("OSSBSS_DB_PASSWORD", "")
	_, err := Load()
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"OSSBSS_ADDR", "OSSBSS_LOG_LEVEL", "OSSBSS_DB_ADDR", "OSSBSS_DB_MAX_CONNS", "OSSBSS_DB_PASSWORD is not set"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}
