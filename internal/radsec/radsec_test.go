package radsec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oyaguma3/aka-ossbss/internal/audit"
	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// memStore はメモリーの DB。配置の順は db の placementOrder と同じにしておく。
type memStore struct {
	edges    []db.Edge
	tenants  []db.TenantRadsec
	places   []db.Placement
	aps      map[int64][]db.AP
	brought  []db.BroughtCA
	released []db.TenantRadsec
	apCerts  []db.ExpiringAPCert
	audit    []db.AuditEntry
	writes   int
}

func (s *memStore) InsertAudit(_ context.Context, e db.AuditEntry) (int64, time.Time, error) {
	s.audit = append(s.audit, e)
	return int64(len(s.audit)), time.Now(), nil
}
func (s *memStore) Edges(context.Context) ([]db.Edge, error) { return slices.Clone(s.edges), nil }
func (s *memStore) RadsecOverviews(context.Context) ([]db.RadsecOverview, error) {
	var out []db.RadsecOverview
	for _, t := range s.tenants {
		o := db.RadsecOverview{TenantRadsec: t}
		for _, p := range s.places {
			if p.TenantID == t.TenantID {
				p.SupersededSerials = slices.Clone(p.SupersededSerials)
				o.Placements = append(o.Placements, p)
			}
		}
		out = append(out, o)
	}
	return out, nil
}
func (s *memStore) APs(_ context.Context, t int64) ([]db.AP, error) { return s.aps[t], nil }
func (s *memStore) BroughtCAs(_ context.Context, t int64) ([]db.BroughtCA, error) {
	var out []db.BroughtCA
	for _, b := range s.brought {
		if b.TenantID == t {
			out = append(out, b)
		}
	}
	return out, nil
}
func (s *memStore) place(id int64) *db.Placement {
	i := slices.IndexFunc(s.places, func(p db.Placement) bool { return p.ID == id })
	return &s.places[i]
}
func (s *memStore) SetPlacementApply(_ context.Context, id, applied, failed int64, result, errText string) error {
	p := s.place(id)
	p.AppliedRevision, p.FailedRevision, p.ApplyResult, p.ApplyError = applied, failed, result, errText
	s.writes++
	return nil
}
func (s *memStore) TouchPlacements(context.Context, string) error { return nil }
func (s *memStore) MarkPlacementRemoved(_ context.Context, id int64) error {
	s.places = slices.DeleteFunc(s.places, func(p db.Placement) bool { return p.ID == id })
	return nil
}
func (s *memStore) RecordPlacementCert(_ context.Context, id int64, kind, serial string, notAfter time.Time) error {
	p := s.place(id)
	if kind == db.CertKindProbe {
		p.ProbeSerial, p.ProbeNotAfter = serial, notAfter
	} else {
		p.ServerSerial, p.ServerNotAfter = serial, notAfter
	}
	return nil
}
func (s *memStore) PlacementCertPlaced(_ context.Context, id int64, kind, serial string, notAfter time.Time) error {
	p := s.place(id)
	cur := &p.ServerSerial
	at := &p.ServerNotAfter
	if kind == db.CertKindProbe {
		cur, at = &p.ProbeSerial, &p.ProbeNotAfter
	}
	if *cur != "" && *cur != serial && !slices.Contains(p.SupersededSerials, *cur) {
		p.SupersededSerials = append(p.SupersededSerials, *cur)
	}
	*cur, *at = serial, notAfter
	p.CertGeneration++
	p.CertApplyNeeded = true
	return nil
}
func (s *memStore) BumpCertGeneration(_ context.Context, id int64) error {
	s.place(id).CertGeneration++
	return nil
}
func (s *memStore) ClearRenew(_ context.Context, id int64) error {
	s.place(id).RenewRequested = false
	return nil
}
func (s *memStore) FinishCertApply(_ context.Context, id int64, remaining []string) error {
	p := s.place(id)
	p.CertApplyNeeded, p.SupersededSerials = false, remaining
	return nil
}
func (s *memStore) ClearRetry(_ context.Context, id int64) error {
	p := s.place(id)
	p.RetryRequested, p.FailedRevision = false, 0
	return nil
}
func (s *memStore) ClearCRLDirty(_ context.Context, tenant, revision int64) error {
	for i := range s.tenants {
		if s.tenants[i].TenantID == tenant && s.tenants[i].Revision == revision {
			s.tenants[i].CRLDirty = false
		}
	}
	return nil
}
func (s *memStore) RemovingBroughtCAs(context.Context) ([]db.BroughtCA, error) {
	var out []db.BroughtCA
	for _, b := range s.brought {
		if b.Removing() {
			out = append(out, b)
		}
	}
	return out, nil
}
func (s *memStore) FinishRemoveBrought(_ context.Context, caID string) error {
	s.brought = slices.DeleteFunc(s.brought, func(b db.BroughtCA) bool { return b.CAID == caID })
	return nil
}
func (s *memStore) RadsecCADisablePending(context.Context) ([]db.TenantRadsec, error) {
	return slices.Clone(s.released), nil
}
func (s *memStore) MarkRadsecCADisabled(_ context.Context, tenant int64) error {
	s.released = slices.DeleteFunc(s.released, func(t db.TenantRadsec) bool { return t.TenantID == tenant })
	return nil
}
func (s *memStore) MarkRenewals(_ context.Context, before time.Time) (int64, error) {
	var n int64
	for i := range s.places {
		p := &s.places[i]
		if !p.RenewRequested && ((!p.ServerNotAfter.IsZero() && p.ServerNotAfter.Before(before)) || (!p.ProbeNotAfter.IsZero() && p.ProbeNotAfter.Before(before))) {
			p.RenewRequested = true
			n++
		}
	}
	return n, nil
}
func (s *memStore) ExpiringAPCerts(context.Context, time.Time) ([]db.ExpiringAPCert, error) {
	return s.apCerts, nil
}
func (s *memStore) TryNamedLock(context.Context, string) (func(), bool, error) {
	return func() {}, true, nil
}
func (s *memStore) tenant(id int64) *db.TenantRadsec {
	i := slices.IndexFunc(s.tenants, func(t db.TenantRadsec) bool { return t.TenantID == id })
	return &s.tenants[i]
}

// fakeTenant はエッジのテナント。
type fakeTenant struct {
	sum    provisioner.EdgeTenantSummary
	decl   provisioner.EdgeDeclaration
	server *provisioner.EdgeCertInfo
	probe  *provisioner.EdgeCertInfo
}

// fakeEdges は偽の aka-provisioner。エッジの反映は finish を呼ぶまで動いている。
type fakeEdges struct {
	mu      sync.Mutex
	edges   map[string]map[int64]*fakeTenant
	down    map[string]bool
	stopped map[string]bool
	// calls は書き込みの記録（「口 エッジ キー」）。errs は口ごとの誤り。
	calls  []string
	errs   map[string]error
	serial int
	// crlResult は RefreshEdgeCRL の結果を上書きする。
	crlResult *provisioner.EdgeCRLRefresh
	// replayed は書き込んだキー（aka-provisioner とエッジと同じく、同じキーでは何もせず前の応答を返す）。
	replayed map[string]provisioner.EdgeTenant
}

func newFakeEdges(ids ...string) *fakeEdges {
	f := &fakeEdges{edges: map[string]map[int64]*fakeTenant{}, down: map[string]bool{}, stopped: map[string]bool{}, errs: map[string]error{}, replayed: map[string]provisioner.EdgeTenant{}}
	for _, id := range ids {
		f.edges[id] = map[int64]*fakeTenant{}
	}
	return f
}

var errUnreachable = errors.New("dial tcp: connection refused")

func (f *fakeEdges) note(op, edge, key string) error {
	f.calls = append(f.calls, op+" "+edge+" "+key)
	if err := f.errs[op+" "+edge]; err != nil {
		return err
	}
	return f.errs[op]
}

// count は口の呼び出しの数を返す。
func (f *fakeEdges) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeEdges) summary(edge string, t *fakeTenant) provisioner.EdgeTenantSummary {
	s := t.sum
	s.Enabled = !f.stopped[edge]
	s.Missing = []string{}
	if t.server == nil {
		s.Missing = append(s.Missing, "サーバー証明書")
	}
	if t.probe == nil {
		s.Missing = append(s.Missing, "ヘルスチェック用の証明書")
	}
	return s
}

func (f *fakeEdges) detail(edge string, t *fakeTenant) provisioner.EdgeTenant {
	return provisioner.EdgeTenant{EdgeTenantSummary: f.summary(edge, t), ServerCert: t.server, ProbeCert: t.probe}
}

func (f *fakeEdges) EdgeTenants(_ context.Context, edge string) ([]provisioner.EdgeTenantSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down[edge] {
		return nil, errUnreachable
	}
	var out []provisioner.EdgeTenantSummary
	for _, t := range f.edges[edge] {
		out = append(out, f.summary(edge, t))
	}
	return out, nil
}
func (f *fakeEdges) EdgeTenant(_ context.Context, edge string, tenant int64) (provisioner.EdgeTenant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down[edge] {
		return provisioner.EdgeTenant{}, errUnreachable
	}
	return f.detail(edge, f.edges[edge][tenant]), nil
}
func (f *fakeEdges) PutEdgeTenant(_ context.Context, edge string, tenant int64, d provisioner.EdgeDeclaration, key string) (provisioner.EdgeTenant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.note("put", edge, key); err != nil {
		return provisioner.EdgeTenant{}, err
	}
	if got, ok := f.replayed[key]; ok {
		return got, nil
	}
	defer func() { f.replayed[key] = f.detail(edge, f.edges[edge][tenant]) }()
	t := f.edges[edge][tenant]
	if t == nil {
		t = &fakeTenant{sum: provisioner.EdgeTenantSummary{ID: strconv.FormatInt(tenant, 10), Slug: d.Slug, Port: d.Port, Apply: provisioner.EdgeApply{State: "idle"}}}
		f.edges[edge][tenant] = t
	}
	t.sum.Revision, t.sum.Pending, t.decl = d.Revision, true, d
	return f.detail(edge, t), nil
}
func (f *fakeEdges) DeleteEdgeTenant(_ context.Context, edge string, tenant int64, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.note("delete", edge, key); err != nil {
		return err
	}
	delete(f.edges[edge], tenant)
	return nil
}
func (f *fakeEdges) EdgeServerCSR(_ context.Context, edge string, _ int64, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return "CSR", f.note("server-csr", edge, key)
}
func (f *fakeEdges) EdgeProbeCSR(_ context.Context, edge string, _ int64, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return "CSR", f.note("probe-csr", edge, key)
}
func (f *fakeEdges) PutEdgeServerCert(_ context.Context, edge string, tenant int64, cert, key string) (provisioner.EdgeTenant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.note("server-cert", edge, key); err != nil {
		return provisioner.EdgeTenant{}, err
	}
	if got, ok := f.replayed[key]; ok {
		return got, nil
	}
	defer func() { f.replayed[key] = f.detail(edge, f.edges[edge][tenant]) }()
	t := f.edges[edge][tenant]
	t.server = &provisioner.EdgeCertInfo{Serial: cert, NotAfter: certExpiry}
	t.sum.Pending = true
	return f.detail(edge, t), nil
}
func (f *fakeEdges) PutEdgeProbeCert(_ context.Context, edge string, tenant int64, cert, key string) (provisioner.EdgeTenant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.note("probe-cert", edge, key); err != nil {
		return provisioner.EdgeTenant{}, err
	}
	if got, ok := f.replayed[key]; ok {
		return got, nil
	}
	defer func() { f.replayed[key] = f.detail(edge, f.edges[edge][tenant]) }()
	t := f.edges[edge][tenant]
	t.probe = &provisioner.EdgeCertInfo{Serial: cert, NotAfter: certExpiry}
	t.sum.Pending = true
	return f.detail(edge, t), nil
}
func (f *fakeEdges) ApplyEdgeTenant(_ context.Context, edge string, tenant int64, key string) (provisioner.EdgeApply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.note("apply", edge, key); err != nil {
		return provisioner.EdgeApply{}, err
	}
	t := f.edges[edge][tenant]
	t.sum.Apply.State = "running" // 本物のエッジと同じく、前回の結果は残したまま
	return t.sum.Apply, nil
}
func (f *fakeEdges) RefreshEdgeCRL(_ context.Context, edge string, _ int64, key string) (provisioner.EdgeCRLRefresh, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.note("crl", edge, key); err != nil {
		return provisioner.EdgeCRLRefresh{}, err
	}
	if f.crlResult != nil {
		return *f.crlResult, nil
	}
	return provisioner.EdgeCRLRefresh{OK: true}, nil
}
func (f *fakeEdges) IssueCert(_ context.Context, i provisioner.CertIssue, key string) (provisioner.Cert, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.note("issue", i.Edge, key); err != nil {
		return provisioner.Cert{}, err
	}
	f.serial++
	serial := fmt.Sprintf("%s-%d", i.Profile, f.serial)
	return provisioner.Cert{Serial: serial, Certificate: serial, NotAfter: certExpiry}, nil
}
func (f *fakeEdges) RevokeCert(_ context.Context, serial, reason, key string) (provisioner.Cert, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return provisioner.Cert{}, f.note("revoke", serial+"."+reason, key)
}
func (f *fakeEdges) DisableCA(_ context.Context, id, key string) (provisioner.CA, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return provisioner.CA{}, f.note("disable", id, key)
}

// finish はエッジの反映を終える。ok なら今の宣言の版が反映済みになる。
func (f *fakeEdges) finish(edge string, tenant int64, result string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.edges[edge][tenant]
	t.sum.Apply = provisioner.EdgeApply{State: "idle", LastResult: result, LastRevision: t.sum.Revision}
	if result == "ok" {
		t.sum.AppliedRevision, t.sum.Pending = t.sum.Revision, false
	} else {
		t.sum.Apply.LastError = "ヘルスチェックに失敗しました"
	}
}

var (
	testNow    = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	certExpiry = testNow.AddDate(1, 0, 0)
)

type env struct {
	t  *testing.T
	s  *memStore
	pv *fakeEdges
	r  *Runner
}

// newEnv はエッジ 2 台とテナント 1 つ（primary が edge-01、secondary が edge-02）の環境を作る。
func newEnv(t *testing.T) *env {
	t.Helper()
	s := &memStore{aps: map[int64][]db.AP{}}
	for _, id := range []string{"edge-01", "edge-02"} {
		s.edges = append(s.edges, db.Edge{ID: id, AuthUpstream: "10.0.0.1:1812", AcctUpstream: "10.0.0.1:1813", Capacity: 150})
	}
	s.tenants = []db.TenantRadsec{{TenantID: 42, Slug: "acme", Port: 20000, CACertificate: "CA", Revision: 1}}
	s.places = []db.Placement{{ID: 1, TenantID: 42, EdgeID: "edge-01", Slot: "primary"}, {ID: 2, TenantID: 42, EdgeID: "edge-02", Slot: "secondary"}}
	pv := newFakeEdges("edge-01", "edge-02")
	// 時計は呼ぶたびに少し進む（周回ごとに時刻が変わる）。
	var tick atomic.Int64
	r := New(Options{Log: discard, DB: s, PV: pv, Audit: &audit.Recorder{DB: s, Log: discard}, Now: func() time.Time { return testNow.Add(time.Duration(tick.Add(1)) * time.Second) }})
	return &env{t: t, s: s, pv: pv, r: r}
}

func (e *env) pass() {
	e.t.Helper()
	if _, err := e.r.RunScheduled(e.t.Context()); err != nil {
		e.t.Fatal(err)
	}
}

// settle は最初の反映まで済ませる（2 台とも）。
func (e *env) settle() {
	e.t.Helper()
	e.pass()
	e.pv.finish("edge-01", 42, "ok")
	e.pass()
	e.pv.finish("edge-02", 42, "ok")
	e.pass()
	e.pv.calls = nil
}

// TestFirstRolloutInOrder は新しい配置の宣言・証明書と、順の反映を確かめる（OB2・S4-8）。
func TestFirstRolloutInOrder(t *testing.T) {
	e := newEnv(t)
	e.pass()
	// 宣言と証明書は 2 台とも。反映は primary だけ。
	for _, edge := range []string{"edge-01", "edge-02"} {
		for _, op := range []string{"put", "server-csr", "server-cert", "probe-csr", "probe-cert"} {
			if e.pv.count(op+" "+edge) != 1 {
				t.Errorf("%s %s: calls %q", op, edge, e.pv.calls)
			}
		}
	}
	if e.pv.count("apply edge-01") != 1 || e.pv.count("apply edge-02") != 0 {
		t.Fatalf("calls %q", e.pv.calls)
	}
	if !e.r.busy.Load() {
		t.Error("should be busy while applying")
	}
	// primary が動いている間は secondary に送らない。重ねて apply もしない。
	e.pass()
	if e.pv.count("apply") != 1 {
		t.Fatalf("applied twice: %q", e.pv.calls)
	}
	e.pv.finish("edge-01", 42, "ok")
	e.pass()
	if e.pv.count("apply edge-02") != 1 {
		t.Fatalf("secondary was not applied: %q", e.pv.calls)
	}
	if p := e.s.place(1); p.AppliedRevision != 1 || p.ApplyResult != "ok" || p.ServerSerial == "" || p.ProbeSerial == "" {
		t.Errorf("primary = %+v", p)
	}
	e.pv.finish("edge-02", 42, "ok")
	e.pass()
	if e.r.busy.Load() {
		t.Error("should not be busy")
	}
	if e.pv.count("apply") != 2 || e.pv.count("put") != 2 {
		t.Errorf("calls %q", e.pv.calls)
	}
	// 落ち着いたら何も書かない。
	calls, writes := len(e.pv.calls), e.s.writes
	e.pass()
	if len(e.pv.calls) != calls || e.s.writes != writes {
		t.Errorf("extra work: %q, writes %d → %d", e.pv.calls[calls:], writes, e.s.writes)
	}
	// 監査ログは操作者 system とトレースつき。
	a := e.s.audit[0]
	if a.ActorEmail != Operator || !strings.HasPrefix(a.TraceID, "radsec-") || a.TenantID != 42 {
		t.Errorf("audit = %+v", a)
	}
}

// TestDeclareWithoutRequest は、版が上がっても反映を求めるまで apply しないことを確かめる（OB1）。
func TestDeclareWithoutRequest(t *testing.T) {
	e := newEnv(t)
	e.settle()
	e.s.tenant(42).Revision = 2
	e.pass()
	if e.pv.count("put") != 2 || e.pv.count("apply") != 0 {
		t.Fatalf("calls %q", e.pv.calls)
	}
	e.s.tenant(42).RequestedRevision = 2
	e.pass()
	if e.pv.count("apply edge-01") != 1 || e.pv.count("apply edge-02") != 0 {
		t.Fatalf("calls %q", e.pv.calls)
	}
	// PUT は本文のハッシュを含むキー、apply は毎回新しいキー。
	if !strings.Contains(e.pv.calls[0], "radsec-p1-put-2-") {
		t.Errorf("put key: %q", e.pv.calls[0])
	}
	e.pv.finish("edge-01", 42, "ok")
	e.pass()
	e.pv.finish("edge-02", 42, "ok")
	e.pass()
	e.s.tenant(42).Revision, e.s.tenant(42).RequestedRevision = 3, 3
	e.pass()
	var keys []string
	for _, c := range e.pv.calls {
		if strings.HasPrefix(c, "apply edge-01") {
			keys = append(keys, c)
		}
	}
	if len(keys) != 2 || keys[0] == keys[1] {
		t.Errorf("apply keys = %q", keys)
	}
}

// TestFailureStopsRollout は途中の失敗で残りに送らず、同じ版を自動でやり直さないことを確かめる（OB2・S4-8）。
func TestFailureStopsRollout(t *testing.T) {
	e := newEnv(t)
	e.settle()
	e.s.tenant(42).Revision, e.s.tenant(42).RequestedRevision = 2, 2
	e.pass()
	e.pv.finish("edge-01", 42, "rolled_back")
	for range 3 {
		e.pass()
	}
	if e.pv.count("apply") != 1 {
		t.Fatalf("retried automatically or sent to the secondary: %q", e.pv.calls)
	}
	p := e.s.place(1)
	if p.ApplyResult != "rolled_back" || p.ApplyError == "" || p.FailedRevision != 2 || p.AppliedRevision != 1 {
		t.Errorf("primary = %+v", p)
	}
	if !slices.ContainsFunc(e.s.audit, func(a db.AuditEntry) bool {
		return a.Action == audit.ActionLoopApplyResult && a.Detail["result"] == "rolled_back"
	}) {
		t.Error("no audit for the failure")
	}
	// CRL の更新で最後の結果が ok に変わっても、失敗した版はやり直さない。
	e.pv.edges["edge-01"][42].sum.Apply.LastResult = "ok"
	e.pass()
	if e.pv.count("apply") != 1 {
		t.Fatalf("a crl record unblocked the retry: %q", e.pv.calls)
	}
	// 新しい版を求めると進む。
	e.s.tenant(42).Revision, e.s.tenant(42).RequestedRevision = 3, 3
	e.pass()
	if e.pv.count("apply edge-01") != 2 {
		t.Fatalf("new revision was not applied: %q", e.pv.calls)
	}
	e.pv.finish("edge-01", 42, "failed")
	e.pass()
	if e.pv.count("apply edge-01") != 2 {
		t.Fatalf("retried automatically: %q", e.pv.calls)
	}
	// 「やり直す」で動き、成功したら secondary へ進む。
	e.s.place(1).RetryRequested = true
	e.pass()
	if p := e.s.place(1); e.pv.count("apply edge-01") != 3 || p.RetryRequested || p.FailedRevision != 0 || p.ApplyResult != "" {
		t.Fatalf("retry: %q, %+v", e.pv.calls, p)
	}
	// 動いている間も前回の結果が返るが、前の失敗をもう一度数えない。
	failures := func() int {
		n := 0
		for _, a := range e.s.audit {
			if a.Action == audit.ActionLoopApplyResult && a.Detail["result"] == "failed" {
				n++
			}
		}
		return n
	}
	before := failures()
	e.pass()
	if p := e.s.place(1); p.ApplyResult != "" || p.ApplyError != "" || p.FailedRevision != 0 || failures() != before {
		t.Fatalf("while running: %+v, failures %d → %d", p, before, failures())
	}
	e.pv.finish("edge-01", 42, "ok")
	e.pass()
	if e.pv.count("apply edge-02") != 1 {
		t.Fatalf("secondary: %q", e.pv.calls)
	}
}

// TestInterruptedIsRetried は途中で止まった反映だけは自動でやり直すことを確かめる（OB2）。
func TestInterruptedIsRetried(t *testing.T) {
	e := newEnv(t)
	e.settle()
	e.s.tenant(42).Revision, e.s.tenant(42).RequestedRevision = 2, 2
	e.pass()
	e.pv.finish("edge-01", 42, "interrupted")
	e.pass()
	if e.pv.count("apply edge-01") != 2 {
		t.Fatalf("calls %q", e.pv.calls)
	}
}

// TestUnreachableEdgeIsSkipped は届かないエッジを飛ばし、戻ったら追いつくことを確かめる（OB2・S4-8）。
func TestUnreachableEdgeIsSkipped(t *testing.T) {
	e := newEnv(t)
	e.settle()
	e.s.tenant(42).Revision, e.s.tenant(42).RequestedRevision = 2, 2
	e.pv.down["edge-01"] = true
	e.pass()
	if e.pv.count("apply edge-02") != 1 || e.pv.count("put edge-01") != 0 {
		t.Fatalf("calls %q", e.pv.calls)
	}
	if !e.r.down["edge-01"] {
		t.Error("the edge should be remembered as down")
	}
	e.pv.finish("edge-02", 42, "ok")
	e.pv.down["edge-01"] = false
	e.pass()
	if e.pv.count("put edge-01") != 1 || e.pv.count("apply edge-01") != 1 || e.r.down["edge-01"] {
		t.Fatalf("calls %q, down %v", e.pv.calls, e.r.down)
	}
	if p := e.s.place(1); p.AppliedRevision != 1 {
		t.Errorf("primary = %+v", p)
	}
}

// TestStoppedEdgeIsSkipped はエッジの画面で止めたテナントを飛ばすことを確かめる。
func TestStoppedEdgeIsSkipped(t *testing.T) {
	e := newEnv(t)
	e.settle()
	e.s.tenant(42).Revision, e.s.tenant(42).RequestedRevision = 2, 2
	e.pv.stopped["edge-01"] = true
	e.pass()
	if e.pv.count("apply edge-01") != 0 || e.pv.count("put edge-01") != 0 || e.pv.count("apply edge-02") != 1 {
		t.Fatalf("calls %q", e.pv.calls)
	}
	if p := e.s.place(1); p.ApplyResult != db.ApplyResultStopped {
		t.Errorf("primary = %+v", p)
	}
}

// TestEdgeLostItsData はエッジのデータを失ったときに宣言と証明書を出し直し、古い証明書を反映の後に失効させることを確かめる（OB4）。
func TestEdgeLostItsData(t *testing.T) {
	e := newEnv(t)
	e.settle()
	oldServer, oldProbe := e.s.place(1).ServerSerial, e.s.place(1).ProbeSerial
	delete(e.pv.edges["edge-01"], 42)
	e.pass()
	if e.pv.count("put edge-01") != 1 || e.pv.count("server-csr edge-01") != 1 || e.pv.count("probe-csr edge-01") != 1 || e.pv.count("apply edge-01") != 1 {
		t.Fatalf("calls %q", e.pv.calls)
	}
	if e.pv.count("revoke") != 0 {
		t.Fatalf("revoked before the apply: %q", e.pv.calls)
	}
	e.pv.finish("edge-01", 42, "ok")
	e.pass()
	if e.pv.count("revoke "+oldServer+".superseded") != 1 || e.pv.count("revoke "+oldProbe+".superseded") != 1 {
		t.Fatalf("calls %q", e.pv.calls)
	}
	if p := e.s.place(1); p.CertApplyNeeded || len(p.SupersededSerials) != 0 {
		t.Errorf("primary = %+v", p)
	}
}

// TestProbeProblemIsBounded はヘルスチェック用の証明書の問題で、周回ごとに証明書を増やさないことを確かめる。
func TestProbeProblemIsBounded(t *testing.T) {
	e := newEnv(t)
	e.settle()
	e.pv.edges["edge-01"][42].sum.ProbeProblem = "失効しています"
	e.pass()
	e.pass()
	if e.pv.count("probe-csr edge-01") != 1 {
		t.Fatalf("calls %q", e.pv.calls)
	}
}

// TestRenewal は期限が近い証明書の更新を確かめる（OB4）。1 台ずつ反映し、反映の後に古いものを失効させる。
func TestRenewal(t *testing.T) {
	e := newEnv(t)
	e.settle()
	soon := testNow.Add(20 * 24 * time.Hour)
	for _, id := range []int64{1, 2} {
		e.s.place(id).ServerNotAfter = soon
	}
	oldProbe := e.s.place(1).ProbeSerial
	oldServer := e.s.place(1).ServerSerial
	if _, err := e.r.RunRenew(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !e.s.place(1).RenewRequested {
		t.Fatal("not marked")
	}
	e.pass()
	// 期限の近いサーバー証明書だけを両方に置き、反映は primary から。
	if e.pv.count("server-csr") != 2 || e.pv.count("probe-csr") != 0 || e.pv.count("apply edge-01") != 1 || e.pv.count("apply edge-02") != 0 {
		t.Fatalf("calls %q", e.pv.calls)
	}
	if p := e.s.place(1); p.RenewRequested || p.ProbeSerial != oldProbe || p.ServerNotAfter != certExpiry || !p.CertApplyNeeded {
		t.Errorf("primary = %+v", p)
	}
	e.pv.finish("edge-01", 42, "ok")
	e.pass()
	if e.pv.count("revoke "+oldServer+".superseded") != 1 || e.pv.count("apply edge-02") != 1 {
		t.Fatalf("calls %q", e.pv.calls)
	}
	e.pv.finish("edge-02", 42, "ok")
	e.pass()
	if e.s.place(2).CertApplyNeeded || e.pv.count("revoke") != 2 {
		t.Errorf("calls %q, secondary %+v", e.pv.calls, e.s.place(2))
	}
}

// TestCertKeys は発行のキーの世代を確かめる。結果が分からないときは同じキー、断られたら新しいキーにする。
func TestCertKeys(t *testing.T) {
	e := newEnv(t)
	e.pv.errs["issue edge-01"] = errUnreachable
	e.pass()
	e.pass()
	if got := e.pv.calls[slices.IndexFunc(e.pv.calls, func(c string) bool { return strings.HasPrefix(c, "server-csr edge-01") })]; !strings.HasSuffix(got, "radsec-p1-server-csr-0") {
		t.Errorf("csr key = %q", got)
	}
	if e.pv.count("server-csr edge-01 radsec-p1-server-csr-0") != 2 || e.pv.count("apply edge-01") != 0 {
		t.Fatalf("calls %q", e.pv.calls)
	}
	if p := e.s.place(1); p.ApplyResult != db.ApplyResultSyncError || !strings.Contains(p.ApplyError, "radsec-ca") {
		t.Errorf("primary = %+v", p)
	}
	e.pv.errs["issue edge-01"] = &provisioner.Error{Status: 422, Problem: provisioner.Problem{Cause: provisioner.CauseCSRInvalid, Detail: "CSR が読めません"}}
	e.pv.calls = nil
	e.pass()
	if p := e.s.place(1); p.CertGeneration != 1 || p.ApplyError != "CSR が読めません" {
		t.Errorf("primary = %+v", p)
	}
	delete(e.pv.errs, "issue edge-01")
	e.pv.calls = nil
	e.pass()
	if e.pv.count("server-csr edge-01 radsec-p1-server-csr-1") != 1 || e.pv.count("apply edge-01") != 1 {
		t.Fatalf("calls %q", e.pv.calls)
	}
	if p := e.s.place(1); p.ApplyResult != "" || p.ApplyError != "" {
		t.Errorf("primary = %+v", p)
	}
}

// TestBusyEdge は TENANT_BUSY を誤りにせず、すぐ確かめ直すことを確かめる。
func TestBusyEdge(t *testing.T) {
	e := newEnv(t)
	e.settle()
	e.s.tenant(42).Revision = 2
	e.pv.errs["put edge-01"] = &provisioner.Error{Status: 503, Problem: provisioner.Problem{Cause: provisioner.CauseTenantBusy}}
	e.pass()
	if !e.r.busy.Load() || e.s.place(1).ApplyResult != "ok" || e.r.down["edge-01"] {
		t.Errorf("busy %v, primary %+v, down %v", e.r.busy.Load(), e.s.place(1), e.r.down)
	}
	delete(e.pv.errs, "put edge-01")
	e.pass()
	if e.pv.count("put edge-01") != 2 {
		t.Errorf("calls %q", e.pv.calls)
	}
}

// TestApplyLimitPerEdge は 1 エッジで同時に動かす反映の上限を確かめる（OB18）。
func TestApplyLimitPerEdge(t *testing.T) {
	e := newEnv(t)
	e.s.tenants, e.s.places = nil, nil
	e.pv.edges["edge-01"] = map[int64]*fakeTenant{}
	for i := range int64(7) {
		id := 100 + i
		e.s.tenants = append(e.s.tenants, db.TenantRadsec{TenantID: id, Slug: fmt.Sprintf("t%d", id), Port: 20000 + int(i), CACertificate: "CA", Revision: 1})
		e.s.places = append(e.s.places, db.Placement{ID: id, TenantID: id, EdgeID: "edge-01", Slot: "primary"})
	}
	e.pass()
	if e.pv.count("apply edge-01") != maxApplyPerEdge {
		t.Fatalf("started %d: %q", e.pv.count("apply edge-01"), e.pv.calls)
	}
	e.pass()
	if e.pv.count("apply edge-01") != maxApplyPerEdge {
		t.Fatalf("the limit was exceeded while running: %d", e.pv.count("apply edge-01"))
	}
	e.pv.finish("edge-01", 100, "ok")
	e.pass()
	if e.pv.count("apply edge-01") != maxApplyPerEdge+1 {
		t.Fatalf("started %d", e.pv.count("apply edge-01"))
	}
}

// TestRemovePlacement は外した配置をエッジから消すことを確かめる。届かない間は待つ。
func TestRemovePlacement(t *testing.T) {
	e := newEnv(t)
	e.settle()
	e.s.place(2).DeletedAt = testNow
	e.pv.down["edge-02"] = true
	e.pass()
	if e.pv.count("delete") != 0 || len(e.s.places) != 2 {
		t.Fatalf("calls %q", e.pv.calls)
	}
	e.pv.down["edge-02"] = false
	e.pass()
	if e.pv.count("delete edge-02 radsec-p2-delete-") != 1 || len(e.s.places) != 1 {
		t.Fatalf("calls %q, places %+v", e.pv.calls, e.s.places)
	}
	// エッジに既にないなら呼ばずに終える。
	e.s.places = append(e.s.places, db.Placement{ID: 3, TenantID: 42, EdgeID: "edge-02", Slot: "tertiary", DeletedAt: testNow})
	e.pv.calls = nil
	e.pass()
	if e.pv.count("delete") != 0 || len(e.s.places) != 1 {
		t.Fatalf("calls %q, places %+v", e.pv.calls, e.s.places)
	}
}

// TestNoCallsWhileRunning は反映が動いているエッジに宣言と CRL の取り直しを送らないことを確かめる（テナントの排他を待たされるため）。
func TestNoCallsWhileRunning(t *testing.T) {
	e := newEnv(t)
	e.settle()
	e.s.tenant(42).Revision, e.s.tenant(42).RequestedRevision = 2, 2
	e.pass()
	e.pv.calls = nil
	e.s.tenant(42).Revision = 3
	e.s.tenant(42).CRLDirty = true
	e.pass()
	if e.pv.count("put edge-01") != 0 || e.pv.count("crl edge-01") != 0 || e.pv.count("put edge-02") != 1 || !e.s.tenant(42).CRLDirty {
		t.Fatalf("calls %q, dirty %v", e.pv.calls, e.s.tenant(42).CRLDirty)
	}
	e.pv.finish("edge-01", 42, "ok")
	e.pass()
	if e.pv.count("put edge-01") != 1 || e.pv.count("crl edge-01") != 1 {
		t.Fatalf("calls %q", e.pv.calls)
	}
}

// TestCRLDirty は CRL の取り直しの印を全配置が成功するまで残すことを確かめる（OB18）。
func TestCRLDirty(t *testing.T) {
	e := newEnv(t)
	e.settle()
	e.s.tenant(42).CRLDirty = true
	e.pv.down["edge-02"] = true
	e.pass()
	if !e.s.tenant(42).CRLDirty || e.pv.count("crl edge-01") != 1 {
		t.Fatalf("dirty %v, calls %q", e.s.tenant(42).CRLDirty, e.pv.calls)
	}
	e.pv.down["edge-02"] = false
	// 発行した CA の取得に失敗したら残す。持ち込みの CA の失敗では残さない。
	e.pv.crlResult = &provisioner.EdgeCRLRefresh{OK: false, Items: []provisioner.EdgeCRLState{{CAID: "tenant.acme", OK: false, LastError: "届きません"}}}
	e.pass()
	if !e.s.tenant(42).CRLDirty {
		t.Fatal("cleared although the issued ca failed")
	}
	e.pv.crlResult = &provisioner.EdgeCRLRefresh{OK: false, Items: []provisioner.EdgeCRLState{{CAID: "tenant.acme", OK: true}, {CAID: "brought.acme.v", OK: false}}}
	e.pass()
	if e.s.tenant(42).CRLDirty {
		t.Fatal("not cleared")
	}
	// 読んだ版から進んでいたら消さない（取り直しの間の新しい失効を取りこぼさない）。
	if err := e.s.ClearCRLDirty(t.Context(), 42, 99); err != nil {
		t.Fatal(err)
	}
}

// TestRemoveBrought は全配置が外した版を反映するまで持ち込みの CA を止めないことを確かめる（OB6）。
func TestRemoveBrought(t *testing.T) {
	e := newEnv(t)
	e.s.brought = []db.BroughtCA{{CAID: "brought.acme.v", Name: "v", TenantID: 42, Certificate: "V", HasCRL: true}}
	e.s.tenant(42).RequestedRevision = 1
	e.settle()
	if d := e.pv.edges["edge-01"][42].decl; len(d.ClientCAs) != 2 {
		t.Fatalf("declaration = %+v", d)
	}
	e.s.brought[0].RemovingAt, e.s.brought[0].RemovingRevision = testNow, 2
	e.s.tenant(42).Revision, e.s.tenant(42).RequestedRevision = 2, 2
	e.pass()
	if d := e.pv.edges["edge-01"][42].decl; len(d.ClientCAs) != 1 || e.pv.count("disable") != 0 {
		t.Fatalf("declaration = %+v, calls %q", d, e.pv.calls)
	}
	e.pv.finish("edge-01", 42, "ok")
	e.pass()
	e.pv.down["edge-02"] = true
	e.pv.finish("edge-02", 42, "ok")
	// secondary が届かない間（写しの版が古い間）は待つ。
	e.pass()
	if e.pv.count("disable") != 0 {
		t.Fatalf("disabled too early: %q", e.pv.calls)
	}
	e.pv.down["edge-02"] = false
	e.pass()
	e.pass()
	if e.pv.count("disable brought.acme.v") != 1 || len(e.s.brought) != 0 {
		t.Fatalf("calls %q, brought %+v", e.pv.calls, e.s.brought)
	}
}

// TestDisableReleasedCA は削除したテナントの CA を止め直すことを確かめる（OB12）。
func TestDisableReleasedCA(t *testing.T) {
	e := newEnv(t)
	e.s.released = []db.TenantRadsec{{TenantID: 7, Slug: "gone"}}
	e.pv.errs["disable tenant.gone"] = errUnreachable
	e.pass()
	if len(e.s.released) != 1 {
		t.Fatal("marked although it failed")
	}
	e.pv.errs["disable tenant.gone"] = &provisioner.Error{Status: 404, Problem: provisioner.Problem{Cause: provisioner.CauseCANotFound}}
	e.pass()
	if len(e.s.released) != 0 {
		t.Fatal("not marked")
	}
}

// TestOrphanIsOnlyReported はエッジにしかないテナントを消さず、変わったときだけ報告することを確かめる（OB18）。
func TestOrphanIsOnlyReported(t *testing.T) {
	var b strings.Builder
	e := newEnv(t)
	e.r.opts.Log = slog.New(slog.NewTextHandler(&b, nil))
	e.pv.edges["edge-01"][99] = &fakeTenant{sum: provisioner.EdgeTenantSummary{ID: "99", Slug: "stale"}}
	e.pass()
	e.pass()
	if e.pv.count("delete") != 0 || e.pv.edges["edge-01"][99] == nil {
		t.Fatalf("calls %q", e.pv.calls)
	}
	if n := strings.Count(b.String(), "RADSEC_ORPHAN_TENANT"); n != 1 {
		t.Errorf("logged %d times", n)
	}
}

// TestEdgeEventsOnChange は接続のログを変わったときだけ出すことを確かめる（OB19）。
func TestEdgeEventsOnChange(t *testing.T) {
	var b strings.Builder
	e := newEnv(t)
	e.r.opts.Log = slog.New(slog.NewTextHandler(&b, nil))
	e.pv.down["edge-02"] = true
	e.pass()
	e.pass()
	e.pv.down["edge-02"] = false
	e.pass()
	e.pass()
	if u, r := strings.Count(b.String(), "RADSEC_EDGE_UNREACHABLE"), strings.Count(b.String(), "RADSEC_EDGE_REACHABLE"); u != 1 || r != 1 {
		t.Errorf("unreachable %d, reachable %d\n%s", u, r, b.String())
	}
}

// TestDeclaration は宣言の中身を確かめる。
func TestDeclaration(t *testing.T) {
	tr := db.TenantRadsec{TenantID: 42, Slug: "acme", Port: 20000, CACertificate: "CA", Revision: 5, SourceCIDRs: []string{"192.0.2.0/24"}, MatchAttr: "CN", MatchPattern: "^a$"}
	e := db.Edge{AuthUpstream: "10.0.0.1:1812", AcctUpstream: "10.0.0.1:1813"}
	aps := []db.AP{
		{Name: "ap1", Kind: db.KindAP, Certs: []db.APCert{{Serial: "new", NotAfter: certExpiry, RevokedAt: testNow}, {Serial: "old", NotAfter: certExpiry}}},
		{Name: "g1", Kind: db.KindGroup, Purpose: "unifi", Certs: []db.APCert{{Serial: "expired", NotAfter: testNow.Add(-time.Hour)}}},
	}
	brought := []db.BroughtCA{{CAID: "brought.acme.a", Certificate: "A", HasCRL: false}, {CAID: "brought.acme.b", Certificate: "B", HasCRL: true, RemovingAt: testNow}}
	d := Declaration(tr, e, aps, brought, testNow)
	if d.Revision != 5 || d.Slug != "acme" || d.AuthUpstream != e.AuthUpstream || d.SharedMatch == nil || d.SharedMatch.Pattern != "^a$" || len(d.SourceCIDRs) != 1 {
		t.Errorf("declaration = %+v", d)
	}
	if len(d.ClientCAs) != 2 || d.ClientCAs[0].ID != "tenant.acme" || d.ClientCAs[0].Kind != "issued" || !d.ClientCAs[0].HasCRL || d.ClientCAs[1].ID != "brought.acme.a" || d.ClientCAs[1].HasCRL {
		t.Errorf("client cas = %+v", d.ClientCAs)
	}
	if d.APs[0].CertSerial != "old" || d.APs[1].CertSerial != "" || d.APs[1].Purpose != "unifi" {
		t.Errorf("aps = %+v", d.APs)
	}
}

// TestRunWake は起こされると周回が動き、反映の間は短い間隔で動くことを確かめる。
func TestRunWake(t *testing.T) {
	e := newEnv(t)
	e.r.opts.BusyInterval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		e.r.Run(ctx)
		close(done)
	}()
	e.r.Wake()
	deadline := time.Now().Add(5 * time.Second)
	for e.pv.count("apply edge-01") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the wake did not run a pass")
		}
		time.Sleep(time.Millisecond)
	}
	e.pv.finish("edge-01", 42, "ok")
	// 起こさなくても、動いている間の間隔で secondary へ進む。
	for e.pv.count("apply edge-02") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the busy interval did not run a pass")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
}
