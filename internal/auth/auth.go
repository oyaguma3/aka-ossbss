// Package auth は、要求から利用者のメールアドレスを決める（設計概要 §5）。
//
// 本番では Cloudflare Access の JWT（ヘッダー Cf-Access-Jwt-Assertion）を検証し、そのメールアドレスを使う。
// 開発用の設定（OSSBSS_DEV_USER_EMAIL。ループバックで待ち受けるときだけ）では、決まったメールアドレスを使う。
// 役割とテナントは、メールアドレスで DB のアカウントを引いて決める（呼び出し側）。
package auth

import (
	"context"
	"net"
	"net/http"
	"net/netip"

	"github.com/oyaguma3/eapaka-ossbss-sample/internal/access"
	"github.com/oyaguma3/eapaka-ossbss-sample/internal/authz"
)

// Identifier は要求から利用者を決める。
type Identifier interface {
	// Identify は要求の利用者のメールアドレス（正規化したもの）を返す。JWT がない・検証できなければ
	// access.ErrInvalid、公開鍵がなく検証できなければ access.ErrUnavailable を包んだエラーを返す。
	Identify(r *http.Request) (string, error)
	// ClientIP は要求の接続元の IP アドレスを返す（ログと監査ログに残す）。
	ClientIP(r *http.Request) string
	// Dev は開発用の設定か。
	Dev() bool
}

// Verifier は JWT の検証。*access.Verifier が満たす。
type Verifier interface {
	Verify(ctx context.Context, token string) (access.Claims, error)
}

// Access は Cloudflare Access の JWT で利用者を決める。
func Access(v Verifier) Identifier { return accessIdentifier{v: v} }

type accessIdentifier struct{ v Verifier }

func (a accessIdentifier) Identify(r *http.Request) (string, error) {
	c, err := a.v.Verify(r.Context(), r.Header.Get(access.Header))
	if err != nil {
		return "", err
	}
	return authz.NormalizeEmail(c.Email), nil
}

// ClientIP は Cf-Connecting-IP（Cloudflare が付ける、利用者の接続元）を返す。本BFF はポートを公開せず、
// 要求は cloudflared からだけ届く（接続元のアドレスは cloudflared のもの）ので、Cloudflare の値を使う（設計概要 §17.2）。
func (accessIdentifier) ClientIP(r *http.Request) string {
	if ip, err := netip.ParseAddr(r.Header.Get("Cf-Connecting-IP")); err == nil {
		return ip.String()
	}
	return remoteIP(r)
}

func (accessIdentifier) Dev() bool { return false }

// Dev は開発用の設定で、いつも email の利用者とする。
func Dev(email string) Identifier { return devIdentifier{email: authz.NormalizeEmail(email)} }

type devIdentifier struct{ email string }

func (d devIdentifier) Identify(*http.Request) (string, error) { return d.email, nil }

// ClientIP は接続元のアドレスを返す（開発用の設定では Cloudflare を通らないので、ヘッダーは信用しない）。
func (devIdentifier) ClientIP(r *http.Request) string { return remoteIP(r) }

func (devIdentifier) Dev() bool { return true }

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
