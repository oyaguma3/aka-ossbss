package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/oyaguma3/aka-ossbss/internal/auth"
	"github.com/oyaguma3/aka-ossbss/internal/authz"
	"github.com/oyaguma3/aka-ossbss/internal/db"
)

// TestAdminRoutesByRole は、テナント・アカウント・監査ログの画面を役割で断ることを確かめる（設計概要 §5）。
func TestAdminRoutesByRole(t *testing.T) {
	routes := []struct {
		method, path string
		form         url.Values
		allowed      []string // 許可する役割（メールアドレス）
	}{
		{"GET", "/tenants", nil, []string{"c", "d"}},
		{"POST", "/tenants", url.Values{"name": {"新テナント"}}, []string{"d"}},
		{"GET", "/tenants/10", nil, []string{"c", "d"}},
		{"POST", "/tenants/10/rename", url.Values{"name": {"改名"}}, []string{"d"}},
		{"POST", "/tenants/20/delete", url.Values{}, []string{"d"}},
		{"GET", "/audit", nil, []string{"b", "c", "d"}},
		{"GET", "/audit/provisioner", nil, []string{"c", "d"}},
		{"GET", "/audit/vecgen", nil, []string{"c", "d"}},
		{"GET", "/audit/radius", nil, []string{"c", "d"}},
		{"GET", "/accounts", nil, []string{"b", "d"}},
		{"POST", "/accounts", url.Values{"email": {"new@example.com"}}, []string{"b", "d"}},
	}
	for _, rt := range routes {
		for _, role := range []string{"a", "b", "c", "d"} {
			e := newTestEnv()
			h := e.handler(t, role+"@example.com")
			var code int
			if rt.method == "GET" {
				code = get(h, rt.path).Code
			} else {
				code = post(h, rt.path, rt.form).Code
			}
			allowed := strings.Contains(strings.Join(rt.allowed, ","), role)
			switch {
			case allowed && (code == http.StatusForbidden || code == http.StatusNotFound || code >= 500):
				t.Errorf("%s %s by %s = %d, want success", rt.method, rt.path, role, code)
			case !allowed && code != http.StatusForbidden:
				t.Errorf("%s %s by %s = %d, want 403", rt.method, rt.path, role, code)
			}
			if !allowed && (len(e.pv.writes()) != 0 || len(e.db.audit) != 0) {
				t.Errorf("%s %s by %s: side effects %v %v", rt.method, rt.path, role, e.pv.writes(), e.db.audit)
			}
		}
	}
	// 知らない監査ログの元は 404。
	if w := get(newTestEnv().handler(t, "d@example.com"), "/audit/prov"); w.Code != 404 {
		t.Errorf("unknown audit source = %d", w.Code)
	}
}

// ナビゲーションは役割で出し分ける。
func TestNavByRole(t *testing.T) {
	e := newTestEnv()
	for _, c := range []struct {
		email string
		want  []string
		not   []string
	}{
		{"a@example.com", []string{`href="/subscribers"`, `href="/sessions"`}, []string{`href="/tenants"`, `href="/accounts"`, `href="/audit"`, `href="/clients"`}},
		{"b@example.com", []string{`href="/accounts"`, `href="/audit"`}, []string{`href="/tenants"`, `href="/operations"`}},
		{"c@example.com", []string{`href="/audit"`, `href="/tenants"`, `href="/charging"`}, []string{`href="/accounts"`, `href="/operations"`}},
		{"d@example.com", []string{`href="/tenants"`, `href="/accounts"`, `href="/audit"`}, []string{`href="/operations"`}},
	} {
		body := get(e.handler(t, c.email), "/sessions").Body.String()
		nav := body[strings.Index(body, `aria-label="メニュー"`):strings.Index(body, "</nav>")]
		mustContain(t, nav, c.want...)
		mustNotContain(t, nav, c.not...)
		mustContain(t, nav, `href="/sessions" class="menu-active"`)
	}
}

// アカウントの管理の権限（設計概要 §15 の 11、§17.5 の 4・7）。
func TestAccountsList(t *testing.T) {
	e := newTestEnv()
	e.db.accounts["a2@example.com"] = db.Account{ID: 6, Email: "a2@example.com", Role: authz.TenantUser, TenantID: 20}
	// テナント管理者には自分のテナントのアカウントをすべて出し、リンクはテナントユーザーにだけ付ける。
	body := get(e.handler(t, "b@example.com"), "/accounts").Body.String()
	mustContain(t, body, `<a class="link" href="/accounts/1">a@example.com</a>`, "b@example.com", "自分")
	mustNotContain(t, body, `href="/accounts/2"`, "a2@example.com", "c@example.com", `name="role"`)
	// システム管理者はすべて。テナントで絞れる。
	h := e.handler(t, "d@example.com")
	body = get(h, "/accounts").Body.String()
	mustContain(t, body, "a2@example.com", "c@example.com", `href="/accounts/2"`, `name="role"`)
	body = get(h, "/accounts?tenant=none").Body.String()
	mustContain(t, body, "c@example.com")
	mustNotContain(t, body, "a@example.com")
}

func TestAccountCreate(t *testing.T) {
	e := newTestEnv()
	// テナント管理者は自分のテナントのテナントユーザーを作る（役割・テナントの指定は無視する）。
	w := post(e.handler(t, "b@example.com"), "/accounts", url.Values{"email": {" New.User@Example.com "}, "role": {"system_admin"}, "tenant": {"20"}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "アカウント new.user@example.com を作成しました。Cloudflare Access のポリシーにも") {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	if a := e.db.accounts["new.user@example.com"]; a.Role != authz.TenantUser || a.TenantID != 10 || a.CreatedBy != "b@example.com" {
		t.Errorf("account = %+v", a)
	}
	if a := e.db.lastAudit(); a.Action != "account.create" || a.Target != "new.user@example.com" || a.TenantID != 10 {
		t.Errorf("audit = %+v", a)
	}
	// 形式と重複。
	w = post(e.handler(t, "b@example.com"), "/accounts", url.Values{"email": {"x+tag@example.com"}})
	if w.Code != 400 || !strings.Contains(w.Body.String(), "+ などは使えません") {
		t.Errorf("format = %d", w.Code)
	}
	if w := post(e.handler(t, "d@example.com"), "/accounts", url.Values{"email": {"a@example.com"}, "role": {"operator"}}); w.Code != 409 {
		t.Errorf("duplicate = %d", w.Code)
	}
	// システム管理者: 役割とテナントの組み合わせ。
	hd := e.handler(t, "d@example.com")
	if w := post(hd, "/accounts", url.Values{"email": {"tu@example.com"}, "role": {"tenant_user"}, "tenant": {"none"}}); w.Code != 400 ||
		!strings.Contains(w.Body.String(), "テナントを選んでください") {
		t.Errorf("tenant user without tenant = %d", w.Code)
	}
	if w := post(hd, "/accounts", url.Values{"email": {"op@example.com"}, "role": {"operator"}, "tenant": {"10"}}); w.Code != 200 {
		t.Errorf("operator = %d", w.Code)
	}
	if a := e.db.accounts["op@example.com"]; a.Role != authz.Operator || a.TenantID != 0 {
		t.Errorf("operator account = %+v", a)
	}
	if w := post(hd, "/accounts", url.Values{"email": {"ta@example.com"}, "role": {"tenant_admin"}, "tenant": {"20"}}); w.Code != 200 {
		t.Errorf("tenant admin = %d", w.Code)
	}
	if w := post(hd, "/accounts", url.Values{"email": {"r@example.com"}, "role": {"root"}}); w.Code != 400 {
		t.Errorf("unknown role = %d", w.Code)
	}
}

// accountPost はアカウントの操作のフォーム（画面を開いた時点の役割・テナントつき）。
func accountPost(a db.Account, kv ...string) url.Values {
	v := url.Values{"from_role": {string(a.Role)}, "from_tenant": {strconv.FormatInt(a.TenantID, 10)}}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v
}

func TestAccountManage(t *testing.T) {
	e := newTestEnv()
	e.db.accounts["a2@example.com"] = db.Account{ID: 6, Email: "a2@example.com", Role: authz.TenantUser, TenantID: 20}
	e.db.accounts["d2@example.com"] = db.Account{ID: 7, Email: "d2@example.com", Role: authz.SystemAdmin}
	hb := e.handler(t, "b@example.com")
	// テナント管理者: 他のテナントは 404、自分のテナントのテナント管理者は 403。
	if w := get(hb, "/accounts/6"); w.Code != 404 {
		t.Errorf("other tenant = %d", w.Code)
	}
	if w := get(hb, "/accounts/2"); w.Code != 403 {
		t.Errorf("tenant admin = %d", w.Code)
	}
	if w := post(hb, "/accounts/6/disable", accountPost(e.db.accounts["a2@example.com"])); w.Code != 404 {
		t.Errorf("disable other tenant = %d", w.Code)
	}
	// 自分のテナントのテナントユーザーは無効化・有効化・削除できるが、役割の変更はできない。
	a := e.db.accounts["a@example.com"]
	body := get(hb, "/accounts/1").Body.String()
	mustContain(t, body, "無効にする", "削除")
	mustNotContain(t, body, "役割とテナントの変更")
	if w := post(hb, "/accounts/1/update", accountPost(a, "role", "tenant_admin", "tenant", "10")); w.Code != 403 {
		t.Errorf("promote by tenant admin = %d", w.Code)
	}
	w := post(hb, "/accounts/1/disable", accountPost(a))
	if w.Code != 200 || !e.db.accounts["a@example.com"].Disabled || !strings.Contains(w.Body.String(), "無効にしました") {
		t.Fatalf("disable = %d %s", w.Code, w.Body)
	}
	if l := e.db.lastAudit(); l.Action != "account.disable" || l.TenantID != 10 {
		t.Errorf("audit = %+v", l)
	}
	if w := post(hb, "/accounts/1/enable", accountPost(a)); w.Code != 200 || e.db.accounts["a@example.com"].Disabled {
		t.Errorf("enable = %d", w.Code)
	}
	// 画面を開いた後に変わっていたら断る。
	if w := post(hb, "/accounts/1/delete", accountPost(db.Account{Role: authz.TenantUser, TenantID: 20})); w.Code != 409 ||
		!strings.Contains(w.Body.String(), "他の操作で変更されました") {
		t.Errorf("stale = %d", w.Code)
	}
	if w := post(hb, "/accounts/1/delete", accountPost(a)); w.Code != 200 || w.Header().Get("HX-Redirect") != "/accounts?deleted=1" {
		t.Errorf("delete = %d", w.Code)
	}
	if _, ok := e.db.accounts["a@example.com"]; ok {
		t.Error("not deleted")
	}

	// システム管理者: 役割とテナントの変更（変更前と変更後のテナントに 1 行ずつ監査ログ）。
	hd := newHandlerPVWithAdmin(t, e, "d@example.com", "d@example.com")
	a2 := e.db.accounts["a2@example.com"]
	n := len(e.db.audit)
	w = post(hd, "/accounts/6/update", accountPost(a2, "role", "tenant_admin", "tenant", "10"))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "役割とテナントを変更しました") {
		t.Fatalf("update = %d %s", w.Code, w.Body)
	}
	if got := e.db.accounts["a2@example.com"]; got.Role != authz.TenantAdmin || got.TenantID != 10 {
		t.Errorf("updated = %+v", got)
	}
	if rows := e.db.audit[n:]; len(rows) != 2 || rows[0].TenantID != 20 || rows[1].TenantID != 10 || rows[0].Action != "account.update" {
		t.Errorf("audit rows = %+v", rows)
	}
	// 自分自身と最初のシステム管理者は変更・削除できない。
	d := e.db.accounts["d@example.com"]
	for _, path := range []string{"/accounts/4/disable", "/accounts/4/delete", "/accounts/4/update"} {
		w := post(hd, path, accountPost(d, "role", "operator"))
		if w.Code != 403 || !strings.Contains(w.Body.String(), "最初のシステム管理者は変更・削除できません") {
			t.Errorf("%s (initial admin, self) = %d", path, w.Code)
		}
	}
	mustContain(t, get(hd, "/accounts/4").Body.String(), "最初のシステム管理者は変更・削除できません")
	hd2 := newHandlerPVWithAdmin(t, e, "d2@example.com", "d@example.com")
	if w := post(hd2, "/accounts/7/disable", accountPost(e.db.accounts["d2@example.com"])); w.Code != 403 || !strings.Contains(w.Body.String(), "自分のアカウントは変更・削除できません") {
		t.Errorf("self = %d", w.Code)
	}
	// 別のシステム管理者は降格できる。
	if w := post(hd2, "/accounts/4/update", accountPost(d, "role", "operator")); w.Code != 403 {
		t.Errorf("demote initial admin = %d", w.Code)
	}
	if w := post(hd, "/accounts/7/update", accountPost(e.db.accounts["d2@example.com"], "role", "operator", "tenant", "none")); w.Code != 200 {
		t.Errorf("demote other admin = %d", w.Code)
	}
}

// newHandlerPVWithAdmin は、最初のシステム管理者を initialAdmin にした画面を作る。
func newHandlerPVWithAdmin(t *testing.T, e *testEnv, email, initialAdmin string) http.Handler {
	t.Helper()
	h, err := New(Options{Log: discard, Version: "test", DB: e.db, Identifier: auth.Dev(email), Provisioner: e.pv, InitialAdminEmail: initialAdmin})
	if err != nil {
		t.Fatal(err)
	}
	return h.Routes()
}

func TestTenants(t *testing.T) {
	e := newTestEnv()
	h := e.handler(t, "d@example.com")
	body := get(h, "/tenants").Body.String()
	// テナントA: 加入者 2（own・both。削除済みは数えない）、アカウント 2（a・b）。
	mustContain(t, body, "テナントA", `href="/subscribers?tenant=10">2<`, `href="/accounts?tenant=10">2<`)
	mustNotContain(t, body, "/clients")
	// 作成（名前の誤り、重複）。
	if w := post(h, "/tenants", url.Values{"name": {"  "}}); w.Code != 400 {
		t.Errorf("empty = %d", w.Code)
	}
	if w := post(h, "/tenants", url.Values{"name": {"テナントA"}}); w.Code != 409 || !strings.Contains(w.Body.String(), "既に使われています") {
		t.Errorf("duplicate = %d", w.Code)
	}
	w := post(h, "/tenants", url.Values{"name": {" テナントC "}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "テナント「テナントC」を作成しました。") {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	created := e.db.tenants[len(e.db.tenants)-1]
	if a := e.db.lastAudit(); a.Action != "tenant.create" || a.TenantID != created.ID {
		t.Errorf("audit = %+v", a)
	}
	id := strconv.FormatInt(created.ID, 10)
	if w := post(h, "/tenants/"+id+"/rename", url.Values{"name": {"テナント丙"}}); w.Code != 200 || e.db.tenantName(created.ID) != "テナント丙" {
		t.Errorf("rename = %d", w.Code)
	}
	// 属するものが残っていれば削除を断り、件数を出す。
	w = post(h, "/tenants/10/delete", url.Values{})
	if w.Code != 409 || !strings.Contains(w.Body.String(), "加入者 2 件、アカウント 2 件が属しています") {
		t.Errorf("in use = %d %s", w.Code, w.Body)
	}
	if w := post(h, "/tenants/"+id+"/delete", url.Values{}); w.Code != 200 || w.Header().Get("HX-Redirect") != "/tenants?deleted=1" {
		t.Errorf("delete = %d", w.Code)
	}
	if a := e.db.lastAudit(); a.Action != "tenant.delete" || a.Target != "テナント丙" {
		t.Errorf("audit = %+v", a)
	}
	if w := get(h, "/tenants/999"); w.Code != 404 {
		t.Errorf("missing = %d", w.Code)
	}
}
