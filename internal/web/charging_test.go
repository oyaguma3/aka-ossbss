package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oyaguma3/aka-ossbss/internal/audit"
	"github.com/oyaguma3/aka-ossbss/internal/auth"
	"github.com/oyaguma3/aka-ossbss/internal/charging"
	"github.com/oyaguma3/aka-ossbss/internal/db"
)

// fakeDB の課金レコードの操作。

func (f *fakeDB) SetTenantExternalID(_ context.Context, id int64, value string, _ db.Meta) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.ContainsFunc(f.tenants, func(t db.Tenant) bool { return t.ID == id }) {
		return "", db.ErrNotFound
	}
	if f.tenantExternal == nil {
		f.tenantExternal = map[int64]string{}
	}
	before := f.tenantExternal[id]
	f.tenantExternal[id] = value
	return before, f.err
}

func (f *fakeDB) SetLineExternalID(_ context.Context, imsi, value string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.subs[imsi]
	if !ok {
		return "", db.ErrNotFound
	}
	before := e.ExternalLineID
	e.ExternalLineID = value
	f.subs[imsi] = e
	return before, f.err
}

func (f *fakeDB) ChargingPeriods(_ context.Context, limit int) ([]db.ChargingPeriod, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.periods[:min(limit, len(f.periods))], f.err
}

func (f *fakeDB) PendingChargingPeriods(context.Context) ([]db.ChargingPeriod, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []db.ChargingPeriod
	for _, p := range f.periods {
		if p.State == db.PeriodComputed {
			out = append(out, p)
		}
	}
	return out, f.err
}

func (f *fakeDB) ChargingFiles(context.Context) ([]db.ChargingFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.files, f.err
}

func (f *fakeDB) ChargingFileData(_ context.Context, id int64) (db.ChargingFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, x := range f.files {
		if x.ID == id {
			return x, f.err
		}
	}
	return db.ChargingFile{}, db.ErrNotFound
}

func (f *fakeDB) CurrentPGPKey(context.Context) (db.PGPKey, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pgpKeys) == 0 {
		return db.PGPKey{}, false, f.err
	}
	return f.pgpKeys[0], true, f.err
}

func (f *fakeDB) PGPKeys(_ context.Context, limit int) ([]db.PGPKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pgpKeys[:min(limit, len(f.pgpKeys))], f.err
}

func (f *fakeDB) UsageImportFailures(context.Context, int) ([]db.UsageImportFailure, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.usageFailures, f.err
}

// fakeCharging は課金レコードの実行の代わり（呼ばれた操作を記録する）。
type fakeCharging struct {
	calls []string
	busy  bool
	err   error
	key   error
}

func (f *fakeCharging) Busy() bool { return f.busy }

func (f *fakeCharging) StartImport(_ context.Context, by string) error {
	f.calls = append(f.calls, "import "+by)
	return f.err
}

func (f *fakeCharging) StartClose(_ context.Context, by string) error {
	f.calls = append(f.calls, "close "+by)
	return f.err
}

func (f *fakeCharging) StartRebuild(_ context.Context, month time.Time, by string) error {
	f.calls = append(f.calls, "rebuild "+month.Format("2006-01")+" "+by)
	return f.err
}

func (f *fakeCharging) RegisterKey(_ context.Context, armored, by string) (db.PGPKey, error) {
	f.calls = append(f.calls, "key "+by)
	return db.PGPKey{}, f.key
}

type chargingEnv struct {
	*testEnv
	run *fakeCharging
}

func newChargingEnv() *chargingEnv {
	return &chargingEnv{testEnv: newTestEnv(), run: &fakeCharging{}}
}

func (e *chargingEnv) handler(t *testing.T, email string) http.Handler {
	t.Helper()
	h, err := New(Options{Log: discard, Version: "test", DB: e.db, Identifier: auth.Dev(email), Provisioner: e.pv,
		AuditFiles: audit.NewFiles(t.TempDir(), 14), Charging: e.run})
	if err != nil {
		t.Fatal(err)
	}
	return h.Routes()
}

func octoberPeriod(state string) db.ChargingPeriod {
	return db.ChargingPeriod{Period: time.Date(2026, 10, 1, 0, 0, 0, 0, db.JST), Sequence: 3, State: state, LineCount: 11, TenantCount: 2, EventCount: 5, AdjCount: 1, ClosedAt: time.Now(), ClosedBy: "system"}
}

func TestChargingRoles(t *testing.T) {
	e := newChargingEnv()
	e.db.periods = []db.ChargingPeriod{octoberPeriod(db.PeriodFiled)}
	e.db.files = []db.ChargingFile{{ID: 7, Period: octoberPeriod("").Period, Version: 2, FileName: "charging_202610_000003_v2.zip.pgp",
		KeyFingerprint: "ABCDEF0123456789", Size: 2048, CreatedAt: time.Now(), Data: []byte("secret-bytes")}}
	// A・B は見られず、操作もできない。
	for _, who := range []string{"a@example.com", "b@example.com"} {
		h := e.handler(t, who)
		for _, p := range []string{"/charging", "/charging/files/7"} {
			if w := get(h, p); w.Code != 403 {
				t.Errorf("%s GET %s = %d", who, p, w.Code)
			}
		}
		mustNotContain(t, get(h, "/").Body.String(), `href="/charging"`)
	}
	// C は見てダウンロードできるが、操作はできない。
	h := e.handler(t, "c@example.com")
	w := get(h, "/charging")
	if w.Code != 200 {
		t.Fatalf("operator list = %d", w.Code)
	}
	body := w.Body.String()
	mustContain(t, body, "charging_202610_000003_v2.zip.pgp", "2026-10", `href="/charging"`)
	mustNotContain(t, body, "今すぐ締める", "公開鍵を登録する", "作り直す")
	for _, p := range []string{"/charging/import", "/charging/close", "/charging/key", "/charging/2026-10/rebuild"} {
		if w := post(h, p, url.Values{}); w.Code != 403 {
			t.Errorf("operator POST %s = %d", p, w.Code)
		}
	}
	if len(e.run.calls) != 0 {
		t.Errorf("calls = %v", e.run.calls)
	}
	// ダウンロードは暗号文をそのまま返し、毎回監査ログに残す。
	w = get(h, "/charging/files/7")
	if w.Code != 200 || w.Body.String() != "secret-bytes" || !strings.Contains(w.Header().Get("Content-Disposition"), "charging_202610_000003_v2.zip.pgp") ||
		w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("download = %d %q %v", w.Code, w.Body.String(), w.Header())
	}
	get(h, "/charging/files/7")
	n := 0
	for _, a := range e.db.audit {
		if a.Action == audit.ActionChargingDownload && a.Target == "charging_202610_000003_v2.zip.pgp" && a.ActorEmail == "c@example.com" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("download audit = %d: %+v", n, e.db.audit)
	}
	if w := get(h, "/charging/files/8"); w.Code != 404 {
		t.Errorf("missing file = %d", w.Code)
	}
	// D は操作できる。
	h = e.handler(t, "d@example.com")
	mustContain(t, get(h, "/charging").Body.String(), "今すぐ締める", "公開鍵を登録する", "作り直す")
	for _, p := range []string{"/charging/import", "/charging/close", "/charging/2026-10/rebuild"} {
		if w := post(h, p, url.Values{}); w.Code != 200 || !strings.HasPrefix(w.Header().Get("HX-Redirect"), "/charging?started=") {
			t.Errorf("admin POST %s = %d %v", p, w.Code, w.Header())
		}
	}
	if got := strings.Join(e.run.calls, ","); got != "import d@example.com,close d@example.com,rebuild 2026-10 d@example.com" {
		t.Errorf("calls = %s", got)
	}
	if w := post(h, "/charging/not-a-month/rebuild", url.Values{}); w.Code != 404 {
		t.Errorf("bad month = %d", w.Code)
	}
}

func TestChargingErrors(t *testing.T) {
	e := newChargingEnv()
	h := e.handler(t, "d@example.com")
	for _, c := range []struct {
		err  error
		code int
		want string
	}{
		{charging.ErrRunning, 409, "動いています"},
		{charging.ErrNoKey, 409, "公開鍵がありません"},
		{charging.ErrNotClosed, 400, "まだ締めていません"},
	} {
		e.run.err = c.err
		w := post(h, "/charging/2026-10/rebuild", url.Values{})
		if w.Code != c.code {
			t.Errorf("%v = %d", c.err, w.Code)
		}
		mustContain(t, w.Body.String(), c.want)
	}
	e.run.err = nil
	// 登録できない鍵は誤りを出し、公開鍵の入力は残す。秘密鍵は戻さない。
	e.run.key = &charging.KeyError{Message: "鍵は 1 つだけ登録できます。"}
	w := post(h, "/charging/key", url.Values{"armored": {"PUBLIC-BLOCK"}})
	if w.Code != 400 {
		t.Errorf("bad key = %d", w.Code)
	}
	mustContain(t, w.Body.String(), "鍵は 1 つだけ", "PUBLIC-BLOCK")
	w = post(h, "/charging/key", url.Values{"armored": {"-----BEGIN PGP PRIVATE KEY BLOCK-----"}})
	mustNotContain(t, w.Body.String(), "PRIVATE KEY BLOCK")
	e.run.key = errors.New("db")
	if w := post(h, "/charging/key", url.Values{"armored": {"x"}}); w.Code != 503 {
		t.Errorf("db error = %d", w.Code)
	}
}

func TestChargingDashboard(t *testing.T) {
	e := newChargingEnv()
	e.db.periods = []db.ChargingPeriod{octoberPeriod(db.PeriodComputed)}
	// C・D には最新の締めと保留の警告を出す。A・B には出さない。
	body := get(e.handler(t, "c@example.com"), "/").Body.String()
	mustContain(t, body, "最新の締め: 2026-10", "1 か月分のファイルを保留", "ファイルのない月: 1 か月")
	mustNotContain(t, get(e.handler(t, "b@example.com"), "/").Body.String(), "最新の締め")
	// 一覧にも同じ警告と、読み切っていない日を出す。
	e.db.usageFailures = []db.UsageImportFailure{{Date: time.Date(2026, 10, 12, 0, 0, 0, 0, db.JST)}}
	mustContain(t, get(e.handler(t, "c@example.com"), "/charging").Body.String(), "1 か月分のファイルを保留", "2026-10-12")
	// 実行が動いている間は読み込み直す。動いていなければしない。
	mustNotContain(t, get(e.handler(t, "c@example.com"), "/charging").Body.String(), `hx-trigger="every 3s"`)
	e.run.busy = true
	mustContain(t, get(e.handler(t, "c@example.com"), "/charging").Body.String(), `hx-trigger="every 3s"`)
}

func TestExternalIDs(t *testing.T) {
	e := newChargingEnv()
	// 運用担当はテナントの一覧と詳細を見て、外部の顧客番号を変えられる。名前の変更・削除・作成はできない。
	h := e.handler(t, "c@example.com")
	w := get(h, "/tenants")
	if w.Code != 200 {
		t.Fatalf("operator tenants = %d", w.Code)
	}
	mustNotContain(t, w.Body.String(), `hx-post="/tenants"`)
	body := get(h, "/tenants/10").Body.String()
	mustContain(t, body, `/tenants/10/external`)
	mustNotContain(t, body, "/tenants/10/rename", "/tenants/10/delete")
	if w := post(h, "/tenants/10/rename", url.Values{"name": {"x"}}); w.Code != 403 {
		t.Errorf("operator rename = %d", w.Code)
	}
	if w := post(h, "/tenants/10/external", url.Values{"external": {"  C-100 "}}); w.Code != 200 {
		t.Errorf("operator external = %d", w.Code)
	}
	if e.db.tenantExternal[10] != "C-100" {
		t.Errorf("external = %v", e.db.tenantExternal)
	}
	if a := e.db.audit[len(e.db.audit)-1]; a.Action != audit.ActionTenantExternal || a.TenantID != 10 {
		t.Errorf("audit = %+v", a)
	}
	if w := post(h, "/tenants/10/external", url.Values{"external": {strings.Repeat("あ", 101)}}); w.Code != 400 {
		t.Errorf("too long = %d", w.Code)
	}
	if w := post(h, "/tenants/10/external", url.Values{"external": {"a\nb"}}); w.Code != 400 {
		t.Errorf("control character = %d", w.Code)
	}
	if w := post(h, "/tenants/999/external", url.Values{"external": {"x"}}); w.Code != 404 {
		t.Errorf("missing tenant = %d", w.Code)
	}
	// A・B はテナントの画面を開けない。
	if w := get(e.handler(t, "b@example.com"), "/tenants"); w.Code != 403 {
		t.Errorf("tenant admin tenants = %d", w.Code)
	}
	if w := post(e.handler(t, "b@example.com"), "/tenants/10/external", url.Values{"external": {"x"}}); w.Code != 403 {
		t.Errorf("tenant admin external = %d", w.Code)
	}

	// 回線の外部の番号は運用担当とシステム管理者が設定でき、A・B には出さない。
	body = get(h, "/subscribers/"+imsiOwn).Body.String()
	mustContain(t, body, "/subscribers/"+imsiOwn+"/external")
	if w := post(h, "/subscribers/"+imsiOwn+"/external", url.Values{"external": {"L-1"}}); w.Code != 200 {
		t.Errorf("line external = %d", w.Code)
	}
	if got, _ := e.db.entry(imsiOwn); got.ExternalLineID != "L-1" {
		t.Errorf("line = %+v", got)
	}
	if a := e.db.audit[len(e.db.audit)-1]; a.Action != audit.ActionLineExternal || a.Target != imsiOwn || a.TenantID != 10 {
		t.Errorf("audit = %+v", a)
	}
	b := e.handler(t, "b@example.com")
	mustNotContain(t, get(b, "/subscribers/"+imsiOwn).Body.String(), "/external", "L-1")
	if w := post(b, "/subscribers/"+imsiOwn+"/external", url.Values{"external": {"x"}}); w.Code != 403 {
		t.Errorf("tenant admin line external = %d", w.Code)
	}
}
