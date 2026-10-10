package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/oyaguma3/eapaka-ossbss-sample/internal/auth"
	"github.com/oyaguma3/eapaka-ossbss-sample/internal/authz"
)

// テスト用の加入者と RADIUSクライアント。own はテナントA（a・b のテナント）、other はテナントB、none はテナントなし。
const (
	imsiOwn   = "001010000000001"
	imsiOther = "001010000000002"
	imsiNone  = "001010000000003"
	imsiAka   = "001020000000004"
)

var testRoles = []string{"a@example.com", "b@example.com", "c@example.com", "d@example.com"}

// testEnv は加入者・RADIUSクライアントと所属の記録を用意した DB と provisioner。
type testEnv struct {
	db *fakeDB
	pv *fakePV
	// clientOwn・clientOther・clientNone は RADIUSクライアントの ID。
	clientOwn, clientOther, clientNone int64
}

func newTestEnv() *testEnv {
	e := &testEnv{db: newFakeDB(), pv: newFakePV()}
	for _, imsi := range []string{imsiOwn, imsiOther, imsiNone, imsiAka} {
		e.pv.addSubscriber(imsi)
	}
	e.db.subTenants[imsiOwn] = 10
	e.db.subTenants[imsiAka] = 10
	e.db.subTenants[imsiOther] = 20
	e.clientOwn = e.pv.addClient("192.0.2.1", "ap-own")
	e.clientOther = e.pv.addClient("192.0.2.2", "ap-other")
	e.clientNone = e.pv.addClient("192.0.2.3", "ap-none")
	e.db.clientTenants[e.clientOwn] = 10
	e.db.clientTenants[e.clientOther] = 20
	return e
}

// handler は email の利用者（開発用の設定）の画面を作る。
func (e *testEnv) handler(t *testing.T, email string) http.Handler {
	return newHandlerPV(t, e.db, auth.Dev(email), discard, e.pv)
}

// TestScopeMatrix は、対象を持つすべての操作を、役割 × 対象のテナント（自分のテナント・他のテナント・テナントなし）で確かめる
// （設計概要 §6.2、§14。画面仕様 §1.2）。範囲の外は 404、範囲の中でも役割にない操作は 403。どちらのときも provisioner に
// 書き込み・秘密の値の取得を送らない。
func TestScopeMatrix(t *testing.T) {
	type target struct {
		name   string
		tenant int64
		imsi   string
		client func(e *testEnv) int64
	}
	targets := []target{
		{"own", 10, imsiOwn, func(e *testEnv) int64 { return e.clientOwn }},
		{"other", 20, imsiOther, func(e *testEnv) int64 { return e.clientOther }},
		{"none", authz.NoTenant, imsiNone, func(e *testEnv) int64 { return e.clientNone }},
	}
	routes := []struct {
		method, path string
		form         url.Values
		op           authz.Op
		// client は RADIUSクライアントの操作か（{id} を置き換える）。そうでなければ {imsi} を置き換える。
		client bool
		view   authz.Op
	}{
		{"GET", "/subscribers/{imsi}", nil, authz.SubscriberView, false, authz.SubscriberView},
		{"POST", "/subscribers/{imsi}/keys", url.Values{}, authz.SubscriberKeysView, false, authz.SubscriberView},
		{"POST", "/subscribers/{imsi}/auth", url.Values{"ki": {strings.Repeat("a", 32)}}, authz.SubscriberKeysUpdate, false, authz.SubscriberView},
		{"POST", "/subscribers/{imsi}/status", url.Values{"status": {"suspended"}}, authz.SubscriberSuspend, false, authz.SubscriberView},
		{"GET", "/subscribers/{imsi}/policy", nil, authz.SubscriberPolicyUpdate, false, authz.SubscriberView},
		{"POST", "/subscribers/{imsi}/policy/edit", url.Values{"default": {"deny"}, "op": {"add"}}, authz.SubscriberPolicyUpdate, false, authz.SubscriberView},
		{"POST", "/subscribers/{imsi}/policy", url.Values{"default": {"deny"}}, authz.SubscriberPolicyUpdate, false, authz.SubscriberView},
		{"POST", "/subscribers/{imsi}/policy/status", url.Values{"default": {"deny"}, "status": {"suspended"}}, authz.SubscriberSuspend, false, authz.SubscriberView},
		{"POST", "/subscribers/{imsi}/delete", url.Values{}, authz.SubscriberDelete, false, authz.SubscriberView},
		{"GET", "/sessions?imsi={imsi}", nil, authz.SessionView, false, authz.SubscriberView},
		{"GET", "/clients/{id}", nil, authz.ClientView, true, authz.ClientView},
		{"POST", "/clients/{id}/update", url.Values{"ip": {"198.51.100.9"}, "name": {"renamed"}}, authz.ClientUpdate, true, authz.ClientView},
		{"POST", "/clients/{id}/secret", url.Values{}, authz.ClientSecretView, true, authz.ClientView},
		{"POST", "/clients/{id}/delete", url.Values{}, authz.ClientUpdate, true, authz.ClientView},
	}
	for _, rt := range routes {
		for _, email := range testRoles {
			for _, tg := range targets {
				e := newTestEnv()
				path := strings.ReplaceAll(rt.path, "{imsi}", tg.imsi)
				path = strings.ReplaceAll(path, "{id}", strconv.FormatInt(tg.client(e), 10))
				h := e.handler(t, email)
				var code int
				if rt.method == "GET" {
					code = get(h, path).Code
				} else {
					code = post(h, path, rt.form).Code
				}
				actor := e.db.accounts[email].Actor()
				name := rt.method + " " + rt.path + " " + email + " " + tg.name
				switch {
				case !authz.Allowed(actor, rt.view, tg.tenant):
					if code != http.StatusNotFound {
						t.Errorf("%s = %d, want 404 (out of scope)", name, code)
					}
				case !authz.Allowed(actor, rt.op, tg.tenant):
					if code != http.StatusForbidden {
						t.Errorf("%s = %d, want 403", name, code)
					}
				default:
					if code == http.StatusNotFound || code == http.StatusForbidden || code >= 500 {
						t.Errorf("%s = %d, want success", name, code)
					}
					continue
				}
				if len(e.pv.ops) != 0 {
					t.Errorf("%s: provisioner was called: %v", name, e.pv.ops)
				}
				if len(e.db.audit) != 0 {
					t.Errorf("%s: audit log was written: %v", name, e.db.audit)
				}
			}
		}
	}
}

// 対象を持たない操作（登録の画面）も役割で断る。
func TestCreateRoutesByRole(t *testing.T) {
	routes := []struct {
		method, path string
		form         url.Values
		op           authz.Op
	}{
		{"GET", "/subscribers/new", nil, authz.SubscriberCreate},
		{"POST", "/subscribers/new/form", url.Values{"default": {"allow"}}, authz.SubscriberCreate},
		{"POST", "/subscribers", url.Values{"tenant": {"10"}, "imsi": {"001010000000099"}, "ki": {strings.Repeat("a", 32)},
			"opc": {strings.Repeat("b", 32)}, "amf": {"8000"}, "sqn": {"000000000000"}}, authz.SubscriberCreate},
		{"POST", "/clients", url.Values{"tenant": {"10"}, "ip": {"198.51.100.10"}, "secret": {"s3cret"}, "name": {"new-ap"}}, authz.ClientCreate},
	}
	for _, rt := range routes {
		for _, email := range testRoles {
			e := newTestEnv()
			h := e.handler(t, email)
			var code int
			if rt.method == "GET" {
				code = get(h, rt.path).Code
			} else {
				code = post(h, rt.path, rt.form).Code
			}
			a := e.db.accounts[email].Actor()
			allowed := authz.Allowed(a, rt.op, a.TenantID)
			if allowed && (code == http.StatusForbidden || code >= 400) {
				t.Errorf("%s %s by %s = %d, want success", rt.method, rt.path, email, code)
			}
			if !allowed && code != http.StatusForbidden {
				t.Errorf("%s %s by %s = %d, want 403", rt.method, rt.path, email, code)
			}
			if !allowed && len(e.pv.ops) != 0 {
				t.Errorf("%s %s by %s: provisioner was called: %v", rt.method, rt.path, email, e.pv.ops)
			}
		}
	}
}

// 範囲の外の 404 は、存在しない IMSI のときと同じ文にする（存在の有無を漏らさない）。
func TestOutOfScopeLooksLikeMissing(t *testing.T) {
	e := newTestEnv()
	h := e.handler(t, "a@example.com")
	other := get(h, "/subscribers/"+imsiOther)
	missing := get(h, "/subscribers/001019999999999")
	if other.Code != 404 || missing.Code != 404 {
		t.Fatalf("codes = %d, %d", other.Code, missing.Code)
	}
	mustContain(t, other.Body.String(), "このテナントに属していないか、登録されていません")
	if strings.ReplaceAll(other.Body.String(), imsiOther, "X") != strings.ReplaceAll(missing.Body.String(), "001019999999999", "X") {
		t.Error("out-of-scope and missing pages differ")
	}
	// 存在しない IMSI では provisioner に問い合わせない（所属の記録だけで決まる）。
	if e.pv.gets != 0 {
		t.Errorf("provisioner was asked %d times", e.pv.gets)
	}
}
