package web

import (
	"context"
	"encoding/json/jsontext"
	"errors"
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
		{"POST", "/tenants/10/radsec/apply", no, ok, ok, ok},
		{"POST", "/tenants/20/radsec/apply", nf, nf, ok, ok},
		{"POST", "/edges/none/delete", no, no, no, nf},
		{"POST", "/tenants/10/radsec/sources", no, no, ok, ok},
		{"POST", "/tenants/10/radsec/enable", no, no, 400, 400},
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
