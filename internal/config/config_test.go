package config

import (
	"log/slog"
	"slices"
	"strings"
	"testing"
)

// setAccess は Access で動く最小の設定を入れる。
func setAccess(t *testing.T) {
	t.Setenv("OSSBSS_DB_PASSWORD", "pw")
	t.Setenv("OSSBSS_INITIAL_ADMIN_EMAIL", " Root@Example.com ")
	t.Setenv("OSSBSS_ACCESS_TEAM_DOMAIN", "Steep-Term-C7DA.cloudflareaccess.com")
	t.Setenv("OSSBSS_ACCESS_AUD", "aud-1, aud-2,")
}

func TestLoadDefaults(t *testing.T) {
	setAccess(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != ":8446" || c.LogLevel != slog.LevelInfo || c.DB != (DB{Addr: "postgres:5432", Name: "ossbss", User: "ossbss", Password: "pw", MaxConns: 10}) {
		t.Errorf("config = %+v", c)
	}
	// メールアドレスとチームのドメインは小文字にする。AUD はカンマ区切りで複数。
	if c.InitialAdminEmail != "root@example.com" || c.AccessTeamDomain != "steep-term-c7da.cloudflareaccess.com" ||
		!slices.Equal(c.AccessAUDs, []string{"aud-1", "aud-2"}) || c.DevMode() {
		t.Errorf("auth = %q %q %v %v", c.InitialAdminEmail, c.AccessTeamDomain, c.AccessAUDs, c.DevMode())
	}
}

func TestLoadValues(t *testing.T) {
	setAccess(t)
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

// 開発用の設定は、ループバックで待ち受けるときだけ使える。
func TestLoadDevMode(t *testing.T) {
	t.Setenv("OSSBSS_DB_PASSWORD", "pw")
	t.Setenv("OSSBSS_INITIAL_ADMIN_EMAIL", "root@example.com")
	t.Setenv("OSSBSS_DEV_USER_EMAIL", "Dev@Example.com")
	for _, addr := range []string{"127.0.0.1:8446", "[::1]:8446", "localhost:8446"} {
		t.Setenv("OSSBSS_ADDR", addr)
		c, err := Load()
		if err != nil || !c.DevMode() || c.DevUserEmail != "dev@example.com" {
			t.Errorf("%s: %+v, %v", addr, c, err)
		}
	}
	for _, addr := range []string{":8446", "0.0.0.0:8446", "[::]:8446", "192.0.2.1:8446", "100.126.128.93:8446"} {
		t.Setenv("OSSBSS_ADDR", addr)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "only when OSSBSS_ADDR is a loopback address") {
			t.Errorf("%s: %v", addr, err)
		}
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
	for _, want := range []string{"OSSBSS_ADDR", "OSSBSS_LOG_LEVEL", "OSSBSS_DB_ADDR", "OSSBSS_DB_MAX_CONNS", "OSSBSS_DB_PASSWORD is not set",
		"OSSBSS_INITIAL_ADMIN_EMAIL is not set", "set OSSBSS_ACCESS_TEAM_DOMAIN and OSSBSS_ACCESS_AUD"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

func TestLoadAuthErrors(t *testing.T) {
	for name, c := range map[string]struct {
		env  map[string]string
		want string
	}{
		"admin with plus":  {map[string]string{"OSSBSS_INITIAL_ADMIN_EMAIL": "root+1@example.com"}, "OSSBSS_INITIAL_ADMIN_EMAIL: must be"},
		"team without aud": {map[string]string{"OSSBSS_ACCESS_AUD": ""}, "both OSSBSS_ACCESS_TEAM_DOMAIN and OSSBSS_ACCESS_AUD are required"},
		"aud without team": {map[string]string{"OSSBSS_ACCESS_TEAM_DOMAIN": ""}, "both OSSBSS_ACCESS_TEAM_DOMAIN and OSSBSS_ACCESS_AUD are required"},
		"team with scheme": {map[string]string{"OSSBSS_ACCESS_TEAM_DOMAIN": "https://x.cloudflareaccess.com"}, "must be a host name"},
		"dev with access":  {map[string]string{"OSSBSS_DEV_USER_EMAIL": "dev@example.com", "OSSBSS_ADDR": "127.0.0.1:8446"}, "cannot be used with the Cloudflare Access settings"},
	} {
		t.Run(name, func(t *testing.T) {
			setAccess(t)
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v", err)
			}
		})
	}
}
