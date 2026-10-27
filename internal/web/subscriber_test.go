package web

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/oyaguma3/aka-ossbss/internal/auth"
	"github.com/oyaguma3/aka-ossbss/internal/authz"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
)

// テナントの利用者の一覧は、自分のテナントの所属の記録から作り、鍵の情報を出さない。
func TestSubscribersTenantUser(t *testing.T) {
	e := newTestEnv()
	// 所属の記録だけが残っている（本BFF の外で削除された）加入者。
	e.db.subTenants["001010000000009"] = 10
	w := get(e.handler(t, "a@example.com"), "/subscribers")
	if w.Code != 200 {
		t.Fatalf("list = %d %s", w.Code, w.Body)
	}
	body := w.Body.String()
	mustContain(t, body, imsiOwn, imsiAka, "001010000000009", "下流にない", "3 件", "既定 deny、ルール 1 件", "利用中")
	mustNotContain(t, body, imsiOther, imsiNone, "鍵の置き場所", "000000000020", "加入者の登録", "<th>テナント</th>")
	// provisioner の一覧は使わない（所属の記録の IMSI ごとに取得する）。
	if e.pv.listCalls != 0 || e.pv.gets != 3 {
		t.Errorf("list calls %d, gets %d", e.pv.listCalls, e.pv.gets)
	}
	// テナントの指定は無視する（自分のテナントに固定）。
	body = get(e.handler(t, "b@example.com"), "/subscribers?tenant=20").Body.String()
	mustNotContain(t, body, imsiOther)
	mustContain(t, body, imsiOwn)
}

// テナントの範囲の一覧のページ（20 件ずつ、続きは IMSI）と前方一致。
func TestSubscribersTenantPaging(t *testing.T) {
	e := newTestEnv()
	for i := range 25 {
		imsi := fmt.Sprintf("0010311%08d", i)
		e.pv.addSubscriber(imsi)
		e.db.subTenants[imsi] = 10
	}
	h := e.handler(t, "a@example.com")
	body := get(h, "/subscribers?prefix=00103").Body.String()
	mustContain(t, body, "25 件", "001031100000000", "001031100000019", "cursor=001031100000019", "次のページ")
	mustNotContain(t, body, "001031100000020", imsiOwn)
	body = get(h, "/subscribers?prefix=00103&cursor=001031100000019").Body.String()
	mustContain(t, body, "001031100000020", "001031100000024", "先頭へ")
	mustNotContain(t, body, "001031100000019", "次のページ")
	// 前方一致の形式の誤り。
	if w := get(h, "/subscribers?prefix=abc"); w.Code != 400 || !strings.Contains(w.Body.String(), "15 桁までの数字") {
		t.Errorf("bad prefix = %d", w.Code)
	}
	if w := get(h, "/subscribers?cursor=x"); w.Code != 400 {
		t.Errorf("bad cursor = %d", w.Code)
	}
}

// 運用担当の一覧: すべて（provisioner の一覧にテナントを添える）、テナントの絞り込み、テナントなし。
func TestSubscribersStaff(t *testing.T) {
	e := newTestEnv()
	h := e.handler(t, "c@example.com")
	body := get(h, "/subscribers").Body.String()
	mustContain(t, body, imsiOwn, imsiOther, imsiNone, imsiAka, "テナントA", "テナントB", "テナントなし", "aka-vecgen",
		"000000000020", "加入者の登録")
	if e.pv.listCalls != 1 || e.pv.lastList.Limit != subscribersPerPage {
		t.Errorf("list = %d %+v", e.pv.listCalls, e.pv.lastList)
	}
	body = get(h, "/subscribers?tenant=20").Body.String()
	mustContain(t, body, imsiOther, "1 件")
	mustNotContain(t, body, imsiOwn, imsiNone)
	body = get(h, "/subscribers?tenant=none").Body.String()
	mustContain(t, body, imsiNone)
	mustNotContain(t, body, imsiOwn, imsiOther, imsiAka)
	// 知らないテナント。
	if w := get(h, "/subscribers?tenant=99"); w.Code != 400 || !strings.Contains(w.Body.String(), "テナントの指定が正しくありません") {
		t.Errorf("unknown tenant = %d", w.Code)
	}
}

// 「テナントなし」は provisioner の一覧を 500 件ずつ調べ、50 件たまるか 5,000 件を調べたところで止める。
func TestSubscribersUnassignedScan(t *testing.T) {
	e := newTestEnv()
	e.pv.subs = map[string]provisioner.Subscriber{}
	// 6,000 件のうち、IMSI の順で 5,500 件目から後ろだけがテナントなし。
	for i := range 6000 {
		imsi := fmt.Sprintf("00101%010d", i)
		e.pv.subs[imsi] = provisioner.Subscriber{IMSI: imsi, KeyStore: provisioner.KeyStorePoC}
		if i < 5500 {
			e.db.subTenants[imsi] = 10
		}
	}
	h := e.handler(t, "d@example.com")
	body := get(h, "/subscribers?tenant=none").Body.String()
	mustContain(t, body, "5000 件を調べました（IMSI 001010000004999 まで）", "cursor=001010000004999", "続きを探す")
	if e.pv.listCalls != 10 || e.pv.lastList.Limit != unassignedScanPage {
		t.Errorf("list calls = %d %+v", e.pv.listCalls, e.pv.lastList)
	}
	// 続き: 5,500 件目から 50 件を出し、続きの位置はその最後の IMSI。
	body = get(h, "/subscribers?tenant=none&cursor=001010000004999").Body.String()
	mustContain(t, body, "001010000005500", "001010000005549", "cursor=001010000005549", "次のページ")
	mustNotContain(t, body, "001010000005499", "001010000005550", "件を調べました")
	// 最後のページ。
	body = get(h, "/subscribers?tenant=none&cursor=001010000005949").Body.String()
	mustContain(t, body, "001010000005950", "001010000005999")
	mustNotContain(t, body, "次のページ", "続きを探す")
}

func TestSubscribersListErrors(t *testing.T) {
	e := newTestEnv()
	e.pv.errs["GetSubscriber"] = &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	// テナントの利用者には設定の名前を出さない。
	w := get(e.handler(t, "a@example.com"), "/subscribers")
	if w.Code != 502 {
		t.Errorf("unavailable = %d", w.Code)
	}
	mustContain(t, w.Body.String(), "provisioner に接続できません。しばらくしてから", "取得できませんでした")
	mustNotContain(t, w.Body.String(), "OSSBSS_PROVISIONER")
	// 運用担当には原因の見当を出す。
	e.pv.errs["ListSubscribers"] = &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	w = get(e.handler(t, "c@example.com"), "/subscribers")
	mustContain(t, w.Body.String(), "provisioner が起動しているか")
}

func TestSubscriberCreate(t *testing.T) {
	e := newTestEnv()
	h := e.handler(t, "c@example.com")
	// 登録の画面: テナントの選択（既定は「選んでください」）と PLMN マップ。
	body := get(h, "/subscribers/new").Body.String()
	mustContain(t, body, `<option value="" selected disabled>選んでください</option>`, "テナントA", "テナントB",
		"<code>00102</code> → aka-vecgen", `name="idem" value="`)

	form := url.Values{"imsi": {"001010000000050"}, "ki": {strings.Repeat("A", 32)}, "opc": {strings.Repeat("b", 32)},
		"amf": {"8000"}, "sqn": {"000000000000"}, "default": {"deny"}, "idem": {"KEYKEYKEYKEYKEYKEYKEYKEYKE"}}
	// テナントを選んでいない。秘密の値は画面に戻す（入力の誤りのときだけ。provisioner には送っていない）。
	w := post(h, "/subscribers", form)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "所属のテナントを選んでください。") || len(e.pv.ops) != 0 {
		t.Fatalf("no tenant = %d %v", w.Code, e.pv.ops)
	}
	// ないテナント。
	form.Set("tenant", "99")
	if w := post(h, "/subscribers", form); w.Code != 400 || !strings.Contains(w.Body.String(), "選んだテナントはありません") {
		t.Errorf("unknown tenant = %d", w.Code)
	}
	// テナントB に登録。Ki は小文字にして送り、所属を記録し、監査ログに残す（秘密の値は入れない）。
	form.Set("tenant", "20")
	w = post(h, "/subscribers", form)
	if w.Code != 200 || w.Header().Get("HX-Redirect") != "/subscribers/001010000000050?created=1" {
		t.Fatalf("create = %d %v %s", w.Code, w.Header(), w.Body)
	}
	if e.pv.lastCreate.Ki != strings.Repeat("a", 32) || e.pv.lastCreate.Policy.Default != "deny" ||
		e.pv.lastOp() != "CreateSubscriber c@example.com KEYKEYKEYKEYKEYKEYKEYKEYKE" {
		t.Errorf("create = %+v %s", e.pv.lastCreate, e.pv.lastOp())
	}
	if e.db.subTenants["001010000000050"] != 20 {
		t.Errorf("assignment = %v", e.db.subTenants)
	}
	a := e.db.lastAudit()
	if a.Action != "subscriber.create" || a.Target != "001010000000050" || a.TenantID != 20 || a.ActorEmail != "c@example.com" ||
		a.ActorRole != authz.Operator || a.Detail["keyStore"] != "poc" || a.TraceID == "" {
		t.Errorf("audit = %+v", a)
	}
	if fmt.Sprint(a.Detail) != "map[amf:8000 keyStore:poc policyDefault:deny sqn:000000000000]" {
		t.Errorf("audit detail = %v", a.Detail)
	}
	body = get(h, "/subscribers/001010000000050?created=1").Body.String()
	mustContain(t, body, "加入者を登録しました（認可ポリシーも作りました）。", "テナントB")

	// 既にある: provisioner が断った（4xx）ので新しいキーにし、秘密の値は画面に戻さない。
	w = post(h, "/subscribers", form)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "既に登録されています") || strings.Contains(w.Body.String(), "KEYKEYKEYKEYKEYKEYKEYKEYKE") ||
		strings.Contains(w.Body.String(), strings.Repeat("a", 32)) {
		t.Errorf("duplicate = %d %s", w.Code, w.Body)
	}
	// provisioner に届かなかったときは同じキーを残す。
	e.pv.errs["CreateSubscriber"] = &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	form.Set("imsi", "001010000000051")
	w = post(h, "/subscribers", form)
	if w.Code != 502 || !strings.Contains(w.Body.String(), `value="KEYKEYKEYKEYKEYKEYKEYKEYKE"`) {
		t.Errorf("unavailable = %d", w.Code)
	}
	delete(e.pv.errs, "CreateSubscriber")

	// テナントなしで登録すると、残っていた所属の記録を消す。
	e.db.subTenants["001010000000052"] = 10
	form.Set("imsi", "001010000000052")
	form.Set("tenant", "none")
	if w := post(h, "/subscribers", form); w.Code != 200 {
		t.Fatalf("create none = %d", w.Code)
	}
	if _, ok := e.db.subTenants["001010000000052"]; ok {
		t.Error("stale assignment remains")
	}
	// 所属の記録に失敗しても登録は済んでいる。詳細で知らせる。
	e.db.assignErr = errors.New("db down")
	form.Set("imsi", "001010000000053")
	form.Set("tenant", "10")
	w = post(h, "/subscribers", form)
	if to := w.Header().Get("HX-Redirect"); to != "/subscribers/001010000000053?created=1&assign=failed" {
		t.Fatalf("assign failure redirect = %q", to)
	}
	e.db.assignErr = nil
	body = get(h, "/subscribers/001010000000053?created=1&assign=failed").Body.String()
	mustContain(t, body, "所属の記録に失敗しました（テナントなしになっています）")
}

// 既定の動作を allow に切り替えると、登録の前に確認ダイアログを出す。
func TestSubscriberNewFormAllow(t *testing.T) {
	e := newTestEnv()
	w := post(e.handler(t, "d@example.com"), "/subscribers/new/form", url.Values{"default": {"allow"}, "imsi": {"001010000000060"}, "tenant": {"10"}})
	body := w.Body.String()
	mustContain(t, body, `commandfor="dialog-create"`, `<dialog id="dialog-create" class="modal">`, `form="subscriber-new-form"`,
		"どのルールにも一致しない接続も許可します", `value="001010000000060"`, `<option value="10" selected>`)
	// ダイアログはフォームの外に置く（フォームの入れ子にしない）。
	if strings.Index(body, `<dialog id="dialog-create"`) < strings.Index(body, "</form>") {
		t.Error("dialog is inside the form")
	}
	if len(e.pv.ops) != 0 {
		t.Errorf("provisioner was called: %v", e.pv.ops)
	}
}

// 加入者の詳細は役割で項目を変える（設計概要 §6.2）。
func TestSubscriberDetailByRole(t *testing.T) {
	e := newTestEnv()
	hidden := []string{"鍵の置き場所", "000000000020", "Ki / OPc を表示", "鍵の属性の変更", "この加入者を削除", "<th class=\"w-48\">テナント</th>"}
	for _, c := range []struct {
		email      string
		wantShown  []string
		wantHidden []string
	}{
		{"a@example.com", []string{"停止する", "ap-01", "corp"}, append(hidden, "編集する", "Ki / OPc の変更")},
		{"b@example.com", []string{"停止する", "編集する"}, append(hidden, "Ki / OPc の変更")},
		{"c@example.com", []string{"停止する", "編集する", "鍵の置き場所", "000000000020", "Ki / OPc を表示", "Ki / OPc の変更", "この加入者を削除", "テナントA"},
			[]string{"鍵の属性の変更", `name="amf"`}},
		{"d@example.com", []string{"鍵の属性の変更", `name="amf"`, `name="sqn"`, "この加入者を削除"}, nil},
	} {
		w := get(e.handler(t, c.email), "/subscribers/"+imsiOwn)
		if w.Code != 200 {
			t.Fatalf("%s = %d", c.email, w.Code)
		}
		mustContain(t, w.Body.String(), c.wantShown...)
		mustNotContain(t, w.Body.String(), c.wantHidden...)
	}
	// aka-vecgen の加入者は、システム管理者に SQN の増加タイプと平文HTTP の許可を出す。
	body := get(e.handler(t, "d@example.com"), "/subscribers/"+imsiAka).Body.String()
	mustContain(t, body, `name="sqn_type"`, `name="allow_plain"`, "許可するクライアントID")
	// 食い違いは運用担当・システム管理者にだけ出す。
	s := e.pv.subs[imsiOwn]
	s.Issues = []provisioner.Issue{provisioner.IssueAVClientNotAllowed}
	e.pv.subs[imsiOwn] = s
	mustContain(t, get(e.handler(t, "c@example.com"), "/subscribers/"+imsiOwn).Body.String(), "vector-gateway の AVクライアントを許可していません")
	mustNotContain(t, get(e.handler(t, "b@example.com"), "/subscribers/"+imsiOwn).Body.String(), "AVクライアント")
}

// 所属の記録があるのに provisioner にない加入者。
func TestSubscriberMissingDownstream(t *testing.T) {
	e := newTestEnv()
	e.db.subTenants["001010000000009"] = 10
	w := get(e.handler(t, "a@example.com"), "/subscribers/001010000000009")
	if w.Code != 404 || !strings.Contains(w.Body.String(), "本BFF の外で削除された可能性があります。所属の記録はシステム管理者が消します。") {
		t.Errorf("missing = %d %s", w.Code, w.Body)
	}
}

func TestSubscriberKeys(t *testing.T) {
	e := newTestEnv()
	w := post(e.handler(t, "c@example.com"), "/subscribers/"+imsiOther+"/keys", nil)
	if w.Code != 200 {
		t.Fatalf("keys = %d", w.Code)
	}
	mustContain(t, w.Body.String(), strings.Repeat("1", 32), strings.Repeat("2", 32), "監査ログに残りました")
	if e.pv.lastOp() != "GetSubscriberKeys c@example.com" {
		t.Errorf("op = %s", e.pv.lastOp())
	}
	if a := e.db.lastAudit(); a.Action != "subscriber.keys.read" || a.TenantID != 20 || len(a.Detail) != 0 {
		t.Errorf("audit = %+v", a)
	}
}

func TestSubscriberAuth(t *testing.T) {
	e := newTestEnv()
	// 運用担当は Ki / OPc だけ。AMF などを送ってきたら断る。
	hc := e.handler(t, "c@example.com")
	w := post(hc, "/subscribers/"+imsiOwn+"/auth", url.Values{"ki": {strings.Repeat("c", 32)}, "idem": {"AAAAAAAAAAAAAAAAAAAAAAAAAA"}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Ki を変更しました。") {
		t.Fatalf("operator ki = %d %s", w.Code, w.Body)
	}
	if e.pv.lastUpdate.Ki != strings.Repeat("c", 32) || e.pv.lastUpdate.AMF != "" || e.pv.lastOp() != "UpdateSubscriber c@example.com AAAAAAAAAAAAAAAAAAAAAAAAAA" {
		t.Errorf("update = %+v %s", e.pv.lastUpdate, e.pv.lastOp())
	}
	if a := e.db.lastAudit(); a.Action != "subscriber.update" || fmt.Sprint(a.Detail["fields"]) != "[ki]" {
		t.Errorf("audit = %+v", a)
	}
	n := len(e.pv.ops)
	if w := post(hc, "/subscribers/"+imsiOwn+"/auth", url.Values{"amf": {"9000"}}); w.Code != 403 || len(e.pv.ops) != n {
		t.Errorf("operator amf = %d", w.Code)
	}
	// 何も入力しなければ「変更はありません」。
	if w := post(hc, "/subscribers/"+imsiOwn+"/auth", url.Values{}); !strings.Contains(w.Body.String(), "変更はありません。") {
		t.Error("no change is not reported")
	}

	// システム管理者は変わった項目だけを送る（SQN を変えなければ送らない）。
	hd := e.handler(t, "d@example.com")
	w = post(hd, "/subscribers/"+imsiOwn+"/auth", url.Values{"amf": {"9000"}, "sqn": {"000000000020"}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "AMF を変更しました。") || e.pv.lastUpdate.SQN != "" || e.pv.lastUpdate.AMF != "9000" {
		t.Errorf("admin amf = %d %+v", w.Code, e.pv.lastUpdate)
	}
	w = post(hd, "/subscribers/"+imsiAka+"/auth", url.Values{"amf": {"8000"}, "sqn": {"000000000020"}, "sqn_type": {"inc1"}, "allow_plain": {"1"}})
	if !strings.Contains(w.Body.String(), "SQN の増加タイプ、平文HTTP の許可 を変更しました。") {
		t.Errorf("aka = %s", w.Body)
	}
	// 入力の誤り。
	w = post(hd, "/subscribers/"+imsiOwn+"/auth", url.Values{"amf": {"xyz"}, "sqn": {"000000000020"}, "ki": {"12"}})
	mustContain(t, w.Body.String(), "AMF は 16 進 4 桁", "Ki は 16 進 32 桁")
	if w.Code != 400 {
		t.Errorf("invalid = %d", w.Code)
	}
	// 鍵のない加入者。
	s := e.pv.subs[imsiOwn]
	s.Key = nil
	e.pv.subs[imsiOwn] = s
	if w := post(hd, "/subscribers/"+imsiOwn+"/auth", url.Values{"amf": {"8000"}, "sqn": {"000000000000"}}); w.Code != 409 {
		t.Errorf("no key = %d", w.Code)
	}
}

func TestSubscriberDelete(t *testing.T) {
	e := newTestEnv()
	h := e.handler(t, "c@example.com")
	w := post(h, "/subscribers/"+imsiOwn+"/delete", url.Values{"idem": {"BBBBBBBBBBBBBBBBBBBBBBBBBB"}})
	if w.Code != 200 || w.Header().Get("HX-Redirect") != "/subscribers?deleted="+imsiOwn {
		t.Fatalf("delete = %d %v", w.Code, w.Header())
	}
	if _, ok := e.db.subTenants[imsiOwn]; ok {
		t.Error("assignment remains")
	}
	if e.pv.lastOp() != "DeleteSubscriber c@example.com BBBBBBBBBBBBBBBBBBBBBBBBBB" {
		t.Errorf("op = %s", e.pv.lastOp())
	}
	if a := e.db.lastAudit(); a.Action != "subscriber.delete" || a.TenantID != 10 {
		t.Errorf("audit = %+v", a)
	}
	mustContain(t, get(h, "/subscribers?deleted="+imsiOwn).Body.String(), "を削除しました（認可ポリシーも削除しました）。")
	// 所属の記録を消せなかった。
	e.db.assignErr = errors.New("db down")
	w = post(h, "/subscribers/"+imsiOther+"/delete", url.Values{})
	if w.Header().Get("HX-Redirect") != "/subscribers?deleted="+imsiOther+"&unassign=failed" {
		t.Errorf("unassign failure = %v", w.Header())
	}
	e.db.assignErr = nil
	mustContain(t, get(h, "/subscribers?deleted="+imsiOther+"&unassign=failed").Body.String(), "所属の記録を消せませんでした")
	// 操作が途中で失敗した（元に戻せなかった）: 運用担当には操作の記録の ID を出す。
	e.pv.errs["DeleteSubscriber"] = &provisioner.Error{Status: 500, Problem: provisioner.Problem{
		Cause: provisioner.CauseOperationIncomplete, Downstream: "aka", OperationID: "op-123"}}
	w = post(h, "/subscribers/"+imsiNone+"/delete", url.Values{})
	if w.Code != 500 {
		t.Errorf("incomplete = %d", w.Code)
	}
	mustContain(t, w.Body.String(), "aka-vecgen への操作が途中で失敗し、元に戻せませんでした", `<a class="link" href="/operations/op-123">操作の記録を開く</a>`)
}

func TestSubscriberStatus(t *testing.T) {
	e := newTestEnv()
	h := e.handler(t, "a@example.com")
	// 詳細の停止のボタンは確認ダイアログを開く。
	body := get(h, "/subscribers/"+imsiOwn).Body.String()
	mustContain(t, body, `command="show-modal" commandfor="dialog-suspend"`, `form="status-suspend"`,
		"加入者 "+imsiOwn+" を停止します。次の認証から拒否します（接続中のセッションは切れません）。よろしいですか？")
	w := post(h, "/subscribers/"+imsiOwn+"/status", url.Values{"status": {"suspended"}, "status_idem": {"CCCCCCCCCCCCCCCCCCCCCCCCCC"}})
	if w.Code != 200 {
		t.Fatalf("suspend = %d %s", w.Code, w.Body)
	}
	mustContain(t, w.Body.String(), "停止しました。次の認証から拒否します", `<span class="badge badge-error">停止中</span>`, "再開する")
	if e.pv.lastOp() != "SetPolicyStatus suspended a@example.com CCCCCCCCCCCCCCCCCCCCCCCCCC" {
		t.Errorf("op = %s", e.pv.lastOp())
	}
	if a := e.db.lastAudit(); a.Action != "policy.suspend" || a.ActorRole != authz.TenantUser || a.TenantID != 10 {
		t.Errorf("audit = %+v", a)
	}
	// 一覧にもバッジを出す。
	mustContain(t, get(h, "/subscribers").Body.String(), `<span class="badge badge-error badge-sm">停止中</span>`)
	// 再開。
	w = post(h, "/subscribers/"+imsiOwn+"/status", url.Values{"status": {"active"}})
	mustContain(t, w.Body.String(), "再開しました。次の認証から認可ポリシーのルールに従います。")
	if a := e.db.lastAudit(); a.Action != "policy.resume" {
		t.Errorf("audit = %+v", a)
	}
	// 状態の値が正しくない。
	if w := post(h, "/subscribers/"+imsiOwn+"/status", url.Values{"status": {"x"}}); w.Code != 400 {
		t.Errorf("bad status = %d", w.Code)
	}
	// 同じ IMSI に完了していない操作がある: テナントの利用者には操作の記録を出さない。
	e.pv.errs["SetPolicyStatus"] = &provisioner.Error{Status: 409, Problem: provisioner.Problem{
		Cause: provisioner.CauseOperationUnresolved, OperationID: "op-9"}}
	w = post(h, "/subscribers/"+imsiOwn+"/status", url.Values{"status": {"suspended"}})
	if w.Code != 409 || !strings.Contains(w.Body.String(), "運用担当に対応を依頼してください") || strings.Contains(w.Body.String(), "op-9") {
		t.Errorf("unresolved = %d %s", w.Code, w.Body)
	}
	// provisioner に届かなかったときは同じキーを残す。
	e.pv.errs["SetPolicyStatus"] = &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	w = post(h, "/subscribers/"+imsiOwn+"/status", url.Values{"status": {"suspended"}, "status_idem": {"DDDDDDDDDDDDDDDDDDDDDDDDDD"}})
	if w.Code != 502 || !strings.Contains(w.Body.String(), `value="DDDDDDDDDDDDDDDDDDDDDDDDDD"`) {
		t.Errorf("unavailable = %d", w.Code)
	}
}

// 停止・再開の表示（状態のない provisioner、認可ポリシーのない加入者、不正な値）。
func TestPolicyStatusDisplay(t *testing.T) {
	e := newTestEnv()
	set := func(imsi string, f func(*provisioner.Subscriber)) {
		s := e.pv.subs[imsi]
		f(&s)
		e.pv.subs[imsi] = s
	}
	set(imsiOwn, func(s *provisioner.Subscriber) { s.Status = "" })
	set(imsiOther, func(s *provisioner.Subscriber) { s.Policy, s.Status = nil, "" })
	set(imsiNone, func(s *provisioner.Subscriber) { s.Status = "paused" })
	h := e.handler(t, "d@example.com")
	body := get(h, "/subscribers/"+imsiOwn).Body.String()
	mustContain(t, body, "接続先が停止・再開に対応していません")
	mustNotContain(t, body, "停止する")
	body = get(h, "/subscribers/"+imsiOther).Body.String()
	mustContain(t, body, "認可ポリシーがないため、停止・再開できません", "作成する")
	body = get(h, "/subscribers/"+imsiNone).Body.String()
	mustContain(t, body, "不明な状態（paused）", "停止する", "再開する")
	if !slices.Contains([]string{"不明な状態（paused）"}, policyStatusLabel("paused")) {
		t.Error("label")
	}
}

// 監査ログの書き込みに失敗しても、済んだ操作は成功として返し、ERROR のログを出す（画面仕様 §1.7）。
func TestAuditFailureDoesNotFailOperation(t *testing.T) {
	e := newTestEnv()
	e.db.auditErr = errors.New("db down")
	var logs strings.Builder
	h := newHandlerPV(t, e.db, auth.Dev("a@example.com"), slog.New(slog.NewTextHandler(&logs, nil)), e.pv)
	w := post(h, "/subscribers/"+imsiOwn+"/status", url.Values{"status": {"suspended"}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "停止しました。") {
		t.Errorf("status = %d", w.Code)
	}
	if !strings.Contains(logs.String(), "audit log write failed") || !strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("logs = %s", logs.String())
	}
}
