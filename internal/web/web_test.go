package web

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oyaguma3/eapaka-ossbss-sample/internal/access"
	"github.com/oyaguma3/eapaka-ossbss-sample/internal/access/accesstest"
	"github.com/oyaguma3/eapaka-ossbss-sample/internal/auth"
	"github.com/oyaguma3/eapaka-ossbss-sample/internal/authz"
	"github.com/oyaguma3/eapaka-ossbss-sample/internal/db"
	"github.com/oyaguma3/eapaka-ossbss-sample/internal/provisioner"
)

var discard = slog.New(slog.DiscardHandler)

// fakeDB はテスト用の DB。err を設定すると、すべての呼び出しがそのエラーを返す。
// テナントは 10（テナントA）と 20（テナントB）。テナントユーザー a とテナント管理者 b はテナントA に属する。
type fakeDB struct {
	mu       sync.Mutex
	version  int
	err      error
	accounts map[string]db.Account
	tenants  []db.Tenant
	// subTenants と clientTenants は加入者・RADIUSクライアントの所属の記録。
	subTenants    map[string]int64
	clientTenants map[int64]int64
	// assignErr を設定すると、所属の記録の書き込みだけがそのエラーを返す。auditErr は監査ログの書き込みだけ。
	assignErr, auditErr error
	audit               []db.AuditEntry
}

func newFakeDB() *fakeDB {
	return &fakeDB{version: 1, accounts: map[string]db.Account{
		"a@example.com": {ID: 1, Email: "a@example.com", Role: authz.TenantUser, TenantID: 10, TenantName: "テナントA"},
		"b@example.com": {ID: 2, Email: "b@example.com", Role: authz.TenantAdmin, TenantID: 10, TenantName: "テナントA"},
		"c@example.com": {ID: 3, Email: "c@example.com", Role: authz.Operator},
		"d@example.com": {ID: 4, Email: "d@example.com", Role: authz.SystemAdmin},
		"x@example.com": {ID: 5, Email: "x@example.com", Role: authz.Operator, Disabled: true},
	},
		tenants:    []db.Tenant{{ID: 10, Name: "テナントA"}, {ID: 20, Name: "テナントB"}},
		subTenants: map[string]int64{}, clientTenants: map[int64]int64{}}
}

func (f *fakeDB) Ping(context.Context) error { return f.err }
func (f *fakeDB) SchemaVersion(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
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
func (f *fakeDB) Tenants(context.Context) ([]db.Tenant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.tenants), f.err
}
func (f *fakeDB) SubscriberTenant(_ context.Context, imsi string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subTenants[imsi], f.err
}
func (f *fakeDB) SubscriberTenants(_ context.Context, imsis []string) (map[string]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int64{}
	for _, imsi := range imsis {
		if t, ok := f.subTenants[imsi]; ok {
			out[imsi] = t
		}
	}
	return out, f.err
}
func (f *fakeDB) TenantSubscribers(_ context.Context, tenantID int64, prefix, after string, limit int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, imsi := range slices.Sorted(maps.Keys(f.subTenants)) {
		if f.subTenants[imsi] == tenantID && strings.HasPrefix(imsi, prefix) && imsi > after && len(out) < limit {
			out = append(out, imsi)
		}
	}
	return out, f.err
}
func (f *fakeDB) CountTenantSubscribers(_ context.Context, tenantID int64, prefix string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for imsi, t := range f.subTenants {
		if t == tenantID && strings.HasPrefix(imsi, prefix) {
			n++
		}
	}
	return n, f.err
}
func (f *fakeDB) AssignSubscriber(_ context.Context, imsi string, tenantID int64, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := cmp.Or(f.err, f.assignErr); err != nil {
		return err
	}
	if tenantID == 0 {
		delete(f.subTenants, imsi)
	} else {
		f.subTenants[imsi] = tenantID
	}
	return nil
}
func (f *fakeDB) ClientTenant(_ context.Context, id int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clientTenants[id], f.err
}
func (f *fakeDB) ClientTenants(context.Context) (map[int64]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.clientTenants), f.err
}
func (f *fakeDB) AssignClient(_ context.Context, id, tenantID int64, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := cmp.Or(f.err, f.assignErr); err != nil {
		return err
	}
	if tenantID == 0 {
		delete(f.clientTenants, id)
	} else {
		f.clientTenants[id] = tenantID
	}
	return nil
}
func (f *fakeDB) InsertAudit(_ context.Context, e db.AuditEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := cmp.Or(f.err, f.auditErr); err != nil {
		return err
	}
	f.audit = append(f.audit, e)
	return nil
}

// lastAudit は最後の監査ログを返す（なければゼロ値）。
func (f *fakeDB) lastAudit() db.AuditEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.audit) == 0 {
		return db.AuditEntry{}
	}
	return f.audit[len(f.audit)-1]
}

func newHandler(t *testing.T, d *fakeDB, ident auth.Identifier, log *slog.Logger) http.Handler {
	return newHandlerPV(t, d, ident, log, newFakePV())
}

func newHandlerPV(t *testing.T, d *fakeDB, ident auth.Identifier, log *slog.Logger, pv *fakePV) http.Handler {
	t.Helper()
	h, err := New(Options{Log: log, Version: "test", DB: d, Identifier: ident, Provisioner: pv})
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

// post はフォームを POST する（htmx の要求として送る）。
func post(h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "http://ossbss.example"+path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// get は GET する。
func get(h http.Handler, path string) *httptest.ResponseRecorder { return do(h, "GET", path, nil) }

func mustNotContain(t *testing.T, body string, unwanted ...string) {
	t.Helper()
	for _, s := range unwanted {
		if strings.Contains(body, s) {
			t.Errorf("body contains %q", s)
		}
	}
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
	// 画面は内容のハッシュつきの URL で参照し（app.css は CI のテストでは作らないので htmx で確かめる）、その URL でも配信できる（途中のキャッシュの古い版を使わせない）。
	body := do(h, "GET", "/", nil).Body.String()
	url := assetURL("htmx.min.js")
	if !strings.HasPrefix(url, "/static/htmx.min.js?v=") || !strings.Contains(etag, strings.TrimPrefix(url, "/static/htmx.min.js?v=")) ||
		!strings.Contains(body, `src="`+url+`"`) {
		t.Errorf("asset url = %s", url)
	}
	if w := do(h, "GET", url, nil); w.Code != 200 || w.Header().Get("ETag") != etag {
		t.Errorf("versioned static = %d", w.Code)
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

// provisioner の状態は、運用担当とシステム管理者にだけ出す（設計概要 §6.2）。
func TestHomeProvisioner(t *testing.T) {
	d := newFakeDB()
	for _, c := range []struct {
		email string
		shown bool
	}{{"a@example.com", false}, {"b@example.com", false}, {"c@example.com", true}, {"d@example.com", true}} {
		pv := newFakePV()
		body := do(newHandlerPV(t, d, auth.Dev(c.email), discard, pv), "GET", "/", nil).Body.String()
		if got := strings.Contains(body, "<h2 class=\"card-title\">provisioner</h2>"); got != c.shown || (pv.calls > 0) != c.shown {
			t.Errorf("%s: shown %v, calls %d", c.email, got, pv.calls)
		}
		if c.shown {
			mustContain(t, body, "版 0.3.0", "版 0.4.0（ノード poc-01、加入者 3）", "provisioner から接続できません", "aka-only-server に接続できません。",
				"実行中 0、やり直し中 0、失敗 1")
			// aka-only-server に接続できなければ、AVクライアントは確かめられていないので出さない。
			if strings.Contains(body, "AVクライアント") {
				t.Error("av client is shown while aka-only-server is unreachable")
			}
		}
	}

	// provisioner に接続できなければ、原因の見当を出す（画面は返す）。
	pv := newFakePV()
	pv.err = &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	w := do(newHandlerPV(t, d, auth.Dev("d@example.com"), discard, pv), "GET", "/", nil)
	if w.Code != 200 || strings.Count(w.Body.String(), "provisioner に接続できません。") != 1 ||
		!strings.Contains(w.Body.String(), "provisioner が起動しているか") {
		t.Errorf("unavailable = %d %s", w.Code, w.Body)
	}
	// provisioner の応答に時間がかかっても、DB の状態は正しく出す。
	pv.delay = 3500 * time.Millisecond
	if body := do(newHandlerPV(t, d, auth.Dev("d@example.com"), discard, pv), "GET", "/", nil).Body.String(); !strings.Contains(body, "スキーマの版 1") {
		t.Error("db status is wrong while provisioner is slow")
	}
	pv.delay = 0
	// provisioner がエラーを返した。
	pv.err = &provisioner.Error{Status: 500}
	if body := do(newHandlerPV(t, d, auth.Dev("d@example.com"), discard, pv), "GET", "/", nil).Body.String(); !strings.Contains(body, "provisioner がエラーを返しました。") {
		t.Error("error is not shown")
	}
	// provisioner でない相手（本PoCの provisioning-api など）。
	pv.err, pv.status = nil, provisioner.Status{Version: "0.4.0"}
	if body := do(newHandlerPV(t, d, auth.Dev("d@example.com"), discard, pv), "GET", "/", nil).Body.String(); !strings.Contains(body, "provisioner ではないようです") {
		t.Error("not provisioner is not shown")
	}
}

// ---- fakeDB: テナント・アカウント・監査ログ・所属の記録の点検（ステップ 5） ----

func (f *fakeDB) tenantName(id int64) string {
	for _, t := range f.tenants {
		if t.ID == id {
			return t.Name
		}
	}
	return ""
}

func (f *fakeDB) summary(t db.Tenant) db.TenantSummary {
	s := db.TenantSummary{Tenant: t}
	for _, v := range f.subTenants {
		if v == t.ID {
			s.Subscribers++
		}
	}
	for _, v := range f.clientTenants {
		if v == t.ID {
			s.Clients++
		}
	}
	for _, a := range f.accounts {
		if a.TenantID == t.ID {
			s.Accounts++
		}
	}
	return s
}

func (f *fakeDB) TenantSummaries(context.Context) ([]db.TenantSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []db.TenantSummary
	for _, t := range f.tenants {
		out = append(out, f.summary(t))
	}
	return out, f.err
}

func (f *fakeDB) TenantSummary(_ context.Context, id int64) (db.TenantSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.tenants {
		if t.ID == id {
			return f.summary(t), f.err
		}
	}
	return db.TenantSummary{}, cmp.Or(f.err, db.ErrNotFound)
}

func (f *fakeDB) CreateTenant(_ context.Context, name string) (db.Tenant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.tenants {
		if t.Name == name {
			return db.Tenant{}, db.ErrConflict
		}
	}
	t := db.Tenant{ID: int64(100 + len(f.tenants)), Name: name}
	f.tenants = append(f.tenants, t)
	return t, f.err
}

func (f *fakeDB) RenameTenant(_ context.Context, id int64, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := slices.IndexFunc(f.tenants, func(t db.Tenant) bool { return t.ID == id })
	if i < 0 {
		return db.ErrNotFound
	}
	if slices.ContainsFunc(f.tenants, func(t db.Tenant) bool { return t.Name == name && t.ID != id }) {
		return db.ErrConflict
	}
	f.tenants[i].Name = name
	return f.err
}

func (f *fakeDB) DeleteTenant(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := slices.IndexFunc(f.tenants, func(t db.Tenant) bool { return t.ID == id })
	if i < 0 {
		return db.ErrNotFound
	}
	if s := f.summary(f.tenants[i]); s.Subscribers+s.Clients+s.Accounts > 0 {
		return db.ErrInUse
	}
	f.tenants = slices.Delete(f.tenants, i, i+1)
	return f.err
}

func (f *fakeDB) withTenantName(a db.Account) db.Account {
	a.TenantName = f.tenantName(a.TenantID)
	return a
}

func (f *fakeDB) Accounts(_ context.Context, flt db.AccountFilter) ([]db.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []db.Account
	for _, email := range slices.Sorted(maps.Keys(f.accounts)) {
		a := f.accounts[email]
		if flt.All || (flt.NoTenant && a.TenantID == 0) || (!flt.NoTenant && a.TenantID == flt.TenantID) {
			out = append(out, f.withTenantName(a))
		}
	}
	return out, f.err
}

func (f *fakeDB) AccountByID(_ context.Context, id int64) (db.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.accounts {
		if a.ID == id {
			return f.withTenantName(a), f.err
		}
	}
	return db.Account{}, cmp.Or(f.err, db.ErrNotFound)
}

func (f *fakeDB) CreateAccount(_ context.Context, email string, role authz.Role, tenantID int64, by string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.accounts[email]; ok {
		return 0, db.ErrConflict
	}
	id := int64(100 + len(f.accounts))
	f.accounts[email] = db.Account{ID: id, Email: email, Role: role, TenantID: tenantID, CreatedBy: by}
	return id, f.err
}

// accountByID は ID のアカウントのメールアドレスを返す（ロックを持って呼ぶ）。
func (f *fakeDB) emailOf(id int64, from db.AccountState) (string, bool) {
	for email, a := range f.accounts {
		if a.ID == id && a.Role == from.Role && a.TenantID == from.TenantID {
			return email, true
		}
	}
	return "", false
}

func (f *fakeDB) UpdateAccount(_ context.Context, id int64, from, to db.AccountState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	email, ok := f.emailOf(id, from)
	if !ok {
		return db.ErrNotFound
	}
	a := f.accounts[email]
	a.Role, a.TenantID = to.Role, to.TenantID
	f.accounts[email] = a
	return f.err
}

func (f *fakeDB) SetAccountDisabled(_ context.Context, id int64, from db.AccountState, disabled bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	email, ok := f.emailOf(id, from)
	if !ok {
		return db.ErrNotFound
	}
	a := f.accounts[email]
	a.Disabled = disabled
	f.accounts[email] = a
	return f.err
}

func (f *fakeDB) DeleteAccount(_ context.Context, id int64, from db.AccountState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	email, ok := f.emailOf(id, from)
	if !ok {
		return db.ErrNotFound
	}
	delete(f.accounts, email)
	return f.err
}

func (f *fakeDB) ListAudit(_ context.Context, q db.AuditQuery) ([]db.AuditRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []db.AuditRecord
	for i := len(f.audit) - 1; i >= 0; i-- {
		e, id := f.audit[i], int64(i+1)
		switch {
		case q.NoTenant && e.TenantID != 0, !q.AllTenants && !q.NoTenant && e.TenantID != q.TenantID,
			q.Action != "" && e.Action != q.Action, q.Before != 0 && id >= q.Before:
			continue
		}
		if len(out) == q.Limit {
			break
		}
		out = append(out, db.AuditRecord{ID: id, ActorEmail: e.ActorEmail, ActorRole: e.ActorRole, TenantID: e.TenantID,
			Action: e.Action, Target: e.Target, Detail: e.Detail, TraceID: e.TraceID, Remote: e.Remote})
	}
	return out, f.err
}

func (f *fakeDB) SubscriberAssignments(_ context.Context, after string, limit int) ([]db.Assignment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []db.Assignment
	for _, imsi := range slices.Sorted(maps.Keys(f.subTenants)) {
		if imsi > after && len(out) < limit {
			out = append(out, db.Assignment{IMSI: imsi, TenantID: f.subTenants[imsi], AssignedBy: "c@example.com"})
		}
	}
	return out, f.err
}

func (f *fakeDB) ClientAssignments(context.Context) ([]db.Assignment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []db.Assignment
	for _, id := range slices.Sorted(maps.Keys(f.clientTenants)) {
		out = append(out, db.Assignment{ClientID: id, TenantID: f.clientTenants[id]})
	}
	return out, f.err
}
