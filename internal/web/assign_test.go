package web

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/oyaguma3/aka-ossbss/internal/authz"
	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
)

// provisioner_failedOp は失敗（手での対応が必要）の操作の記録を作る。
func provisioner_failedOp(imsi string) provisioner.Operation {
	return provisioner.Operation{ID: testOperationID, Kind: "subscriber.create", IMSI: imsi, KeyStore: provisioner.KeyStorePoC,
		Status: provisioner.OpFailed, Attempts: 3, Operator: "c@example.com", MgmtClient: "ossbss", TraceID: "trace-1",
		CreatedAt: time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC),
		Steps: []provisioner.OperationStep{{Name: "subscriber.create", Downstream: "prov", State: "done"},
			{Name: "subscriber.compensate", Downstream: "prov", State: "failed",
				Error: &provisioner.StepError{Status: 503, Cause: "DOWNSTREAM_UNAVAILABLE", Detail: "connection refused"}}}}
}

func TestSubscriberTenantChange(t *testing.T) {
	e := newTestEnv()
	h := e.handler(t, "d@example.com")
	// 詳細に所属の変更を出す（システム管理者だけ）。
	mustContain(t, get(h, "/subscribers/"+imsiOwn).Body.String(), "所属の変更", `commandfor="dialog-assign"`, `<option value="10" selected>`)
	mustNotContain(t, get(e.handler(t, "c@example.com"), "/subscribers/"+imsiOwn).Body.String(), "所属の変更")
	// テナントA → テナントB: 両方のテナントに監査ログ。
	w := post(h, "/subscribers/"+imsiOwn+"/tenant", url.Values{"tenant": {"20"}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "所属を「テナントB」にしました。") || e.db.subTenants[imsiOwn] != 20 {
		t.Fatalf("change = %d %s", w.Code, w.Body)
	}
	if len(e.db.audit) != 2 || e.db.audit[0].TenantID != 10 || e.db.audit[1].TenantID != 20 || e.db.audit[0].Action != "assignment.update" ||
		e.db.audit[0].Detail["from"] != "テナントA" || e.db.audit[0].Detail["to"] != "テナントB" {
		t.Errorf("audit = %+v", e.db.audit)
	}
	// テナントなしにする: 移動元にだけ。
	post(h, "/subscribers/"+imsiOwn+"/tenant", url.Values{"tenant": {"none"}})
	if _, ok := e.db.subTenants[imsiOwn]; ok || len(e.db.audit) != 3 || e.db.audit[2].TenantID != 20 {
		t.Errorf("unassign = %v %+v", e.db.subTenants, e.db.audit)
	}
	// 同じテナントなら記録しない。
	if w := post(h, "/subscribers/"+imsiOwn+"/tenant", url.Values{"tenant": {"none"}}); !strings.Contains(w.Body.String(), "所属は変わりません") || len(e.db.audit) != 3 {
		t.Errorf("same = %s", w.Body)
	}
	// 付け替え先を選んでいない、provisioner にない。
	if w := post(h, "/subscribers/"+imsiOwn+"/tenant", url.Values{}); w.Code != 400 || !strings.Contains(w.Body.String(), "付け替え先のテナントを選んでください") {
		t.Errorf("no tenant = %d", w.Code)
	}
	if w := post(h, "/subscribers/001019999999999/tenant", url.Values{"tenant": {"10"}}); w.Code != 404 {
		t.Errorf("missing = %d", w.Code)
	}
	if _, ok := e.db.subTenants["001019999999999"]; ok {
		t.Error("assignment was created for a missing subscriber")
	}
}

func TestClientTenantChange(t *testing.T) {
	e := newTestEnv()
	h := e.handler(t, "d@example.com")
	id := fmt.Sprint(e.clientNone)
	w := post(h, "/clients/"+id+"/tenant", url.Values{"tenant": {"10"}})
	if w.Code != 200 || e.db.clientTenants[e.clientNone] != 10 || !strings.Contains(w.Body.String(), "所属を「テナントA」にしました。") {
		t.Fatalf("change = %d %s", w.Code, w.Body)
	}
	if a := e.db.lastAudit(); a.Target != "#"+id || a.TenantID != 10 || len(e.db.audit) != 1 {
		t.Errorf("audit = %+v", e.db.audit)
	}
	if w := post(h, "/clients/99/tenant", url.Values{"tenant": {"10"}}); w.Code != 404 {
		t.Errorf("missing = %d", w.Code)
	}
}

func TestSubscribersBulkAssign(t *testing.T) {
	e := newTestEnv()
	h := e.handler(t, "d@example.com")
	// 一覧にチェックボックスとまとめての付け替えを出す（システム管理者だけ）。
	body := get(h, "/subscribers?tenant=none").Body.String()
	mustContain(t, body, `<form id="bulk-assign" method="post" action="/subscribers/assign"`, `name="back" value="/subscribers?tenant=none"`,
		`name="imsi" value="`+imsiNone+`" form="bulk-assign"`)
	mustNotContain(t, get(e.handler(t, "c@example.com"), "/subscribers").Body.String(), "bulk-assign")
	// テナントなしの加入者と、provisioner にない IMSI と、既にテナントB のものをテナントB に。
	w := post(h, "/subscribers/assign", url.Values{"imsi": {imsiNone, "001019999999999", imsiOther}, "tenant": {"20"},
		"back": {"/subscribers?tenant=none"}})
	to := w.Header().Get("HX-Redirect")
	if !strings.HasPrefix(to, "/subscribers?") || !strings.Contains(to, "bulk_changed=1") || !strings.Contains(to, "bulk_skipped=1") ||
		!strings.Contains(to, "tenant=none") {
		t.Fatalf("redirect = %q", to)
	}
	if e.db.subTenants[imsiNone] != 20 || len(e.db.audit) != 1 {
		t.Errorf("assigned = %v %+v", e.db.subTenants, e.db.audit)
	}
	if _, ok := e.db.subTenants["001019999999999"]; ok {
		t.Error("missing subscriber was assigned")
	}
	mustContain(t, get(h, to).Body.String(), "1 件の所属を変更しました。1 件は provisioner にないため飛ばしました。")
	// 戻り先は一覧だけ（別のサイトや画面へは戻さない）。
	w = post(h, "/subscribers/assign", url.Values{"imsi": {imsiOwn}, "tenant": {"20"}, "back": {"//evil.example/"}})
	if to := w.Header().Get("HX-Redirect"); !strings.HasPrefix(to, "/subscribers?") {
		t.Errorf("unsafe back = %q", to)
	}
	// 選んでいない、付け替え先がない、上限を超える。
	w = post(h, "/subscribers/assign", url.Values{"tenant": {"20"}})
	mustContain(t, get(h, w.Header().Get("HX-Redirect")).Body.String(), "所属を変えるものを選んでください。")
	w = post(h, "/subscribers/assign", url.Values{"imsi": {imsiOwn}, "tenant": {"99"}})
	mustContain(t, get(h, w.Header().Get("HX-Redirect")).Body.String(), "選んだテナントはありません")
	many := url.Values{"tenant": {"10"}}
	for i := range maxBulkSubscribers + 1 {
		many.Add("imsi", fmt.Sprintf("00101%010d", i))
	}
	if w := post(h, "/subscribers/assign", many); w.Code != 400 {
		t.Errorf("too many = %d", w.Code)
	}
	// 戻り先のクエリの文は受け付けない（数と誤りの種類から作る）。
	mustNotContain(t, get(h, "/subscribers?bulk_error=<b>x</b>&bulk_changed=x").Body.String(), "<b>x</b>", "件の所属を変更しました")
	// DB に書けなかったら途中で止める。
	// （imsiOwn は上の戻り先の確認でテナントB に移っているので、テナントA に戻す。）
	e.db.assignErr = errors.New("db down")
	w = post(h, "/subscribers/assign", url.Values{"imsi": {imsiOwn}, "tenant": {"10"}})
	mustContain(t, get(h, w.Header().Get("HX-Redirect")).Body.String(), "途中で止めました（"+imsiOwn+" から後ろは変えていません）")
}

func TestClientsBulkAssign(t *testing.T) {
	e := newTestEnv()
	h := e.handler(t, "d@example.com")
	body := get(h, "/clients").Body.String()
	mustContain(t, body, `action="/clients/assign"`, fmt.Sprintf(`name="id" value="%d" form="bulk-assign"`, e.clientNone))
	w := post(h, "/clients/assign", url.Values{"id": {fmt.Sprint(e.clientNone), fmt.Sprint(e.clientOwn), "99"}, "tenant": {"10"}, "back": {"/clients?tenant=none"}})
	to := w.Header().Get("HX-Redirect")
	if !strings.Contains(to, "bulk_changed=1") || !strings.Contains(to, "bulk_skipped=1") || e.db.clientTenants[e.clientNone] != 10 {
		t.Errorf("bulk = %q %v", to, e.db.clientTenants)
	}
	if w := post(h, "/clients/assign", url.Values{"id": {"x"}, "tenant": {"10"}}); w.Code != 400 {
		t.Errorf("bad id = %d", w.Code)
	}
}

func TestOrphans(t *testing.T) {
	e := newTestEnv()
	// 所属の記録だけが残っている加入者と RADIUSクライアント。
	e.db.subTenants["001010000000009"] = 20
	e.db.clientTenants[77] = 10
	h := e.handler(t, "d@example.com")
	body := get(h, "/assignments/orphans").Body.String()
	mustContain(t, body, `name="imsi" value="001010000000009"`, `name="client" value="77"`, "所属の記録を 4 件調べました", "最後まで調べました")
	mustNotContain(t, body, `value="`+imsiOwn+`"`)
	// 消す直前に確かめ直す（その間に作り直されたものは消さない）。
	e.db.subTenants["001010000000008"] = 10
	e.pv.addSubscriber("001010000000008")
	w := post(h, "/assignments/orphans/delete", url.Values{"imsi": {"001010000000009", "001010000000008"}, "client": {"77"}})
	to := w.Header().Get("HX-Redirect")
	if to != "/assignments/orphans?deleted=2&kept=1" {
		t.Fatalf("redirect = %q", to)
	}
	if _, ok := e.db.subTenants["001010000000009"]; ok {
		t.Error("orphan remains")
	}
	if e.db.subTenants["001010000000008"] != 10 {
		t.Error("existing subscriber's assignment was deleted")
	}
	if _, ok := e.db.clientTenants[77]; ok {
		t.Error("client orphan remains")
	}
	if len(e.db.audit) != 2 || e.db.audit[0].Action != "assignment.delete" || e.db.audit[0].TenantID != 20 || e.db.audit[1].TenantID != 10 {
		t.Errorf("audit = %+v", e.db.audit)
	}
	mustContain(t, get(h, to).Body.String(), "2 件の所属の記録を消しました。1 件は provisioner にあったため消しませんでした。")
	// 200 件ずつ調べ、続きは調べ終えた IMSI から。
	for i := range 250 {
		e.db.subTenants[fmt.Sprintf("00105%010d", i)] = 10
	}
	body = get(h, "/assignments/orphans").Body.String()
	mustContain(t, body, "所属の記録を 200 件調べました", "続きを調べる")
	if e.pv.gets == 0 {
		t.Error("provisioner was not asked")
	}
	// provisioner に接続できなければ止める。
	e.pv.errs["GetSubscriber"] = &provisioner.Error{Status: 503, Problem: provisioner.Problem{Cause: provisioner.CauseDownstreamUnavailable, Downstream: "prov"}}
	if w := get(h, "/assignments/orphans"); w.Code != 503 {
		t.Errorf("unavailable = %d", w.Code)
	}
}

func TestOperations(t *testing.T) {
	e := newTestEnv()
	e.pv.opsByID[testOperationID] = provisioner_failedOp(imsiOwn)
	hc := e.handler(t, "c@example.com")
	body := get(hc, "/operations").Body.String()
	mustContain(t, body, "1 件", `href="/operations/`+testOperationID+`"`, "加入者の登録", "テナントA", "失敗（手での対応が必要）")
	// 運用担当は見るだけ。
	body = get(hc, "/operations/"+testOperationID).Body.String()
	mustContain(t, body, "やり直しと閉じるは、システム管理者が行います。", "作った加入者（鍵）の削除（元に戻す）", "connection refused", "テナント: テナントA")
	mustNotContain(t, body, "やり直す</button>")
	if w := get(hc, "/operations?status=x"); w.Code != 400 {
		t.Errorf("bad status = %d", w.Code)
	}
	if w := get(hc, "/operations/not-a-uuid"); w.Code != 404 {
		t.Errorf("bad id = %d", w.Code)
	}
	// システム管理者はやり直す・閉じる。
	hd := e.handler(t, "d@example.com")
	mustContain(t, get(hd, "/operations/"+testOperationID).Body.String(), "やり直す</button>", `commandfor="dialog-dismiss"`)
	w := post(hd, "/operations/"+testOperationID+"/retry", url.Values{"idem": {"JJJJJJJJJJJJJJJJJJJJJJJJJJ"}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "やり直しました。操作は「完了」になりました。") {
		t.Fatalf("retry = %d %s", w.Code, w.Body)
	}
	if e.pv.lastOp() != "RetryOperation d@example.com JJJJJJJJJJJJJJJJJJJJJJJJJJ" {
		t.Errorf("op = %s", e.pv.lastOp())
	}
	if a := e.db.lastAudit(); a.Action != "operation.retry" || a.Target != testOperationID || a.TenantID != 10 {
		t.Errorf("audit = %+v", a)
	}
	e.pv.opsByID[testOperationID] = provisioner_failedOp(imsiOwn)
	if w := post(hd, "/operations/"+testOperationID+"/dismiss", url.Values{}); !strings.Contains(w.Body.String(), "操作を閉じました。") {
		t.Errorf("dismiss = %s", w.Body)
	}
	if w := get(hd, "/operations/99999999-2222-4333-8444-555555555555"); w.Code != 404 || !strings.Contains(w.Body.String(), "7 日で消えます") {
		t.Errorf("missing = %d", w.Code)
	}
}

func TestAuditView(t *testing.T) {
	e := newTestEnv()
	for _, a := range []db.AuditEntry{
		{ActorEmail: "c@example.com", ActorRole: authz.Operator, TenantID: 10, Action: "subscriber.create", Target: imsiOwn, Detail: map[string]any{"amf": "8000"}},
		{ActorEmail: "c@example.com", ActorRole: authz.Operator, TenantID: 20, Action: "subscriber.create", Target: imsiOther},
		{ActorEmail: "c@example.com", ActorRole: authz.Operator, Action: "client.create", Target: "#3"},
		{ActorEmail: "b@example.com", ActorRole: authz.TenantAdmin, TenantID: 10, Action: "policy.suspend", Target: imsiOwn, TraceID: "t-9"},
	} {
		e.db.audit = append(e.db.audit, a)
	}
	// テナント管理者は自分のテナントのものだけ（テナントの指定は無視する）。
	body := get(e.handler(t, "b@example.com"), "/audit?tenant=20").Body.String()
	mustContain(t, body, imsiOwn, "加入者の停止", "t-9", "amf: 8000", "あなたのテナントが対象のものだけ")
	mustNotContain(t, body, imsiOther, "#3", `href="/audit/provisioner"`)
	// 運用担当はすべて。テナントと操作で絞れる。provisioner と下流のタブは出さない。
	hc := e.handler(t, "c@example.com")
	body = get(hc, "/audit").Body.String()
	mustContain(t, body, imsiOwn, imsiOther, "#3")
	mustNotContain(t, body, `href="/audit/provisioner"`)
	mustNotContain(t, get(hc, "/audit?tenant=none").Body.String(), imsiOwn)
	body = get(hc, "/audit?action=client.create").Body.String()
	mustContain(t, body, "#3")
	mustNotContain(t, body, imsiOther)
	if w := get(hc, "/audit?action=drop"); w.Code != 400 {
		t.Errorf("bad action = %d", w.Code)
	}
	// 続きの読み込み（50 件ずつ）。
	for i := range 60 {
		e.db.audit = append(e.db.audit, db.AuditEntry{ActorEmail: "c@example.com", ActorRole: authz.Operator, Action: "client.update", Target: fmt.Sprintf("#x%d", i)})
	}
	body = get(hc, "/audit").Body.String()
	mustContain(t, body, "さらに古いものを表示", "#x59")
	mustNotContain(t, body, "#x9<")
	w := do(hc, "GET", "/audit?tenant=&action=&before=15", map[string]string{"HX-Request": "true"})
	mustContain(t, w.Body.String(), `hx-swap-oob="beforeend:#audit-rows"`, "#x9<", imsiOwn)
	// システム管理者には provisioner と下流のタブ。
	e.pv.pvAudit = []provisioner.AuditLogEntry{{ID: "1-0", Operator: "c@example.com", Action: "operation.resume", Target: imsiOwn, OperationID: testOperationID, Result: "completed"}}
	e.pv.provAudit = []provisioner.ProvAuditLogEntry{{ID: "2-0", Action: "policy.update", Target: imsiOwn, Details: "default: deny -> allow"}}
	e.pv.akaAudit = []provisioner.AkaAuditLogEntry{{ID: "3-0", Action: "subscriber.update", Target: "440100000000001", Detail: map[string]any{"amf": map[string]any{"from": "8000", "to": "9000"}}}}
	hd := e.handler(t, "d@example.com")
	mustContain(t, get(hd, "/audit").Body.String(), `href="/audit/provisioner"`, `href="/audit/aka"`)
	mustContain(t, get(hd, "/audit/provisioner").Body.String(), "自動のやり直し", `href="/operations/`+testOperationID+`"`, "完了")
	mustContain(t, get(hd, "/audit/prov").Body.String(), "認可ポリシーの変更", "default: deny -&gt; allow")
	mustContain(t, get(hd, "/audit/aka").Body.String(), "amf: 8000 → 9000")
	if w := get(hd, "/audit/aka?before=x"); w.Code != 400 {
		t.Errorf("bad before = %d", w.Code)
	}
	e.pv.errs["ListAkaAuditLogs"] = &provisioner.Error{Status: 404, Problem: provisioner.Problem{Cause: provisioner.CauseDownstreamNotConfigured}}
	mustContain(t, get(hd, "/audit/aka").Body.String(), "aka-vecgen を扱わない設定です")
}

func TestDashboard(t *testing.T) {
	e := newTestEnv()
	s := e.pv.subs[imsiOwn]
	s.Status = provisioner.PolicySuspended
	e.pv.subs[imsiOwn] = s
	s = e.pv.subs[imsiOther]
	s.Status = provisioner.PolicySuspended
	e.pv.subs[imsiOther] = s
	// テナントユーザー: 所属の記録の件数（2）、RADIUSクライアント（1）、停止中は後から読み込む。
	ha := e.handler(t, "a@example.com")
	body := get(ha, "/").Body.String()
	mustContain(t, body, `hx-get="/dashboard/suspended" hx-trigger="load"`, "自分のテナントの所属の記録の数", "できること")
	if !strings.Contains(body, `<div class="stat-value">2</div>`) || !strings.Contains(body, `<div class="stat-value">1</div>`) {
		t.Errorf("counts: %s", body[strings.Index(body, "stats"):strings.Index(body, "stats")+1500])
	}
	mustNotContain(t, body, "<div class=\"stat-title\">テナント</div>")
	if e.pv.policyCalls != 0 {
		t.Error("policies were listed for the dashboard of a tenant user")
	}
	// 停止中の数は範囲で数える（テナントA の停止中は 1、すべてでは 2）。
	if w := get(ha, "/dashboard/suspended"); strings.TrimSpace(w.Body.String()) != "1" {
		t.Errorf("tenant suspended = %q", w.Body)
	}
	hd := e.handler(t, "d@example.com")
	if w := get(hd, "/dashboard/suspended"); strings.TrimSpace(w.Body.String()) != "2" {
		t.Errorf("all suspended = %q", w.Body)
	}
	body = get(hd, "/").Body.String()
	mustContain(t, body, "<div class=\"stat-title\">テナント</div>", "認可ポリシーの数", "完了していない操作があります")
	// provisioner に接続できなければ「-」。
	e.pv.errs["ListPolicies"] = errors.New("down")
	if w := get(hd, "/dashboard/suspended"); !strings.Contains(w.Body.String(), "-") {
		t.Errorf("unavailable = %q", w.Body)
	}
	delete(e.pv.errs, "ListPolicies")
	// 10,000 件を超えたら止める。
	for i := range suspendedScanMax + 10 {
		imsi := fmt.Sprintf("00109%010d", i)
		e.pv.subs[imsi] = provisioner.Subscriber{IMSI: imsi, Policy: &provisioner.PolicyPut{Default: "deny"}, Status: provisioner.PolicyActive}
	}
	if w := get(hd, "/dashboard/suspended"); !strings.Contains(w.Body.String(), "10,000 件までを数えました") {
		t.Errorf("truncated = %q", w.Body)
	}
}
