package db

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func radsecFixture(t *testing.T) (*DB, Tenant) {
	t.Helper()
	d := openTestDB(t)
	if _, err := d.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	tn, err := d.CreateTenant(t.Context(), "acme", Meta{Operator: "op"})
	if err != nil {
		t.Fatal(err)
	}
	return d, tn
}

func testEdge(id string, capacity int) Edge {
	return Edge{ID: id, Name: id + " の名前", Capacity: capacity, PublicAddr: "203.0.113.1", AuthUpstream: "10.0.0.1:1812", AcctUpstream: "10.0.0.1:1813"}
}

func TestRadsecEdges(t *testing.T) {
	d, tn := radsecFixture(t)
	ctx := t.Context()
	if err := d.CreateEdge(ctx, testEdge("edge-01", 1), "op"); err != nil {
		t.Fatal(err)
	}
	if err := d.CreateEdge(ctx, testEdge("edge-01", 1), "op"); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate edge = %v", err)
	}
	if _, err := d.EnableRadsec(ctx, tn.ID, "acme", "PEM", "op"); err != nil {
		t.Fatal(err)
	}
	p, err := d.AddPlacement(ctx, tn.ID, "edge-01", "primary", "op")
	if err != nil {
		t.Fatal(err)
	}
	// 配置が残っていれば消せない。今の配置より容量を減らせない。
	if err := d.DeleteEdge(ctx, "edge-01"); !errors.Is(err, ErrInUse) {
		t.Errorf("delete in use = %v", err)
	}
	e := testEdge("edge-01", 0)
	if _, err := d.UpdateEdge(ctx, e); !errors.Is(err, ErrCapacity) {
		t.Errorf("shrink = %v", err)
	}
	// 上流を変えると、置いているテナントの版が上がって反映を求める。上流が同じなら上がらない。
	before, _ := d.TenantRadsec(ctx, tn.ID)
	e = testEdge("edge-01", 5)
	if _, err := d.UpdateEdge(ctx, e); err != nil {
		t.Fatal(err)
	}
	if after, _ := d.TenantRadsec(ctx, tn.ID); after.Revision != before.Revision {
		t.Errorf("revision changed without an upstream change: %d", after.Revision)
	}
	e.AuthUpstream = "10.0.0.2:1812"
	if _, err := d.UpdateEdge(ctx, e); err != nil {
		t.Fatal(err)
	}
	after, _ := d.TenantRadsec(ctx, tn.ID)
	if after.Revision != before.Revision+1 || after.RequestedRevision != after.Revision {
		t.Errorf("after upstream change = %+v", after)
	}
	// 外してエッジから消し終えれば消せる。消した ID は登録し直せる。
	if _, err := d.RemovePlacement(ctx, tn.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteEdge(ctx, "edge-01"); !errors.Is(err, ErrInUse) {
		t.Errorf("delete while removing = %v", err)
	}
	if _, err := d.Pool.Exec(ctx, "UPDATE placements SET removed_at = now() WHERE id = $1", p.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteEdge(ctx, "edge-01"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.EdgeByID(ctx, "edge-01"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted edge = %v", err)
	}
	if err := d.CreateEdge(ctx, testEdge("edge-01", 2), "op2"); err != nil {
		t.Errorf("recreate = %v", err)
	}
	if got, err := d.EdgeByID(ctx, "edge-01"); err != nil || got.Capacity != 2 || got.CreatedBy != "op2" {
		t.Errorf("recreated = %+v, %v", got, err)
	}
}

func TestRadsecEnableAndPorts(t *testing.T) {
	d, tn := radsecFixture(t)
	ctx := t.Context()
	r, err := d.EnableRadsec(ctx, tn.ID, "acme", "PEM", "op")
	if err != nil || r.Port != RadsecPortMin || r.Revision != 1 || r.RequestedRevision != 0 {
		t.Fatalf("enable = %+v, %v", r, err)
	}
	if _, err := d.EnableRadsec(ctx, tn.ID, "other", "PEM", "op"); !errors.Is(err, ErrConflict) {
		t.Errorf("twice = %v", err)
	}
	other, _ := d.CreateTenant(ctx, "other", Meta{})
	if _, err := d.EnableRadsec(ctx, other.ID, "acme", "PEM", "op"); !errors.Is(err, ErrConflict) {
		t.Errorf("same slug = %v", err)
	}
	r2, err := d.EnableRadsec(ctx, other.ID, "other", "PEM", "op")
	if err != nil || r2.Port != RadsecPortMin+1 {
		t.Fatalf("second = %+v, %v", r2, err)
	}
	// テナントを削除するとポートは空き、slug は使い回さない。
	if err := d.DeleteTenant(ctx, tn.ID, Meta{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.TenantRadsec(ctx, tn.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("released = %v", err)
	}
	third, _ := d.CreateTenant(ctx, "third", Meta{})
	if _, err := d.EnableRadsec(ctx, third.ID, "acme", "PEM", "op"); !errors.Is(err, ErrConflict) {
		t.Errorf("reused slug = %v", err)
	}
	r3, err := d.EnableRadsec(ctx, third.ID, "third", "PEM", "op")
	if err != nil || r3.Port != RadsecPortMin {
		t.Errorf("reused port = %+v, %v", r3, err)
	}
	if used, err := d.SlugUsed(ctx, "acme"); err != nil || !used {
		t.Errorf("slug used = %v, %v", used, err)
	}
}

func TestRadsecPlacementsAndMove(t *testing.T) {
	d, tn := radsecFixture(t)
	ctx := t.Context()
	if _, err := d.EnableRadsec(ctx, tn.ID, "acme", "PEM", "op"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"edge-01", "edge-02", "edge-03"} {
		if err := d.CreateEdge(ctx, testEdge(id, 1), "op"); err != nil {
			t.Fatal(err)
		}
	}
	p1, err := d.AddPlacement(ctx, tn.ID, "edge-01", "primary", "op")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.AddPlacement(ctx, tn.ID, "edge-02", "primary", "op"); !errors.Is(err, ErrConflict) {
		t.Errorf("same slot = %v", err)
	}
	other, _ := d.CreateTenant(ctx, "other", Meta{})
	if _, err := d.EnableRadsec(ctx, other.ID, "other", "PEM", "op"); err != nil {
		t.Fatal(err)
	}
	// 容量は 1。
	if _, err := d.AddPlacement(ctx, other.ID, "edge-01", "primary", "op"); !errors.Is(err, ErrCapacity) {
		t.Errorf("over capacity = %v", err)
	}
	if err := d.CreateEdge(ctx, testEdge("edge-09", 9), "op"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AddPlacement(ctx, tn.ID, "edge-09", "secondary", "op"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AddPlacement(ctx, tn.ID, "edge-09", "tertiary", "op"); !errors.Is(err, ErrConflict) {
		t.Errorf("same edge = %v", err)
	}

	// 移動: 同じスロットに新しい配置を足す。途中は外せない。反映するまで完了できない。
	mv, err := d.StartMove(ctx, tn.ID, p1.ID, "edge-02", "op")
	if err != nil || mv.Slot != "primary" || mv.Replaces != p1.ID {
		t.Fatalf("start move = %+v, %v", mv, err)
	}
	if _, err := d.StartMove(ctx, tn.ID, p1.ID, "edge-03", "op"); !errors.Is(err, ErrInUse) {
		t.Errorf("double move = %v", err)
	}
	if _, err := d.RemovePlacement(ctx, tn.ID, p1.ID); !errors.Is(err, ErrInUse) {
		t.Errorf("remove during move = %v", err)
	}
	if _, err := d.FinishMove(ctx, tn.ID, mv.ID); !errors.Is(err, ErrNotReady) {
		t.Errorf("finish before apply = %v", err)
	}
	if _, err := d.Pool.Exec(ctx, "UPDATE placements SET applied_revision = 3 WHERE id = $1", mv.ID); err != nil {
		t.Fatal(err)
	}
	old, err := d.FinishMove(ctx, tn.ID, mv.ID)
	if err != nil || old.ID != p1.ID {
		t.Fatalf("finish move = %+v, %v", old, err)
	}
	ps, err := d.Placements(ctx, tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	var edges []string
	for _, p := range ps {
		edges = append(edges, p.EdgeID)
		if p.Removing() != (p.ID == p1.ID) || p.Replaces != 0 {
			t.Errorf("placement = %+v", p)
		}
	}
	if !slices.Equal(edges, []string{"edge-01", "edge-02", "edge-09"}) {
		t.Errorf("edges = %v", edges)
	}

	// やめる: 新しい配置だけ外す。
	mv2, err := d.StartMove(ctx, tn.ID, mv.ID, "edge-03", "op")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CancelMove(ctx, tn.ID, mv2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CancelMove(ctx, tn.ID, mv.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cancel a normal placement = %v", err)
	}

	// 配置が残っているテナントは削除できない。
	if err := d.DeleteTenant(ctx, tn.ID, Meta{}); !errors.Is(err, ErrInUse) {
		t.Errorf("delete tenant with placements = %v", err)
	}
	rows, err := d.RadsecOverviews(ctx)
	if err != nil || len(rows) != 2 || rows[0].TenantName != "acme" || len(rows[0].Placements) != 4 {
		t.Errorf("overviews = %+v, %v", rows, err)
	}
}

func TestRadsecAPs(t *testing.T) {
	d, tn := radsecFixture(t)
	ctx := t.Context()
	r, err := d.EnableRadsec(ctx, tn.ID, "a", "PEM", "op")
	if err != nil {
		t.Fatal(err)
	}
	cert := func(s string) APCert { return APCert{Serial: s, NotAfter: time.Now().Add(time.Hour), IssuedBy: "op"} }
	ap := AP{TenantID: tn.ID, Name: "b-c", Kind: KindAP, NASID: "a-b-c", CreatedBy: "op"}
	got, err := d.CreateAP(ctx, ap, cert("01"))
	if err != nil {
		t.Fatal(err)
	}
	after, _ := d.TenantRadsec(ctx, tn.ID)
	if after.Revision != r.Revision+1 || after.RequestedRevision != 0 {
		t.Errorf("after create = %+v", after)
	}
	if _, err := d.CreateAP(ctx, ap, cert("02")); !errors.Is(err, ErrConflict) {
		t.Errorf("same name = %v", err)
	}
	// slug に - があるので、NAS-Identifier は別のテナントと重なりうる（OB8）。
	other, _ := d.CreateTenant(ctx, "other", Meta{})
	if _, err := d.EnableRadsec(ctx, other.ID, "a-b", "PEM", "op"); err != nil {
		t.Fatal(err)
	}
	if err := d.CheckAPName(ctx, other.ID, "c", "a-b-c"); !errors.Is(err, ErrNASIDUsed) {
		t.Errorf("same NAS-Identifier = %v", err)
	}
	// グループの用途の組み合わせは表の制約で守る。
	if _, err := d.CreateAP(ctx, AP{TenantID: tn.ID, Name: "g", Kind: KindGroup, NASID: "a-g"}, cert("03")); err == nil {
		t.Error("group without purpose: want error")
	}
	if _, err := d.CreateAP(ctx, AP{TenantID: tn.ID, Name: "probe", Kind: KindAP, NASID: "a-probe"}, cert("04")); err == nil {
		t.Error("reserved name: want error")
	}
	if _, err := d.CreateAP(ctx, AP{TenantID: tn.ID, Name: "g", Kind: KindGroup, Purpose: "unifi", NASID: "a-g"}, cert("05")); err != nil {
		t.Fatal(err)
	}

	// 更新は証明書を足し、再送は何もしない。
	rev := func() int64 { x, _ := d.TenantRadsec(ctx, tn.ID); return x.Revision }
	before := rev()
	if err := d.AddAPCert(ctx, tn.ID, got.ID, cert("06")); err != nil {
		t.Fatal(err)
	}
	if err := d.AddAPCert(ctx, tn.ID, got.ID, cert("06")); err != nil || rev() != before+1 {
		t.Errorf("replayed cert: %v, revision %d", err, rev())
	}
	if err := d.MarkAPCertRevoked(ctx, tn.ID, got.ID, "01", "superseded"); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkAPCertRevoked(ctx, tn.ID, got.ID, "01", "superseded"); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoke twice = %v", err)
	}
	if x, _ := d.TenantRadsec(ctx, tn.ID); !x.CRLDirty {
		t.Error("crl_dirty is not set")
	}
	a, err := d.APByID(ctx, tn.ID, got.ID)
	if err != nil || len(a.Certs) != 2 {
		t.Fatalf("ap = %+v, %v", a, err)
	}
	list, err := d.APs(ctx, tn.ID)
	if err != nil || len(list) != 2 || list[0].Name != "b-c" || len(list[0].Certs) != 2 {
		t.Errorf("aps = %+v, %v", list, err)
	}
	if ids, err := d.NASIDs(ctx, tn.ID); err != nil || !slices.Equal(ids, []string{"a-b-c", "a-g"}) {
		t.Errorf("nas ids = %v, %v", ids, err)
	}

	// 削除は、CRL があれば反映を求めず、名前を使い回せる。
	x, _ := d.TenantRadsec(ctx, tn.ID)
	requested, err := d.DeleteAP(ctx, tn.ID, got.ID)
	if err != nil || requested {
		t.Fatalf("delete = %v, %v", requested, err)
	}
	if y, _ := d.TenantRadsec(ctx, tn.ID); y.Revision != x.Revision+1 || y.RequestedRevision != x.RequestedRevision {
		t.Errorf("after delete = %+v", y)
	}
	if err := d.CheckAPName(ctx, tn.ID, "b-c", "a-b-c"); err != nil {
		t.Errorf("reuse with CRL = %v", err)
	}

	// CRL のないテナントでは、削除で反映を求め、削除した名前を使い回せない（OB5）。
	if err := d.AddBroughtCA(ctx, BroughtCA{CAID: "brought.a.v", Name: "v", TenantID: tn.ID, Certificate: "PEM", Subject: "CN=v", HasCRL: false, CreatedBy: "op"}, "CN", "^x$"); err != nil {
		t.Fatal(err)
	}
	if err := d.CheckAPName(ctx, tn.ID, "b-c", "a-b-c"); !errors.Is(err, ErrNameRetired) {
		t.Errorf("reuse without CRL = %v", err)
	}
	list, _ = d.APs(ctx, tn.ID)
	requested, err = d.DeleteAP(ctx, tn.ID, list[0].ID)
	if err != nil || !requested {
		t.Errorf("delete without CRL = %v, %v", requested, err)
	}
	if y, _ := d.TenantRadsec(ctx, tn.ID); y.RequestedRevision != y.Revision {
		t.Errorf("after delete without CRL = %+v", y)
	}
	if err := d.DeleteTenant(ctx, tn.ID, Meta{}); !errors.Is(err, ErrInUse) {
		t.Errorf("delete tenant with a brought CA = %v", err)
	}
}

func TestRadsecBroughtCA(t *testing.T) {
	d, tn := radsecFixture(t)
	ctx := t.Context()
	if _, err := d.EnableRadsec(ctx, tn.ID, "acme", "PEM", "op"); err != nil {
		t.Fatal(err)
	}
	b := BroughtCA{CAID: "brought.acme.v", Name: "v", TenantID: tn.ID, Certificate: "PEM", Subject: "CN=v", HasCRL: true, CreatedBy: "op"}
	if err := d.AddBroughtCA(ctx, b, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := d.AddBroughtCA(ctx, b, "", ""); !errors.Is(err, ErrConflict) {
		t.Errorf("twice = %v", err)
	}
	x, _ := d.TenantRadsec(ctx, tn.ID)
	if x.RequestedRevision != 0 {
		t.Errorf("adding requested apply: %+v", x)
	}
	// CRL の有無を替えると、形を置いて反映を求める。
	if err := d.SetBroughtHasCRL(ctx, tn.ID, b.CAID, false, "CN", "^x$"); err != nil {
		t.Fatal(err)
	}
	y, _ := d.TenantRadsec(ctx, tn.ID)
	if y.Revision != x.Revision+1 || y.RequestedRevision != y.Revision || y.MatchPattern != "^x$" {
		t.Errorf("after none = %+v", y)
	}
	// CRL のない CA があるときに形は消せない。
	if err := d.SetSharedMatch(ctx, tn.ID, "", ""); !errors.Is(err, ErrInUse) {
		t.Errorf("clear match = %v", err)
	}
	// 外すと版が上がり、反映を求め、外した版を覚える。もう一度外しても何も起きない。
	if err := d.StartRemoveBrought(ctx, tn.ID, b.CAID); err != nil {
		t.Fatal(err)
	}
	z, _ := d.TenantRadsec(ctx, tn.ID)
	list, err := d.BroughtCAs(ctx, tn.ID)
	if err != nil || len(list) != 1 || !list[0].Removing() || list[0].RemovingRevision != z.Revision || z.RequestedRevision != z.Revision {
		t.Fatalf("after remove = %+v, %+v, %v", list, z, err)
	}
	if err := d.StartRemoveBrought(ctx, tn.ID, b.CAID); err != nil {
		t.Fatal(err)
	}
	if w, _ := d.TenantRadsec(ctx, tn.ID); w.Revision != z.Revision {
		t.Errorf("remove twice changed the revision: %d", w.Revision)
	}
	// 外している CA は CRL なしに数えない。形を消せる。
	if err := d.SetSharedMatch(ctx, tn.ID, "", ""); err != nil {
		t.Errorf("clear match after remove = %v", err)
	}
	if err := d.StartRemoveBrought(ctx, tn.ID, "brought.acme.none"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown = %v", err)
	}
}

func TestRadsecSourcesAndApply(t *testing.T) {
	d, tn := radsecFixture(t)
	ctx := t.Context()
	if _, err := d.EnableRadsec(ctx, tn.ID, "acme", "PEM", "op"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SetRadsecSources(ctx, tn.ID, []string{"203.0.113.0/24"}); err != nil {
		t.Fatal(err)
	}
	x, _ := d.TenantRadsec(ctx, tn.ID)
	if x.Revision != 2 || x.RequestedRevision != 2 || !slices.Equal(x.SourceCIDRs, []string{"203.0.113.0/24"}) {
		t.Errorf("sources = %+v", x)
	}
	if before, err := d.SetRadsecSources(ctx, tn.ID, []string{"203.0.113.0/24"}); err != nil || len(before) != 1 {
		t.Errorf("same sources = %v, %v", before, err)
	}
	if y, _ := d.TenantRadsec(ctx, tn.ID); y.Revision != 2 {
		t.Errorf("same value bumped: %d", y.Revision)
	}
	if _, err := d.SetRadsecSources(ctx, tn.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := d.SetSharedMatch(ctx, tn.ID, "CN", "^x$"); err != nil {
		t.Fatal(err)
	}
	if y, _ := d.TenantRadsec(ctx, tn.ID); y.Revision != 4 || y.RequestedRevision != 3 {
		t.Errorf("match = %+v", y)
	}
	if rev, err := d.RequestApply(ctx, tn.ID); err != nil || rev != 4 {
		t.Errorf("request = %d, %v", rev, err)
	}
	if y, _ := d.TenantRadsec(ctx, tn.ID); y.RequestedRevision != 4 {
		t.Errorf("after request = %+v", y)
	}
	if _, err := d.RequestApply(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown tenant = %v", err)
	}
}

func TestRadsecCountsAt(t *testing.T) {
	d, tn := radsecFixture(t)
	ctx := t.Context()
	if _, err := d.EnableRadsec(ctx, tn.ID, "acme", "PEM", "op"); err != nil {
		t.Fatal(err)
	}
	if err := d.CreateEdge(ctx, testEdge("edge-01", 5), "op"); err != nil {
		t.Fatal(err)
	}
	if err := d.CreateEdge(ctx, testEdge("edge-02", 5), "op"); err != nil {
		t.Fatal(err)
	}
	cert := func(s string) APCert { return APCert{Serial: s, NotAfter: time.Now().Add(time.Hour)} }
	a1, err := d.CreateAP(ctx, AP{TenantID: tn.ID, Name: "a1", Kind: KindAP, NASID: "acme-a1"}, cert("01"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateAP(ctx, AP{TenantID: tn.ID, Name: "a2", Kind: KindAP, NASID: "acme-a2"}, cert("02")); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateAP(ctx, AP{TenantID: tn.ID, Name: "g1", Kind: KindGroup, Purpose: "box", NASID: "acme-g1"}, cert("03")); err != nil {
		t.Fatal(err)
	}
	if err := d.AddBroughtCA(ctx, BroughtCA{CAID: "brought.acme.v", Name: "v", TenantID: tn.ID, Certificate: "PEM", Subject: "CN=v", HasCRL: true}, "", ""); err != nil {
		t.Fatal(err)
	}
	p, err := d.AddPlacement(ctx, tn.ID, "edge-01", "primary", "op")
	if err != nil {
		t.Fatal(err)
	}
	// 移動の途中は両方を数える。
	if _, err := d.StartMove(ctx, tn.ID, p.ID, "edge-02", "op"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.DeleteAP(ctx, tn.ID, a1.ID); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	c, err := d.RadsecCountsAt(ctx, future)
	if err != nil {
		t.Fatal(err)
	}
	if got := c[tn.ID]; got != (RadsecCount{APs: 1, Groups: 1, BroughtCAs: 1, Placements: 2}) {
		t.Errorf("counts = %+v", got)
	}
	// 消した時刻より前の時点では、消したものも数える。
	if _, err := d.Pool.Exec(ctx, "UPDATE aps SET deleted_at = $2 WHERE id = $1", a1.ID, future); err != nil {
		t.Fatal(err)
	}
	if c, _ := d.RadsecCountsAt(ctx, future.Add(-time.Minute)); c[tn.ID].APs != 2 {
		t.Errorf("before deletion = %+v", c[tn.ID])
	}
	// 作る前の時点では数えない。
	if c, _ := d.RadsecCountsAt(ctx, time.Now().Add(-time.Hour)); len(c) != 0 {
		t.Errorf("before creation = %+v", c)
	}
	// 13 か月より古い消した行は消す。
	if _, err := d.Pool.Exec(ctx, "UPDATE aps SET deleted_at = now() - interval '14 months' WHERE id = $1", a1.ID); err != nil {
		t.Fatal(err)
	}
	pc, err := d.PurgeOld(ctx, RetentionCutoff(time.Now()))
	if err != nil || pc.Radsec != 1 {
		t.Errorf("purge = %+v, %v", pc, err)
	}
}

// TestRadsecLoopState は中央の整合のループの DB の操作を確かめる（段階 4-4）。
func TestRadsecLoopState(t *testing.T) {
	d, tn := radsecFixture(t)
	ctx := t.Context()
	if err := d.CreateEdge(ctx, testEdge("edge-01", 5), "op"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.EnableRadsec(ctx, tn.ID, "acme", "PEM", "op"); err != nil {
		t.Fatal(err)
	}
	p, err := d.AddPlacement(ctx, tn.ID, "edge-01", "primary", "op")
	if err != nil {
		t.Fatal(err)
	}
	if p.ServerSerial != "" || !p.ServerNotAfter.IsZero() || p.CertGeneration != 0 || p.CertApplyNeeded || len(p.SupersededSerials) != 0 {
		t.Errorf("new placement = %+v", p)
	}
	read := func() Placement {
		t.Helper()
		ps, err := d.Placements(ctx, tn.ID)
		if err != nil || len(ps) != 1 {
			t.Fatalf("placements = %+v, %v", ps, err)
		}
		return ps[0]
	}

	// 状態の写し。
	if err := d.SetPlacementApply(ctx, p.ID, 3, 4, "rolled_back", "誤り"); err != nil {
		t.Fatal(err)
	}
	if got := read(); got.AppliedRevision != 3 || got.FailedRevision != 4 || got.ApplyResult != "rolled_back" || got.ApplyError != "誤り" || got.CheckedAt.IsZero() {
		t.Errorf("state = %+v", got)
	}
	before := read().CheckedAt
	if err := d.TouchPlacements(ctx, "edge-01"); err != nil {
		t.Fatal(err)
	}
	if got := read(); got.CheckedAt.Before(before) {
		t.Errorf("checked at %v before %v", got.CheckedAt, before)
	}

	// 証明書の控えと世代。最初の配置では失効の待ちに入らない。同じ証明書を置き直しても入らない。新しい証明書で古いものが入る。
	at := time.Date(2027, 11, 2, 0, 0, 0, 0, time.UTC)
	if err := d.PlacementCertPlaced(ctx, p.ID, CertKindServer, "s1", at); err != nil {
		t.Fatal(err)
	}
	if err := d.PlacementCertPlaced(ctx, p.ID, CertKindServer, "s1", at); err != nil {
		t.Fatal(err)
	}
	if got := read(); got.ServerSerial != "s1" || !got.ServerNotAfter.Equal(at) || got.CertGeneration != 2 || !got.CertApplyNeeded || len(got.SupersededSerials) != 0 {
		t.Errorf("placed = %+v", got)
	}
	if err := d.PlacementCertPlaced(ctx, p.ID, CertKindServer, "s2", at); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordPlacementCert(ctx, p.ID, CertKindProbe, "p1", at); err != nil {
		t.Fatal(err)
	}
	if err := d.BumpCertGeneration(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if got := read(); got.ServerSerial != "s2" || !slices.Equal(got.SupersededSerials, []string{"s1"}) || got.ProbeSerial != "p1" || !got.ProbeNotAfter.Equal(at) || got.CertGeneration != 4 {
		t.Errorf("replaced = %+v", got)
	}
	if err := d.FinishCertApply(ctx, p.ID, []string{"s1"}); err != nil {
		t.Fatal(err)
	}
	if got := read(); got.CertApplyNeeded || !slices.Equal(got.SupersededSerials, []string{"s1"}) {
		t.Errorf("finish = %+v", got)
	}
	if err := d.FinishCertApply(ctx, p.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got := read(); len(got.SupersededSerials) != 0 {
		t.Errorf("finish = %+v", got)
	}

	// 更新の印は期限の近いものだけ。もう一度数えない。
	if n, err := d.MarkRenewals(ctx, at.Add(-time.Hour)); err != nil || n != 0 {
		t.Errorf("mark = %d, %v", n, err)
	}
	if n, err := d.MarkRenewals(ctx, at.Add(time.Hour)); err != nil || n != 1 {
		t.Errorf("mark = %d, %v", n, err)
	}
	if n, err := d.MarkRenewals(ctx, at.Add(time.Hour)); err != nil || n != 0 {
		t.Errorf("mark again = %d, %v", n, err)
	}
	if !read().RenewRequested {
		t.Error("not marked")
	}
	if err := d.ClearRenew(ctx, p.ID); err != nil || read().RenewRequested {
		t.Errorf("clear renew: %v", err)
	}

	// やり直しの印。別のテナントの配置には立てられない。
	if err := d.RequestRetry(ctx, tn.ID+1, p.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("other tenant: %v", err)
	}
	if err := d.RequestRetry(ctx, tn.ID, p.ID); err != nil || !read().RetryRequested {
		t.Errorf("retry: %v", err)
	}
	if err := d.ClearRetry(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if got := read(); got.RetryRequested || got.FailedRevision != 0 {
		t.Errorf("clear retry = %+v", got)
	}

	// CRL の印は読んだ版のときだけ消す。
	if err := d.MarkAPCertRevokedForTest(ctx, tn.ID); err != nil {
		t.Fatal(err)
	}
	r, _ := d.TenantRadsec(ctx, tn.ID)
	if !r.CRLDirty {
		t.Fatal("not dirty")
	}
	if err := d.ClearCRLDirty(ctx, tn.ID, r.Revision-1); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.TenantRadsec(ctx, tn.ID); !got.CRLDirty {
		t.Error("cleared with an old revision")
	}
	if err := d.ClearCRLDirty(ctx, tn.ID, r.Revision); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.TenantRadsec(ctx, tn.ID); got.CRLDirty {
		t.Error("not cleared")
	}

	// 持ち込みの CA を外す。
	if err := d.AddBroughtCA(ctx, BroughtCA{CAID: "brought.acme.v", Name: "v", TenantID: tn.ID, Certificate: "V", Subject: "CN=v", HasCRL: true, CreatedBy: "op"}, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := d.StartRemoveBrought(ctx, tn.ID, "brought.acme.v"); err != nil {
		t.Fatal(err)
	}
	list, err := d.RemovingBroughtCAs(ctx)
	if err != nil || len(list) != 1 || list[0].CAID != "brought.acme.v" {
		t.Fatalf("removing = %+v, %v", list, err)
	}
	if err := d.FinishRemoveBrought(ctx, "brought.acme.v"); err != nil {
		t.Fatal(err)
	}
	if list, err := d.RemovingBroughtCAs(ctx); err != nil || len(list) != 0 {
		t.Errorf("after finish = %+v, %v", list, err)
	}
	if list, err := d.BroughtCAs(ctx, tn.ID); err != nil || len(list) != 0 {
		t.Errorf("brought = %+v, %v", list, err)
	}

	// AP・グループの期限は、有効な証明書のうち一番新しいもので見る。
	ap, err := d.CreateAP(ctx, AP{TenantID: tn.ID, Name: "ap1", Kind: KindAP, NASID: "acme-ap1", CreatedBy: "op"}, APCert{Serial: "a1", NotAfter: at})
	if err != nil {
		t.Fatal(err)
	}
	if list, err := d.ExpiringAPCerts(ctx, at.Add(time.Hour)); err != nil || len(list) != 1 || list[0].Serial != "a1" || list[0].TenantName == "" || list[0].AP != "ap1" {
		t.Errorf("expiring = %+v, %v", list, err)
	}
	if err := d.AddAPCert(ctx, tn.ID, ap.ID, APCert{Serial: "a2", NotAfter: at.AddDate(1, 0, 0)}); err != nil {
		t.Fatal(err)
	}
	if list, err := d.ExpiringAPCerts(ctx, at.Add(time.Hour)); err != nil || len(list) != 0 {
		t.Errorf("renewed = %+v, %v", list, err)
	}

	// 配置を消し終えると一覧から消え、テナントを消して CA を止める待ちに入る。
	if _, err := d.DeleteAP(ctx, tn.ID, ap.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.RemovePlacement(ctx, tn.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkPlacementRemoved(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if ps, err := d.Placements(ctx, tn.ID); err != nil || len(ps) != 0 {
		t.Errorf("placements = %+v, %v", ps, err)
	}
	if list, err := d.RadsecCADisablePending(ctx); err != nil || len(list) != 0 {
		t.Errorf("pending = %+v, %v", list, err)
	}
	if err := d.DeleteTenant(ctx, tn.ID, Meta{Operator: "op"}); err != nil {
		t.Fatal(err)
	}
	list2, err := d.RadsecCADisablePending(ctx)
	if err != nil || len(list2) != 1 || list2[0].Slug != "acme" {
		t.Fatalf("pending = %+v, %v", list2, err)
	}
	if err := d.MarkRadsecCADisabled(ctx, tn.ID); err != nil {
		t.Fatal(err)
	}
	if list, err := d.RadsecCADisablePending(ctx); err != nil || len(list) != 0 {
		t.Errorf("pending after = %+v, %v", list, err)
	}
}

// MarkAPCertRevokedForTest は CRL の印を立て、版を上げる（失効の操作と同じ書き方）。
func (d *DB) MarkAPCertRevokedForTest(ctx context.Context, tenant int64) error {
	return d.radsecTx(ctx, "test", tenant, func(tx pgx.Tx, _ TenantRadsec) error {
		if _, err := tx.Exec(ctx, "UPDATE tenant_radsec SET crl_dirty = true WHERE tenant_id = $1", tenant); err != nil {
			return err
		}
		return bumpRevision(ctx, tx, tenant, false)
	})
}
