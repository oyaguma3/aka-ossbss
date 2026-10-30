package web

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
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

	"github.com/oyaguma3/aka-ossbss/internal/access"
	"github.com/oyaguma3/aka-ossbss/internal/access/accesstest"
	"github.com/oyaguma3/aka-ossbss/internal/audit"
	"github.com/oyaguma3/aka-ossbss/internal/auth"
	"github.com/oyaguma3/aka-ossbss/internal/authz"
	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
)

var discard = slog.New(slog.DiscardHandler)

// fakeDB はテスト用の DB。err を設定すると、すべての呼び出しがそのエラーを返す。
// テナントは 10（テナントA）と 20（テナントB）。テナントユーザー a とテナント管理者 b はテナントA に属する。
// 台帳は db.Transition で書く（本物の DB と同じ移り方）。
type fakeDB struct {
	mu       sync.Mutex
	version  int
	err      error
	accounts map[string]db.Account
	tenants  []db.Tenant
	// subs は台帳、events は状態の事実。
	subs   map[string]db.Entry
	events []db.ChargingEvent
	// ledgerErr を設定すると、台帳の書き込み（Apply）だけがそのエラーを返す。auditErr は監査ログの書き込みだけ。
	ledgerErr, auditErr error
	audit               []db.AuditEntry
	// ジョブ（jobs_test.go）。
	jobs       map[int64]*db.Job
	jobDeleted map[int64][]string
	jobSealed  map[int64][]byte
	jobRows    map[int64][]db.JobRow
}

func newFakeDB() *fakeDB {
	return &fakeDB{version: 1, accounts: map[string]db.Account{
		"a@example.com": {ID: 1, Email: "a@example.com", Role: authz.TenantUser, TenantID: 10, TenantName: "テナントA"},
		"b@example.com": {ID: 2, Email: "b@example.com", Role: authz.TenantAdmin, TenantID: 10, TenantName: "テナントA"},
		"c@example.com": {ID: 3, Email: "c@example.com", Role: authz.Operator},
		"d@example.com": {ID: 4, Email: "d@example.com", Role: authz.SystemAdmin},
		"x@example.com": {ID: 5, Email: "x@example.com", Role: authz.Operator, Disabled: true},
	},
		tenants: []db.Tenant{{ID: 10, Name: "テナントA"}, {ID: 20, Name: "テナントB"}},
		subs:    map[string]db.Entry{}}
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

// put は台帳に行を置く（試験の準備）。
func (f *fakeDB) put(e db.Entry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs[e.IMSI] = e
}

// entry は台帳の行を返す。
func (f *fakeDB) entry(imsi string) (db.Entry, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.subs[imsi]
	return e, ok
}

// eventTexts は状態の事実を「事実 側 テナント 前→後」で返す。
func (f *fakeDB) eventTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, ev := range f.events {
		out = append(out, strings.TrimSpace(fmt.Sprintf("%s %s %d %s→%s", ev.Event, ev.Side, ev.TenantID, ev.From, ev.To)))
	}
	return out
}

func (f *fakeDB) Subscriber(_ context.Context, imsi string) (db.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return db.Entry{}, f.err
	}
	e, ok := f.subs[imsi]
	if !ok {
		return db.Entry{}, db.ErrNotFound
	}
	return e, nil
}

// match は台帳の絞り込みに当たるか。
func match(e db.Entry, flt db.SubscriberFilter) bool {
	state := func(present bool, s, want string) bool {
		switch want {
		case "":
			return true
		case db.FilterAbsent:
			return !present
		}
		return s == want
	}
	return (flt.TenantID == 0 || e.TenantID == flt.TenantID) && strings.HasPrefix(e.IMSI, flt.Prefix) &&
		state(e.VecgenPresent, e.VecgenState, flt.VecgenState) && state(e.RadiusPresent, e.RadiusState, flt.RadiusState) &&
		(flt.SourceID == "" || e.RadiusSourceID == flt.SourceID) && (!flt.Mismatch || e.Mismatch()) &&
		e.Deleted() == flt.Deleted && (!flt.CheckNeeded || e.CheckNeeded)
}

func (f *fakeDB) ListSubscribers(_ context.Context, flt db.SubscriberFilter, after string, limit int) ([]db.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []db.Entry
	for _, imsi := range slices.Sorted(maps.Keys(f.subs)) {
		if e := f.subs[imsi]; imsi > after && match(e, flt) && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, f.err
}
func (f *fakeDB) CountSubscribers(_ context.Context, flt db.SubscriberFilter) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, e := range f.subs {
		if match(e, flt) {
			n++
		}
	}
	return n, f.err
}
func (f *fakeDB) SubscriberSources(_ context.Context, tenant int64) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, e := range f.subs {
		if e.RadiusSourceID != "" && (tenant == 0 || e.TenantID == tenant) && !slices.Contains(out, e.RadiusSourceID) {
			out = append(out, e.RadiusSourceID)
		}
	}
	slices.Sort(out)
	return out, f.err
}
func (f *fakeDB) CountLedger(_ context.Context, tenant int64) (db.LedgerCounts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := db.LedgerCounts{Vecgen: map[string]int64{}, Radius: map[string]int64{}}
	for _, e := range f.subs {
		switch {
		case tenant != 0 && e.TenantID != tenant:
			continue
		case e.Deleted():
			c.Deleted++
			continue
		}
		c.Total++
		c.Vecgen[cmp.Or(e.VecgenState, db.FilterAbsent)]++
		c.Radius[cmp.Or(e.RadiusState, db.FilterAbsent)]++
		if e.Mismatch() {
			c.Mismatch++
		}
		if e.CheckNeeded {
			c.CheckNeeded++
		}
	}
	return c, f.err
}

// fakeLedger は WithSubscriber の中の台帳（書いた変更は fn が成功したときだけ残す）。
type fakeLedger struct {
	f      *fakeDB
	meta   db.Meta
	cur    db.Entry
	exists bool
	events []db.ChargingEvent
}

func (l *fakeLedger) Entry() (db.Entry, bool) { return l.cur, l.exists }
func (l *fakeLedger) Apply(ch db.Change) error {
	if l.f.ledgerErr != nil {
		return l.f.ledgerErr
	}
	res, err := db.Transition(l.cur, l.exists, ch, l.meta, time.Now())
	if err != nil {
		return err
	}
	if e := res.Entry; res.Changed && !res.Remove && !e.Deleted() && !slices.ContainsFunc(l.f.tenants, func(t db.Tenant) bool { return t.ID == e.TenantID }) {
		return db.ErrTenantNotFound
	}
	l.cur, l.exists = res.Entry, !res.Remove
	l.events = append(l.events, res.Events...)
	return nil
}

func (f *fakeDB) WithSubscriber(_ context.Context, imsi string, meta db.Meta, fn func(db.Ledger) error) error {
	f.mu.Lock()
	if f.err != nil {
		f.mu.Unlock()
		return f.err
	}
	cur, ok := f.subs[imsi]
	if !ok {
		cur = db.Entry{IMSI: imsi}
	}
	l := &fakeLedger{f: f, meta: meta, cur: cur, exists: ok}
	f.mu.Unlock()
	if err := fn(l); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if l.exists {
		f.subs[imsi] = l.cur
	} else {
		delete(f.subs, imsi)
	}
	f.events = append(f.events, l.events...)
	return nil
}

func (f *fakeDB) InsertAudit(_ context.Context, e db.AuditEntry) (int64, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := cmp.Or(f.err, f.auditErr); err != nil {
		return 0, time.Time{}, err
	}
	f.audit = append(f.audit, e)
	return int64(len(f.audit)), time.Now(), nil
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
	h, err := New(Options{Log: log, Version: "test", DB: d, Identifier: ident, Provisioner: pv, AuditFiles: audit.NewFiles(t.TempDir(), 14)})
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
	mustContain(t, body, "<title>ダッシュボード | aka-ossbss</title>", "a@example.com", "テナントユーザー", "テナントA",
		"<td>加入者の状態の変更</td>\n            <td>自分のテナントのもの</td>",
		"<td>radius 側の登録</td>\n            <td>できない</td>", "開発用の設定", `<span class="badge badge-success">接続中</span> スキーマのバージョン 1`)
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
	// 運用担当: テナントに属さない。vecgen 側の登録・変更はできない。
	body = do(newDevHandler(t, d, "c@example.com"), "GET", "/", nil).Body.String()
	mustContain(t, body, "運用担当", "なし（すべてのテナントを扱う）",
		"<td>radius 側の登録</td>\n            <td>すべて</td>", "<td>vecgen 側の登録（鍵の入力）</td>\n            <td>できない</td>")
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

// aka-provisioner と下流の状態は、運用担当とシステム管理者にだけ出す（設計概要 §5）。台帳の件数は役割の範囲で数える。
func TestHomeProvisioner(t *testing.T) {
	d := newFakeDB()
	d.put(db.Entry{IMSI: imsiOwn, TenantID: 10, VecgenPresent: true, VecgenState: "active"})
	d.put(db.Entry{IMSI: imsiOther, TenantID: 20, VecgenPresent: true, VecgenState: "active", RadiusPresent: true, RadiusState: "active", RadiusSourceID: "vecgen"})
	for _, c := range []struct {
		email string
		shown bool
		total string
	}{{"a@example.com", false, "1"}, {"b@example.com", false, "1"}, {"c@example.com", true, "2"}, {"d@example.com", true, "2"}} {
		pv := newFakePV()
		body := do(newHandlerPV(t, d, auth.Dev(c.email), discard, pv), "GET", "/", nil).Body.String()
		if got := strings.Contains(body, "aka-provisioner と下流</h2>"); got != c.shown || (pv.statusCalls > 0) != c.shown {
			t.Errorf("%s: shown %v, calls %d", c.email, got, pv.statusCalls)
		}
		mustContain(t, body, `<div class="stat-title">加入者（台帳）</div>
    <div class="stat-value">`+c.total+`</div>`)
		if c.shown {
			mustContain(t, body, "バージョン 2.0.0（起動", "バージョン 0.5.0（加入者 3）",
				`<span class="badge badge-error">切断</span> aka-radius に届きません。`)
		}
	}
	// aka-provisioner に接続できなければ、原因の見当を出す（画面は返す）。
	pv := newFakePV()
	pv.statusErr = &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	w := do(newHandlerPV(t, d, auth.Dev("d@example.com"), discard, pv), "GET", "/", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "provisioner が起動しているか") {
		t.Errorf("unavailable = %d %s", w.Code, w.Body)
	}
	// aka-provisioner 2.0.0 でない相手。
	pv.statusErr, pv.status = nil, provisioner.Status{Version: "0.3.0"}
	if body := do(newHandlerPV(t, d, auth.Dev("d@example.com"), discard, pv), "GET", "/", nil).Body.String(); !strings.Contains(body, "aka-provisioner 2.0.0 ではないようです") {
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
	for _, e := range f.subs {
		if e.TenantID == t.ID && !e.Deleted() {
			s.Subscribers++
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

func (f *fakeDB) CreateTenant(_ context.Context, name string, _ db.Meta) (db.Tenant, error) {
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

func (f *fakeDB) RenameTenant(_ context.Context, id int64, name string, _ db.Meta) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := slices.IndexFunc(f.tenants, func(t db.Tenant) bool { return t.ID == id })
	if i < 0 {
		return "", db.ErrNotFound
	}
	if slices.ContainsFunc(f.tenants, func(t db.Tenant) bool { return t.Name == name && t.ID != id }) {
		return "", db.ErrConflict
	}
	before := f.tenants[i].Name
	f.tenants[i].Name = name
	return before, f.err
}

func (f *fakeDB) DeleteTenant(_ context.Context, id int64, _ db.Meta) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := slices.IndexFunc(f.tenants, func(t db.Tenant) bool { return t.ID == id })
	if i < 0 {
		return db.ErrNotFound
	}
	if s := f.summary(f.tenants[i]); s.Subscribers+s.Accounts > 0 {
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
