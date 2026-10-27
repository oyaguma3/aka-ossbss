package provisioner_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"testing"

	. "github.com/oyaguma3/aka-ossbss/internal/provisioner"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner/provisionertest"
	"github.com/oyaguma3/aka-ossbss/internal/trace"
)

// 実際の aka-provisioner を相手にした契約テスト。次の環境変数を指定したときだけ実行する。
//
//	OSSBSS_TEST_PROVISIONER_URL          provisioner のベース URL（例: https://127.0.0.1:19446/admin/v1）
//	OSSBSS_TEST_PROVISIONER_CLIENT_CERT  本BFF のクライアント証明書の PEM（秘密鍵も含める）。
//	                                            provisioner の PROVISIONER_ADMIN_CLIENTS に登録しておく
//	OSSBSS_TEST_PROVISIONER_SERVER_CERT  provisioner のサーバー証明書の PEM（aka-provisioner server-cert の出力）
//
// 鍵を本PoCに置く加入者は IMSI 00101 で始まるテスト用の番号を使う。provisioner の PLMN マップで
// 00102 を 01（aka-vecgen）にしてあれば、00102 で始まる加入者で aka-vecgen の側も確かめる。
// RADIUSクライアントは文書用のアドレス（198.51.100.0/24）を使う。作ったものは終わったら削除する。

// integrationOperator は操作者（本BFF は利用者のメールアドレスを渡す）。
const integrationOperator = "it-alice@example.com"

// akaPLMN は、aka-vecgen の側を確かめるときの PLMN（provisioner の PLMN マップで 01 にしておく）。
const akaPLMN = "00102"

// newIntegrationClients は provisioner のクライアントを返す（既存の BFF のテストと同じ形にするため、同じものを 2 つ返す）。
func newIntegrationClients(t *testing.T) (*Client, *Client) {
	t.Helper()
	url := os.Getenv("OSSBSS_TEST_PROVISIONER_URL")
	if url == "" {
		t.Skip("OSSBSS_TEST_PROVISIONER_URL is not set")
	}
	c, err := New(Options{
		BaseURL:        url,
		ClientCertFile: os.Getenv("OSSBSS_TEST_PROVISIONER_CLIENT_CERT"),
		ServerCertFile: os.Getenv("OSSBSS_TEST_PROVISIONER_SERVER_CERT"),
		Log:            provisionertest.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, c
}

// itCtx は、操作者と新しいトレースID を入れたコンテキストと、そのトレースID を返す。
func itCtx(t *testing.T) (context.Context, string) {
	id := trace.New()
	return trace.With(WithOperator(t.Context(), integrationOperator), id), id
}

func testIMSI(plmn string) string { return fmt.Sprintf("%s%010d", plmn, rand.Int64N(1e10)) }

func newKey() string { return fmt.Sprintf("it-%016x", rand.Uint64()) }

func cause(err error) string { return CauseOf(err) }

// cleanupSubscriber は、テストの失敗で残らないよう、終わったら加入者を消す（既にないなら何もしない）。
func cleanupSubscriber(t *testing.T, pv *Client, imsi string) {
	t.Cleanup(func() {
		ctx := WithOperator(context.WithoutCancel(t.Context()), integrationOperator)
		if err := pv.DeleteSubscriber(ctx, imsi, ""); err != nil && cause(err) != CauseUserNotFound {
			t.Logf("cleanup %s: %v", imsi, err)
		}
	})
}

func TestIntegrationStatus(t *testing.T) {
	_, prov := newIntegrationClients(t)
	st, err := prov.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !st.IsProvisioner() || st.Version == "" || st.StartedAt.IsZero() || !st.Downstreams.Prov.Configured || !st.Downstreams.Prov.Reachable ||
		st.Downstreams.Prov.NodeName == "" || !st.Valkey.Reachable || st.Operations == nil {
		t.Errorf("status = %+v", st)
	}
	if st.Downstreams.Aka.Configured && (!st.Downstreams.Aka.Reachable || !st.AVClient.Exists || !st.AVClient.Enabled) {
		t.Errorf("aka = %+v, av client = %+v", st.Downstreams.Aka, st.AVClient)
	}
}

func TestIntegrationSubscriberPoC(t *testing.T) {
	pv, prov := newIntegrationClients(t)
	imsi := testIMSI("00101")
	cleanupSubscriber(t, pv, imsi)
	ctx, traceID := itCtx(t)
	in := SubscriberCreate{IMSI: imsi, Ki: "465B5CE8B199B49FAA5F0A2EE238A6BC", OPc: "cd63cb71954a9f4e48a5994e37a02baf", AMF: "b9b9",
		Policy: PolicyPut{Default: "deny", Rules: []PolicyRule{{NASID: "*", AllowedSSIDs: []string{"CORP"}}}}}

	// 作成。置き場所は PLMN マップから決まる（00101 は poc）。
	key := newKey()
	sub, err := pv.CreateSubscriber(ctx, in, key)
	if err != nil {
		t.Fatal(err)
	}
	if sub.IMSI != imsi || sub.KeyStore != KeyStorePoC || sub.Key == nil || sub.Key.AMF != "b9b9" || sub.Key.SQN != "000000000000" ||
		sub.Policy == nil || sub.Policy.Default != "deny" || len(sub.Policy.Rules) != 1 || sub.Status != PolicyActive || len(sub.Issues) != 0 {
		t.Errorf("create = %+v", sub)
	}
	// 同じ Idempotency-Key で送り直すと、最初の結果が返る（二重に作らない）。
	if again, err := pv.CreateSubscriber(ctx, in, key); err != nil || again.IMSI != imsi {
		t.Errorf("replay = %+v, %v", again, err)
	}
	// キーなしで送ると、既にあるので 409（あった場所を conflicts に入れる）。
	_, err = pv.CreateSubscriber(ctx, in, "")
	if apiErr, ok := errors.AsType[*Error](err); !ok || apiErr.Status != 409 || cause(err) != CauseSubscriberExists ||
		!slices.Contains(apiErr.Problem.Conflicts, "poc") || !slices.Contains(apiErr.Problem.Conflicts, "policy") {
		t.Errorf("duplicate: %v (%+v)", err, apiErr)
	}

	// 取得と一覧。
	if got, err := pv.GetSubscriber(ctx, imsi); err != nil || got.Key == nil || got.Policy == nil || len(got.Issues) != 0 {
		t.Errorf("get = %+v, %v", got, err)
	}
	list, err := pv.ListSubscribers(ctx, ListParams{Prefix: imsi})
	if err != nil || len(list.Items) != 1 || list.Items[0].IMSI != imsi {
		t.Errorf("list = %+v, %v", list, err)
	}

	// 変更（鍵の属性とポリシー）。鍵の取得は中継（provapi のクライアントのまま）。
	upd, err := pv.UpdateSubscriber(ctx, imsi, SubscriberUpdate{AMF: "8000", SQN: "000000000040",
		Policy: &PolicyPut{Default: "allow", Rules: []PolicyRule{}}}, newKey())
	if err != nil || upd.Key.AMF != "8000" || upd.Key.SQN != "000000000040" || upd.Policy.Default != "allow" || len(upd.Policy.Rules) != 0 {
		t.Errorf("update = %+v, %v", upd, err)
	}

	// 停止・再開は認可ポリシーの中継（provapi のクライアントのまま）。加入者の status に出る。
	// 停止・再開には Idempotency-Key を付ける（設計概要 §10）。同じキーで送り直すと、最初の応答が返る。
	suspendKey := newKey()
	if p, err := prov.SetPolicyStatus(ctx, imsi, PolicySuspended, suspendKey); err != nil || p.Status != PolicySuspended {
		t.Errorf("suspend = %+v, %v", p, err)
	}
	if got, err := pv.GetSubscriber(ctx, imsi); err != nil || got.Status != PolicySuspended {
		t.Errorf("get after suspend = %+v, %v", got, err)
	}
	if list, err := pv.ListSubscribers(ctx, ListParams{Prefix: imsi}); err != nil || len(list.Items) != 1 || list.Items[0].Status != PolicySuspended {
		t.Errorf("list after suspend = %+v, %v", list, err)
	}
	// ポリシーを置き換えても停止のまま。
	if upd, err := pv.UpdateSubscriber(ctx, imsi, SubscriberUpdate{Policy: &PolicyPut{Default: "deny", Rules: []PolicyRule{}}}, newKey()); err != nil ||
		upd.Status != PolicySuspended {
		t.Errorf("update while suspended = %+v, %v", upd, err)
	}
	if p, err := prov.SetPolicyStatus(ctx, imsi, PolicySuspended, suspendKey); err != nil || p.Status != PolicySuspended {
		t.Errorf("suspend (replay) = %+v, %v", p, err)
	}
	// 同じキーで違う内容を送ると 422（IDEMPOTENCY_KEY_MISMATCH）。
	if _, err := prov.SetPolicyStatus(ctx, imsi, PolicyActive, suspendKey); CauseOf(err) != CauseIdempotencyKeyMismatch {
		t.Errorf("key mismatch: %v", err)
	}
	if p, err := prov.SetPolicyStatus(ctx, imsi, PolicyActive, newKey()); err != nil || p.Status != PolicyActive {
		t.Errorf("resume = %+v, %v", p, err)
	}
	if keys, err := prov.GetSubscriberKeys(ctx, imsi); err != nil || keys.Ki != "465b5ce8b199b49faa5f0a2ee238a6bc" {
		t.Errorf("keys = %+v, %v", keys, err)
	}
	// 本PoCの側だけの項目は、置き場所が poc なら断られる。
	plain := true
	if _, err := pv.UpdateSubscriber(ctx, imsi, SubscriberUpdate{AllowPlain: &plain}, ""); err == nil {
		t.Error("allowPlain for poc: want error")
	}

	// 削除（認可ポリシーも消える）。
	if err := pv.DeleteSubscriber(ctx, imsi, newKey()); err != nil {
		t.Fatal(err)
	}
	if _, err := pv.GetSubscriber(ctx, imsi); cause(err) != CauseUserNotFound {
		t.Errorf("get after delete: %v", err)
	}
	if _, err := prov.GetPolicy(ctx, imsi); cause(err) != CausePolicyNotFound {
		t.Errorf("policy after delete: %v", err)
	}

	// provisioner の監査ログに、操作者・管理クライアント・トレースID・操作の記録の ID が残る。
	logs, err := pv.ListAuditLogs(ctx, AuditLogParams{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range logs.Items {
		if e.TraceID == traceID && e.Action == "subscriber.create" {
			found = true
			if e.Operator != integrationOperator || e.MgmtClient == "" || e.OperationID == "" || e.Result != "completed" || e.Target != imsi {
				t.Errorf("audit = %+v", e)
			}
		}
	}
	if !found {
		t.Errorf("audit log for trace %s not found", traceID)
	}
	// provisioning-api の監査ログ（中継）にも同じトレースID で残る（管理クライアントは provisioner）。
	provLogs, err := pv.ListProvAuditLogs(ctx, AuditLogParams{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(provLogs.Items, func(e ProvAuditLogEntry) bool { return e.TraceID == traceID }); i < 0 ||
		provLogs.Items[i].Operator != integrationOperator {
		t.Errorf("prov audit log for trace %s not found", traceID)
	}
}

func TestIntegrationSubscriberAka(t *testing.T) {
	pv, _ := newIntegrationClients(t)
	st, err := pv.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Downstreams.Aka.Configured || !slices.Contains(st.PLMNMap, PLMNEntry{PLMN: akaPLMN, KeyStore: KeyStoreAKA}) {
		t.Skipf("provisioner does not map %s to aka-vecgen", akaPLMN)
	}
	imsi := testIMSI(akaPLMN)
	cleanupSubscriber(t, pv, imsi)
	ctx, traceID := itCtx(t)

	sub, err := pv.CreateSubscriber(ctx, SubscriberCreate{IMSI: imsi, Ki: "465b5ce8b199b49faa5f0a2ee238a6bc", OPc: "cd63cb71954a9f4e48a5994e37a02baf",
		Policy: PolicyPut{Default: "allow", Rules: []PolicyRule{}}}, "")
	if err != nil {
		t.Fatal(err)
	}
	// aka-vecgen の加入者は、vector-gateway の AVクライアントを許可する。SQN の増加タイプ等は aka-vecgen の既定。
	if sub.KeyStore != KeyStoreAKA || sub.Key == nil || sub.Key.SQNType != "inc32" || sub.Key.AllowPlain == nil || *sub.Key.AllowPlain ||
		!slices.Equal(sub.Key.AllowedClientIDs, []int64{st.AVClient.ID}) || len(sub.Issues) != 0 {
		t.Errorf("create = %+v (key %+v)", sub, sub.Key)
	}
	plain := true
	upd, err := pv.UpdateSubscriber(ctx, imsi, SubscriberUpdate{SQNType: "inc33", AllowPlain: &plain}, "")
	if err != nil || upd.Key.SQNType != "inc33" || !*upd.Key.AllowPlain {
		t.Errorf("update = %+v, %v", upd, err)
	}
	if err := pv.DeleteSubscriber(ctx, imsi, ""); err != nil {
		t.Fatal(err)
	}
	// aka-vecgen の監査ログ（中継）に同じトレースID で残る。
	akaLogs, err := pv.ListAkaAuditLogs(ctx, AuditLogParams{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(akaLogs.Items, func(e AkaAuditLogEntry) bool {
		return e.TraceID == traceID && e.Action == "subscriber.update" && e.Detail["sqnType"] != nil
	}) {
		t.Errorf("aka audit log for trace %s not found", traceID)
	}
}

func TestIntegrationOperations(t *testing.T) {
	pv, _ := newIntegrationClients(t)
	ctx, _ := itCtx(t)
	if _, err := pv.ListOperations(ctx, OpFailed, 10); err != nil {
		t.Errorf("list: %v", err)
	}
	const missing = "0199c8a2-0000-7000-8000-000000000000"
	if _, err := pv.GetOperation(ctx, missing); cause(err) != CauseOperationNotFound {
		t.Errorf("get missing: %v", err)
	}
	if _, err := pv.DismissOperation(ctx, missing, ""); cause(err) != CauseOperationNotFound {
		t.Errorf("dismiss missing: %v", err)
	}
}

// TestIntegrationRelay は、接続先を provisioner にした Client で、中継の部分（RADIUSクライアント・
// 認可ポリシー・セッション）がそのまま使えることを確かめる（BFF の画面はこのクライアントで中継を呼ぶ）。
func TestIntegrationRelay(t *testing.T) {
	_, prov := newIntegrationClients(t)
	ctx, _ := itCtx(t)

	var ip string
	for range 50 {
		cand := fmt.Sprintf("198.51.100.%d", 1+rand.IntN(254))
		if _, used, err := prov.FindRADIUSClientByIP(ctx, cand); err != nil {
			t.Fatal(err)
		} else if !used {
			ip = cand
			break
		}
	}
	if ip == "" {
		t.Fatal("no free test IP address")
	}
	rc, err := prov.CreateRADIUSClient(ctx, RADIUSClientCreate{IP: ip, Secret: "it-secret-1", Name: "it-ap", Vendor: "test"}, newKey())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		prov.DeleteRADIUSClient(WithOperator(context.WithoutCancel(t.Context()), integrationOperator), rc.ID, "")
	})
	if got, err := prov.GetRADIUSClient(ctx, rc.ID); err != nil || got.IP != ip {
		t.Errorf("get client = %+v, %v", got, err)
	}
	if secret, err := prov.GetRADIUSClientSecret(ctx, rc.ID); err != nil || secret != "it-secret-1" {
		t.Errorf("secret = %q, %v", secret, err)
	}
	name := "it-ap-2"
	if got, err := prov.UpdateRADIUSClient(ctx, rc.ID, RADIUSClientUpdate{Name: &name}, newKey()); err != nil || got.Name != name {
		t.Errorf("update client = %+v, %v", got, err)
	}
	if err := prov.DeleteRADIUSClient(ctx, rc.ID, newKey()); err != nil {
		t.Errorf("delete client: %v", err)
	}

	imsi := testIMSI("00101")
	t.Cleanup(func() {
		prov.DeletePolicy(WithOperator(context.WithoutCancel(t.Context()), integrationOperator), imsi, "")
	})
	if _, created, err := prov.PutPolicy(ctx, imsi, PolicyPut{Default: "deny", Rules: []PolicyRule{}}, newKey()); err != nil || !created {
		t.Errorf("put policy: created %v, %v", created, err)
	}
	if p, err := prov.GetPolicy(ctx, imsi); err != nil || p.Default != "deny" {
		t.Errorf("get policy = %+v, %v", p, err)
	}
	if err := prov.DeletePolicy(ctx, imsi, newKey()); err != nil {
		t.Errorf("delete policy: %v", err)
	}
	if _, err := prov.ListSessions(ctx, SessionParams{IMSI: imsi}); err != nil {
		t.Errorf("sessions: %v", err)
	}
}
