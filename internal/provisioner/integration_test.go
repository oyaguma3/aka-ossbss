package provisioner_test

import (
	"cmp"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner/provisionertest"
	"github.com/oyaguma3/aka-ossbss/internal/trace"
)

// 本物の aka-provisioner と下流（aka-vecgen・aka-radius）を相手にする契約の試験（段階 3-1 の W12）。
// aka-provisioner の scripts/contract.sh up で起動したものに、その contract.env の管理クライアントの証明書でつなぐ。
// OSSBSS_TEST_PROVISIONER_URL（と _CLIENT_CERT、_SERVER_CERT、_AV_CLIENT_ID、_SOURCE_ID）を指定したときだけ動く。

func integrationClient(t *testing.T) (*provisioner.Client, int64, string) {
	t.Helper()
	u := os.Getenv("OSSBSS_TEST_PROVISIONER_URL")
	if u == "" {
		t.Skip("OSSBSS_TEST_PROVISIONER_URL is not set")
	}
	c, err := provisioner.New(provisioner.Options{BaseURL: u,
		ClientCertFile: os.Getenv("OSSBSS_TEST_PROVISIONER_CLIENT_CERT"),
		ServerCertFile: os.Getenv("OSSBSS_TEST_PROVISIONER_SERVER_CERT"),
		Timeout:        30 * time.Second, RadsecTimeout: 40 * time.Second, Log: provisionertest.Discard})
	if err != nil {
		t.Fatal(err)
	}
	av, err := strconv.ParseInt(cmp.Or(os.Getenv("OSSBSS_TEST_PROVISIONER_AV_CLIENT_ID"), "1"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return c, av, cmp.Or(os.Getenv("OSSBSS_TEST_PROVISIONER_SOURCE_ID"), "vecgen")
}

// wantCause は err が provisioner の誤りの応答で、ステータスと cause が一致することを確かめる。
func wantCause(t *testing.T, what string, err error, status int, cause string) {
	t.Helper()
	if provisioner.StatusOf(err) != status || provisioner.CauseOf(err) != cause {
		t.Errorf("%s: err = %v, want %d %s", what, err, status, cause)
	}
}

func TestIntegrationLifecycle(t *testing.T) {
	c, avClient, source := integrationClient(t)
	ctx := trace.With(provisioner.WithOperator(t.Context(), "ossbss-ci@example.com"), "ossbss-contract")
	imsi := fmt.Sprintf("00101%010d", time.Now().UnixNano()%1e10)
	key := func(name string) string { return "contract-" + imsi + "-" + name }

	st, err := c.Status(ctx)
	if err != nil || !st.IsProvisioner() {
		t.Fatalf("status = %+v, %v", st, err)
	}
	clients, err := c.VecgenClients(ctx)
	if err != nil || !slices.ContainsFunc(clients, func(cl provisioner.VecgenClient) bool { return cl.ID == avClient }) {
		t.Fatalf("vecgen clients = %+v, %v", clients, err)
	}
	sources, err := c.RadiusSources(ctx)
	if err != nil || !slices.ContainsFunc(sources, func(s provisioner.Source) bool { return s.ID == source }) {
		t.Fatalf("radius sources = %+v, %v", sources, err)
	}

	// 登録（vecgen 側・radius 側）。
	vc := provisioner.VecgenSubscriberCreate{IMSI: imsi, Ki: "465b5ce8b199b49faa5f0a2ee238a6bc", OPc: "cd63cb71954a9f4e48a5994e37a02baf",
		State: "active", AMF: "8000", SQN: "000000000000", SQNType: "inc32", AllowedClientIDs: []int64{avClient}}
	v, err := c.CreateVecgenSubscriber(ctx, vc, key("vc"))
	if err != nil || v.IMSI != imsi || v.State != "active" || !slices.Equal(v.AllowedClientIDs, []int64{avClient}) {
		t.Fatalf("create vecgen = %+v, %v", v, err)
	}
	// 同じキーの再送は最初の応答を返す（同じバイト列を送る）。同じキーで内容が違えば断られる。
	if v2, err := c.CreateVecgenSubscriber(ctx, vc, key("vc")); err != nil || v2.CreatedAt != v.CreatedAt {
		t.Errorf("replay = %+v, %v", v2, err)
	}
	vc2 := vc
	vc2.State = "inactive"
	_, err = c.CreateVecgenSubscriber(ctx, vc2, key("vc"))
	wantCause(t, "key mismatch", err, 422, provisioner.CauseIdempotencyKeyMismatch)
	_, err = c.CreateVecgenSubscriber(ctx, vc, key("vc-again"))
	wantCause(t, "create twice", err, 409, provisioner.CauseSubscriberExists)

	rules := []provisioner.Rule{{TenantID: 42, NASID: "*", SSIDs: []string{"*"}, VLANID: 100, SessionTimeout: 3600}}
	r, err := c.CreateRadiusSubscriber(ctx, provisioner.RadiusSubscriberCreate{IMSI: imsi, State: "inactive", SourceID: source, Rules: rules}, key("rc"))
	if err != nil || r.State != "inactive" || r.SourceID != source || len(r.Rules) != 1 || r.Rules[0].VLANID != 100 {
		t.Fatalf("create radius = %+v, %v", r, err)
	}
	_, err = c.CreateRadiusSubscriber(ctx, provisioner.RadiusSubscriberCreate{IMSI: imsi, State: "active", SourceID: "no-such-source",
		Rules: rules}, key("rc-bad"))
	if provisioner.StatusOf(err) != 400 && provisioner.StatusOf(err) != 409 {
		t.Errorf("create with unknown source = %v", err)
	}

	// 一覧のページ送り（突き合わせの読み取り。1 件ずつたどっても、登録した IMSI に届く）。
	foundV, foundR := false, false
	for cursor, pages := "", 0; pages < 1000; pages++ {
		l, err := c.ListVecgenSubscribers(ctx, cursor, 1)
		if err != nil {
			t.Fatalf("list vecgen: %v", err)
		}
		foundV = foundV || slices.ContainsFunc(l.Items, func(s provisioner.VecgenSubscriber) bool { return s.IMSI == imsi && s.State == "active" })
		if cursor = l.NextCursor; cursor == "" {
			break
		}
	}
	for cursor, pages := "", 0; pages < 1000; pages++ {
		l, err := c.ListRadiusSubscribers(ctx, cursor, 1)
		if err != nil {
			t.Fatalf("list radius: %v", err)
		}
		foundR = foundR || slices.ContainsFunc(l.Items, func(s provisioner.RadiusSubscriber) bool {
			return s.IMSI == imsi && s.State == "inactive" && s.SourceID == source && len(s.Rules) == 1
		})
		if cursor = l.NextCursor; cursor == "" {
			break
		}
	}
	if !foundV || !foundR {
		t.Errorf("list: vecgen found = %v, radius found = %v", foundV, foundR)
	}

	// 読み出し（1 件、一括取得）。
	if got, err := c.VecgenSubscriber(ctx, imsi); err != nil || got.SQNType != "inc32" || got.AMF != "8000" {
		t.Errorf("get vecgen = %+v, %v", got, err)
	}
	missing := "001019999999999"
	vb, err := c.VecgenBatchGet(ctx, []string{imsi, missing})
	if err != nil || len(vb.Items) != 1 || vb.Items[0].IMSI != imsi || !slices.Equal(vb.NotFound, []string{missing}) {
		t.Errorf("vecgen batch = %+v, %v", vb, err)
	}
	rb, err := c.RadiusBatchGet(ctx, []string{missing, imsi})
	if err != nil || len(rb.Items) != 1 || rb.Items[0].Rules[0].TenantID != 42 || !slices.Equal(rb.NotFound, []string{missing}) {
		t.Errorf("radius batch = %+v, %v", rb, err)
	}

	// 変更（vecgen 側の AMF と許可クライアント、radius 側のルール）。
	v, err = c.UpdateVecgenSubscriber(ctx, imsi, provisioner.VecgenSubscriberUpdate{AMF: "9001", AllowedClientIDs: new([]int64{})}, key("vu"))
	if err != nil || v.AMF != "9001" || len(v.AllowedClientIDs) != 0 {
		t.Errorf("update vecgen = %+v, %v", v, err)
	}
	rules = append(rules, provisioner.Rule{TenantID: 42, NASID: "ap-01", SSIDs: []string{"corp", "guest"}})
	r, err = c.UpdateRadiusSubscriber(ctx, imsi, provisioner.RadiusSubscriberUpdate{Rules: rules}, key("ru"))
	if err != nil || len(r.Rules) != 2 || r.Rules[1].NASID != "ap-01" || r.Rules[1].VLANID != 0 {
		t.Errorf("update radius = %+v, %v", r, err)
	}

	// 状態: Active は削除できない。止める・解約・戻す。
	err = c.DeleteSubscriber(ctx, provisioner.Vecgen, imsi, key("vd-active"))
	wantCause(t, "delete active", err, 409, provisioner.CauseSubscriberActive)
	if s, err := c.SetState(ctx, provisioner.Radius, imsi, provisioner.StateChange{State: "active"}, key("rs-active")); err != nil || s != "active" {
		t.Errorf("radius active = %q, %v", s, err)
	}
	for _, d := range []provisioner.Downstream{provisioner.Radius, provisioner.Vecgen} {
		if s, err := c.SetState(ctx, d, imsi, provisioner.StateChange{State: "terminated", Reason: "契約の試験"}, key(string(d)+"-term")); err != nil || s != "terminated" {
			t.Errorf("%s terminate = %q, %v", d, s, err)
		}
		_, err := c.SetState(ctx, d, imsi, provisioner.StateChange{State: "active"}, key(string(d)+"-term-active"))
		wantCause(t, string(d)+" from terminated", err, 409, provisioner.CauseStateTransitionNotAllowed)
		if s, err := c.Reinstate(ctx, d, imsi, "契約の試験の戻し", key(string(d)+"-reinstate")); err != nil || s != "inactive" {
			t.Errorf("%s reinstate = %q, %v", d, s, err)
		}
	}

	// 利用の集計（日本時間の 1 日ごと。誰も認証していないので空。口と形を確かめる）。
	today := time.Now().In(time.FixedZone("JST", 9*60*60)).Format(time.DateOnly)
	if l, err := c.RadiusUsage(ctx, today, "", provisioner.MaxUsagePage); err != nil || l.NextCursor != "" {
		t.Errorf("usage = %+v, %v", l, err)
	}
	_, err = c.RadiusUsage(ctx, "2026-13-40", "", 0)
	wantCause(t, "usage bad date", err, 400, provisioner.CauseInvalidQueryParam)

	// セッション（テナントか IMSI で絞る）。
	if _, err := c.Sessions(ctx, provisioner.SessionParams{TenantID: 42, Limit: 10}); err != nil {
		t.Errorf("sessions by tenant = %v", err)
	}
	if _, err := c.Sessions(ctx, provisioner.SessionParams{IMSIs: []string{imsi, missing}}); err != nil {
		t.Errorf("sessions by imsi = %v", err)
	}

	// 監査ログ（provisioner・aka-vecgen・aka-radius）に、この IMSI の操作が操作者つきで残る。
	for _, src := range []provisioner.AuditSource{provisioner.AuditProvisioner, provisioner.AuditVecgen, provisioner.AuditRadius} {
		l, err := c.AuditLogs(ctx, src, provisioner.AuditLogParams{Limit: 100})
		if err != nil {
			t.Errorf("%s audit: %v", src, err)
			continue
		}
		if !slices.ContainsFunc(l.Items, func(e provisioner.AuditLogEntry) bool {
			return e.Target == imsi && e.Operator == "ossbss-ci@example.com" && e.TraceID == "ossbss-contract"
		}) {
			t.Errorf("%s audit has no entry for %s: %+v", src, imsi, l.Items)
		}
	}

	// 物理削除（両側）。消した後は 404。
	for _, d := range []provisioner.Downstream{provisioner.Vecgen, provisioner.Radius} {
		if err := c.DeleteSubscriber(ctx, d, imsi, key(string(d)+"-delete")); err != nil {
			t.Errorf("%s delete = %v", d, err)
		}
		err := c.DeleteSubscriber(ctx, d, imsi, key(string(d)+"-delete-again"))
		wantCause(t, string(d)+" delete again", err, 404, provisioner.CauseUserNotFound)
	}
	_, err = c.VecgenSubscriber(ctx, imsi)
	wantCause(t, "get deleted vecgen", err, 404, provisioner.CauseUserNotFound)
	_, err = c.RadiusSubscriber(ctx, imsi)
	wantCause(t, "get deleted radius", err, 404, provisioner.CauseUserNotFound)
}

// TestIntegrationRadsec は radsec-ca とエッジの実物を相手に、型つきのクライアントの項目と口を確かめる（段階 4-3 の OB17・OB23）。
// aka-provisioner の scripts/contract.sh に CA_SRC・EDGE_SRC を与えて起動したものにつなぐ。OSSBSS_TEST_EDGE_ID を指定したときだけ動く。
// テナントの名前は contract.sh の片付けに合わせて acme にする。
func TestIntegrationRadsec(t *testing.T) {
	edge := os.Getenv("OSSBSS_TEST_EDGE_ID")
	if edge == "" {
		t.Skip("OSSBSS_TEST_EDGE_ID is not set")
	}
	c, _, _ := integrationClient(t)
	ctx := trace.With(provisioner.WithOperator(t.Context(), "ossbss-ci@example.com"), "ossbss-contract-radsec")
	run := time.Now().UTC().Format("150405")
	key := func(name string) string { return "contract-" + run + "-" + name }
	const slug, tenant = "acme", int64(42)

	st, err := c.CAStatus(ctx)
	if err != nil || st.DNSDomain == "" || st.URNPrefix == "" {
		t.Fatalf("ca status = %+v, %v", st, err)
	}
	ca, err := c.CreateTenantCA(ctx, slug, key("ca"))
	if err != nil || ca.ID != "tenant."+slug || ca.Certificate == "" || !ca.HasCRL || !ca.SelfSigned {
		t.Fatalf("create ca = %+v, %v", ca, err)
	}
	_, err = c.CreateTenantCA(ctx, slug, key("ca-again"))
	wantCause(t, "create the ca again", err, 409, provisioner.CauseCAExists)
	if got, err := c.GetCA(ctx, ca.ID); err != nil || got.Certificate != ca.Certificate {
		t.Errorf("get ca = %+v, %v", got, err)
	}

	// 持ち込みの CA。Meraki の取得先は届かないので断られる。upload は登録できる。
	vendorPEM, crlPEM := vendorCA(t)
	_, err = c.CreateBroughtCA(ctx, provisioner.BroughtCACreate{Tenant: slug, Name: "meraki", CertPEM: vendorPEM, TenantOnlyConfirmed: true,
		CRL: provisioner.CRLSource{Source: provisioner.CRLSourceMeraki, MerakiOrgID: "1", MerakiAPIKey: "contract-key"}}, key("meraki"))
	wantCause(t, "meraki", err, 422, provisioner.CauseCRLInvalid)
	brought, err := c.CreateBroughtCA(ctx, provisioner.BroughtCACreate{Tenant: slug, Name: "vendor", CertPEM: vendorPEM, TenantOnlyConfirmed: true,
		CRL: provisioner.CRLSource{Source: provisioner.CRLSourceUpload, PEM: crlPEM}}, key("brought"))
	if err != nil || brought.ID != "brought."+slug+".vendor" || brought.CRLSource != provisioner.CRLSourceUpload || brought.CRL == nil {
		t.Fatalf("brought ca = %+v, %v", brought, err)
	}
	if list, err := c.TenantCAs(ctx, slug); err != nil || len(list) != 2 {
		t.Errorf("tenant cas = %+v, %v", list, err)
	}
	if _, err := c.UploadCRL(ctx, brought.ID, crlPEM, key("upload")); err != nil {
		t.Errorf("upload crl: %v", err)
	}
	if _, err := c.ChangeCRLSource(ctx, brought.ID, provisioner.CRLSourceChange{CRL: provisioner.CRLSource{Source: provisioner.CRLSourceUpload, PEM: crlPEM}}, key("source")); err != nil {
		t.Errorf("change crl source: %v", err)
	}
	_, err = c.RefreshCACRL(ctx, brought.ID, key("refresh"))
	wantCause(t, "refresh upload", err, 400, provisioner.CauseValidationError)

	// 宣言。
	decl := provisioner.EdgeDeclaration{Revision: 1, Slug: slug, Port: 20993, AuthUpstream: "10.255.0.1:1812", AcctUpstream: "10.255.0.1:1813",
		ClientCAs: []provisioner.EdgeClientCA{
			{ID: ca.ID, Kind: "issued", Certificate: ca.Certificate, HasCRL: true},
			{ID: brought.ID, Kind: "brought", Certificate: vendorPEM, HasCRL: true},
		},
		APs: []provisioner.EdgeAP{{Name: "hq", Kind: "group", Purpose: "unifi"}}}
	got, err := c.PutEdgeTenant(ctx, edge, tenant, decl, key("put"))
	if err != nil || got.ID != "42" || got.Revision != 1 || !got.Pending || got.ServerCert != nil {
		t.Fatalf("put = %+v, %v", got, err)
	}
	_, err = c.ApplyEdgeTenant(ctx, edge, tenant, key("apply0"))
	wantCause(t, "apply without certificates", err, 422, provisioner.CauseNotReady)

	// CSR に署名して置く。
	serverCSR, err := c.EdgeServerCSR(ctx, edge, tenant, key("server-csr"))
	if err != nil {
		t.Fatal(err)
	}
	server, err := c.IssueCert(ctx, provisioner.CertIssue{Profile: provisioner.ProfileTenantServer, Tenant: slug, Edge: edge, CSR: serverCSR}, key("server"))
	if err != nil || server.Serial == "" || len(server.SANDNS) != 3 || server.NotAfter.IsZero() {
		t.Fatalf("issue server = %+v, %v", server, err)
	}
	got, err = c.PutEdgeServerCert(ctx, edge, tenant, server.Certificate, key("server-cert"))
	if err != nil || got.ServerCert == nil || got.ServerCert.Serial != server.Serial || got.ServerCert.NotAfter.IsZero() {
		t.Fatalf("put server = %+v, %v", got, err)
	}
	probeCSR, err := c.EdgeProbeCSR(ctx, edge, tenant, key("probe-csr"))
	if err != nil {
		t.Fatal(err)
	}
	probe, err := c.IssueCert(ctx, provisioner.CertIssue{Profile: provisioner.ProfileProbeClient, Tenant: slug, Edge: edge, CSR: probeCSR}, key("probe"))
	if err != nil {
		t.Fatal(err)
	}
	got, err = c.PutEdgeProbeCert(ctx, edge, tenant, probe.Certificate, key("probe-cert"))
	if err != nil || got.ProbeCert == nil {
		t.Fatalf("put probe = %+v, %v", got, err)
	}

	// 反映は裏で動く。
	if a, err := c.ApplyEdgeTenant(ctx, edge, tenant, key("apply1")); err != nil || a.State != "running" {
		t.Fatalf("apply = %+v, %v", a, err)
	}
	deadline := time.Now().Add(150 * time.Second)
	for {
		got, err = c.EdgeTenant(ctx, edge, tenant)
		if err != nil {
			t.Fatal(err)
		}
		if got.Apply.State == "idle" && got.Apply.LastResult != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("apply did not finish: %+v", got.Apply)
		}
		time.Sleep(2 * time.Second)
	}
	if got.Apply.LastResult != "ok" || got.AppliedRevision != 1 || got.Pending || len(got.Missing) != 0 || got.Apply.LastRevision != 1 {
		logs, _ := c.EdgeTenantLogs(ctx, edge, tenant, 100)
		t.Fatalf("after apply = %+v\n%s", got, logs)
	}
	list, err := c.EdgeTenants(ctx, edge)
	if err != nil || !slices.ContainsFunc(list, func(s provisioner.EdgeTenantSummary) bool {
		return s.ID == "42" && s.Slug == slug && s.Enabled && s.Revision == 1 && s.AppliedRevision == 1 && s.APCount == 1
	}) {
		t.Errorf("edge tenants = %+v, %v", list, err)
	}
	if st, err := c.Status(ctx); err != nil || !slices.ContainsFunc(st.Downstreams, func(d provisioner.DownstreamStatus) bool {
		return d.Name == provisioner.EdgeDownstream(edge) && d.Reachable
	}) {
		t.Errorf("status = %+v, %v", st, err)
	}
	if logs, err := c.EdgeTenantLogs(ctx, edge, tenant, 50); err != nil || logs == "" {
		t.Errorf("logs = %q, %v", logs, err)
	}
	if seen, err := c.EdgeTenantLastSeen(ctx, edge, tenant); err != nil || len(seen.APs) != 1 || seen.APs[0].Name != "hq" {
		t.Errorf("last seen = %+v, %v", seen, err)
	}

	// グループの証明書の発行と失効、CRL の取り直し。
	_, groupCSR := testCSR(t)
	group, err := c.IssueCert(ctx, provisioner.CertIssue{Profile: provisioner.ProfileGroupClient, Tenant: slug, Group: "hq", CSR: groupCSR}, key("group"))
	if err != nil || group.SANURI != "urn:"+st.URNPrefix+":grp:"+slug+":hq" || group.CACertificate == "" {
		t.Fatalf("issue group = %+v, %v", group, err)
	}
	if got, err := c.GetCert(ctx, group.Serial); err != nil || got.Status != "valid" {
		t.Errorf("get cert = %+v, %v", got, err)
	}
	if got, err := c.RevokeCert(ctx, group.Serial, provisioner.RevokeSuperseded, key("revoke")); err != nil || got.Status != "revoked" || got.RevokeReason != provisioner.RevokeSuperseded {
		t.Errorf("revoke = %+v, %v", got, err)
	}
	if r, err := c.RefreshEdgeCRL(ctx, edge, tenant, key("crl")); err != nil || !r.OK || len(r.Items) != 2 {
		t.Errorf("edge crl refresh = %+v, %v", r, err)
	}
	if err := c.RestartEdgeTenant(ctx, edge, tenant, key("restart")); err != nil {
		t.Errorf("restart: %v", err)
	}

	// 片付け。
	if err := c.DeleteEdgeTenant(ctx, edge, tenant, key("delete")); err != nil {
		t.Fatal(err)
	}
	_, err = c.EdgeTenant(ctx, edge, tenant)
	wantCause(t, "deleted tenant", err, 404, provisioner.CauseTenantNotFound)
	if got, err := c.DisableCA(ctx, brought.ID, key("disable")); err != nil || got.Status != "disabled" {
		t.Errorf("disable = %+v, %v", got, err)
	}
	_, err = c.EdgeTenants(ctx, "no-such-edge")
	wantCause(t, "unknown edge", err, 404, provisioner.CauseEdgeNotFound)
}

// vendorCA は持ち込みの CA の代わり（自己署名）の証明書と CRL（PEM）を作る。
func vendorCA(t *testing.T) (certPEM, crlPEM string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "contract vendor CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, k.Public(), k)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	crl, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{Number: big.NewInt(1),
		ThisUpdate: time.Now().Add(-time.Minute), NextUpdate: time.Now().Add(7 * 24 * time.Hour)}, cert, k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "X509 CRL", Bytes: crl}))
}

// testCSR は鍵と CSR（PEM）を作る。
func testCSR(t *testing.T) (*ecdsa.PrivateKey, string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "contract"}}, k)
	if err != nil {
		t.Fatal(err)
	}
	return k, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}
