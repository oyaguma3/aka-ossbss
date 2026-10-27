package auth

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/oyaguma3/aka-ossbss/internal/access"
)

type fakeVerifier struct {
	email string
	err   error
	token string
}

func (f *fakeVerifier) Verify(_ context.Context, token string) (access.Claims, error) {
	f.token = token
	return access.Claims{Email: f.email}, f.err
}

func TestAccess(t *testing.T) {
	v := &fakeVerifier{email: " Alice@Example.COM"}
	id := Access(v)
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "172.18.0.5:41234"
	r.Header.Set(access.Header, "jwt")
	if email, err := id.Identify(r); err != nil || email != "alice@example.com" || v.token != "jwt" {
		t.Errorf("identify = %q, %v (token %q)", email, err, v.token)
	}
	// 接続元は Cf-Connecting-IP。なければ（または形式が違えば）接続元のアドレス。
	if ip := id.ClientIP(r); ip != "172.18.0.5" {
		t.Errorf("client ip without header = %q", ip)
	}
	r.Header.Set("Cf-Connecting-IP", "2001:db8::1")
	if ip := id.ClientIP(r); ip != "2001:db8::1" {
		t.Errorf("client ip = %q", ip)
	}
	r.Header.Set("Cf-Connecting-IP", "not-an-ip")
	if ip := id.ClientIP(r); ip != "172.18.0.5" {
		t.Errorf("client ip with bad header = %q", ip)
	}
	if id.Dev() {
		t.Error("access is dev")
	}
	v.err = access.ErrInvalid
	if _, err := id.Identify(r); !errors.Is(err, access.ErrInvalid) {
		t.Errorf("err = %v", err)
	}
}

func TestDev(t *testing.T) {
	id := Dev("Dev@Example.com")
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:5555"
	// 開発用の設定では Cf-Connecting-IP を信用しない。
	r.Header.Set("Cf-Connecting-IP", "203.0.113.7")
	if email, err := id.Identify(r); err != nil || email != "dev@example.com" || id.ClientIP(r) != "127.0.0.1" || !id.Dev() {
		t.Errorf("dev = %q, %v, %q", email, err, id.ClientIP(r))
	}
}
