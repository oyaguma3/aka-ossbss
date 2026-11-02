package radsec

import (
	"cmp"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/oyaguma3/aka-ossbss/internal/audit"
	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner/provisionertest"
)

// TestIntegrationLoop は radsec-ca とエッジの実物を相手に、ループを宣言から反映・証明書の更新・削除まで動かす（段階 4-4 の OB23）。
// aka-provisioner の scripts/contract.sh に CA_SRC・EDGE_SRC を与えて起動したものにつなぐ。OSSBSS_TEST_EDGE_ID を指定したときだけ動く。
// DB はメモリーの代わり（DB の操作は internal/db の結合試験で確かめる）。テナントの名前とポートは contract.sh の片付けと公開の口に合わせる。
func TestIntegrationLoop(t *testing.T) {
	edge := os.Getenv("OSSBSS_TEST_EDGE_ID")
	u := os.Getenv("OSSBSS_TEST_PROVISIONER_URL")
	if edge == "" || u == "" {
		t.Skip("OSSBSS_TEST_EDGE_ID or OSSBSS_TEST_PROVISIONER_URL is not set")
	}
	pv, err := provisioner.New(provisioner.Options{BaseURL: u, ClientCertFile: os.Getenv("OSSBSS_TEST_PROVISIONER_CLIENT_CERT"), ServerCertFile: os.Getenv("OSSBSS_TEST_PROVISIONER_SERVER_CERT"),
		Timeout: 30 * time.Second, RadsecTimeout: 40 * time.Second, Log: provisionertest.Discard})
	if err != nil {
		t.Fatal(err)
	}
	const slug, tenant = "acme", int64(43)
	ctx := provisioner.WithOperator(t.Context(), "ossbss-ci@example.com")
	ca, err := pv.CreateTenantCA(ctx, slug, "loop-ca-"+time.Now().UTC().Format("150405"))
	if provisioner.CauseOf(err) == provisioner.CauseCAExists {
		ca, err = pv.GetCA(ctx, "tenant."+slug)
	}
	if err != nil {
		t.Fatal(err)
	}

	down := cmp.Or(os.Getenv("OSSBSS_TEST_EDGE_DOWN_ID"), "edge-02")
	s := &memStore{aps: map[int64][]db.AP{tenant: {{Name: "hq", Kind: db.KindGroup, Purpose: "unifi", Certs: []db.APCert{{Serial: "aa", NotAfter: time.Now().Add(time.Hour)}}}}}}
	s.edges = []db.Edge{{ID: edge, AuthUpstream: "10.255.0.1:1812", AcctUpstream: "10.255.0.1:1813"}, {ID: down, AuthUpstream: "10.255.0.1:1812", AcctUpstream: "10.255.0.1:1813"}}
	s.tenants = []db.TenantRadsec{{TenantID: tenant, Slug: slug, Port: 20993, CACertificate: ca.Certificate, Revision: 1}}
	// 届かないエッジを primary に置いても、飛ばして次へ進む（S4-8 の実物の確かめ）。
	s.places = []db.Placement{{ID: 2, TenantID: tenant, EdgeID: down, Slot: "primary"}, {ID: 1, TenantID: tenant, EdgeID: edge, Slot: "secondary"}}
	r := New(Options{Log: discard, DB: s, PV: pv, Audit: &audit.Recorder{DB: s, Log: discard}})
	pass := func() {
		t.Helper()
		if _, err := r.RunScheduled(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// until は周回を動かして条件を待つ。
	until := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(170 * time.Second)
		for {
			pass()
			if ok() {
				return
			}
			if time.Now().After(deadline) {
				logs, _ := pv.EdgeTenantLogs(ctx, edge, tenant, 100)
				t.Fatalf("%s: placement %+v\n%s", what, *s.place(1), logs)
			}
			time.Sleep(2 * time.Second)
		}
	}
	defer func() {
		// 途中で落ちても次の試験の邪魔をしない。
		_ = pv.DeleteEdgeTenant(ctx, edge, tenant, "loop-cleanup-"+time.Now().UTC().Format("150405.000"))
	}()

	// 最初の反映（宣言 → CSR → 署名 → 配置 → apply → ヘルスチェック）。
	pass()
	if !r.down[down] || s.place(1).ServerSerial == "" || s.place(1).ProbeSerial == "" {
		t.Fatalf("after the first pass: down %v, placement %+v", r.down, *s.place(1))
	}
	// 2 周目から届かないエッジの待ちを省く。
	s.edges = s.edges[:1]
	s.places = s.places[1:]
	until("first apply", func() bool { return s.place(1).AppliedRevision == 1 })
	if p := s.place(1); p.ApplyResult != "ok" || p.ApplyError != "" {
		t.Fatalf("first apply = %+v", p)
	}
	got, err := pv.EdgeTenant(ctx, edge, tenant)
	if err != nil || got.Slug != slug || got.Pending || got.ServerCert == nil || got.ServerCert.Serial != s.place(1).ServerSerial || len(got.Missing) != 0 {
		t.Fatalf("edge tenant = %+v, %v", got, err)
	}
	until("cert apply finished", func() bool { return !s.place(1).CertApplyNeeded })

	// 版を上げるだけでは反映しない。求めると反映する。
	s.tenant(tenant).Revision = 2
	s.aps[tenant] = append(s.aps[tenant], db.AP{Name: "ap1", Kind: db.KindAP})
	pass()
	got, err = pv.EdgeTenant(ctx, edge, tenant)
	if err != nil || got.Revision != 2 || got.AppliedRevision != 1 || !got.Pending || got.APCount != 2 {
		t.Fatalf("after declare = %+v, %v", got, err)
	}
	s.tenant(tenant).RequestedRevision = 2
	until("requested apply", func() bool { return s.place(1).AppliedRevision == 2 })

	// エッジのデータを失う。同じ版でも宣言を送り直し（aka-provisioner が同じキーの応答を再生しないこと）、証明書を出し直して反映する。
	lost := s.place(1).ServerSerial
	if err := pv.DeleteEdgeTenant(ctx, edge, tenant, "loop-lost-"+time.Now().UTC().Format("150405.000")); err != nil {
		t.Fatal(err)
	}
	pass()
	if got, err := pv.EdgeTenant(ctx, edge, tenant); err != nil || got.ServerCert == nil || got.ServerCert.Serial == lost {
		t.Fatalf("after the loss = %+v, %v", got, err)
	}
	until("apply after the loss", func() bool { return s.place(1).AppliedRevision == 2 && !s.place(1).CertApplyNeeded })
	if c, err := pv.GetCert(ctx, lost); err != nil || c.Status != "revoked" {
		t.Errorf("lost certificate = %+v, %v", c, err)
	}

	// 証明書の更新。反映の後に古いものを失効させる。
	old := s.place(1).ServerSerial
	s.place(1).ServerNotAfter = time.Now().Add(24 * time.Hour)
	s.place(1).RenewRequested = true
	until("renewal", func() bool { return !s.place(1).CertApplyNeeded && s.place(1).ServerSerial != old })
	if c, err := pv.GetCert(ctx, old); err != nil || c.Status != "revoked" || c.RevokeReason != provisioner.RevokeSuperseded {
		t.Errorf("old certificate = %+v, %v", c, err)
	}
	got, err = pv.EdgeTenant(ctx, edge, tenant)
	if err != nil || got.ServerCert == nil || got.ServerCert.Serial != s.place(1).ServerSerial || got.Pending {
		t.Errorf("after renewal = %+v, %v", got, err)
	}

	// CRL の取り直し。
	s.tenant(tenant).CRLDirty = true
	pass()
	if s.tenant(tenant).CRLDirty {
		t.Error("crl is still dirty")
	}

	// 配置を外すとエッジから消す。
	s.place(1).DeletedAt = time.Now()
	pass()
	if len(s.places) != 0 {
		t.Errorf("placements = %+v", s.places)
	}
	if _, err := pv.EdgeTenant(ctx, edge, tenant); provisioner.CauseOf(err) != provisioner.CauseTenantNotFound {
		t.Errorf("after remove: %v", errors.Join(err))
	}
}
