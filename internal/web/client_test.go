package web

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestClientsList(t *testing.T) {
	e := newTestEnv()
	// 所属の記録だけが残っているもの（下流にない）。
	e.db.clientTenants[99] = 10
	// テナントユーザー: 自分のテナントのものだけ。登録のフォームとテナントの列は出さない。
	body := get(e.handler(t, "a@example.com"), "/clients").Body.String()
	mustContain(t, body, "192.0.2.1", "ap-own", "下流にない")
	mustNotContain(t, body, "192.0.2.2", "192.0.2.3", `hx-post="/clients"`, "<th>テナント</th>")
	// テナント管理者: 登録のフォーム（テナントは選ばない）。
	body = get(e.handler(t, "b@example.com"), "/clients").Body.String()
	mustContain(t, body, `hx-post="/clients"`, "あなたのテナントに属します")
	mustNotContain(t, body, "選んでください")
	// 運用担当: すべて（テナントを添える）、絞り込み、テナントの選択。
	h := e.handler(t, "c@example.com")
	body = get(h, "/clients").Body.String()
	mustContain(t, body, "192.0.2.1", "192.0.2.2", "192.0.2.3", "テナントA", "テナントB", "テナントなし", "選んでください")
	body = get(h, "/clients?tenant=none").Body.String()
	mustContain(t, body, "192.0.2.3")
	mustNotContain(t, body, "192.0.2.1", "192.0.2.2", "下流にない")
	body = get(h, "/clients?tenant=10").Body.String()
	mustContain(t, body, "192.0.2.1", "下流にない")
	mustNotContain(t, body, "192.0.2.2", "192.0.2.3")
	if w := get(h, "/clients?tenant=abc"); w.Code != 400 {
		t.Errorf("bad tenant = %d", w.Code)
	}
}

func TestClientCreate(t *testing.T) {
	e := newTestEnv()
	// テナント管理者が登録したものは、自分のテナントに属する（テナントの指定は無視する）。
	form := url.Values{"ip": {"198.51.100.1"}, "secret": {"s3cret!"}, "name": {"ap-new"}, "vendor": {"acme"},
		"tenant": {"20"}, "idem": {"FFFFFFFFFFFFFFFFFFFFFFFFFF"}}
	w := post(e.handler(t, "b@example.com"), "/clients", form)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "RADIUSクライアント ap-new（#4、198.51.100.1）を登録しました。") {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	if e.db.clientTenants[4] != 10 || e.pv.lastOp() != "CreateRADIUSClient b@example.com FFFFFFFFFFFFFFFFFFFFFFFFFF" {
		t.Errorf("assignment = %v, op = %s", e.db.clientTenants, e.pv.lastOp())
	}
	if a := e.db.lastAudit(); a.Action != "client.create" || a.Target != "#4" || a.TenantID != 10 || a.Detail["secret"] != nil {
		t.Errorf("audit = %+v", a)
	}
	mustContain(t, w.Body.String(), "198.51.100.1")
	mustNotContain(t, w.Body.String(), "s3cret!")

	// 運用担当はテナントを選ぶ。
	h := e.handler(t, "c@example.com")
	form = url.Values{"ip": {"198.51.100.2"}, "secret": {"x"}, "name": {"ap-c"}}
	w = post(h, "/clients", form)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "所属のテナントを選んでください。") || len(e.pv.ops) != 1 {
		t.Errorf("no tenant = %d", w.Code)
	}
	form.Set("tenant", "none")
	if w := post(h, "/clients", form); w.Code != 200 {
		t.Fatalf("create none = %d", w.Code)
	}
	if _, ok := e.db.clientTenants[5]; ok {
		t.Error("none was recorded")
	}
	// 同じ IP アドレス（他のテナントのものでも同じ文）。
	form.Set("tenant", "10")
	w = post(h, "/clients", form)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "その IP アドレスの RADIUSクライアントは既に登録されています") {
		t.Errorf("duplicate = %d", w.Code)
	}
	// 入力の誤り。
	w = post(h, "/clients", url.Values{"ip": {"010.0.0.1"}, "secret": {"a b"}, "name": {"bad name"}, "tenant": {"10"}})
	mustContain(t, w.Body.String(), "IPv4 のドット区切り", "共有シークレットは", "名前は英数字")
	// 所属の記録に失敗した（テナント管理者の一覧には出ないことを添える）。
	e.db.assignErr = errors.New("db down")
	w = post(e.handler(t, "b@example.com"), "/clients", url.Values{"ip": {"198.51.100.3"}, "secret": {"x"}, "name": {"ap-b2"}})
	mustContain(t, w.Body.String(), "所属の記録に失敗しました", "一覧に出ません")
}

func TestClientDetail(t *testing.T) {
	e := newTestEnv()
	id := strconv.FormatInt(e.clientOwn, 10)
	body := get(e.handler(t, "a@example.com"), "/clients/"+id).Body.String()
	mustContain(t, body, "192.0.2.1", "ap-own")
	mustNotContain(t, body, "共有シークレットを表示", "設定の変更", "このクライアントを削除", "<th>テナント</th>")
	body = get(e.handler(t, "b@example.com"), "/clients/"+id).Body.String()
	mustContain(t, body, "共有シークレットを表示", "設定の変更", `commandfor="dialog-delete"`)
	mustContain(t, get(e.handler(t, "d@example.com"), "/clients/"+id).Body.String(), "<tr><th>テナント</th><td>テナントA</td></tr>")
}

func TestClientUpdateSecretDelete(t *testing.T) {
	e := newTestEnv()
	h := e.handler(t, "b@example.com")
	id := strconv.FormatInt(e.clientOwn, 10)
	// 変わった項目だけを送る。
	w := post(h, "/clients/"+id+"/update", url.Values{"ip": {"192.0.2.10"}, "name": {"ap-own"}, "vendor": {"acme"}, "secret": {"newsecret"},
		"idem": {"GGGGGGGGGGGGGGGGGGGGGGGGGG"}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "IP アドレス、共有シークレット を変更しました。") {
		t.Fatalf("update = %d %s", w.Code, w.Body)
	}
	if u := e.pv.lastClientUpdate; u.IP == nil || *u.IP != "192.0.2.10" || u.Name != nil || u.Secret == nil {
		t.Errorf("update = %+v", u)
	}
	if a := e.db.lastAudit(); a.Action != "client.update" || a.Detail["ip"] != "192.0.2.1 -> 192.0.2.10" || a.Detail["secret"] != nil {
		t.Errorf("audit = %+v", a)
	}
	mustNotContain(t, w.Body.String(), "newsecret")
	if w := post(h, "/clients/"+id+"/update", url.Values{"ip": {"192.0.2.10"}, "name": {"ap-own"}, "vendor": {"acme"}}); !strings.Contains(w.Body.String(), "変更はありません。") {
		t.Error("no change is not reported")
	}
	// 共有シークレットの表示。
	w = post(h, "/clients/"+id+"/secret", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "newsecret") {
		t.Errorf("secret = %d %s", w.Code, w.Body)
	}
	if a := e.db.lastAudit(); a.Action != "client.secret.read" || a.Target != "#"+id {
		t.Errorf("audit = %+v", a)
	}
	// 削除すると所属の記録も消す。
	w = post(h, "/clients/"+id+"/delete", url.Values{"idem": {"HHHHHHHHHHHHHHHHHHHHHHHHHH"}})
	if w.Code != 200 || w.Header().Get("HX-Redirect") != "/clients?deleted="+id {
		t.Fatalf("delete = %d %v", w.Code, w.Header())
	}
	if _, ok := e.db.clientTenants[e.clientOwn]; ok {
		t.Error("assignment remains")
	}
	if a := e.db.lastAudit(); a.Action != "client.delete" || a.TenantID != 10 {
		t.Errorf("audit = %+v", a)
	}
	mustContain(t, get(h, "/clients?deleted="+id).Body.String(), "RADIUSクライアント #"+id+" を削除しました。")
	// 下流にない（所属の記録だけが残っている）。
	e.db.clientTenants[99] = 10
	if w := get(h, "/clients/99"); w.Code != 404 || !strings.Contains(w.Body.String(), "本BFF の外で削除された可能性があります") {
		t.Errorf("missing = %d", w.Code)
	}
}
