package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var discard = slog.New(slog.DiscardHandler)

// fakeDB はテスト用の DB。err を設定すると、すべての呼び出しがそのエラーを返す。
type fakeDB struct {
	version int
	err     error
}

func (f *fakeDB) Ping(context.Context) error                 { return f.err }
func (f *fakeDB) SchemaVersion(context.Context) (int, error) { return f.version, f.err }

func newTestHandler(t *testing.T, d *fakeDB) http.Handler {
	t.Helper()
	h, err := New(Options{Log: discard, Version: "test", DB: d})
	if err != nil {
		t.Fatal(err)
	}
	return h.Routes()
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

func TestHome(t *testing.T) {
	d := &fakeDB{version: 1}
	h := newTestHandler(t, d)
	w := do(h, "GET", "/", nil)
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "<title>ダッシュボード | EAP-AKA OSS/BSS サンプル</title>") ||
		!strings.Contains(body, "接続できます") || !strings.Contains(body, "スキーマの版 1") || !strings.Contains(body, "eapaka-ossbss-sample test") {
		t.Errorf("home = %d %s", w.Code, body)
	}
	// セキュリティのヘッダー。
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") ||
		w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("headers = %v", w.Header())
	}

	// DB に接続できなくても画面は返す。
	d.err = errors.New("down")
	if w := do(h, "GET", "/", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "接続できません") {
		t.Errorf("db down = %d", w.Code)
	}
}

func TestHealthz(t *testing.T) {
	d := &fakeDB{}
	h := newTestHandler(t, d)
	if w := do(h, "GET", "/healthz", nil); w.Code != 200 || w.Body.String() != "ok\n" {
		t.Errorf("healthz = %d %q", w.Code, w.Body)
	}
	d.err = errors.New("down")
	if w := do(h, "GET", "/healthz", nil); w.Code != 503 {
		t.Errorf("healthz (db down) = %d", w.Code)
	}
}

func TestNotFoundAndStatic(t *testing.T) {
	h := newTestHandler(t, &fakeDB{})
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
	h := newTestHandler(t, &fakeDB{})
	w := do(h, "POST", "/", map[string]string{"Sec-Fetch-Site": "cross-site"})
	if w.Code != 403 || !strings.Contains(w.Body.String(), "画面を開き直してから") {
		b, _ := io.ReadAll(w.Body)
		t.Errorf("cross-origin = %d %s", w.Code, b)
	}
}
