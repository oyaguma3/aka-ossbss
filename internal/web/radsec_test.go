package web

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json/jsontext"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oyaguma3/aka-ossbss/internal/audit"
	"github.com/oyaguma3/aka-ossbss/internal/auth"
	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
)

// fakeRadsec は RadsecStore の簡単な代わり（版と制約の細かい確かめは internal/db の結合試験）。
type fakeRadsec struct {
	edges     map[string]db.Edge
	radsec    map[int64]db.TenantRadsec
	places    []db.Placement
	aps       []db.AP
	brought   []db.BroughtCA
	requested int
	nextID    int64
	expiring  []db.ExpiringAPCert
}

func newFakeRadsec() *fakeRadsec {
	return &fakeRadsec{edges: map[string]db.Edge{}, radsec: map[int64]db.TenantRadsec{}, nextID: 100}
}

func (f *fakeRadsec) Edges(context.Context) ([]db.Edge, error) {
	var out []db.Edge
	for _, e := range f.edges {
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b db.Edge) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}
func (f *fakeRadsec) EdgeByID(_ context.Context, id string) (db.Edge, error) {
	if e, ok := f.edges[id]; ok {
		return e, nil
	}
	return db.Edge{}, db.ErrNotFound
}
func (f *fakeRadsec) CreateEdge(_ context.Context, e db.Edge, by string) error {
	if _, ok := f.edges[e.ID]; ok {
		return db.ErrConflict
	}
	e.CreatedBy = by
	f.edges[e.ID] = e
	return nil
}
func (f *fakeRadsec) UpdateEdge(_ context.Context, e db.Edge) (db.Edge, error) {
	before, ok := f.edges[e.ID]
	if !ok {
		return db.Edge{}, db.ErrNotFound
	}
	if e.Capacity < before.Placements {
		return db.Edge{}, db.ErrCapacity
	}
	e.Placements = before.Placements
	f.edges[e.ID] = e
	return before, nil
}
func (f *fakeRadsec) DeleteEdge(_ context.Context, id string) error {
	e, ok := f.edges[id]
	switch {
	case !ok:
		return db.ErrNotFound
	case e.Placements > 0:
		return db.ErrInUse
	}
	delete(f.edges, id)
	return nil
}
func (f *fakeRadsec) TenantRadsec(_ context.Context, t int64) (db.TenantRadsec, error) {
	if r, ok := f.radsec[t]; ok && r.ReleasedAt.IsZero() {
		return r, nil
	}
	return db.TenantRadsec{}, db.ErrNotFound
}
func (f *fakeRadsec) ReleasedRadsec(_ context.Context, t int64) (db.TenantRadsec, error) {
	if r, ok := f.radsec[t]; ok {
		return r, nil
	}
	return db.TenantRadsec{}, db.ErrNotFound
}
func (f *fakeRadsec) SlugUsed(_ context.Context, slug string) (bool, error) {
	for _, r := range f.radsec {
		if r.Slug == slug {
			return true, nil
		}
	}
	return false, nil
}
func (f *fakeRadsec) EnableRadsec(_ context.Context, t int64, slug, ca, by string) (db.TenantRadsec, error) {
	r := db.TenantRadsec{TenantID: t, Slug: slug, Port: db.RadsecPortMin + len(f.radsec), CACertificate: ca, Revision: 1, CreatedBy: by}
	f.radsec[t] = r
	return r, nil
}
func (f *fakeRadsec) RequestRetry(_ context.Context, t, id int64) error {
	for i := range f.places {
		if f.places[i].ID == id && f.places[i].TenantID == t {
			f.places[i].RetryRequested = true
			return nil
		}
	}
	return db.ErrNotFound
}
func (f *fakeRadsec) ExpiringAPCerts(context.Context, time.Time) ([]db.ExpiringAPCert, error) {
	return f.expiring, nil
}
func (f *fakeRadsec) bump(t int64, request bool) {
	r := f.radsec[t]
	r.Revision++
	if request {
		r.RequestedRevision = r.Revision
	}
	f.radsec[t] = r
}
func (f *fakeRadsec) SetRadsecSources(_ context.Context, t int64, cidrs []string) ([]string, error) {
	r := f.radsec[t]
	before := r.SourceCIDRs
	r.SourceCIDRs = cidrs
	f.radsec[t] = r
	f.bump(t, true)
	return before, nil
}
func (f *fakeRadsec) SetSharedMatch(_ context.Context, t int64, attr, pattern string) error {
	r := f.radsec[t]
	r.MatchAttr, r.MatchPattern = attr, pattern
	f.radsec[t] = r
	f.bump(t, false)
	return nil
}
func (f *fakeRadsec) RequestApply(_ context.Context, t int64) (int64, error) {
	r, ok := f.radsec[t]
	if !ok {
		return 0, db.ErrNotFound
	}
	r.RequestedRevision = r.Revision
	f.radsec[t] = r
	f.requested++
	return r.Revision, nil
}
func (f *fakeRadsec) MarkRadsecCADisabled(_ context.Context, t int64) error {
	r := f.radsec[t]
	r.CADisabledAt = time.Now()
	f.radsec[t] = r
	return nil
}
func (f *fakeRadsec) Placements(_ context.Context, t int64) ([]db.Placement, error) {
	var out []db.Placement
	for _, p := range f.places {
		if p.TenantID == t {
			out = append(out, p)
		}
	}
	return out, nil
}
func (f *fakeRadsec) EdgePlacements(_ context.Context, edge string) ([]db.Placement, error) {
	var out []db.Placement
	for _, p := range f.places {
		if p.EdgeID == edge {
			out = append(out, p)
		}
	}
	return out, nil
}
func (f *fakeRadsec) AddPlacement(_ context.Context, t int64, edge, slot, by string) (db.Placement, error) {
	e, ok := f.edges[edge]
	if !ok {
		return db.Placement{}, db.ErrNotFound
	}
	if e.Placements >= e.Capacity {
		return db.Placement{}, db.ErrCapacity
	}
	e.Placements++
	f.edges[edge] = e
	f.nextID++
	p := db.Placement{ID: f.nextID, TenantID: t, EdgeID: edge, Slot: slot, CreatedBy: by}
	f.places = append(f.places, p)
	return p, nil
}
func (f *fakeRadsec) mark(t, id int64) (db.Placement, error) {
	for i, p := range f.places {
		if p.ID == id && p.TenantID == t {
			f.places[i].DeletedAt = time.Now()
			return f.places[i], nil
		}
	}
	return db.Placement{}, db.ErrNotFound
}
func (f *fakeRadsec) RemovePlacement(_ context.Context, t, id int64) (db.Placement, error) {
	return f.mark(t, id)
}
func (f *fakeRadsec) StartMove(ctx context.Context, t, id int64, edge, by string) (db.Placement, error) {
	p, err := f.AddPlacement(ctx, t, edge, "primary", by)
	if err == nil {
		f.places[len(f.places)-1].Replaces = id
		p.Replaces = id
	}
	return p, err
}
func (f *fakeRadsec) FinishMove(_ context.Context, t, id int64) (db.Placement, error) {
	for _, p := range f.places {
		if p.ID == id && p.Replaces != 0 {
			if p.AppliedRevision == 0 {
				return db.Placement{}, db.ErrNotReady
			}
			return f.mark(t, p.Replaces)
		}
	}
	return db.Placement{}, db.ErrNotFound
}
func (f *fakeRadsec) CancelMove(_ context.Context, t, id int64) (db.Placement, error) {
	return f.mark(t, id)
}
func (f *fakeRadsec) RadsecOverviews(context.Context) ([]db.RadsecOverview, error) {
	var out []db.RadsecOverview
	for t, r := range f.radsec {
		ps, _ := f.Placements(context.Background(), t)
		out = append(out, db.RadsecOverview{TenantRadsec: r, TenantName: "テナント" + strconv.FormatInt(t, 10), Placements: ps})
	}
	return out, nil
}
func (f *fakeRadsec) APs(_ context.Context, t int64) ([]db.AP, error) {
	var out []db.AP
	for _, a := range f.aps {
		if a.TenantID == t {
			out = append(out, a)
		}
	}
	return out, nil
}
func (f *fakeRadsec) APByID(_ context.Context, t, id int64) (db.AP, error) {
	for _, a := range f.aps {
		if a.TenantID == t && a.ID == id {
			return a, nil
		}
	}
	return db.AP{}, db.ErrNotFound
}
func (f *fakeRadsec) CheckAPName(_ context.Context, t int64, name, nas string) error {
	for _, a := range f.aps {
		if a.TenantID == t && a.Name == name {
			return db.ErrConflict
		}
		if a.NASID == nas {
			return db.ErrNASIDUsed
		}
	}
	return nil
}
func (f *fakeRadsec) CreateAP(_ context.Context, a db.AP, c db.APCert) (db.AP, error) {
	f.nextID++
	a.ID, a.Certs = f.nextID, []db.APCert{c}
	f.aps = append(f.aps, a)
	f.bump(a.TenantID, false)
	return a, nil
}
func (f *fakeRadsec) AddAPCert(_ context.Context, t, ap int64, c db.APCert) error {
	for i, a := range f.aps {
		if a.ID == ap {
			f.aps[i].Certs = append([]db.APCert{c}, a.Certs...)
		}
	}
	f.bump(t, false)
	return nil
}
func (f *fakeRadsec) MarkAPCertRevoked(_ context.Context, t, ap int64, serial, reason string) error {
	for i, a := range f.aps {
		if a.ID != ap {
			continue
		}
		for j, c := range a.Certs {
			if c.Serial == serial {
				f.aps[i].Certs[j].RevokedAt, f.aps[i].Certs[j].RevokeReason = time.Now(), reason
				r := f.radsec[t]
				r.CRLDirty = true
				f.radsec[t] = r
				return nil
			}
		}
	}
	return db.ErrNotFound
}
func (f *fakeRadsec) DeleteAP(_ context.Context, t, id int64) (bool, error) {
	f.aps = slices.DeleteFunc(f.aps, func(a db.AP) bool { return a.ID == id })
	noCRL := slices.ContainsFunc(f.brought, func(b db.BroughtCA) bool { return b.TenantID == t && !b.HasCRL })
	f.bump(t, noCRL)
	return noCRL, nil
}
func (f *fakeRadsec) NASIDs(_ context.Context, t int64) ([]string, error) {
	var out []string
	for _, a := range f.aps {
		if a.TenantID == t {
			out = append(out, a.NASID)
		}
	}
	return out, nil
}
func (f *fakeRadsec) BroughtCAs(_ context.Context, t int64) ([]db.BroughtCA, error) {
	var out []db.BroughtCA
	for _, b := range f.brought {
		if b.TenantID == t {
			out = append(out, b)
		}
	}
	return out, nil
}
func (f *fakeRadsec) AddBroughtCA(_ context.Context, b db.BroughtCA, attr, pattern string) error {
	f.brought = append(f.brought, b)
	if pattern != "" {
		r := f.radsec[b.TenantID]
		r.MatchAttr, r.MatchPattern = attr, pattern
		f.radsec[b.TenantID] = r
	}
	return nil
}
func (f *fakeRadsec) SetBroughtHasCRL(_ context.Context, t int64, id string, has bool, _, _ string) error {
	for i, b := range f.brought {
		if b.CAID == id && b.HasCRL != has {
			f.brought[i].HasCRL = has
			f.bump(t, true)
		}
	}
	return nil
}
func (f *fakeRadsec) StartRemoveBrought(_ context.Context, t int64, id string) error {
	for i, b := range f.brought {
		if b.CAID == id {
			f.brought[i].RemovingAt = time.Now()
			f.bump(t, true)
			return nil
		}
	}
	return db.ErrNotFound
}

// radsecEnv は RadSec の画面の試験の環境。
type radsecEnv struct {
	t  *testing.T
	db *fakeDB
	pv *fakePV
	rs *fakeRadsec
}

func newRadsecEnv(t *testing.T) *radsecEnv {
	t.Helper()
	e := &radsecEnv{t: t, db: newFakeDB(), pv: newFakePV(), rs: newFakeRadsec()}
	e.pv.status.Downstreams = append(e.pv.status.Downstreams,
		provisioner.DownstreamStatus{Name: provisioner.DownstreamCA, Reachable: true, Status: jsontext.Value(`{"service":"radsec-ca","version":"1","dnsDomain":"radsec.example.net","urnPrefix":"example"}`)},
		provisioner.DownstreamStatus{Name: provisioner.EdgeDownstream("edge-01"), Reachable: true, Status: jsontext.Value(`{"version":"2"}`)},
		provisioner.DownstreamStatus{Name: provisioner.EdgeDownstream("edge-02"), Reachable: false, Hint: "edge:edge-02 に接続できません。"})
	return e
}

func (e *radsecEnv) as(email string) http.Handler {
	e.t.Helper()
	h, err := New(Options{Log: discard, Version: "test", DB: e.db, Identifier: auth.Dev(email), Provisioner: e.pv, AuditFiles: audit.NewFiles(e.t.TempDir(), 14), Radsec: e.rs})
	if err != nil {
		e.t.Fatal(err)
	}
	return h.Routes()
}

func (e *radsecEnv) enable(tenant int64, slug string) {
	e.t.Helper()
	if _, err := e.rs.EnableRadsec(context.Background(), tenant, slug, "TENANT-CA-PEM", "op"); err != nil {
		e.t.Fatal(err)
	}
}

func (e *radsecEnv) addEdge(id string, capacity int) {
	e.t.Helper()
	e.rs.edges[id] = db.Edge{ID: id, Name: id, Capacity: capacity, PublicAddr: "203.0.113.1", AuthUpstream: "10.0.0.1:1812", AcctUpstream: "10.0.0.1:1813"}
}

func wantStatus(t *testing.T, what string, got, want int, body string) {
	t.Helper()
	if got != want {
		t.Errorf("%s: status %d, want %d: %s", what, got, want, body)
	}
}

// TestRadsecPermissions は画面の口の権限を役割ごとに確かめる（設計概要 §5。OB16）。A・B は自分のテナント 10 だけで、20 は 404。
func TestRadsecPermissions(t *testing.T) {
	e := newRadsecEnv(t)
	e.enable(10, "aaa")
	e.enable(20, "bbb")
	e.addEdge("edge-01", 5)
	type row struct {
		method, path string
		a, b, c, d   int
	}
	const ok, no, nf = 200, 403, 404
	for _, c := range []row{
		{"GET", "/edges", no, no, ok, ok},
		{"GET", "/edges/edge-01", no, no, ok, ok},
		{"GET", "/radsec", no, no, ok, ok},
		{"GET", "/tenants/10/radsec", ok, ok, ok, ok},
		{"GET", "/tenants/20/radsec", nf, nf, ok, ok},
		{"GET", "/tenants/10/aps", ok, ok, ok, ok},
		{"GET", "/tenants/20/aps", nf, nf, ok, ok},
		{"GET", "/tenants/10/aps/seen", ok, ok, ok, ok},
		{"GET", "/tenants/10/brought-cas", no, no, ok, ok},
		{"GET", "/tenants/20/brought-cas", nf, nf, ok, ok},
		{"POST", "/tenants/10/radsec/apply", no, ok, ok, ok},
		{"POST", "/tenants/20/radsec/apply", nf, nf, ok, ok},
		{"POST", "/edges/none/delete", no, no, no, nf},
		{"POST", "/tenants/10/radsec/sources", no, no, ok, ok},
		{"POST", "/tenants/10/radsec/enable", no, no, 400, 400},
		{"POST", "/tenants/10/brought-cas/match", no, no, no, ok},
		{"POST", "/tenants/10/brought-cas/none/remove", no, no, no, nf},
		{"POST", "/tenants/10/aps", no, 400, 400, 400},
		{"POST", "/tenants/20/aps", nf, nf, 400, 400},
		{"GET", "/tenants/10/radsec/server-ca.pem", no, ok, ok, ok},
	} {
		for i, email := range []string{"a@example.com", "b@example.com", "c@example.com", "d@example.com"} {
			h := e.as(email)
			want := []int{c.a, c.b, c.c, c.d}[i]
			var got int
			var body string
			if c.method == "GET" {
				w := get(h, c.path)
				got, body = w.Code, w.Body.String()
			} else {
				w := post(h, c.path, url.Values{})
				got, body = w.Code, w.Body.String()
			}
			if got != want {
				t.Errorf("%s %s as %s: %d, want %d: %s", c.method, c.path, email, got, want, body)
			}
		}
	}
	// ナビゲーション。A・B は自分のテナントの画面、C・D は全テナントの画面とエッジ。
	mustContain(t, get(e.as("a@example.com"), "/").Body.String(), `href="/tenants/10/radsec"`)
	mustNotContain(t, get(e.as("a@example.com"), "/").Body.String(), `href="/edges"`)
	mustContain(t, get(e.as("c@example.com"), "/").Body.String(), `href="/radsec"`, `href="/edges"`)
}

func TestEdgeLedger(t *testing.T) {
	e := newRadsecEnv(t)
	h := e.as("d@example.com")
	form := url.Values{"id": {"edge-01"}, "name": {"東京 1"}, "capacity": {"150"}, "publicAddr": {"203.0.113.10"}, "authUpstream": {"10.20.0.2:1812"}, "acctUpstream": {"10.20.0.2:1813"}}
	// 形の誤り。
	bad := url.Values{"id": {"Edge_1"}, "name": {""}, "capacity": {"0"}, "publicAddr": {"edge.example"}, "authUpstream": {"radius:1812"}, "acctUpstream": {"x"}}
	w := post(h, "/edges", bad)
	wantStatus(t, "bad form", w.Code, 400, w.Body.String())
	mustContain(t, w.Body.String(), "小文字の英数字", "1〜100 文字", "1〜2000", "IP アドレスで", "ホスト名は使えません")
	// 登録簿にない ID は断る（OB11）。
	other := url.Values{}
	for k, v := range form {
		other[k] = v
	}
	other.Set("id", "edge-99")
	w = post(h, "/edges", other)
	wantStatus(t, "not in registry", w.Code, 400, w.Body.String())
	mustContain(t, w.Body.String(), "登録簿にこの ID のエッジがありません")
	// aka-provisioner を読めなければ断る。
	e.pv.statusErr = errors.New("dial")
	w = post(h, "/edges", form)
	wantStatus(t, "provisioner unavailable", w.Code, 502, w.Body.String())
	e.pv.statusErr = nil
	// 足す。
	w = post(h, "/edges", form)
	wantStatus(t, "create", w.Code, 200, w.Body.String())
	mustContain(t, w.Body.String(), "edge-01", "203.0.113.10", "接続中")
	if a := e.db.lastAudit(); a.Action != audit.ActionEdgeCreate || a.Target != "edge-01" {
		t.Errorf("audit = %+v", a)
	}
	w = post(h, "/edges", form)
	wantStatus(t, "duplicate", w.Code, 409, w.Body.String())
	// 詳細に A レコードが出る。
	w = get(h, "/edges/edge-01")
	mustContain(t, w.Body.String(), "edge-01.radsec.example.net → 203.0.113.10")
	// 容量を配置より減らせない。
	e.rs.edges["edge-01"] = func() db.Edge { x := e.rs.edges["edge-01"]; x.Placements = 3; return x }()
	less := url.Values{"name": {"東京 1"}, "capacity": {"2"}, "publicAddr": {"203.0.113.10"}, "authUpstream": {"10.20.0.2:1812"}, "acctUpstream": {"10.20.0.2:1813"}}
	w = post(h, "/edges/edge-01/update", less)
	wantStatus(t, "shrink", w.Code, 409, w.Body.String())
	mustContain(t, w.Body.String(), "今の配置の数（3）")
	w = post(h, "/edges/edge-01/delete", url.Values{})
	wantStatus(t, "delete in use", w.Code, 409, w.Body.String())
	mustContain(t, w.Body.String(), "配置が残っている")
	// C は見られるが変えられない（フォームを出さない）。
	mustNotContain(t, get(e.as("c@example.com"), "/edges").Body.String(), `hx-post="/edges"`)
}

func TestRadsecEnable(t *testing.T) {
	e := newRadsecEnv(t)
	h := e.as("c@example.com")
	w := get(h, "/tenants/10/radsec")
	mustContain(t, w.Body.String(), "有効にしていません", `hx-post="/tenants/10/radsec/enable"`)
	mustNotContain(t, get(e.as("b@example.com"), "/tenants/10/radsec").Body.String(), `hx-post="/tenants/10/radsec/enable"`)

	w = post(h, "/tenants/10/radsec/enable", url.Values{"slug": {"Bad_Slug"}})
	wantStatus(t, "bad slug", w.Code, 400, w.Body.String())
	// 結果が分からなければ同じキーを残し、やり直せる。
	e.pv.errs["ca.create"] = errors.New("dial tcp: connection refused")
	w = post(h, "/tenants/10/radsec/enable", url.Values{"slug": {"acme"}, "idem": {"AAAAAAAAAAAAAAAAAAAAAAAAAA"}})
	wantStatus(t, "unavailable", w.Code, 502, w.Body.String())
	mustContain(t, w.Body.String(), `value="AAAAAAAAAAAAAAAAAAAAAAAAAA"`)
	if _, ok := e.rs.radsec[10]; ok {
		t.Error("the setting was written after a failure")
	}
	// 同じ slug の CA があれば読み直す（DB に書く前に止まったときのやり直し）。
	e.pv.errs["ca.create"] = &provisioner.Error{Status: 409, Problem: provisioner.Problem{Cause: provisioner.CauseCAExists, Downstream: "ca", Detail: "あります"}}
	w = post(h, "/tenants/10/radsec/enable", url.Values{"slug": {"acme"}})
	wantStatus(t, "enable", w.Code, 200, w.Body.String())
	mustContain(t, w.Body.String(), "RadSec を有効にしました（ポート 20000）")
	if r := e.rs.radsec[10]; r.CACertificate != "CA-PEM-tenant.acme" {
		t.Errorf("radsec = %+v", r)
	}
	if a := e.db.lastAudit(); a.Action != audit.ActionRadsecEnable || a.TenantID != 10 {
		t.Errorf("audit = %+v", a)
	}
	// 使った slug は使い回せない。
	w = post(h, "/tenants/20/radsec/enable", url.Values{"slug": {"acme"}})
	wantStatus(t, "same slug", w.Code, 409, w.Body.String())
	mustContain(t, w.Body.String(), "使い回しません")
}

func TestRadsecPlacementAndDNS(t *testing.T) {
	e := newRadsecEnv(t)
	e.enable(10, "acme")
	e.addEdge("edge-01", 1)
	e.addEdge("edge-02", 5)
	h := e.as("c@example.com")
	w := post(h, "/tenants/10/radsec/placements", url.Values{"slot": {"nope"}, "edge": {"edge-01"}})
	wantStatus(t, "bad slot", w.Code, 400, w.Body.String())
	w = post(h, "/tenants/10/radsec/placements", url.Values{"slot": {"primary"}, "edge": {"edge-01"}})
	wantStatus(t, "add", w.Code, 200, w.Body.String())
	// スタッフには CNAME の案内、A・B にはスロットの名前だけ（エッジの ID を見せない。OB16）。
	mustContain(t, w.Body.String(), "primary.acme.radsec.example.net", "CNAME", "edge-01.radsec.example.net", "配置が 1 つだけ")
	body := get(e.as("b@example.com"), "/tenants/10/radsec").Body.String()
	mustContain(t, body, "primary.acme.radsec.example.net")
	mustNotContain(t, body, "edge-01", "CNAME")
	// 容量の超過。
	w = post(h, "/tenants/10/radsec/placements", url.Values{"slot": {"secondary"}, "edge": {"edge-01"}})
	wantStatus(t, "capacity", w.Code, 409, w.Body.String())
	mustContain(t, w.Body.String(), "容量を超えます")
	// 全テナントの画面の DNS のレコード。
	body = get(h, "/radsec").Body.String()
	mustContain(t, body, "edge-01.radsec.example.net", ">A<", "203.0.113.1", "primary.acme.radsec.example.net", "CNAME")
	// 移動: 反映するまで完了できない。向け先の行は新しいエッジになる。
	pid := e.rs.places[0].ID
	w = post(h, "/tenants/10/radsec/placements/"+strconv.FormatInt(pid, 10)+"/move", url.Values{"edge": {"edge-02"}})
	wantStatus(t, "move", w.Code, 200, w.Body.String())
	mustContain(t, w.Body.String(), "edge-02.radsec.example.net", "移動の途中です", "移動を完了")
	newID := e.rs.places[1].ID
	w = post(h, "/tenants/10/radsec/placements/"+strconv.FormatInt(newID, 10)+"/move-finish", url.Values{})
	wantStatus(t, "finish before apply", w.Code, 409, w.Body.String())
	mustContain(t, w.Body.String(), "まだ反映していません")
	e.rs.places[1].AppliedRevision = 1
	w = post(h, "/tenants/10/radsec/placements/"+strconv.FormatInt(newID, 10)+"/move-finish", url.Values{})
	wantStatus(t, "finish", w.Code, 200, w.Body.String())
	if !e.rs.places[0].Removing() {
		t.Errorf("old placement = %+v", e.rs.places[0])
	}
	// 配置の監査ログはエッジの ID を含むので、テナント管理者の種類の絞り込みに出さない（OB16）。
	mustNotContain(t, get(e.as("b@example.com"), "/audit").Body.String(), "radsec.placement")
	// 再起動はエッジを呼ぶ（毎回新しいキー）。ログは読むだけ。
	w = post(h, "/tenants/10/radsec/placements/"+strconv.FormatInt(newID, 10)+"/restart", url.Values{})
	wantStatus(t, "restart", w.Code, 200, w.Body.String())
	if op := e.pv.lastOp(); !strings.HasPrefix(op, "edge.restart.edge-02 c@example.com ") {
		t.Errorf("restart op = %q", op)
	}
	w = get(h, "/tenants/10/radsec/placements/"+strconv.FormatInt(newID, 10)+"/logs")
	mustContain(t, w.Body.String(), "radsecproxy のログ")
	if w := get(h, "/tenants/10/radsec/placements/999/logs"); w.Code != 404 {
		t.Errorf("unknown placement = %d", w.Code)
	}
	// radsec-ca に届かなければ、名前を出せないと言う。
	e.pv.statusErr = errors.New("dial")
	mustContain(t, get(h, "/tenants/10/radsec").Body.String(), "名前を出せません")
	e.pv.statusErr = nil
}

// TestRadsecApply は手動の反映を確かめる（OB1）。AP を足しても反映を求めず、「反映」で求める。
func TestRadsecApply(t *testing.T) {
	e := newRadsecEnv(t)
	e.enable(10, "acme")
	h := e.as("b@example.com")
	mustContain(t, get(h, "/tenants/10/radsec").Body.String(), "未反映の変更があります")
	w := post(h, "/tenants/10/radsec/apply", url.Values{})
	wantStatus(t, "apply", w.Code, 200, w.Body.String())
	mustContain(t, w.Body.String(), "反映を求めました")
	if e.rs.requested != 1 || e.db.lastAudit().Action != audit.ActionRadsecApply {
		t.Errorf("requested %d, audit %+v", e.rs.requested, e.db.lastAudit())
	}
	// 送信元の範囲は C・D で、すぐ反映を求める。形の誤りは断る。
	c := e.as("c@example.com")
	w = post(c, "/tenants/10/radsec/sources", url.Values{"sources": {"not-a-cidr"}})
	wantStatus(t, "bad cidr", w.Code, 400, w.Body.String())
	w = post(c, "/tenants/10/radsec/sources", url.Values{"sources": {"203.0.113.5/24\n198.51.100.0/24, 203.0.113.0/24"}})
	wantStatus(t, "sources", w.Code, 200, w.Body.String())
	if r := e.rs.radsec[10]; !slices.Equal(r.SourceCIDRs, []string{"198.51.100.0/24", "203.0.113.0/24"}) || r.RequestedRevision != r.Revision {
		t.Errorf("radsec = %+v", r)
	}
}

// TestRadsecRetry は失敗した反映の「やり直す」を確かめる（OB2。B・C・D で、A には出さない）。
func TestRadsecRetry(t *testing.T) {
	e := newRadsecEnv(t)
	e.enable(10, "acme")
	if _, err := e.rs.AddPlacement(context.Background(), 10, "edge-01", "primary", "op"); !errors.Is(err, db.ErrNotFound) {
		t.Fatal(err)
	}
	e.rs.edges["edge-01"] = db.Edge{ID: "edge-01", Capacity: 5}
	p, err := e.rs.AddPlacement(context.Background(), 10, "edge-01", "primary", "op")
	if err != nil {
		t.Fatal(err)
	}
	e.rs.places[0].ApplyResult, e.rs.places[0].ApplyError = "rolled_back", "edge-01 のヘルスチェックに失敗しました"
	path := "/tenants/10/radsec/placements/" + strconv.FormatInt(p.ID, 10) + "/retry"
	b := e.as("b@example.com")
	body := get(b, "/tenants/10/radsec").Body.String()
	mustContain(t, body, "やり直す")
	mustContain(t, body, "反映に失敗しました")
	// A・B にはエッジの ID を含みうる誤りの文を見せない。C・D には見せる。
	if strings.Contains(body, "ヘルスチェックに失敗") {
		t.Errorf("the error text was shown to a tenant admin")
	}
	mustContain(t, get(e.as("c@example.com"), "/tenants/10/radsec").Body.String(), "ヘルスチェックに失敗")
	if strings.Contains(get(e.as("a@example.com"), "/tenants/10/radsec").Body.String(), "やり直す") {
		t.Errorf("the retry button was shown to a tenant user")
	}
	w := post(e.as("a@example.com"), path, url.Values{})
	wantStatus(t, "tenant user", w.Code, 403, w.Body.String())
	w = post(b, path, url.Values{})
	wantStatus(t, "retry", w.Code, 200, w.Body.String())
	mustContain(t, w.Body.String(), "やり直しを求めました")
	if !e.rs.places[0].RetryRequested || e.db.lastAudit().Action != audit.ActionRadsecRetry {
		t.Errorf("place %+v, audit %+v", e.rs.places[0], e.db.lastAudit())
	}
	w = post(b, "/tenants/10/radsec/placements/999/retry", url.Values{})
	wantStatus(t, "unknown placement", w.Code, 404, w.Body.String())
}

// TestAPServerCAButton はサーバーCA の「取り出す」を、取り出せる役割だけに出すことを確かめる（G12）。
func TestAPServerCAButton(t *testing.T) {
	e := newRadsecEnv(t)
	e.enable(10, "acme")
	e.rs.aps = []db.AP{{ID: 5, TenantID: 10, Name: "ap1", Kind: db.KindAP, NASID: "acme-ap1"}}
	if w := get(e.as("a@example.com"), "/tenants/10/aps/5"); w.Code != 200 || strings.Contains(w.Body.String(), "server-ca.pem") {
		t.Errorf("tenant user: %d, shows the button: %v", w.Code, strings.Contains(w.Body.String(), "server-ca.pem"))
	}
	mustContain(t, get(e.as("b@example.com"), "/tenants/10/aps/5").Body.String(), "server-ca.pem")
}

// TestRadsecWake は画面の書き込みの後にループを起こすことを確かめる（OB18）。読み出しでは起こさない。
func TestRadsecWake(t *testing.T) {
	e := newRadsecEnv(t)
	e.enable(10, "acme")
	loop := &fakeLoop{}
	h, err := New(Options{Log: discard, Version: "test", DB: e.db, Identifier: auth.Dev("b@example.com"), Provisioner: e.pv, AuditFiles: audit.NewFiles(t.TempDir(), 14), Radsec: e.rs, RadsecLoop: loop})
	if err != nil {
		t.Fatal(err)
	}
	get(h.Routes(), "/tenants/10/radsec")
	if loop.n != 0 {
		t.Errorf("woken by a read: %d", loop.n)
	}
	w := post(h.Routes(), "/tenants/10/radsec/apply", url.Values{})
	wantStatus(t, "apply", w.Code, 200, w.Body.String())
	if loop.n != 1 {
		t.Errorf("woken %d times", loop.n)
	}
}

type fakeLoop struct{ n int }

func (f *fakeLoop) Wake() { f.n++ }

// TestRadsecState は反映の状態の見出しと行の文を確かめる。
func TestRadsecState(t *testing.T) {
	r := db.TenantRadsec{Revision: 5, RequestedRevision: 4}
	for _, c := range []struct {
		name  string
		ps    []db.Placement
		label string
	}{
		{"配置なし", nil, "未反映の変更あり"},
		{"求めた版より新しい版まで届いた", []db.Placement{{AppliedRevision: 5, ApplyResult: "ok"}}, "反映済み"},
		{"求めた版まで", []db.Placement{{AppliedRevision: 4, ApplyResult: "ok"}}, "未反映の変更あり"},
		{"求めた版に届いていない", []db.Placement{{AppliedRevision: 3, ApplyResult: "ok"}}, "反映待ち"},
		{"最初の反映", []db.Placement{{AppliedRevision: 5}, {}}, "反映待ち"},
		{"失敗", []db.Placement{{AppliedRevision: 3, ApplyResult: "failed"}}, "反映の失敗あり"},
		{"送れていない", []db.Placement{{AppliedRevision: 4, ApplyResult: db.ApplyResultSyncError}}, "反映の失敗あり"},
		{"エッジのほうが新しい", []db.Placement{{AppliedRevision: 7, ApplyResult: "ok"}}, "DB より新しい版のエッジあり"},
		{"消している途中は数えない", []db.Placement{{AppliedRevision: 5}, {AppliedRevision: 1, ApplyResult: "failed", DeletedAt: time.Now()}}, "反映済み"},
	} {
		if got := evaluateRadsec(r, c.ps).Label(); got != c.label {
			t.Errorf("%s: label = %q, want %q", c.name, got, c.label)
		}
	}
	rows := placementRows(r, []db.Placement{
		{ID: 1, EdgeID: "edge-02", AppliedRevision: 5},
		{ID: 2, EdgeID: "edge-01", ApplyResult: db.ApplyResultStopped, AppliedRevision: 5},
		{ID: 3, EdgeID: "edge-01", ApplyResult: db.ApplyResultSyncError},
		{ID: 4, EdgeID: "edge-01", ApplyResult: "failed"},
		{ID: 5, EdgeID: "edge-01", ApplyResult: "failed", DeletedAt: time.Now()},
		{ID: 6, EdgeID: "edge-01", ApplyResult: "ok", AppliedRevision: 7},
	}, map[string]bool{"edge-02": true})
	for i, want := range []string{"エッジに届きません", "止めている", "送れていません", "反映に失敗しました", "エッジから消しています", "エッジのほうが新しい版"} {
		if !strings.Contains(rows[i].Label, want) {
			t.Errorf("row %d: %q, want %q", i, rows[i].Label, want)
		}
	}
	if !rows[5].Retryable {
		t.Error("a placement ahead of the database should be retryable")
	}
	if rows[2].Retryable || !rows[3].Retryable || rows[4].Retryable {
		t.Errorf("retryable = %v %v %v", rows[2].Retryable, rows[3].Retryable, rows[4].Retryable)
	}
}

// TestRadsecDashboard はダッシュボードの欄を確かめる（OB21。C・D だけ）。
func TestRadsecDashboard(t *testing.T) {
	e := newRadsecEnv(t)
	e.enable(10, "acme")
	// エッジがまだなければ欄を出さない。
	if strings.Contains(get(e.as("c@example.com"), "/").Body.String(), "RadSec の受け口") {
		t.Error("the card was shown without edges")
	}
	e.rs.edges["edge-01"] = db.Edge{ID: "edge-01", Capacity: 5}
	e.rs.edges["edge-02"] = db.Edge{ID: "edge-02", Capacity: 5}
	e.rs.places = []db.Placement{{ID: 1, TenantID: 10, EdgeID: "edge-01", Slot: "primary", ApplyResult: "failed", ApplyError: "ヘルスチェックに失敗しました", ServerNotAfter: time.Now().Add(24 * time.Hour)}}
	e.rs.expiring = []db.ExpiringAPCert{{TenantID: 10, TenantName: "acme", AP: "ap-soon", Serial: "aa", NotAfter: time.Now().Add(48 * time.Hour)}}
	for i := range e.pv.status.Downstreams {
		if e.pv.status.Downstreams[i].Name == provisioner.EdgeDownstream("edge-01") {
			e.pv.status.Downstreams[i].Status = jsontext.Value(`{"tenants":[{"id":"10","slug":"acme","crls":[{"caId":"tenant.acme","ok":true,"warn":true,"nextUpdate":"2026-11-05T00:00:00Z"}]}]}`)
		}
	}
	body := get(e.as("c@example.com"), "/").Body.String()
	for _, want := range []string{"RadSec の受け口", "届かないエッジ", "/edges/edge-02", "ヘルスチェックに失敗しました", "サーバー証明書", "ap-soon", "tenant.acme"} {
		mustContain(t, body, want)
	}
	if strings.Contains(get(e.as("b@example.com"), "/").Body.String(), "RadSec の受け口") {
		t.Error("the card was shown to a tenant admin")
	}
	// 問題がなければそう言う。
	e.rs.places, e.rs.expiring = nil, nil
	for i := range e.pv.status.Downstreams {
		if strings.HasPrefix(string(e.pv.status.Downstreams[i].Name), "edge:") {
			e.pv.status.Downstreams[i].Reachable, e.pv.status.Downstreams[i].Status = true, jsontext.Value(`{"tenants":[]}`)
		}
	}
	mustContain(t, get(e.as("c@example.com"), "/").Body.String(), "警告はありません")
}

// TestEdgeOrphans はエッジにしかないテナントの表示と削除を確かめる（OB18。消せるのは D）。
func TestEdgeOrphans(t *testing.T) {
	e := newRadsecEnv(t)
	e.enable(10, "acme")
	e.rs.edges["edge-01"] = db.Edge{ID: "edge-01", Capacity: 5}
	if _, err := e.rs.AddPlacement(context.Background(), 10, "edge-01", "primary", "op"); err != nil {
		t.Fatal(err)
	}
	e.pv.edgeTenants = []provisioner.EdgeTenantSummary{{ID: "10", Slug: "acme"}, {ID: "77", Slug: "stale", APCount: 3}}
	c := e.as("c@example.com")
	w := get(c, "/edges/edge-01/orphans")
	wantStatus(t, "orphans", w.Code, 200, w.Body.String())
	mustContain(t, w.Body.String(), "stale")
	if strings.Contains(w.Body.String(), "acme") || strings.Contains(w.Body.String(), "/orphans/77/delete") {
		t.Errorf("body = %s", w.Body.String())
	}
	if w := post(c, "/edges/edge-01/orphans/77/delete", url.Values{}); w.Code != 403 {
		t.Errorf("operator delete = %d", w.Code)
	}
	if w := get(e.as("b@example.com"), "/edges/edge-01/orphans"); w.Code != 403 {
		t.Errorf("tenant admin = %d", w.Code)
	}
	d := e.as("d@example.com")
	mustContain(t, get(d, "/edges/edge-01/orphans").Body.String(), "/orphans/77/delete")
	// 台帳に配置があるテナントは消せない。
	w = post(d, "/edges/edge-01/orphans/10/delete", url.Values{})
	wantStatus(t, "placed", w.Code, 409, w.Body.String())
	w = post(d, "/edges/edge-01/orphans/77/delete", url.Values{})
	wantStatus(t, "delete", w.Code, 200, w.Body.String())
	mustContain(t, w.Body.String(), "消しました")
	if e.db.lastAudit().Action != audit.ActionEdgeOrphanDelete || !slices.ContainsFunc(e.pv.ops, func(op string) bool { return strings.HasPrefix(op, "edge.delete.edge-01") }) {
		t.Errorf("audit %+v, ops %q", e.db.lastAudit(), e.pv.ops)
	}
	// 同じ番号で slug の違うテナントは台帳のものではないので、出して消せる（G11）。
	e.pv.edgeTenants = []provisioner.EdgeTenantSummary{{ID: "10", Slug: "later"}}
	body := get(d, "/edges/edge-01/orphans").Body.String()
	mustContain(t, body, "later", "別です", "/orphans/10/delete")
	w = post(d, "/edges/edge-01/orphans/10/delete", url.Values{})
	wantStatus(t, "other tenant", w.Code, 200, w.Body.String())
	// エッジに届かなければ、その旨を出す。
	e.pv.errs["edge.tenants"] = errors.New("dial")
	mustContain(t, get(c, "/edges/edge-01/orphans").Body.String(), "エッジを読めませんでした")
}

// testCAPEM は試験の CA の証明書を作る。
func testCAPEM(t *testing.T, cn string) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, k.Public(), k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

const testCSRPEM = "-----BEGIN CERTIFICATE REQUEST-----\nMIIB\n-----END CERTIFICATE REQUEST-----\n"

func TestRadsecAPs(t *testing.T) {
	e := newRadsecEnv(t)
	e.enable(10, "acme")
	e.addEdge("edge-01", 5)
	if _, err := e.rs.AddPlacement(context.Background(), 10, "edge-01", "primary", "op"); err != nil {
		t.Fatal(err)
	}
	h := e.as("b@example.com")
	// A は見るだけ。
	a := e.as("a@example.com")
	mustNotContain(t, get(a, "/tenants/10/aps").Body.String(), `hx-post="/tenants/10/aps"`)
	// 形の誤り。
	w := post(h, "/tenants/10/aps", url.Values{"name": {"probe"}, "kind": {"ap"}, "csr": {"x"}})
	wantStatus(t, "probe", w.Code, 400, w.Body.String())
	mustContain(t, w.Body.String(), "予約", "CSR を PEM")
	w = post(h, "/tenants/10/aps", url.Values{"name": {"g1"}, "kind": {"group"}, "csr": {testCSRPEM}})
	wantStatus(t, "no purpose", w.Code, 400, w.Body.String())
	mustContain(t, w.Body.String(), "用途を選んでください")
	// 発行の失敗（4xx）では新しいキーになる。入力は残す。
	e.pv.errs["ca.issue.ap-client"] = &provisioner.Error{Status: 422, Problem: provisioner.Problem{Cause: provisioner.CauseCSRInvalid, Downstream: "ca", Detail: "CSR を読めません"}}
	w = post(h, "/tenants/10/aps", url.Values{"name": {"ap-1"}, "kind": {"ap"}, "csr": {testCSRPEM}, "idem": {"AAAAAAAAAAAAAAAAAAAAAAAAAA"}})
	wantStatus(t, "csr invalid", w.Code, 502, w.Body.String())
	mustContain(t, w.Body.String(), "radsec-ca: CSR を読めません", "BEGIN CERTIFICATE REQUEST")
	mustNotContain(t, w.Body.String(), "AAAAAAAAAAAAAAAAAAAAAAAAAA")
	delete(e.pv.errs, "ca.issue.ap-client")
	// 登録。版は上がるが反映は求めない。
	before := e.rs.radsec[10]
	w = post(h, "/tenants/10/aps", url.Values{"name": {"ap-1"}, "kind": {"ap"}, "csr": {testCSRPEM}, "idem": {"BBBBBBBBBBBBBBBBBBBBBBBBBB"}})
	if w.Code != 200 || !strings.Contains(w.Header().Get("HX-Redirect"), "/tenants/10/aps/") {
		t.Fatalf("create = %d %v %s", w.Code, w.Header(), w.Body)
	}
	if op := e.pv.lastOp(); op != "ca.issue.ap-client b@example.com BBBBBBBBBBBBBBBBBBBBBBBBBB" {
		t.Errorf("issue op = %q", op)
	}
	after := e.rs.radsec[10]
	if after.Revision != before.Revision+1 || after.RequestedRevision != before.RequestedRevision {
		t.Errorf("radsec = %+v", after)
	}
	ap := e.rs.aps[0]
	if ap.NASID != "acme-ap-1" || len(ap.Certs) != 1 {
		t.Errorf("ap = %+v", ap)
	}
	path := "/tenants/10/aps/" + strconv.FormatInt(ap.ID, 10)
	// 詳細: 接続の案内。A には案内と操作を出さない。
	body := get(h, path+"?created=1").Body.String()
	mustContain(t, body, "反映してください", "primary.acme.radsec.example.net", ">20000<", "/radsec/server-ca.pem", "証明書の更新")
	body = get(a, path).Body.String()
	mustContain(t, body, "acme-ap-1")
	mustNotContain(t, body, "接続の案内", "証明書の更新", "/certs/")
	// 名前の重なり。
	w = post(h, "/tenants/10/aps", url.Values{"name": {"ap-1"}, "kind": {"ap"}, "csr": {testCSRPEM}})
	wantStatus(t, "same name", w.Code, 409, w.Body.String())
	// 証明書の取り出し。別の AP の番号は 404。
	serial := ap.Certs[0].Serial
	if w := get(h, path+"/certs/"+serial+"/pem"); w.Code != 200 || w.Body.String() != "CERT-PEM-"+serial || !strings.Contains(w.Header().Get("Content-Disposition"), "acme-ap-1.pem") {
		t.Errorf("pem = %d %v %s", w.Code, w.Header(), w.Body)
	}
	if w := get(h, path+"/certs/ffff/pem"); w.Code != 404 {
		t.Errorf("other serial = %d", w.Code)
	}
	if w := get(a, path+"/certs/"+serial+"/pem"); w.Code != 403 {
		t.Errorf("A pem = %d", w.Code)
	}
	// 更新と古い証明書の失効。
	w = post(h, path+"/renew", url.Values{"csr": {testCSRPEM}})
	wantStatus(t, "renew", w.Code, 200, w.Body.String())
	mustContain(t, w.Body.String(), "新しい証明書を発行しました")
	w = post(h, path+"/certs/"+serial+"/revoke", url.Values{"reason": {"bad"}})
	wantStatus(t, "bad reason", w.Code, 400, w.Body.String())
	w = post(h, path+"/certs/"+serial+"/revoke", url.Values{"reason": {provisioner.RevokeSuperseded}})
	wantStatus(t, "revoke", w.Code, 200, w.Body.String())
	if r := e.rs.radsec[10]; !r.CRLDirty {
		t.Errorf("crl dirty = %+v", r)
	}
	w = post(h, path+"/certs/"+serial+"/revoke", url.Values{"reason": {provisioner.RevokeSuperseded}})
	wantStatus(t, "revoke twice", w.Code, 409, w.Body.String())
	// 接続の状態（エッジの ID を見せない）。
	w = get(h, "/tenants/10/aps/seen")
	mustContain(t, w.Body.String(), "ap-1", "primary", "198.51.100.1", "接続中")
	mustNotContain(t, w.Body.String(), "edge-01")
	// 認可ポリシーの候補。
	// 削除: 失効できなければ消さない。
	e.pv.errs["ca.revoke.cessationOfOperation"] = errors.New("dial")
	w = post(h, path+"/delete", url.Values{})
	wantStatus(t, "delete failed", w.Code, 502, w.Body.String())
	mustContain(t, w.Body.String(), "削除していません")
	if len(e.rs.aps) != 1 {
		t.Error("deleted without revocation")
	}
	delete(e.pv.errs, "ca.revoke.cessationOfOperation")
	w = post(h, path+"/delete", url.Values{})
	wantStatus(t, "delete", w.Code, 200, w.Body.String())
	if len(e.rs.aps) != 0 || e.db.lastAudit().Action != audit.ActionAPDelete {
		t.Errorf("after delete: %+v, %+v", e.rs.aps, e.db.lastAudit())
	}
}

func TestRadsecBroughtCA(t *testing.T) {
	const apiKey = "very-secret-meraki-key"
	e := newRadsecEnv(t)
	e.enable(10, "acme")
	e.rs.radsec[10] = func() db.TenantRadsec {
		r := e.rs.radsec[10]
		r.CACertificate = testCAPEM(t, "acme client CA")
		return r
	}()
	h := e.as("d@example.com")
	vendor := testCAPEM(t, "vendor CA")
	base := url.Values{"name": {"vendor"}, "certPem": {vendor}, "source": {"meraki"}, "merakiOrgId": {"12"}, "merakiApiKey": {apiKey}, "tenantOnly": {"1"}}
	clone := func(kv ...string) url.Values {
		out := url.Values{}
		for k, v := range base {
			out[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			out.Set(kv[i], kv[i+1])
		}
		return out
	}
	// 確認の印がない。API キーは描き直さない。
	w := post(h, "/tenants/10/brought-cas", clone("tenantOnly", ""))
	wantStatus(t, "no confirm", w.Code, 400, w.Body.String())
	mustContain(t, w.Body.String(), "専用の CA であることを確かめて")
	mustNotContain(t, w.Body.String(), apiKey)
	// API キーがない。
	w = post(h, "/tenants/10/brought-cas", clone("merakiApiKey", ""))
	wantStatus(t, "no key", w.Code, 400, w.Body.String())
	// 同じ Subject の CA は画面で断る（E7）。
	w = post(h, "/tenants/10/brought-cas", clone("certPem", testCAPEM(t, "acme client CA")))
	wantStatus(t, "same subject", w.Code, 400, w.Body.String())
	mustContain(t, w.Body.String(), "同じ Subject")
	// CRL なしは了承と照合の形が必須。
	w = post(h, "/tenants/10/brought-cas", clone("source", "none"))
	wantStatus(t, "none", w.Code, 400, w.Body.String())
	mustContain(t, w.Body.String(), "了承して", "業者の証明書の形")
	w = post(h, "/tenants/10/brought-cas", clone("source", "none", "noCrl", "1", "matchAttr", "CN", "matchPattern", "no-anchor"))
	wantStatus(t, "bad match", w.Code, 400, w.Body.String())
	mustContain(t, w.Body.String(), "^ で始めて $ で終えて")
	// radsec-ca の断り（4xx）: 新しいキーになり、キーの値は画面に出ない。
	e.pv.errs["ca.brought"] = &provisioner.Error{Status: 422, Problem: provisioner.Problem{Cause: provisioner.CauseCRLInvalid, Downstream: "ca", Detail: "CRL を取れません"}}
	w = post(h, "/tenants/10/brought-cas", clone("idem", "CCCCCCCCCCCCCCCCCCCCCCCCCC"))
	wantStatus(t, "crl invalid", w.Code, 502, w.Body.String())
	mustContain(t, w.Body.String(), "CRL を取れません")
	mustNotContain(t, w.Body.String(), apiKey, "CCCCCCCCCCCCCCCCCCCCCCCCCC")
	delete(e.pv.errs, "ca.brought")
	// 登録。キーは radsec-ca に渡すだけで、台帳にも監査ログにもない。
	w = post(h, "/tenants/10/brought-cas", clone())
	wantStatus(t, "create", w.Code, 200, w.Body.String())
	mustNotContain(t, w.Body.String(), apiKey)
	if e.pv.lastBrought.CRL.MerakiAPIKey != apiKey || !e.pv.lastBrought.TenantOnlyConfirmed {
		t.Errorf("sent = %+v", e.pv.lastBrought)
	}
	for _, a := range e.db.audit {
		if strings.Contains(detailText(a.Detail), apiKey) {
			t.Errorf("audit has the key: %+v", a)
		}
	}
	if len(e.rs.brought) != 1 || e.rs.brought[0].CAID != "brought.acme.vendor" || e.db.lastAudit().Action != audit.ActionBroughtCreate {
		t.Fatalf("brought = %+v, %+v", e.rs.brought, e.db.lastAudit())
	}
	if strings.Contains(e.rs.brought[0].Certificate, apiKey) {
		t.Error("key in the row")
	}
	// C は見られるが、操作は出さず断る。
	c := e.as("c@example.com")
	mustNotContain(t, get(c, "/tenants/10/brought-cas").Body.String(), `/brought-cas/brought.acme.vendor/remove`, `hx-post="/tenants/10/brought-cas"`)
	if w := post(c, "/tenants/10/brought-cas/brought.acme.vendor/remove", url.Values{}); w.Code != 403 {
		t.Errorf("C remove = %d", w.Code)
	}
	// 取得先を none にすると反映を求める。
	before := e.rs.radsec[10]
	w = post(h, "/tenants/10/brought-cas/brought.acme.vendor/source", url.Values{"source": {"none"}, "noCrl": {"1"}, "matchAttr": {"CN"}, "matchPattern": {"^[0-9a-f]{12}$"}})
	wantStatus(t, "to none", w.Code, 200, w.Body.String())
	mustContain(t, w.Body.String(), "順に反映します")
	if r := e.rs.radsec[10]; r.Revision != before.Revision+1 || r.RequestedRevision != r.Revision {
		t.Errorf("after none = %+v", r)
	}
	// CRL の取り直しと置き直しは radsec-ca を呼ぶ（毎回新しいキー）。
	w = post(h, "/tenants/10/brought-cas/brought.acme.vendor/refresh", url.Values{})
	wantStatus(t, "refresh", w.Code, 200, w.Body.String())
	w = post(h, "/tenants/10/brought-cas/brought.acme.vendor/crl", url.Values{"crlPem": {"x"}})
	wantStatus(t, "bad crl", w.Code, 400, w.Body.String())
	w = post(h, "/tenants/10/brought-cas/brought.acme.vendor/crl", url.Values{"crlPem": {"-----BEGIN X509 CRL-----\nx\n-----END X509 CRL-----"}})
	wantStatus(t, "upload", w.Code, 200, w.Body.String())
	// 照合の形。
	w = post(h, "/tenants/10/brought-cas/match", url.Values{"matchAttr": {"CN"}, "matchPattern": {`^\d+$`}})
	wantStatus(t, "bad pattern", w.Code, 400, w.Body.String())
	// 外すと反映を求める（OB6）。
	w = post(h, "/tenants/10/brought-cas/brought.acme.vendor/remove", url.Values{})
	wantStatus(t, "remove", w.Code, 200, w.Body.String())
	mustContain(t, w.Body.String(), "外している途中")
	if !e.rs.brought[0].Removing() {
		t.Errorf("brought = %+v", e.rs.brought[0])
	}
}

func TestRadsecPolicyCandidates(t *testing.T) {
	e := newRadsecEnv(t)
	e.enable(10, "acme")
	if _, err := e.rs.CreateAP(context.Background(), db.AP{TenantID: 10, Name: "ap-1", Kind: db.KindAP, NASID: "acme-ap-1"}, db.APCert{Serial: "01"}); err != nil {
		t.Fatal(err)
	}
	e.db.put(db.Entry{IMSI: "440100123456789", TenantID: 10, RadiusPresent: true, RadiusState: "active"})
	e.pv.addRadius("440100123456789", "active", 10)
	body := get(e.as("b@example.com"), "/subscribers/440100123456789/policy").Body.String()
	mustContain(t, body, `<option value="acme-ap-1">`, `list="nas-candidates"`)
}
