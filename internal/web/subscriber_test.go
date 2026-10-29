package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oyaguma3/aka-ossbss/internal/audit"
	"github.com/oyaguma3/aka-ossbss/internal/auth"
	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
)

// 試験の加入者。own はテナントA（a・b のテナント）の両側 Active、other はテナントB の両側 Active、
// vecgenOnly はテナントA の vecgen 側だけ（Inactive）、deleted はテナントA の削除済み。
const (
	imsiOwn        = "001010000000001"
	imsiOther      = "001010000000002"
	imsiVecgenOnly = "001010000000003"
	imsiDeleted    = "001010000000004"
	imsiNew        = "001010000000009"
	testKey        = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
)

var testRoles = []string{"a@example.com", "b@example.com", "c@example.com", "d@example.com"}

// testEnv は台帳と下流に加入者を用意した DB と aka-provisioner。
type testEnv struct {
	db       *fakeDB
	pv       *fakePV
	auditDir string
}

func newTestEnv() *testEnv {
	e := &testEnv{db: newFakeDB(), pv: newFakePV()}
	both := func(imsi string, tenant int64) {
		e.db.put(db.Entry{IMSI: imsi, TenantID: tenant, VecgenPresent: true, VecgenState: "active", RadiusPresent: true,
			RadiusState: "active", RadiusSourceID: "vecgen", RegisteredAt: time.Now()})
		e.pv.addVecgen(imsi, "active")
		e.pv.addRadius(imsi, "active", tenant)
	}
	both(imsiOwn, 10)
	both(imsiOther, 20)
	e.db.put(db.Entry{IMSI: imsiVecgenOnly, TenantID: 10, VecgenPresent: true, VecgenState: "inactive"})
	e.pv.addVecgen(imsiVecgenOnly, "inactive")
	e.db.put(db.Entry{IMSI: imsiDeleted, TenantID: 10, DeletedAt: time.Now()})
	return e
}

// handler は email の利用者（開発用の設定）の画面を作る。監査ログのファイルは e.auditDir に書く。
func (e *testEnv) handler(t *testing.T, email string) http.Handler {
	t.Helper()
	if e.auditDir == "" {
		e.auditDir = t.TempDir()
	}
	h, err := New(Options{Log: discard, Version: "test", DB: e.db, Identifier: auth.Dev(email), Provisioner: e.pv,
		AuditFiles: audit.NewFiles(e.auditDir, 14)})
	if err != nil {
		t.Fatal(err)
	}
	return h.Routes()
}

// form はフォームの値を作る（"名前", "値", …）。
func form(kv ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Add(kv[i], kv[i+1])
	}
	return v
}

// TestSubscriberScopeMatrix は、加入者の操作を役割 × 対象のテナント（自分のテナント・他のテナント）で確かめる
// （設計概要 §5）。範囲の外は 404、範囲の中でも役割にない操作は 403。どちらのときも下流に書き込まず、監査ログも残さない。
func TestSubscriberScopeMatrix(t *testing.T) {
	type route struct {
		method, path string
		form         url.Values
		// roles は、自分のテナントのもので許す役割（a b c d）。scoped は A・B が他のテナントのものを開けないこと。
		roles string
	}
	routes := func(imsi string) []route {
		p := "/subscribers/" + imsi
		return []route{
			{"GET", p, nil, "abcd"},
			{"POST", p + "/state", form("side", "radius", "state", "inactive", "idem", testKey), "abcd"},
			{"POST", p + "/reinstate", form("side", "radius", "reason", "r"), "d"},
			{"POST", p + "/delete", form("side", "radius"), "cd"},
			{"POST", p + "/vecgen", form("amf", "9000", "sqn", "000000000020", "sqn_type", "inc32"), "d"},
			{"POST", p + "/source", form("source", "vecgen-2"), "cd"},
			{"POST", p + "/tenant", form("tenant", "20"), "d"},
			{"GET", p + "/policy", nil, "bcd"},
			{"POST", p + "/policy/edit", form("op", "add"), "bcd"},
			{"POST", p + "/policy", form("nas_id", "*", "ssids", "*", "vlan_id", "", "session_timeout", ""), "bcd"},
		}
	}
	for _, target := range []struct {
		imsi string
		own  bool
	}{{imsiOwn, true}, {imsiOther, false}} {
		for _, rt := range routes(target.imsi) {
			for _, email := range testRoles {
				role := email[:1]
				e := newTestEnv()
				h := e.handler(t, email)
				var w *httpResult
				if rt.method == "GET" {
					w = result(get(h, rt.path))
				} else {
					w = result(post(h, rt.path, rt.form))
				}
				staff := role == "c" || role == "d"
				allowed := strings.Contains(rt.roles, role)
				switch {
				case !target.own && !staff:
					if w.code != http.StatusNotFound {
						t.Errorf("%s %s by %s = %d, want 404", rt.method, rt.path, role, w.code)
					}
				case !allowed:
					if w.code != http.StatusForbidden {
						t.Errorf("%s %s by %s = %d, want 403", rt.method, rt.path, role, w.code)
					}
				default:
					if w.code == http.StatusForbidden || w.code == http.StatusNotFound || w.code >= 500 {
						t.Errorf("%s %s by %s = %d, want success: %s", rt.method, rt.path, role, w.code, w.body)
					}
				}
				if (!allowed || (!target.own && !staff)) && (len(e.pv.writes()) != 0 || len(e.db.audit) != 0 || len(e.db.events) != 0) {
					t.Errorf("%s %s by %s: side effects %v %v", rt.method, rt.path, role, e.pv.writes(), e.db.audit)
				}
			}
		}
	}
	// 台帳にない IMSI は、役割によらず存在しないときと同じ 404。
	if w := get(newTestEnv().handler(t, "d@example.com"), "/subscribers/"+imsiNew); w.Code != 404 {
		t.Errorf("missing = %d", w.Code)
	}
}

type httpResult struct {
	code int
	body string
}

func result(w *httptest.ResponseRecorder) *httpResult {
	return &httpResult{code: w.Code, body: w.Body.String()}
}

// 登録の画面を出せる役割（vecgen 側は D、radius 側は C・D）。
func TestRegisterRoutesByRole(t *testing.T) {
	for _, c := range []struct {
		path  string
		roles string
	}{{"/subscribers/new?side=vecgen", "d"}, {"/subscribers/new?side=radius", "cd"}, {"/subscribers/new", "cd"}} {
		for _, email := range testRoles {
			code := get(newTestEnv().handler(t, email), c.path).Code
			if want := strings.Contains(c.roles, email[:1]); (code == 200) != want {
				t.Errorf("%s by %s = %d", c.path, email, code)
			}
		}
	}
	for _, c := range []struct{ path, roles string }{{"/subscribers/new/vecgen", "d"}, {"/subscribers/new/radius", "cd"}} {
		for _, email := range testRoles {
			e := newTestEnv()
			code := post(e.handler(t, email), c.path, form("imsi", imsiNew)).Code
			if want := strings.Contains(c.roles, email[:1]); (code != 403) != want || (!want && len(e.pv.writes()) != 0) {
				t.Errorf("POST %s by %s = %d", c.path, email, code)
			}
		}
	}
}

func TestRegister(t *testing.T) {
	e := newTestEnv()
	hd := e.handler(t, "d@example.com")
	const ki, opc = "465b5ce8b199b49faa5f0a2ee238a6bc", "cd63cb71954a9f4e48a5994e37a02baf"
	vecgen := form("imsi", imsiNew, "tenant", "10", "state", "active", "ki", ki, "opc", opc, "amf", "8000", "sqn", "000000000000",
		"sqn_type", "inc32", "clients", "2", "clients", "1", "idem", testKey)

	// 入力の誤り（テナントを選んでいない、Ki の桁）。Ki・OPc は画面に戻さない。
	bad := form("imsi", imsiNew, "state", "active", "ki", "zz", "opc", opc, "amf", "8000", "sqn", "000000000000", "sqn_type", "inc32")
	w := post(hd, "/subscribers/new/vecgen", bad)
	if w.Code != 400 || len(e.pv.writes()) != 0 {
		t.Fatalf("bad = %d", w.Code)
	}
	mustContain(t, w.Body.String(), "所属のテナントを選んでください。", "Ki は 16 進 32 桁で入力してください。")
	mustNotContain(t, w.Body.String(), opc)

	w = post(hd, "/subscribers/new/vecgen", vecgen)
	if w.Code != 200 || w.Header().Get("HX-Redirect") != "/subscribers/"+imsiNew+"?created=vecgen" {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	if op := e.pv.lastOp(); op != "vecgen.create d@example.com "+testKey {
		t.Errorf("op = %q", op)
	}
	if c := e.pv.lastVecgenCreate; c.Ki != ki || c.OPc != opc || !slices.Equal(c.AllowedClientIDs, []int64{1, 2}) || c.State != "active" {
		t.Errorf("sent = %+v", c)
	}
	en, _ := e.db.entry(imsiNew)
	if en.TenantID != 10 || en.VecgenState != "active" || en.RadiusPresent || en.RegisteredBy != "d@example.com" {
		t.Errorf("ledger = %+v", en)
	}
	if got := e.db.eventTexts(); !slices.Equal(got, []string{"line_registered vecgen 10 →active"}) {
		t.Errorf("events = %v", got)
	}
	// 鍵は監査ログ（DB・ファイル）に残さない。
	a := e.db.lastAudit()
	if a.Action != auditVecgenCreate || a.TenantID != 10 || a.Target != imsiNew || a.Detail["amf"] != "8000" {
		t.Errorf("audit = %+v", a)
	}
	files, _ := filepath.Glob(filepath.Join(e.auditDir, "audit-*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("audit files = %v", files)
	}
	b, _ := os.ReadFile(files[0])
	if !strings.Contains(string(b), `"action":"subscriber.vecgen.create"`) || strings.Contains(string(b), ki) || strings.Contains(string(b), opc) {
		t.Errorf("audit file = %s", b)
	}
	for _, x := range e.db.audit {
		mustNotContain(t, detailText(x.Detail), ki, opc)
	}

	// 同じ側は登録し直せない（下流に送らない）。
	before := len(e.pv.writes())
	if w := post(hd, "/subscribers/new/vecgen", vecgen); w.Code != 409 || !strings.Contains(w.Body.String(), "登録済みです") || len(e.pv.writes()) != before {
		t.Errorf("twice = %d", w.Code)
	}

	// radius 側（運用担当）。テナントは台帳の所属で固定する（フォームのテナントは使わない）。ルールのテナントも所属のテナント。
	hc := e.handler(t, "c@example.com")
	if body := get(hc, "/subscribers/new?side=radius&imsi="+imsiNew).Body.String(); !strings.Contains(body, "もう一方の側の登録で決まっています") {
		t.Error("fixed tenant is not shown")
	}
	w = post(hc, "/subscribers/new/radius", form("imsi", imsiNew, "tenant", "20", "state", "inactive", "source", "vecgen",
		"nas_id", "ap-01", "ssids", "corp\nguest", "vlan_id", "100", "session_timeout", "3600", "idem", testKey))
	if w.Code != 200 || w.Header().Get("HX-Redirect") == "" {
		t.Fatalf("radius = %d %s", w.Code, w.Body)
	}
	rs := e.pv.radius[imsiNew]
	if r := rs.Rules; rs.SourceID != "vecgen" || len(r) != 1 || r[0].TenantID != 10 || r[0].NASID != "ap-01" ||
		!slices.Equal(r[0].SSIDs, []string{"corp", "guest"}) || r[0].VLANID != 100 || r[0].SessionTimeout != 3600 {
		t.Errorf("radius = %+v", rs)
	}
	if en, _ := e.db.entry(imsiNew); en.TenantID != 10 || en.RadiusState != "inactive" || en.RadiusSourceID != "vecgen" {
		t.Errorf("ledger = %+v", en)
	}
	// ルールの入力の誤り（* とほかの SSID、VLAN の範囲）。
	w = post(hc, "/subscribers/new/radius", form("imsi", "001010000000010", "tenant", "10", "state", "active", "source", "vecgen",
		"nas_id", "*", "ssids", "*\ncorp", "vlan_id", "0", "session_timeout", ""))
	if w.Code != 400 {
		t.Errorf("bad rule = %d", w.Code)
	}
	mustContain(t, w.Body.String(), "* はほかの SSID と一緒に書けません。", "VLAN は 1〜4094")
	// 下流に既にある（台帳にない）: 台帳は書かない。
	e.pv.addRadius("001010000000011", "active", 20)
	w = post(hc, "/subscribers/new/radius", form("imsi", "001010000000011", "tenant", "10", "state", "active", "source", "vecgen",
		"nas_id", "*", "ssids", "*"))
	if w.Code != 409 || !strings.Contains(w.Body.String(), "既に登録されています") {
		t.Errorf("exists downstream = %d", w.Code)
	}
	if _, ok := e.db.entry("001010000000011"); ok {
		t.Error("ledger was written for a failed registration")
	}
}

// 削除済みの IMSI の登録し直しは、システム管理者の承認（確認と理由）が要る（決定事項 2）。
func TestReregisterDeleted(t *testing.T) {
	e := newTestEnv()
	radius := func(extra ...string) url.Values {
		return form(append([]string{"imsi", imsiDeleted, "tenant", "20", "state", "active", "source", "vecgen", "nas_id", "*", "ssids", "*"}, extra...)...)
	}
	w := post(e.handler(t, "c@example.com"), "/subscribers/new/radius", radius())
	if w.Code != 409 || !strings.Contains(w.Body.String(), "システム管理者の承認が要ります") || len(e.pv.writes()) != 0 {
		t.Errorf("operator = %d", w.Code)
	}
	hd := e.handler(t, "d@example.com")
	if body := get(hd, "/subscribers/new?side=radius&imsi="+imsiDeleted).Body.String(); !strings.Contains(body, "この IMSI は削除済みです") {
		t.Error("deleted is not shown")
	}
	w = post(hd, "/subscribers/new/radius", radius("approve", "1"))
	if w.Code != 400 || !strings.Contains(w.Body.String(), "承認にチェックを入れ、理由") || len(e.pv.writes()) != 0 {
		t.Errorf("no reason = %d", w.Code)
	}
	w = post(hd, "/subscribers/new/radius", radius("approve", "1", "approve_reason", "再契約"))
	if w.Code != 200 {
		t.Fatalf("approved = %d %s", w.Code, w.Body)
	}
	if en, _ := e.db.entry(imsiDeleted); en.Deleted() || en.TenantID != 20 || en.RadiusState != "active" {
		t.Errorf("ledger = %+v", en)
	}
	if a := e.db.lastAudit(); a.Action != auditReregister || a.Detail["reason"] != "再契約" || a.TenantID != 20 {
		t.Errorf("audit = %+v", a)
	}
}

func TestStateOperations(t *testing.T) {
	e := newTestEnv()
	ha := e.handler(t, "a@example.com")
	p := "/subscribers/" + imsiOwn

	// 両方を止める: radius 側を先に、1 つずつ。キーは側ごと。
	w := post(ha, p+"/state", form("side", "both", "state", "inactive", "reason", "未払い", "idem", testKey+"-both-inactive"))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "radius 側（aka-radius）を「停止中」にしました。") {
		t.Fatalf("both = %d %s", w.Code, w.Body)
	}
	want := []string{"radius.state.inactive a@example.com " + testKey + "-both-inactive-radius",
		"vecgen.state.inactive a@example.com " + testKey + "-both-inactive-vecgen"}
	if got := e.pv.writes(); !slices.Equal(got, want) {
		t.Errorf("writes = %v", got)
	}
	// 両方を使う: vecgen 側を先に。
	post(ha, p+"/state", form("side", "both", "state", "active", "idem", testKey))
	if got := e.pv.writes()[2:]; !strings.HasPrefix(got[0], "vecgen.state.active") || !strings.HasPrefix(got[1], "radius.state.active") {
		t.Errorf("writes = %v", got)
	}
	if got := e.db.eventTexts(); len(got) != 4 || got[0] != "line_state_changed radius 10 active→inactive" {
		t.Errorf("events = %v", got)
	}
	if a := e.db.lastAudit(); a.Action != auditStateChange || a.Detail["side"] != "radius" || a.Detail["state"] != "active" {
		t.Errorf("audit = %+v", a)
	}

	// 片側だけ失敗しても、もう一方は取り消さない（F5）。
	e.pv.errs["vecgen.state.inactive"] = pvError(409, provisioner.CauseStateTransitionNotAllowed)
	w = post(ha, p+"/state", form("side", "both", "state", "inactive"))
	if w.Code != 409 || !strings.Contains(w.Body.String(), "radius 側（aka-radius）を「停止中」にしました。") ||
		!strings.Contains(w.Body.String(), "vecgen 側（aka-vecgen）の状態の変更に失敗しました") {
		t.Errorf("partial = %d %s", w.Code, w.Body)
	}
	if en, _ := e.db.entry(imsiOwn); en.RadiusState != "inactive" || en.VecgenState != "active" {
		t.Errorf("ledger = %+v", en)
	}
	delete(e.pv.errs, "vecgen.state.inactive")

	// 解約からは状態を変えられない（下流に送らない）。戻すのはシステム管理者だけで、理由が要る。
	post(ha, p+"/state", form("side", "radius", "state", "terminated"))
	n := len(e.pv.writes())
	if w := post(ha, p+"/state", form("side", "radius", "state", "active")); w.Code != 409 || len(e.pv.writes()) != n {
		t.Errorf("from terminated = %d", w.Code)
	}
	hd := e.handler(t, "d@example.com")
	if w := post(hd, p+"/reinstate", form("side", "radius", "reason", " ")); w.Code != 400 || len(e.pv.writes()) != n {
		t.Errorf("reinstate without reason = %d", w.Code)
	}
	if w := post(hd, p+"/reinstate", form("side", "radius", "reason", "誤り")); w.Code != 200 {
		t.Errorf("reinstate = %d %s", w.Code, w.Body)
	}
	if got := e.db.eventTexts(); got[len(got)-1] != "line_state_changed radius 10 terminated→inactive" {
		t.Errorf("events = %v", got)
	}

	// 物理削除: Active は不可（下流に送らない）。両側を消したら削除済み。
	hc := e.handler(t, "c@example.com")
	n = len(e.pv.writes())
	if w := post(hc, p+"/delete", form("side", "vecgen")); w.Code != 409 || len(e.pv.writes()) != n {
		t.Errorf("delete active = %d", w.Code)
	}
	post(hc, p+"/delete", form("side", "radius"))
	post(hc, p+"/state", form("side", "vecgen", "state", "inactive"))
	w = post(hc, p+"/delete", form("side", "vecgen"))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "台帳は削除済みにしました") {
		t.Errorf("delete = %d %s", w.Code, w.Body)
	}
	if en, _ := e.db.entry(imsiOwn); !en.Deleted() {
		t.Errorf("ledger = %+v", en)
	}
}

// 下流の結果が分からないとき（W7）: 台帳を変えずに印を立て、その場で読み直して直す。
func TestUnknownResult(t *testing.T) {
	unavailable := pvError(503, provisioner.CauseDownstreamUnavailable)

	// 下流は変わっていなかった: 読み直して印を下ろす（補いの事実はない）。
	e := newTestEnv()
	h := e.handler(t, "a@example.com")
	e.pv.errs["radius.state.inactive"] = unavailable
	w := post(h, "/subscribers/"+imsiOwn+"/state", form("side", "radius", "state", "inactive", "idem", testKey+"-x"))
	if w.Code != 409 || !strings.Contains(w.Body.String(), "読み直して台帳を下流に合わせました") {
		t.Errorf("fixed = %d %s", w.Code, w.Body)
	}
	if en, _ := e.db.entry(imsiOwn); en.CheckNeeded || en.RadiusState != "active" || len(e.db.events) != 0 {
		t.Errorf("ledger = %+v %v", en, e.db.eventTexts())
	}
	// 同じキーで送り直せるよう、画面のキーを残す。
	mustContain(t, w.Body.String(), `value="`+testKey+`-state-radius-inactive"`)

	// 下流は変わっていた（応答だけが失われた）: 補いの事実を書く。
	e = newTestEnv()
	h = e.handler(t, "a@example.com")
	e.pv.applyErrs["radius.state"] = pvError(502, provisioner.CauseDownstreamError)
	post(h, "/subscribers/"+imsiOwn+"/state", form("side", "radius", "state", "inactive"))
	if en, _ := e.db.entry(imsiOwn); en.CheckNeeded || en.RadiusState != "inactive" {
		t.Errorf("ledger = %+v", en)
	}
	if got := e.db.eventTexts(); !slices.Equal(got, []string{"reconciled radius 10 active→inactive"}) {
		t.Errorf("events = %v", got)
	}
	if a := e.db.lastAudit(); a.Detail["result"] != "unknown" {
		t.Errorf("audit = %+v", a)
	}

	// 読み直せない: 印を残して「結果を確かめています」。詳細を開いたときに読み直す。
	e = newTestEnv()
	h = e.handler(t, "a@example.com")
	e.pv.errs["radius.state.inactive"] = unavailable
	e.pv.errs["radius.batch"] = unavailable
	w = post(h, "/subscribers/"+imsiOwn+"/state", form("side", "radius", "state", "inactive"))
	mustContain(t, w.Body.String(), "結果を確かめています。しばらくしてから開き直してください。")
	if en, _ := e.db.entry(imsiOwn); !en.CheckNeeded || en.RadiusState != "active" {
		t.Errorf("ledger = %+v", en)
	}
	if body := get(h, "/subscribers?check=1").Body.String(); !strings.Contains(body, imsiOwn) {
		t.Error("check filter does not show the subscriber")
	}
	delete(e.pv.errs, "radius.batch")
	if body := get(h, "/subscribers/"+imsiOwn).Body.String(); !strings.Contains(body, "結果が分からなかった操作を読み直し、台帳を下流に合わせました。") {
		t.Error("recheck on open is not shown")
	}
	if en, _ := e.db.entry(imsiOwn); en.CheckNeeded {
		t.Errorf("still check needed: %+v", en)
	}

	// 台帳にない IMSI の登録の結果が分からず、読み直してもなかった: 行を残さない。
	e = newTestEnv()
	e.pv.errs["vecgen.create"] = unavailable
	w = post(e.handler(t, "d@example.com"), "/subscribers/new/vecgen", form("imsi", imsiNew, "tenant", "10", "state", "active",
		"ki", "465b5ce8b199b49faa5f0a2ee238a6bc", "opc", "cd63cb71954a9f4e48a5994e37a02baf", "amf", "8000", "sqn", "000000000000",
		"sqn_type", "inc32", "idem", testKey))
	if _, ok := e.db.entry(imsiNew); ok || !strings.Contains(w.Body.String(), "への登録の結果が分かりませんでした") {
		t.Errorf("unknown create = %d %s", w.Code, w.Body)
	}
	mustContain(t, w.Body.String(), `name="idem" value="`+testKey+`"`)
}

func TestSubscriberList(t *testing.T) {
	e := newTestEnv()
	// テナントユーザー: 自分のテナントだけ。AMF・SQN の列を出さず、vecgen 側を読まない。
	body := get(e.handler(t, "a@example.com"), "/subscribers").Body.String()
	mustContain(t, body, imsiOwn, imsiVecgenOnly, "2 件", "1 件") // 2 件の加入者、ルール 1 件
	mustNotContain(t, body, imsiOther, imsiDeleted, "<th>AMF</th>", "radius 側の登録")
	// 運用担当: すべて。AMF・SQN を一括取得で補う。
	body = get(e.handler(t, "c@example.com"), "/subscribers").Body.String()
	mustContain(t, body, imsiOwn, imsiOther, "<th>AMF</th>", "000000000020", "radius 側の登録")
	mustNotContain(t, body, "vecgen 側の登録")
	// 絞り込み。
	body = get(e.handler(t, "c@example.com"), "/subscribers?mismatch=1").Body.String()
	mustContain(t, body, imsiVecgenOnly)
	mustNotContain(t, body, imsiOwn)
	body = get(e.handler(t, "c@example.com"), "/subscribers?deleted=1").Body.String()
	mustContain(t, body, imsiDeleted)
	if w := get(e.handler(t, "c@example.com"), "/subscribers?vecgen=bad"); w.Code != 400 {
		t.Errorf("bad filter = %d", w.Code)
	}
	if w := get(e.handler(t, "c@example.com"), "/subscribers?tenant=none"); w.Code != 400 {
		t.Errorf("tenant none = %d", w.Code)
	}
	// 下流にない: 印を出す。下流を読めない: 台帳だけを出す。
	delete(e.pv.radius, imsiOwn)
	mustContain(t, get(e.handler(t, "c@example.com"), "/subscribers").Body.String(), "下流にない")
	e.pv.errs["radius.batch"] = pvError(503, provisioner.CauseDownstreamUnavailable)
	w := get(e.handler(t, "c@example.com"), "/subscribers")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "aka-radius を読めませんでした") {
		t.Errorf("batch failed = %d", w.Code)
	}
}

func TestSubscriberDetail(t *testing.T) {
	e := newTestEnv()
	// テナントユーザー: 両側の状態とルールは出す。AMF・SQN・許可クライアント・操作の一部は出さない。
	body := get(e.handler(t, "a@example.com"), "/subscribers/"+imsiOwn).Body.String()
	mustContain(t, body, "vecgen 側（aka-vecgen）", "radius 側（aka-radius）", "ap-01", ">止める<")
	mustNotContain(t, body, "000000000020", "許可クライアント", "物理削除", "取得先の変更", "所属のテナントの付け替え", "ルールを編集する")
	// システム管理者: すべて。
	body = get(e.handler(t, "d@example.com"), "/subscribers/"+imsiOwn).Body.String()
	mustContain(t, body, "000000000020", "radius-av", "取得先の変更", "所属のテナントの付け替え", "vecgen 側の変更", "ルールを編集する")
	// 片側だけ: もう一方の登録への案内。台帳と下流の違い。
	body = get(e.handler(t, "d@example.com"), "/subscribers/"+imsiVecgenOnly).Body.String()
	mustContain(t, body, `href="/subscribers/new?side=radius&imsi=`+imsiVecgenOnly+`"`)
	e.pv.setState(provisioner.Vecgen, imsiVecgenOnly, "active")
	body = get(e.handler(t, "d@example.com"), "/subscribers/"+imsiVecgenOnly).Body.String()
	mustContain(t, body, "vecgen 側（aka-vecgen）の状態が、台帳（停止中）と下流（利用中）で違います。")
}

func TestVecgenUpdateAndSource(t *testing.T) {
	e := newTestEnv()
	hd := e.handler(t, "d@example.com")
	p := "/subscribers/" + imsiOwn
	// 変わった項目だけを送る。許可クライアントの選択肢を出していないフォームからは、許可クライアントを変えない。
	w := post(hd, p+"/vecgen", form("amf", "9000", "sqn", "000000000020", "sqn_type", "inc32"))
	if w.Code != 200 || e.pv.lastVecgenUpdate.AMF != "9000" || e.pv.lastVecgenUpdate.SQN != "" || e.pv.lastVecgenUpdate.AllowedClientIDs != nil {
		t.Errorf("update = %d %+v", w.Code, e.pv.lastVecgenUpdate)
	}
	w = post(hd, p+"/vecgen", form("amf", "9000", "sqn", "000000000020", "sqn_type", "inc32", "clients_shown", "1"))
	if w.Code != 200 || e.pv.lastVecgenUpdate.AllowedClientIDs == nil || len(*e.pv.lastVecgenUpdate.AllowedClientIDs) != 0 {
		t.Errorf("clear clients = %d %+v", w.Code, e.pv.lastVecgenUpdate)
	}
	n := len(e.pv.writes())
	if w := post(hd, p+"/vecgen", form("amf", "9000", "sqn", "000000000020", "sqn_type", "inc32", "clients_shown", "1")); !strings.Contains(w.Body.String(), "変更はありません。") || len(e.pv.writes()) != n {
		t.Error("no change was sent")
	}
	if w := post(hd, p+"/vecgen", form("amf", "x", "sqn", "000000000020", "sqn_type", "inc32")); w.Code != 400 {
		t.Errorf("bad amf = %d", w.Code)
	}
	// 取得先の変更は台帳にも書く（状態の事実は書かない）。
	w = post(e.handler(t, "c@example.com"), p+"/source", form("source", "vecgen-2"))
	if en, _ := e.db.entry(imsiOwn); w.Code != 200 || en.RadiusSourceID != "vecgen-2" || len(e.db.events) != 0 {
		t.Errorf("source = %d %+v", w.Code, en)
	}
}

// 所属の付け替え（W8）: radius 側のルールのテナントを書き換えてから台帳を書き、両方のテナントの監査ログに残す。
func TestTenantMove(t *testing.T) {
	e := newTestEnv()
	hd := e.handler(t, "d@example.com")
	w := post(hd, "/subscribers/"+imsiOwn+"/tenant", form("tenant", "20", "idem", testKey+"-move"))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "所属を「テナントB」に付け替えました。") {
		t.Fatalf("move = %d %s", w.Code, w.Body)
	}
	if r := e.pv.lastRadiusUpdate.Rules; len(r) != 1 || r[0].TenantID != 20 || r[0].NASID != "ap-01" {
		t.Errorf("rules = %+v", r)
	}
	if en, _ := e.db.entry(imsiOwn); en.TenantID != 20 {
		t.Errorf("ledger = %+v", en)
	}
	if got := e.db.eventTexts(); !slices.Equal(got, []string{"line_tenant_moved  20 10→20"}) {
		t.Errorf("events = %v", got)
	}
	if n := len(e.db.audit); n != 2 || e.db.audit[0].TenantID != 10 || e.db.audit[1].TenantID != 20 {
		t.Errorf("audit = %+v", e.db.audit)
	}
	// vecgen 側だけなら下流は呼ばない。同じテナント・ないテナントは断る。
	n := len(e.pv.writes())
	if w := post(hd, "/subscribers/"+imsiVecgenOnly+"/tenant", form("tenant", "20")); w.Code != 200 || len(e.pv.writes()) != n {
		t.Errorf("vecgen only = %d", w.Code)
	}
	for _, tenant := range []string{"20", "99", ""} {
		if w := post(hd, "/subscribers/"+imsiOwn+"/tenant", form("tenant", tenant)); w.Code != 400 {
			t.Errorf("tenant %q = %d", tenant, w.Code)
		}
	}
}

func TestPolicy(t *testing.T) {
	e := newTestEnv()
	hb := e.handler(t, "b@example.com")
	p := "/subscribers/" + imsiOwn + "/policy"
	body := get(hb, p).Body.String()
	mustContain(t, body, "ルールのテナント: <strong>テナントA</strong>", `value="ap-01"`)
	// 編集の操作は下流に送らない。
	w := post(hb, p+"/edit", form("op", "add", "nas_id", "ap-01", "ssids", "corp", "vlan_id", "10", "session_timeout", ""))
	if w.Code != 200 || strings.Count(w.Body.String(), `name="nas_id"`) != 2 || len(e.pv.writes()) != 0 {
		t.Errorf("add = %d", w.Code)
	}
	// 保存: ルールのテナントは所属のテナント。
	w = post(hb, p, form("nas_id", "AP Lobby", "ssids", "*", "vlan_id", "", "session_timeout", "60",
		"nas_id", "ap-01", "ssids", "Corp\nguest", "vlan_id", "20", "session_timeout", "", "idem", testKey))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "保存しました。") {
		t.Fatalf("save = %d %s", w.Code, w.Body)
	}
	want := []provisioner.Rule{{TenantID: 10, NASID: "AP Lobby", SSIDs: []string{"*"}, SessionTimeout: 60},
		{TenantID: 10, NASID: "ap-01", SSIDs: []string{"Corp", "guest"}, VLANID: 20}}
	if got := e.pv.lastRadiusUpdate.Rules; len(got) != 2 || !slices.EqualFunc(got, want, func(a, b provisioner.Rule) bool {
		return a.TenantID == b.TenantID && a.NASID == b.NASID && slices.Equal(a.SSIDs, b.SSIDs) && a.VLANID == b.VLANID && a.SessionTimeout == b.SessionTimeout
	}) {
		t.Errorf("rules = %+v", got)
	}
	if a := e.db.lastAudit(); a.Action != auditPolicyUpdate || a.TenantID != 10 {
		t.Errorf("audit = %+v", a)
	}
	// 入力の誤り（ルールなし、SSID の数）。
	n := len(e.pv.writes())
	if w := post(hb, p, form()); w.Code != 400 || !strings.Contains(w.Body.String(), "ルールは 1〜32 件にしてください") {
		t.Errorf("no rules = %d", w.Code)
	}
	if w := post(hb, p, form("nas_id", "", "ssids", "", "vlan_id", "", "session_timeout", "")); w.Code != 400 || len(e.pv.writes()) != n {
		t.Errorf("empty rule = %d", w.Code)
	}
	// radius 側にない加入者は 404。
	if w := get(e.handler(t, "c@example.com"), "/subscribers/"+imsiVecgenOnly+"/policy"); w.Code != 404 {
		t.Errorf("vecgen only = %d", w.Code)
	}
}

func TestSessions(t *testing.T) {
	e := newTestEnv()
	e.pv.sessions = []provisioner.Session{
		{ID: "s1", IMSI: imsiOwn, TenantID: 10, NASIdentifier: "ap-01"},
		{ID: "s2", IMSI: imsiOwn, TenantID: 20, NASIdentifier: "ap-other"},
		{ID: "s3", IMSI: imsiOther, TenantID: 20, NASIdentifier: "ap-b"},
	}
	// テナントユーザー: 自分のテナントのもの（刻印のテナントで絞る）。
	ha := e.handler(t, "a@example.com")
	body := get(ha, "/sessions?tenant=20").Body.String()
	if e.pv.lastSessions.TenantID != 10 || !strings.Contains(body, "ap-01") || strings.Contains(body, "ap-b") {
		t.Errorf("tenant user = %+v %s", e.pv.lastSessions, body)
	}
	// IMSI で開いても、他のテナントの刻印のものは出さない。範囲の外の IMSI は 404。
	body = get(ha, "/sessions?imsi="+imsiOwn).Body.String()
	mustContain(t, body, "ap-01")
	mustNotContain(t, body, "ap-other")
	if w := get(ha, "/sessions?imsi="+imsiOther); w.Code != 404 {
		t.Errorf("other imsi = %d", w.Code)
	}
	// 運用担当: 条件がなければ読まない。テナントか IMSI（複数）のどちらか。
	hc := e.handler(t, "c@example.com")
	e.pv.lastSessions = provisioner.SessionParams{}
	if body := get(hc, "/sessions").Body.String(); !strings.Contains(body, "テナントか IMSI を選んでください") || e.pv.lastSessions.Limit != 0 {
		t.Error("staff without condition")
	}
	if w := get(hc, "/sessions?tenant=10&imsi="+imsiOwn); w.Code != 400 {
		t.Errorf("both = %d", w.Code)
	}
	body = get(hc, "/sessions?imsi="+imsiOwn+","+imsiOther).Body.String()
	if !slices.Equal(e.pv.lastSessions.IMSIs, []string{imsiOwn, imsiOther}) || !strings.Contains(body, "ap-other") {
		t.Errorf("imsis = %+v", e.pv.lastSessions)
	}
	if w := get(hc, "/sessions?imsi=123"); w.Code != 400 {
		t.Errorf("bad imsi = %d", w.Code)
	}
}

// 監査ログの書き込みに失敗しても、操作は済んでいるので成功として返す。
func TestAuditFailureDoesNotFailOperation(t *testing.T) {
	e := newTestEnv()
	e.db.auditErr = os.ErrClosed
	if w := post(e.handler(t, "a@example.com"), "/subscribers/"+imsiOwn+"/state", form("side", "radius", "state", "inactive")); w.Code != 200 {
		t.Errorf("state = %d", w.Code)
	}
}
