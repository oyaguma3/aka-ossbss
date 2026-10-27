package web

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/oyaguma3/aka-ossbss/internal/access"
	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
	"github.com/oyaguma3/aka-ossbss/internal/trace"
)

// contentSecurityPolicy は画面の CSP。スクリプトとスタイルは同梱のファイルだけを許可する。
// daisyUI はアイコン（チェックボックスの印など）を data: の SVG で埋め込んでいるので、画像は data: も許可する。
// htmx の eval とインラインのスタイル挿入は htmx-config（layout.html）で無効にしている。
const contentSecurityPolicy = "default-src 'self'; img-src 'self' data:; object-src 'none'; " +
	"base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := w.Header()
		hdr.Set("Content-Security-Policy", contentSecurityPolicy)
		hdr.Set("X-Content-Type-Options", "nosniff")
		hdr.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// statusRecorder はアクセスログのためにステータスコードを記録する。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// Unwrap は http.ResponseController が元の ResponseWriter を使えるようにする。
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// reqInfo は、アクセスログに出すために、後のミドルウェアが書き込む値。
type reqInfo struct {
	email string
}

type reqInfoKey struct{}

// accessLog はリクエストごとにアクセスログを出す。静的ファイルと /healthz（compose の healthcheck が定期的に呼ぶ）は
// debug レベルにする。クエリ文字列は出さない（検索条件などを残さないため）。
// リクエストごとにトレースID を採番してコンテキストに入れる。provisioner の呼び出しに X-Trace-ID で渡るので、
// provisioner・下流のログ・監査ログと突き合わせられる。接続元は、Access で動くときは Cf-Connecting-IP を使う。
func (h *Handler) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		traceID := trace.New()
		info := &reqInfo{}
		r = r.WithContext(context.WithValue(trace.With(r.Context(), traceID), reqInfoKey{}, info))
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		level := slog.LevelInfo
		if strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/healthz" {
			level = slog.LevelDebug
		}
		h.log.Log(r.Context(), level, "access",
			"method", r.Method,
			"path", r.URL.Path,
			"status", cmp.Or(rec.status, http.StatusOK),
			"duration_ms", time.Since(start).Milliseconds(),
			"remote", h.ident.ClientIP(r),
			"user", info.email,
			"trace_id", traceID,
		)
	})
}

type userKey struct{}

// userFrom は認証した利用者を返す（authenticate を通った要求だけ）。
func userFrom(ctx context.Context) db.Account {
	u, _ := ctx.Value(userKey{}).(db.Account)
	return u
}

// isPublic は認証の対象外のパス（静的ファイルと /healthz）か。
func isPublic(path string) bool { return strings.HasPrefix(path, "/static/") || path == "/healthz" }

// authenticate は要求の利用者を決め、DB のアカウントを引いてコンテキストに入れる（設計概要 §5.3）。
// JWT がない・検証できなければ 401、アカウントがない・無効なら 403、公開鍵や DB を使えなければ 503 を返す。
// サーバー側のセッションは持たず、要求のたびに確かめる。
func (h *Handler) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPublic(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		email, err := h.ident.Identify(r)
		switch {
		case errors.Is(err, access.ErrUnavailable):
			h.log.Error("cannot verify access token: no public keys", "error", err)
			h.renderError(w, r, http.StatusServiceUnavailable,
				"利用者を確かめられません（Cloudflare Access の公開鍵を取得できません）。しばらくしてから開き直してください。")
			return
		case err != nil:
			h.log.Warn("unauthenticated request", "path", r.URL.Path, "reason", err.Error())
			h.renderError(w, r, http.StatusUnauthorized, "Cloudflare Access を通して開いてください。")
			return
		}
		if info, ok := r.Context().Value(reqInfoKey{}).(*reqInfo); ok {
			info.email = email
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		u, err := h.db.AccountByEmail(ctx, email)
		cancel()
		switch {
		case errors.Is(err, db.ErrNotFound) || (err == nil && u.Disabled):
			h.log.Warn("forbidden user", "email", email, "disabled", err == nil)
			h.render(w, r, http.StatusForbidden, "error", http.StatusText(http.StatusForbidden), errorData{Status: http.StatusForbidden,
				Message: "この利用者（" + email + "）は登録されていません。管理者に登録を依頼してください。", Relogin: true})
			return
		case err != nil:
			h.log.Error("lookup account", "error", err)
			h.renderError(w, r, http.StatusServiceUnavailable, "データベースに接続できません。しばらくしてから開き直してください。")
			return
		}
		// provisioner には、利用者のメールアドレスを操作者（X-Operator-Id）として渡す（設計概要 §15 の 6）。
		ctx = provisioner.WithOperator(context.WithValue(r.Context(), userKey{}, u), u.Email)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
