package provisioner_test

import (
	"encoding/json/v2"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner/provisionertest"
	"github.com/oyaguma3/aka-ossbss/internal/trace"
)

// captured は偽物の provisioner が受け取った要求。
type captured struct {
	method, path, rawQuery, contentType string
	operator, traceID, idemKey          string
	body                                map[string]any
	raw                                 string
}

// recorder は要求を記録し、決めた応答を返す偽物の provisioner。
type recorder struct {
	mu   sync.Mutex
	reqs []captured
	// status と body は返す応答（status が 0 なら 200、body が空なら {}）。
	status int
	body   string
}

func (rec *recorder) handle(w http.ResponseWriter, r *http.Request) {
	c := captured{method: r.Method, path: r.URL.EscapedPath(), rawQuery: r.URL.RawQuery, contentType: r.Header.Get("Content-Type"),
		operator: r.Header.Get("X-Operator-Id"), traceID: r.Header.Get("X-Trace-ID"), idemKey: r.Header.Get("Idempotency-Key")}
	if b, _ := io.ReadAll(r.Body); len(b) > 0 {
		c.raw = string(b)
		json.Unmarshal(b, &c.body)
	}
	rec.mu.Lock()
	rec.reqs = append(rec.reqs, c)
	status, body := rec.status, rec.body
	rec.mu.Unlock()
	if body == "" {
		body = "{}"
	}
	w.Header().Set("X-Trace-ID", c.traceID)
	if status >= 400 {
		w.Header().Set("Content-Type", "application/problem+json")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(max(status, 200))
	io.WriteString(w, body)
}

func (rec *recorder) last(t *testing.T) captured {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.reqs) == 0 {
		t.Fatal("no request")
	}
	return rec.reqs[len(rec.reqs)-1]
}

func newClient(t *testing.T) (*provisioner.Client, *recorder) {
	t.Helper()
	rec := &recorder{}
	c, _ := provisionertest.NewServer(t, rec.handle, provisioner.Options{})
	return c, rec
}

// 各口が送る要求（メソッド、パス、クエリ、本文のバイト列、ヘッダー）。
func TestRequests(t *testing.T) {
	c, rec := newClient(t)
	ctx := trace.With(provisioner.WithOperator(t.Context(), "alice@example.com"), "trace-1")
	const imsi = "001010000000001"
	const key = "KEY1"

	for _, tc := range []struct {
		name                      string
		call                      func() error
		method, path, query, idem string
		ctype                     string
		body                      string // 本文のバイト列（空なら本文なし）
	}{
		{"status", func() error { _, err := c.Status(ctx); return err }, "GET", "/admin/v2/status", "", "", "", ""},
		{"vecgen get", func() error { _, err := c.VecgenSubscriber(ctx, imsi); return err },
			"GET", "/admin/v2/vecgen/subscribers/" + imsi, "", "", "", ""},
		{"vecgen batch", func() error { _, err := c.VecgenBatchGet(ctx, []string{imsi, "001010000000002"}); return err },
			"POST", "/admin/v2/vecgen/subscribers/batch-get", "", "", "application/json", `{"imsis":["001010000000001","001010000000002"]}`},
		{"vecgen create", func() error {
			_, err := c.CreateVecgenSubscriber(ctx, provisioner.VecgenSubscriberCreate{IMSI: imsi, Ki: "k", OPc: "o", State: "active",
				SQNType: "inc32"}, key)
			return err
		}, "POST", "/admin/v2/vecgen/subscribers", "", key, "application/json",
			`{"imsi":"001010000000001","ki":"k","opc":"o","state":"active","sqnType":"inc32","allowedClientIds":[]}`},
		{"vecgen update", func() error {
			_, err := c.UpdateVecgenSubscriber(ctx, imsi, provisioner.VecgenSubscriberUpdate{AMF: "9000", AllowedClientIDs: new([]int64{})}, key)
			return err
		}, "PATCH", "/admin/v2/vecgen/subscribers/" + imsi, "", key, "application/merge-patch+json", `{"amf":"9000","allowedClientIds":[]}`},
		{"vecgen update without clients", func() error {
			_, err := c.UpdateVecgenSubscriber(ctx, imsi, provisioner.VecgenSubscriberUpdate{SQN: "000000000040"}, key)
			return err
		}, "PATCH", "/admin/v2/vecgen/subscribers/" + imsi, "", key, "application/merge-patch+json", `{"sqn":"000000000040"}`},
		{"vecgen state", func() error {
			_, err := c.SetState(ctx, provisioner.Vecgen, imsi, provisioner.StateChange{State: "inactive", Reason: "未払い"}, key)
			return err
		}, "PUT", "/admin/v2/vecgen/subscribers/" + imsi + "/state", "", key, "application/json", `{"state":"inactive","reason":"未払い"}`},
		{"radius state without reason", func() error {
			_, err := c.SetState(ctx, provisioner.Radius, imsi, provisioner.StateChange{State: "active"}, key)
			return err
		}, "PUT", "/admin/v2/radius/subscribers/" + imsi + "/state", "", key, "application/json", `{"state":"active"}`},
		{"radius reinstate", func() error { _, err := c.Reinstate(ctx, provisioner.Radius, imsi, "誤り", key); return err },
			"POST", "/admin/v2/radius/subscribers/" + imsi + "/reinstate", "", key, "application/json", `{"reason":"誤り"}`},
		{"vecgen delete", func() error { return c.DeleteSubscriber(ctx, provisioner.Vecgen, imsi, key) },
			"DELETE", "/admin/v2/vecgen/subscribers/" + imsi, "", key, "", ""},
		{"vecgen clients", func() error { _, err := c.VecgenClients(ctx); return err }, "GET", "/admin/v2/vecgen/clients", "", "", "", ""},
		{"radius get", func() error { _, err := c.RadiusSubscriber(ctx, imsi); return err },
			"GET", "/admin/v2/radius/subscribers/" + imsi, "", "", "", ""},
		{"radius batch", func() error { _, err := c.RadiusBatchGet(ctx, []string{imsi}); return err },
			"POST", "/admin/v2/radius/subscribers/batch-get", "", "", "application/json", `{"imsis":["001010000000001"]}`},
		{"radius create", func() error {
			_, err := c.CreateRadiusSubscriber(ctx, provisioner.RadiusSubscriberCreate{IMSI: imsi, State: "active", SourceID: "vecgen",
				Rules: []provisioner.Rule{{TenantID: 10, NASID: "*", SSIDs: []string{"*"}, VLANID: 100}}}, key)
			return err
		}, "POST", "/admin/v2/radius/subscribers", "", key, "application/json",
			`{"imsi":"001010000000001","state":"active","sourceId":"vecgen","rules":[{"tenantId":10,"nasId":"*","ssids":["*"],"vlanId":100}]}`},
		{"radius update rules", func() error {
			_, err := c.UpdateRadiusSubscriber(ctx, imsi, provisioner.RadiusSubscriberUpdate{
				Rules: []provisioner.Rule{{TenantID: 10, NASID: "ap", SSIDs: []string{"corp"}, SessionTimeout: 60}}}, key)
			return err
		}, "PATCH", "/admin/v2/radius/subscribers/" + imsi, "", key, "application/merge-patch+json",
			`{"rules":[{"tenantId":10,"nasId":"ap","ssids":["corp"],"sessionTimeout":60}]}`},
		{"radius update source", func() error {
			_, err := c.UpdateRadiusSubscriber(ctx, imsi, provisioner.RadiusSubscriberUpdate{SourceID: "other"}, key)
			return err
		}, "PATCH", "/admin/v2/radius/subscribers/" + imsi, "", key, "application/merge-patch+json", `{"sourceId":"other"}`},
		{"radius sources", func() error { _, err := c.RadiusSources(ctx); return err }, "GET", "/admin/v2/radius/sources", "", "", "", ""},
		{"sessions by tenant", func() error {
			_, err := c.Sessions(ctx, provisioner.SessionParams{TenantID: 42, Limit: 100})
			return err
		},
			"GET", "/admin/v2/radius/sessions", "limit=100&tenantId=42", "", "", ""},
		{"sessions by imsi", func() error {
			_, err := c.Sessions(ctx, provisioner.SessionParams{IMSIs: []string{imsi, "001010000000002"}, Cursor: "5"})
			return err
		}, "GET", "/admin/v2/radius/sessions", "cursor=5&imsi=001010000000001%2C001010000000002", "", "", ""},
		{"provisioner audit", func() error {
			_, err := c.AuditLogs(ctx, provisioner.AuditProvisioner, provisioner.AuditLogParams{Before: "1-0", Limit: 50})
			return err
		}, "GET", "/admin/v2/audit-logs", "before=1-0&limit=50", "", "", ""},
		{"radius audit", func() error {
			_, err := c.AuditLogs(ctx, provisioner.AuditRadius, provisioner.AuditLogParams{})
			return err
		},
			"GET", "/admin/v2/radius/audit-logs", "", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); err != nil {
				t.Fatal(err)
			}
			got := rec.last(t)
			if got.method != tc.method || got.path != tc.path || got.rawQuery != tc.query || got.idemKey != tc.idem ||
				got.contentType != tc.ctype || got.raw != tc.body {
				t.Errorf("request = %s %s?%s idem=%q ctype=%q body=%s\nwant      %s %s?%s idem=%q ctype=%q body=%s",
					got.method, got.path, got.rawQuery, got.idemKey, got.contentType, got.raw,
					tc.method, tc.path, tc.query, tc.idem, tc.ctype, tc.body)
			}
			if got.operator != "alice@example.com" || got.traceID != "trace-1" {
				t.Errorf("operator = %q, trace = %q", got.operator, got.traceID)
			}
		})
	}
}

// 書き込みは、操作者と Idempotency-Key がなければ送らない。読み出しは操作者がなくても送る。
func TestWriteRequiresOperatorAndKey(t *testing.T) {
	c, rec := newClient(t)
	noOp := t.Context()
	withOp := provisioner.WithOperator(t.Context(), "alice@example.com")
	if _, err := c.SetState(noOp, provisioner.Vecgen, "001010000000001", provisioner.StateChange{State: "active"}, "K"); !errors.Is(err, provisioner.ErrNoOperator) {
		t.Errorf("no operator = %v", err)
	}
	if _, err := c.SetState(withOp, provisioner.Vecgen, "001010000000001", provisioner.StateChange{State: "active"}, ""); !errors.Is(err, provisioner.ErrNoIdempotencyKey) {
		t.Errorf("no key = %v", err)
	}
	if err := c.DeleteSubscriber(withOp, provisioner.Radius, "001010000000001", "bad key"); !errors.Is(err, provisioner.ErrNoIdempotencyKey) {
		t.Errorf("bad key = %v", err)
	}
	if _, err := c.SetState(provisioner.WithOperator(t.Context(), "a b"), provisioner.Vecgen, "1", provisioner.StateChange{}, "K"); !errors.Is(err, provisioner.ErrInvalidOperator) {
		t.Errorf("bad operator = %v", err)
	}
	if _, err := c.VecgenSubscriber(withOp, ".."); !errors.Is(err, provisioner.ErrInvalidPath) {
		t.Errorf("bad path = %v", err)
	}
	if len(rec.reqs) != 0 {
		t.Errorf("sent %d requests", len(rec.reqs))
	}
	// 読み出しは操作者がなくても送る（トレースID は採番する）。
	if _, err := c.RadiusSources(noOp); err != nil {
		t.Fatal(err)
	}
	if got := rec.last(t); got.operator != "" || got.traceID == "" {
		t.Errorf("read = %+v", got)
	}
	// パスの値は 1 つの要素としてエスケープする。
	if _, err := c.VecgenSubscriber(noOp, "1/keys"); err != nil {
		t.Fatal(err)
	}
	if got := rec.last(t); got.path != "/admin/v2/vecgen/subscribers/1%2Fkeys" {
		t.Errorf("escaped path = %q", got.path)
	}
}

// 応答の読み取り（加入者、一括取得、状態、セッション、監査ログ）。
func TestResponses(t *testing.T) {
	c, rec := newClient(t)
	ctx := provisioner.WithOperator(t.Context(), "alice@example.com")

	rec.body = `{"imsi":"001010000000001","state":"active","stateChangedAt":"2026-10-30T01:00:00Z","sqn":"000000000020","amf":"8000",
		"sqnType":"inc32","allowedClientIds":[1,2],"createdAt":"2026-10-30T01:00:00Z","updatedAt":"2026-10-30T01:00:00Z"}`
	v, err := c.VecgenSubscriber(ctx, "001010000000001")
	if err != nil || v.State != "active" || v.SQN != "000000000020" || len(v.AllowedClientIDs) != 2 || v.CreatedAt.IsZero() {
		t.Errorf("vecgen = %+v, %v", v, err)
	}
	rec.body = `{"items":[{"imsi":"001010000000001","state":"inactive","sourceId":"vecgen","rules":[{"tenantId":10,"nasId":"*","ssids":["*"]}]}],"notFound":["001010000000002"]}`
	b, err := c.RadiusBatchGet(ctx, []string{"001010000000001", "001010000000002"})
	if err != nil || len(b.Items) != 1 || b.Items[0].Rules[0].TenantID != 10 || b.NotFound[0] != "001010000000002" {
		t.Errorf("batch = %+v, %v", b, err)
	}
	rec.body = `{"imsi":"001010000000001","state":"terminated"}`
	if s, err := c.SetState(ctx, provisioner.Radius, "001010000000001", provisioner.StateChange{State: "terminated"}, "K"); err != nil || s != "terminated" {
		t.Errorf("state = %q, %v", s, err)
	}
	rec.body = `{"items":[{"id":"c1","imsi":"001010000000001","tenantId":42,"nasIp":"10.0.0.1","nasIdentifier":"ap","seconds":60,
		"inOctets":1,"outOctets":2,"expiresAt":"2026-10-31T00:00:00Z"}],"nextCursor":"100"}`
	if l, err := c.Sessions(ctx, provisioner.SessionParams{TenantID: 42}); err != nil || len(l.Items) != 1 || l.Items[0].TenantID != 42 || l.NextCursor != "100" {
		t.Errorf("sessions = %+v, %v", l, err)
	}
	rec.body = `{"items":[{"id":"1-0","time":"2026-10-30T01:00:00Z","operator":"a","mgmtClient":"ossbss","action":"vecgen.subscriber.create",
		"target":"001010000000001","detail":{"downstream":"vecgen"},"traceId":"t"}],"nextBefore":"1-0"}`
	if l, err := c.AuditLogs(ctx, provisioner.AuditProvisioner, provisioner.AuditLogParams{}); err != nil || len(l.Items) != 1 ||
		l.Items[0].Detail["downstream"] != "vecgen" || l.NextBefore != "1-0" {
		t.Errorf("audit = %+v, %v", l, err)
	}
	// 削除（204）は本文を読まない。
	rec.status, rec.body = 204, ""
	if err := c.DeleteSubscriber(ctx, provisioner.Vecgen, "001010000000001", "K"); err != nil {
		t.Errorf("delete = %v", err)
	}
}

func TestStatus(t *testing.T) {
	c, rec := newClient(t)
	rec.body = `{"version":"2.0.0","startedAt":"2026-10-30T00:00:00Z","valkey":{"reachable":true},"adminClients":["ossbss"],
		"clientCertificate":{"subject":"CN=provisioner"},"serverCertificate":{"subject":"CN=aka-provisioner","notAfter":"2028-01-01T00:00:00Z"},
		"downstreams":[
			{"name":"vecgen","url":"https://vecgen","serverCertificate":{"subject":"CN=v"},"reachable":true,
			 "status":{"version":"0.5.0","subscriberCount":3,"clientCount":1,"avApiContractVersion":"1.0.0","kekId":"x"}},
			{"name":"radius","url":"https://radius","serverCertificate":{"subject":"CN=r"},"reachable":false,"error":"dial","hint":"届きません"}]}`
	st, err := c.Status(t.Context())
	if err != nil || !st.IsProvisioner() || st.Version != "2.0.0" || !st.Valkey.Reachable || len(st.Downstreams) != 2 ||
		st.ServerCertificate.NotAfter.IsZero() {
		t.Fatalf("status = %+v, %v", st, err)
	}
	n, ok := st.Downstreams[0].Node()
	if !ok || n.Version != "0.5.0" || n.SubscriberCount != 3 || n.AVAPIContractVersion != "1.0.0" || n.SessionCount != nil {
		t.Errorf("vecgen node = %+v, %v", n, ok)
	}
	if _, ok := st.Downstreams[1].Node(); ok || st.Downstreams[1].Hint != "届きません" {
		t.Errorf("radius = %+v", st.Downstreams[1])
	}
	// 1.x の provisioner（下流がオブジェクト）は読めない。
	rec.body = `{"version":"0.3.0","downstreams":{"prov":{}}}`
	if _, err := c.Status(t.Context()); err == nil {
		t.Error("v1 status: want error")
	}
}

// 誤りの応答と、結果が分からないかの判定（段階 3-1 の W7）。
func TestErrors(t *testing.T) {
	c, rec := newClient(t)
	ctx := provisioner.WithOperator(t.Context(), "alice@example.com")
	for _, tc := range []struct {
		status  int
		body    string
		cause   string
		unknown bool
	}{
		{404, `{"status":404,"cause":"USER_NOT_FOUND","downstream":"vecgen"}`, "USER_NOT_FOUND", false},
		{409, `{"status":409,"cause":"SUBSCRIBER_ACTIVE","downstream":"radius"}`, "SUBSCRIBER_ACTIVE", false},
		{400, `{"status":400,"cause":"MANDATORY_IE_INCORRECT","invalidParams":[{"param":"rules[0].ssids"}]}`, "MANDATORY_IE_INCORRECT", false},
		{503, `{"status":503,"cause":"DOWNSTREAM_UNAVAILABLE","downstream":"radius"}`, "DOWNSTREAM_UNAVAILABLE", true},
		{502, `{"status":502,"cause":"DOWNSTREAM_ERROR","downstream":"vecgen","downstreamStatus":500}`, "DOWNSTREAM_ERROR", true},
		{500, `{"status":500,"cause":"SYSTEM_FAILURE","downstream":"vecgen"}`, "SYSTEM_FAILURE", true},
	} {
		rec.status, rec.body = tc.status, tc.body
		_, err := c.SetState(ctx, provisioner.Vecgen, "001010000000001", provisioner.StateChange{State: "inactive"}, "K")
		e, ok := errors.AsType[*provisioner.Error](err)
		if !ok || e.Status != tc.status || provisioner.CauseOf(err) != tc.cause || provisioner.StatusOf(err) != tc.status {
			t.Errorf("%d: err = %v", tc.status, err)
			continue
		}
		if provisioner.ResultUnknown(err) != tc.unknown || provisioner.IsUnavailable(err) {
			t.Errorf("%d: unknown = %v, unavailable = %v", tc.status, provisioner.ResultUnknown(err), provisioner.IsUnavailable(err))
		}
		if tc.status == 400 && (len(e.Problem.InvalidParams) != 1 || e.Problem.InvalidParams[0].Param != "rules[0].ssids") {
			t.Errorf("invalid params = %+v", e.Problem)
		}
		if tc.status == 502 && (e.Problem.Downstream != "vecgen" || e.Problem.DownstreamStatus != 500) {
			t.Errorf("downstream = %+v", e.Problem)
		}
	}
	// 呼び出し側の誤りは、結果が分からないものではない（送っていない）。
	if provisioner.ResultUnknown(provisioner.ErrNoIdempotencyKey) || provisioner.ResultUnknown(nil) {
		t.Error("caller errors are not unknown results")
	}
}

// 接続できないとき: IsUnavailable と ResultUnknown と、原因の見当。
func TestUnavailable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	// 先に閉じると、下のサーバーが同じポートを取って応答してしまうことがあるので、起動してから閉じる。
	_, srv := provisionertest.NewServer(t, func(http.ResponseWriter, *http.Request) {}, provisioner.Options{})
	ln.Close()
	c2, err := provisioner.New(provisioner.Options{BaseURL: "https://" + addr + "/admin/v2",
		ClientCertFile: srv.ClientCertFile, ServerCertFile: srv.ServerCertFile, Log: provisionertest.Discard})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c2.Status(t.Context())
	if !provisioner.IsUnavailable(err) || !provisioner.ResultUnknown(err) || !strings.Contains(provisioner.Diagnose(err), "provisioner に接続できません") {
		t.Errorf("err = %v, hint %q", err, provisioner.Diagnose(err))
	}
}

// 別のサーバー証明書（設定したものと違う）は受け付けない。
func TestServerCertificateMismatch(t *testing.T) {
	_, srv1 := provisionertest.NewServer(t, func(http.ResponseWriter, *http.Request) {}, provisioner.Options{})
	_, srv2 := provisionertest.NewServer(t, func(http.ResponseWriter, *http.Request) {}, provisioner.Options{})
	c, err := provisioner.New(provisioner.Options{BaseURL: srv2.URL + "/admin/v2",
		ClientCertFile: srv1.ClientCertFile, ServerCertFile: srv1.ServerCertFile, Log: provisionertest.Discard})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Status(t.Context())
	if !provisioner.IsUnavailable(err) || !strings.Contains(provisioner.Diagnose(err), "OSSBSS_PROVISIONER_SERVER_CERT") {
		t.Errorf("err = %v, hint %q", err, provisioner.Diagnose(err))
	}
}

func TestNewErrors(t *testing.T) {
	_, srv := provisionertest.NewServer(t, func(http.ResponseWriter, *http.Request) {}, provisioner.Options{})
	for name, opts := range map[string]provisioner.Options{
		"http url":       {BaseURL: "http://x/admin/v2", ClientCertFile: srv.ClientCertFile, ServerCertFile: srv.ServerCertFile},
		"no client cert": {BaseURL: "https://x/admin/v2", ClientCertFile: "/nonexistent", ServerCertFile: srv.ServerCertFile},
		"no server cert": {BaseURL: "https://x/admin/v2", ClientCertFile: srv.ClientCertFile, ServerCertFile: "/nonexistent"},
	} {
		if _, err := provisioner.New(opts); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}
