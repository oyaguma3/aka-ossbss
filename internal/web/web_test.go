package web

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/oyaguma3/eapaka-ossbss-sample/internal/access"
	"github.com/oyaguma3/eapaka-ossbss-sample/internal/access/accesstest"
	"github.com/oyaguma3/eapaka-ossbss-sample/internal/auth"
	"github.com/oyaguma3/eapaka-ossbss-sample/internal/authz"
	"github.com/oyaguma3/eapaka-ossbss-sample/internal/db"
)

var discard = slog.New(slog.DiscardHandler)

// fakeDB はテスト用の DB。err を設定すると、すべての呼び出しがそのエラーを返す。
type fakeDB struct {
	mu       sync.Mutex
	version  int
	err      error
	accounts map[string]db.Account
}

func newFakeDB() *fakeDB {
	return &fakeDB{version: 1, accounts: map[string]db.Account{
		"a@example.com": {ID: 1, Email: "a@example.com", Role: authz.TenantUser, TenantID: 10, TenantName: "テナントA"},
		"b@example.com": {ID: 2, Email: "b@example.com", Role: authz.TenantAdmin, TenantID: 10, TenantName: "テナントA"},
		"c@example.com": {ID: 3, Email: "c@example.com", Role: authz.Operator},
		"d@example.com": {ID: 4, Email: "d@example.com", Role: authz.SystemAdmin},
		"x@example.com": {ID: 5, Email: "x@example.com", Role: authz.Operator, Disabled: true},
	}}
}

func (f *fakeDB) Ping(context.Context) error { return f.err }
func (f *fakeDB) SchemaVersion(context.Context) (int, error) {
	return f.version, f.err
}
func (f *fakeDB) AccountByEmail(_ context.Context, email string) (db.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return db.Account{}, f.err
	}
	a, ok := f.accounts[email]
	if !ok {
		return db.Account{}, db.ErrNotFound
	}
	return a, nil
}

func newHandler(t *testing.T, d *fakeDB, ident auth.Identifier, log *slog.Logger) http.Handler {
	t.Helper()
	h, err := New(Options{Log: log, Version: "test", DB: d, Identifier: ident})
	if err != nil {
		t.Fatal(err)
	}
	return h.Routes()
}

// newDevHandler は開発用の設定（email の利用者）の画面を作る。
func newDevHandler(t *testing.T, d *fakeDB, email string) http.Handler {
	return newHandler(t, d, auth.Dev(email), discard)
}

// newAccessHandler は Cloudflare Access の JWT で利用者を決める画面を作る。
func newAccessHandler(t *testing.T, d *fakeDB, is *accesstest.Issuer, log *slog.Logger) http.Handler {
	t.Helper()
	v, err := access.New(access.Options{TeamDomain: accesstest.TeamDomain, AUDs: []string{accesstest.AUD}, CertsURL: is.CertsURL()})
	if err != nil {
		t.Fatal(err)
	}
	return newHandler(t, d, auth.Access(v), log)
}

func do(h http.Handler, method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://ossbss.example"+path, nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func mustContain(t *testing.T, body string, want ...string) {
	t.Helper()
	for _, s := range want {
		if !strings.Contains(body, s) {
			t.Errorf("body does not contain %q", s)
		}
	}
}

func TestHomeByRole(t *testing.T) {
	d := newFakeDB()
	// テナントユーザー: 自分のテナントのものだけ。登録はできない。
	w := do(newDevHandler(t, d, "a@example.com"), "GET", "/", nil)
	if w.Code != 200 {
		t.Fatalf("home = %d", w.Code)
	}
	body := w.Body.String()
	mustContain(t, body, "<title>ダッシュボード | EAP-AKA OSS/BSS サンプル</title>", "a@example.com", "テナントユーザー", "テナントA",
		"<td>加入者の停止・再開</td>\n            <td>自分のテナントのもの</td>",
		"<td>加入者の登録</td>\n            <td>できない</td>", "開発用の設定", "接続できます", "スキーマの版 1")
	// 開発用の設定ではログアウトのリンクを出さない（Access を通らないため）。
	if strings.Contains(body, accessLogoutURL) {
		t.Error("dev mode shows the access logout link")
	}
	// セキュリティのヘッダー。
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") ||
		w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("headers = %v", w.Header())
	}

	// テナント管理者: アカウントの管理は自分のテナントのテナントユーザーだけ。
	body = do(newDevHandler(t, d, "b@example.com"), "GET", "/", nil).Body.String()
	mustContain(t, body, "テナント管理者", "<td>アカウントの管理</td>\n            <td>自分のテナントのテナントユーザー</td>")
	// 運用担当: テナントなし。AMF / SQN の変更はできない。
	body = do(newDevHandler(t, d, "c@example.com"), "GET", "/", nil).Body.String()
	mustContain(t, body, "運用担当", "なし（全テナントとテナントなしを扱う）",
		"<td>加入者の登録</td>\n            <td>すべて</td>", "<td>AMF / SQN の変更</td>\n            <td>できない</td>")
	// システム管理者: すべて。
	body = do(newDevHandler(t, d, "d@example.com"), "GET", "/", nil).Body.String()
	if strings.Contains(body, "<td>できない</td>") {
		t.Error("system admin cannot do something")
	}

	// DB に接続できなければ 503（利用者を確かめられない）。
	d.err = errors.New("down")
	if w := do(newDevHandler(t, d, "d@example.com"), "GET", "/", nil); w.Code != 503 || !strings.Contains(w.Body.String(), "データベースに接続できません") {
		t.Errorf("db down = %d", w.Code)
	}
}

func TestAccessAuthentication(t *testing.T) {
	is := accesstest.NewIssuer(t)
	d := newFakeDB()
	var logs bytes.Buffer
	h := newAccessHandler(t, d, is, slog.New(slog.NewJSONHandler(&logs, nil)))
	jwt := func(email string) map[string]string {
		return map[string]string{access.Header: is.Sign(t, accesstest.Claims(email)), "Cf-Connecting-IP": "203.0.113.7"}
	}

	// JWT がなければ 401。
	if w := do(h, "GET", "/", nil); w.Code != 401 || !strings.Contains(w.Body.String(), "Cloudflare Access を通して開いてください。") {
		t.Errorf("no token = %d", w.Code)
	}
	// 登録された利用者。メールアドレスは小文字にして引く。ログアウトは Access のログアウト。
	w := do(h, "GET", "/", jwt("C@Example.COM"))
	if w.Code != 200 {
		t.Fatalf("operator = %d %s", w.Code, w.Body)
	}
	mustContain(t, w.Body.String(), "c@example.com", "運用担当", `href="/cdn-cgi/access/logout"`)
	if strings.Contains(w.Body.String(), "開発用の設定") {
		t.Error("access mode shows the dev badge")
	}
	// アクセスログに利用者と、Cloudflare が付けた接続元を出す。
	if !strings.Contains(logs.String(), `"user":"c@example.com"`) || !strings.Contains(logs.String(), `"remote":"203.0.113.7"`) {
		t.Errorf("logs = %s", logs.String())
	}

	// 登録されていない・無効な利用者は 403。別のメールアドレスでログインし直すリンクを出す。
	for _, email := range []string{"nobody@example.com", "x@example.com"} {
		w := do(h, "GET", "/", jwt(email))
		if w.Code != 403 || !strings.Contains(w.Body.String(), "この利用者（"+email+"）は登録されていません。") ||
			!strings.Contains(w.Body.String(), "別のメールアドレスでログインし直す") {
			t.Errorf("%s = %d", email, w.Code)
		}
	}
	// 検証できない JWT は 401。
	claims := accesstest.Claims("c@example.com")
	claims["aud"] = "other"
	if w := do(h, "GET", "/", map[string]string{access.Header: is.Sign(t, claims)}); w.Code != 401 {
		t.Errorf("wrong aud = %d", w.Code)
	}
	// 静的ファイルと /healthz は認証の対象外。
	for _, p := range []string{"/healthz", "/static/htmx.min.js"} {
		if w := do(h, "GET", p, nil); w.Code != 200 {
			t.Errorf("%s = %d", p, w.Code)
		}
	}
	// 知らないパスも、認証してから 404。
	if w := do(h, "GET", "/nothing", nil); w.Code != 401 {
		t.Errorf("unknown path without token = %d", w.Code)
	}
	if w := do(h, "GET", "/nothing", jwt("c@example.com")); w.Code != 404 {
		t.Errorf("unknown path = %d", w.Code)
	}
}

// 公開鍵を 1 つも取得できなければ 503。
func TestAccessKeysUnavailable(t *testing.T) {
	is := accesstest.NewIssuer(t)
	is.SetDown(true)
	h := newAccessHandler(t, newFakeDB(), is, discard)
	w := do(h, "GET", "/", map[string]string{access.Header: is.Sign(t, accesstest.Claims("c@example.com"))})
	if w.Code != 503 || !strings.Contains(w.Body.String(), "Cloudflare Access の公開鍵を取得できません") {
		t.Errorf("keys unavailable = %d %s", w.Code, w.Body)
	}
}

func TestHealthz(t *testing.T) {
	d := newFakeDB()
	h := newDevHandler(t, d, "d@example.com")
	if w := do(h, "GET", "/healthz", nil); w.Code != 200 || w.Body.String() != "ok\n" {
		t.Errorf("healthz = %d %q", w.Code, w.Body)
	}
	d.err = errors.New("down")
	if w := do(h, "GET", "/healthz", nil); w.Code != 503 {
		t.Errorf("healthz (db down) = %d", w.Code)
	}
}

func TestNotFoundAndStatic(t *testing.T) {
	h := newDevHandler(t, newFakeDB(), "d@example.com")
	if w := do(h, "GET", "/nothing", nil); w.Code != 404 || !strings.Contains(w.Body.String(), "ページが見つかりません。") {
		t.Errorf("not found = %d", w.Code)
	}
	// 静的ファイルは ETag で再検証させる。ディレクトリの一覧は返さない。
	w := do(h, "GET", "/static/htmx.min.js", nil)
	etag := w.Header().Get("ETag")
	if w.Code != 200 || etag == "" || w.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("static = %d %v", w.Code, w.Header())
	}
	if w := do(h, "GET", "/static/htmx.min.js", map[string]string{"If-None-Match": etag}); w.Code != 304 {
		t.Errorf("revalidate = %d", w.Code)
	}
	if w := do(h, "GET", "/static/", nil); w.Code != 404 {
		t.Errorf("static dir = %d", w.Code)
	}
}

// 別のオリジンからの書き込みは断る（CrossOriginProtection）。
func TestCrossOrigin(t *testing.T) {
	h := newDevHandler(t, newFakeDB(), "d@example.com")
	w := do(h, "POST", "/", map[string]string{"Sec-Fetch-Site": "cross-site"})
	if w.Code != 403 || !strings.Contains(w.Body.String(), "画面を開き直してから") {
		t.Errorf("cross-origin = %d %s", w.Code, w.Body)
	}
	// Access で動いていても、ログインし直すリンクは出さない（利用者の問題ではない）。
	is := accesstest.NewIssuer(t)
	w = do(newAccessHandler(t, newFakeDB(), is, discard), "POST", "/", map[string]string{"Sec-Fetch-Site": "cross-site"})
	if w.Code != 403 || strings.Contains(w.Body.String(), "ログインし直す") {
		t.Errorf("cross-origin (access) = %d %s", w.Code, w.Body)
	}
}

func TestPermissions(t *testing.T) {
	// 一覧の数は操作の数と同じ（表示の漏れがない）。
	if got := len(permissions(authz.Actor{Role: authz.SystemAdmin})); got != len(authz.Ops()) {
		t.Errorf("permissions = %d", got)
	}
}
