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

// 各操作が送る要求（メソッド、パス、クエリ、本文、ヘッダー）。
func TestRequests(t *testing.T) {
	c, rec := newClient(t)
	ctx := trace.With(provisioner.WithOperator(t.Context(), "alice@example.com"), "trace-1")
	pol := provisioner.PolicyPut{Default: provisioner.PolicyDeny, Rules: []provisioner.PolicyRule{}}

	for _, tc := range []struct {
		name                      string
		call                      func() error
		method, path, query, idem string
		ctype                     string
		body                      string // 本文の項目名（カンマ区切り。空なら本文なし）
	}{
		{"status", func() error { _, err := c.Status(ctx); return err }, "GET", "/admin/v1/status", "", "", "", ""},
		{"list subscribers", func() error {
			_, err := c.ListSubscribers(ctx, provisioner.ListParams{Prefix: "00101", Cursor: "001010000000001", Limit: 20})
			return err
		}, "GET", "/admin/v1/subscribers", "cursor=001010000000001&limit=20&prefix=00101", "", "", ""},
		{"get subscriber", func() error { _, err := c.GetSubscriber(ctx, "001010000000001"); return err },
			"GET", "/admin/v1/subscribers/001010000000001", "", "", "", ""},
		{"create subscriber", func() error {
			_, err := c.CreateSubscriber(ctx, provisioner.SubscriberCreate{IMSI: "001010000000001", Ki: "k", OPc: "o", Policy: pol}, "key-1")
			return err
		}, "POST", "/admin/v1/subscribers", "", "key-1", "application/json", "imsi,ki,opc,policy"},
		{"update subscriber", func() error {
			_, err := c.UpdateSubscriber(ctx, "001010000000001", provisioner.SubscriberUpdate{AMF: "8000"}, "key-2")
			return err
		}, "PATCH", "/admin/v1/subscribers/001010000000001", "", "key-2", "application/merge-patch+json", "amf"},
		{"delete subscriber", func() error { return c.DeleteSubscriber(ctx, "001010000000001", "key-3") },
			"DELETE", "/admin/v1/subscribers/001010000000001", "", "key-3", "", ""},
		{"keys", func() error { _, err := c.GetSubscriberKeys(ctx, "001010000000001"); return err },
			"GET", "/admin/v1/subscribers/001010000000001/keys", "", "", "", ""},
		{"list operations", func() error { _, err := c.ListOperations(ctx, provisioner.OpFailed, 10); return err },
			"GET", "/admin/v1/operations", "limit=10&status=failed", "", "", ""},
		{"retry", func() error { _, err := c.RetryOperation(ctx, "op-1", "key-4"); return err },
			"POST", "/admin/v1/operations/op-1/retry", "", "key-4", "", ""},
		{"dismiss", func() error { _, err := c.DismissOperation(ctx, "op-1", ""); return err },
			"POST", "/admin/v1/operations/op-1/dismiss", "", "", "", ""},
		{"list clients by ip", func() error { _, _, err := c.FindRADIUSClientByIP(ctx, "192.0.2.1"); return err },
			"GET", "/admin/v1/clients", "ip=192.0.2.1", "", "", ""},
		{"create client", func() error {
			_, err := c.CreateRADIUSClient(ctx, provisioner.RADIUSClientCreate{IP: "192.0.2.1", Secret: "s", Name: "ap"}, "key-5")
			return err
		}, "POST", "/admin/v1/clients", "", "key-5", "application/json", "ip,name,secret,vendor"},
		{"update client", func() error {
			name := "ap-2"
			_, err := c.UpdateRADIUSClient(ctx, 3, provisioner.RADIUSClientUpdate{Name: &name}, "key-6")
			return err
		}, "PATCH", "/admin/v1/clients/3", "", "key-6", "application/merge-patch+json", "name"},
		{"secret", func() error { _, err := c.GetRADIUSClientSecret(ctx, 3); return err },
			"GET", "/admin/v1/clients/3/secret", "", "", "", ""},
		{"put policy", func() error { _, _, err := c.PutPolicy(ctx, "001010000000001", pol, "key-7"); return err },
			"PUT", "/admin/v1/policies/001010000000001", "", "key-7", "application/json", "default,rules"},
		// 停止・再開にも Idempotency-Key を付ける（設計概要 §10、§17.3）。
		{"suspend", func() error {
			_, err := c.SetPolicyStatus(ctx, "001010000000001", provisioner.PolicySuspended, "key-8")
			return err
		}, "PUT", "/admin/v1/policies/001010000000001/status", "", "key-8", "application/json", "status"},
		{"sessions", func() error {
			_, err := c.ListSessions(ctx, provisioner.SessionParams{IMSI: "001010000000001", Limit: 1000})
			return err
		}, "GET", "/admin/v1/sessions", "imsi=001010000000001&limit=1000", "", "", ""},
		{"audit", func() error {
			_, err := c.ListAuditLogs(ctx, provisioner.AuditLogParams{Before: "1-0", Limit: 50})
			return err
		},
			"GET", "/admin/v1/audit-logs", "before=1-0&limit=50", "", "", ""},
		{"prov audit", func() error { _, err := c.ListProvAuditLogs(ctx, provisioner.AuditLogParams{}); return err },
			"GET", "/admin/v1/prov/audit-logs", "", "", "", ""},
		{"aka audit", func() error { _, err := c.ListAkaAuditLogs(ctx, provisioner.AuditLogParams{Limit: 5}); return err },
			"GET", "/admin/v1/aka/audit-logs", "limit=5", "", "", ""},
	} {
		if err := tc.call(); err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		got := rec.last(t)
		var keys []string
		for k := range got.body {
			keys = append(keys, k)
		}
		if got.method != tc.method || got.path != tc.path || got.rawQuery != tc.query || got.idemKey != tc.idem ||
			got.contentType != tc.ctype || got.operator != "alice@example.com" || got.traceID != "trace-1" || !sameSet(keys, tc.body) {
			t.Errorf("%s: got %+v", tc.name, got)
		}
	}
}

func sameSet(keys []string, want string) bool {
	if want == "" {
		return len(keys) == 0
	}
	ws := strings.Split(want, ",")
	if len(ws) != len(keys) {
		return false
	}
	for _, w := range ws {
		found := false
		for _, k := range keys {
			found = found || k == w
		}
		if !found {
			return false
		}
	}
	return true
}

// 書き込みと秘密の値の取得は、操作者がなければ送らない。形式の違う操作者も送らない。
// 読み取りは操作者がなくても送る。トレースID がなければ採番する。
func TestOperator(t *testing.T) {
	c, rec := newClient(t)
	ctx := t.Context()
	for name, err := range map[string]error{
		"create": func() error { _, err := c.CreateSubscriber(ctx, provisioner.SubscriberCreate{}, ""); return err }(),
		"keys":   func() error { _, err := c.GetSubscriberKeys(ctx, "001010000000001"); return err }(),
		"secret": func() error { _, err := c.GetRADIUSClientSecret(ctx, 1); return err }(),
		"suspend": func() error {
			_, err := c.SetPolicyStatus(ctx, "001010000000001", provisioner.PolicySuspended, "")
			return err
		}(),
		"dismiss": func() error { _, err := c.DismissOperation(ctx, "op-1", ""); return err }(),
	} {
		if !errors.Is(err, provisioner.ErrNoOperator) || provisioner.IsUnavailable(err) {
			t.Errorf("%s without operator: %v", name, err)
		}
	}
	if _, err := c.GetSubscriberKeys(provisioner.WithOperator(ctx, "alice+1@example.com"), "001010000000001"); !errors.Is(err, provisioner.ErrInvalidOperator) {
		t.Errorf("invalid operator: %v", err)
	}
	if len(rec.reqs) != 0 {
		t.Fatalf("sent %d requests", len(rec.reqs))
	}
	if _, err := c.GetSubscriber(ctx, "001010000000001"); err != nil {
		t.Fatal(err)
	}
	if got := rec.last(t); got.operator != "" || len(got.traceID) != 32 {
		t.Errorf("read = %+v", got)
	}
}

// パスの値は 1 つのセグメントとしてエスケープし、空・"."・".." は送らない。
func TestPathEscape(t *testing.T) {
	c, rec := newClient(t)
	ctx := t.Context()
	if _, err := c.GetSubscriber(ctx, "1/secret"); err != nil {
		t.Fatal(err)
	}
	if got := rec.last(t).path; got != "/admin/v1/subscribers/1%2Fsecret" {
		t.Errorf("path = %q", got)
	}
	n := len(rec.reqs)
	for _, v := range []string{"", ".", ".."} {
		if _, err := c.GetSubscriber(ctx, v); !errors.Is(err, provisioner.ErrInvalidPath) || provisioner.IsUnavailable(err) {
			t.Errorf("%q: %v", v, err)
		}
	}
	if len(rec.reqs) != n {
		t.Error("invalid path was sent")
	}
}

// エラーの応答（ProblemDetails と provisioner の拡張項目）。
func TestErrors(t *testing.T) {
	c, rec := newClient(t)
	ctx := provisioner.WithOperator(t.Context(), "alice@example.com")

	rec.status = 409
	rec.body = `{"title":"Conflict","status":409,"cause":"OPERATION_UNRESOLVED","detail":"retry or dismiss","operationId":"op-9"}`
	_, err := c.SetPolicyStatus(ctx, "001010000000001", provisioner.PolicySuspended, "k")
	apiErr, ok := errors.AsType[*provisioner.Error](err)
	if !ok || apiErr.Status != 409 || apiErr.Problem.OperationID != "op-9" || provisioner.CauseOf(err) != provisioner.CauseOperationUnresolved ||
		provisioner.IsUnavailable(err) || !strings.Contains(err.Error(), "provisioner PUT /admin/v1/policies/001010000000001/status: 409 Conflict (OPERATION_UNRESOLVED)") {
		t.Errorf("409 = %v", err)
	}

	rec.status = 503
	rec.body = `{"status":503,"cause":"DOWNSTREAM_UNAVAILABLE","downstream":"aka"}`
	_, err = c.GetSubscriber(ctx, "001020000000001")
	if apiErr, ok := errors.AsType[*provisioner.Error](err); !ok || apiErr.Problem.Downstream != "aka" || provisioner.CauseOf(err) != provisioner.CauseDownstreamUnavailable {
		t.Errorf("503 = %v", err)
	}

	rec.status = 400
	rec.body = `{"status":400,"cause":"MANDATORY_IE_INCORRECT","invalidParams":[{"param":"policy.rules[0].nasId","reason":"x"}]}`
	_, err = c.CreateSubscriber(ctx, provisioner.SubscriberCreate{}, "")
	if apiErr, ok := errors.AsType[*provisioner.Error](err); !ok || len(apiErr.Problem.InvalidParams) != 1 || apiErr.Problem.InvalidParams[0].Param != "policy.rules[0].nasId" {
		t.Errorf("400 = %v", err)
	}

	// ProblemDetails でない応答も、ステータスで扱える。
	rec.status = 502
	rec.body = "bad gateway"
	_, err = c.Status(ctx)
	if apiErr, ok := errors.AsType[*provisioner.Error](err); !ok || apiErr.Status != 502 || apiErr.Problem.Cause != "" {
		t.Errorf("502 = %v", err)
	}
}

// PUT /policies は、作成（201）と置き換え（200）を区別して返す。
func TestPutPolicyCreated(t *testing.T) {
	c, rec := newClient(t)
	ctx := provisioner.WithOperator(t.Context(), "alice@example.com")
	rec.status = 201
	rec.body = `{"imsi":"001010000000001","default":"deny","rules":[],"status":"active"}`
	p, created, err := c.PutPolicy(ctx, "001010000000001", provisioner.PolicyPut{Default: "deny"}, "")
	if err != nil || !created || p.Status != provisioner.PolicyActive {
		t.Errorf("create = %+v, %v, %v", p, created, err)
	}
	rec.status = 200
	if _, created, err := c.PutPolicy(ctx, "001010000000001", provisioner.PolicyPut{Default: "deny"}, ""); err != nil || created {
		t.Errorf("replace = %v, %v", created, err)
	}
}

// 接続できないとき: IsUnavailable と、原因の見当。
func TestUnavailable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	_, srv := provisionertest.NewServer(t, func(http.ResponseWriter, *http.Request) {}, provisioner.Options{})
	c2, err := provisioner.New(provisioner.Options{BaseURL: "https://" + addr + "/admin/v1",
		ClientCertFile: srv.ClientCertFile, ServerCertFile: srv.ServerCertFile, Log: provisionertest.Discard})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c2.Status(t.Context())
	if !provisioner.IsUnavailable(err) || !strings.Contains(provisioner.Diagnose(err), "provisioner に接続できません") {
		t.Errorf("err = %v, hint %q", err, provisioner.Diagnose(err))
	}
}

// 別のサーバー証明書（設定したものと違う）は受け付けない。
func TestServerCertificateMismatch(t *testing.T) {
	_, srv1 := provisionertest.NewServer(t, func(http.ResponseWriter, *http.Request) {}, provisioner.Options{})
	_, srv2 := provisionertest.NewServer(t, func(http.ResponseWriter, *http.Request) {}, provisioner.Options{})
	c, err := provisioner.New(provisioner.Options{BaseURL: srv2.URL + "/admin/v1",
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
		"http url":       {BaseURL: "http://x/admin/v1", ClientCertFile: srv.ClientCertFile, ServerCertFile: srv.ServerCertFile},
		"no client cert": {BaseURL: "https://x/admin/v1", ClientCertFile: "/nonexistent", ServerCertFile: srv.ServerCertFile},
		"no server cert": {BaseURL: "https://x/admin/v1", ClientCertFile: srv.ClientCertFile, ServerCertFile: "/nonexistent"},
	} {
		if _, err := provisioner.New(opts); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}
