package charging

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"github.com/oyaguma3/aka-ossbss/internal/db"
)

// newKey は試験の鍵（秘密鍵つき）と、公開鍵の ASCII 形式を返す。
func newKey(t *testing.T, cfg *packet.Config) (*openpgp.Entity, string) {
	t.Helper()
	e, err := openpgp.NewEntity("課金", "", "charging@example.com", cfg)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	w, err := armor.Encode(&b, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Serialize(w); err != nil {
		t.Fatal(err)
	}
	w.Close()
	return e, b.String()
}

func curveConfig() *packet.Config {
	return &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA, Curve: packet.Curve25519}
}

// fakeSource は BuildFile の読み取りの偽物。
type fakeSource struct {
	events  []db.ChargingEventRow
	lines   []db.ChargingLine
	adj     []db.ChargingLine
	tenants []db.ChargingTenant
}

func (f *fakeSource) ChargingEventsPage(_ context.Context, _, _ time.Time, after int64, limit int) ([]db.ChargingEventRow, error) {
	var out []db.ChargingEventRow
	for _, e := range f.events {
		if e.ID > after && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

func page(rows []db.ChargingLine, imsi string, tenant int64, limit int) []db.ChargingLine {
	var out []db.ChargingLine
	for _, l := range rows {
		if (l.IMSI > imsi || (l.IMSI == imsi && l.TenantID > tenant)) && len(out) < limit {
			out = append(out, l)
		}
	}
	return out
}

func (f *fakeSource) ChargingLinesPage(_ context.Context, _ time.Time, imsi string, tenant int64, limit int) ([]db.ChargingLine, error) {
	return page(f.lines, imsi, tenant, limit), nil
}

func (f *fakeSource) ChargingAdjPage(_ context.Context, _ time.Time, imsi string, tenant int64, limit int) ([]db.ChargingLine, error) {
	return page(f.adj, imsi, tenant, limit), nil
}

func (f *fakeSource) ChargingTenantsOf(context.Context, time.Time) ([]db.ChargingTenant, error) {
	return f.tenants, nil
}

// readCSV は CSV を読む（全部の行が同じ列を持つことも確かめる）。
func readCSV(t *testing.T, b []byte) [][]string {
	t.Helper()
	rows, err := csv.NewReader(bytes.NewReader(b)).ReadAll()
	if err != nil {
		t.Fatalf("csv: %v\n%s", err, b)
	}
	return rows
}

func TestBuildFile(t *testing.T) {
	ent, armored := newKey(t, curveConfig())
	now := jst(12, 1, 6, 0)
	pub, info, err := ParsePublicKey(armored, now)
	if err != nil {
		t.Fatal(err)
	}
	if info.Algorithm != "Curve25519" || len(info.Fingerprint) != 40 || !info.ExpiresAt.IsZero() {
		t.Errorf("info = %+v", info)
	}
	src := &fakeSource{
		events: []db.ChargingEventRow{{ChargingEvent: db.ChargingEvent{ID: 5, Time: jst(11, 2, 3, 4), Event: db.EventLineRegistered, Side: db.SideRadius, TenantID: 7, IMSI: imsi,
			To: db.StateActive, Operator: "a@example.com", JobID: "9", TraceID: "t", Reason: "コンマ, と\"引用符\""}, TenantExternalID: "C-1", LineExternalID: "L-1"}},
		lines: []db.ChargingLine{{IMSI: imsi, TenantID: 7, RegisteredDays: 29, RadiusActiveDays: 28, VecgenActiveDays: 27, RadiusEnd: db.StateActive,
			FirstRegisteredAt: jst(11, 2, 3, 4), Usage: db.Usage{AuthOK: 1, Sessions: 2, Seconds: 3, InOctets: 4, OutOctets: 5}, UsageComplete: true, TenantExternalID: "C-1", LineExternalID: "L-1"}},
		adj:     []db.ChargingLine{{IMSI: imsi, TenantID: 7, TargetPeriod: jst(10, 1, 0, 0), Usage: db.Usage{AuthOK: 9}, UsageComplete: true}},
		tenants: []db.ChargingTenant{{TenantID: 7, Name: "テナント", ExternalID: "C-1", ExistedDays: 30, LinesEnd: 1, LinesMax: 2, RadiusActiveLinesEnd: 1, RadiusActiveLinesMax: 1, LiveEnd: true}},
	}
	res, err := BuildFile(t.Context(), src, november, 12, 3, pub, now)
	if err != nil {
		t.Fatal(err)
	}
	if res.EventCount != 1 || res.LineCount != 1 || res.TenantCount != 1 || res.AdjCount != 1 {
		t.Errorf("counts = %+v", res)
	}
	if FileName(november, 12, 3) != "charging_202611_000012_v3.zip.pgp" {
		t.Errorf("name = %s", FileName(november, 12, 3))
	}

	// 暗号文に平文の IMSI が見えない。秘密鍵で復号し、完全性の検査を通す。
	if bytes.Contains(res.Data, []byte(imsi)) {
		t.Error("plaintext in the ciphertext")
	}
	md, err := openpgp.ReadMessage(bytes.NewReader(res.Data), openpgp.EntityList{ent}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(md.UnverifiedBody)
	if err != nil {
		t.Fatal(err)
	}
	// ZIP は圧縮済みなので PGP では圧縮しない（圧縮のパケットがなければ本文がそのまま ZIP）。
	if h := sha256.Sum256(plain); hex.EncodeToString(h[:]) != res.ZipSHA256 {
		t.Error("zip hash mismatch")
	}
	zr, err := zip.NewReader(bytes.NewReader(plain), int64(len(plain)))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		files[f.Name] = b
		if f.Method != zip.Deflate {
			t.Errorf("%s method = %d", f.Name, f.Method)
		}
	}
	if len(files) != 5 {
		t.Fatalf("files = %v", len(files))
	}

	// manifest の件数と SHA-256 が合う。
	manifest := readCSV(t, files[fileManifest])
	if len(manifest) != 5 || strings.Join(manifest[0], ",") != strings.Join(manifestHeader, ",") {
		t.Fatalf("manifest = %v", manifest)
	}
	if res.Manifest != string(files[fileManifest]) {
		t.Error("the stored manifest differs from the file in the zip")
	}
	for _, m := range manifest[1:] {
		b := files[m[0]]
		h := sha256.Sum256(b)
		rows := readCSV(t, b)
		if m[6] != hex.EncodeToString(h[:]) || m[5] != strconv.Itoa(len(rows)-1) || m[2] != "2026-11" || m[3] != "12" || m[4] != "3" ||
			m[7] != "2026-12-01T06:00:00+09:00" || m[8] != "1.0" {
			t.Errorf("manifest row = %v (rows %d)", m, len(rows))
		}
		// RFC 4180 の CRLF、BOM なし。
		if !bytes.Contains(b, []byte("\r\n")) || bytes.HasPrefix(b, []byte("\xef\xbb\xbf")) {
			t.Errorf("%s: not CRLF or has a BOM", m[0])
		}
	}

	ev := readCSV(t, files[fileEvents])[1]
	if ev[0] != "E-5" || ev[1] != "2026-11-02T03:04:00+09:00" || ev[5] != "C-1" || ev[7] != "L-1" || ev[13] != `コンマ, と"引用符"` {
		t.Errorf("events = %v", ev)
	}
	line := readCSV(t, files[fileLines])[1]
	if line[0] != "L-202611-"+imsi+"-7" || line[1] != "2026-11" || line[4] != "C-1" || line[5] != "L-1" || line[6] != "29" || line[10] != "active" || line[9] != "" ||
		line[13] != "2026-11-02T03:04:00+09:00" || line[14] != "" || line[15] != "1" || line[19] != "5" || line[20] != "true" {
		t.Errorf("lines = %v", line)
	}
	adj := readCSV(t, files[fileAdj])[1]
	if adj[0] != "A-202611-"+imsi+"-7" || adj[1] != "2026-10" || adj[15] != "9" || adj[21] != "L-202610-"+imsi+"-7" || adj[22] != "late_usage" {
		t.Errorf("adj = %v", adj)
	}
	tenant := readCSV(t, files[fileTenants])[1]
	if tenant[0] != "T-202611-7" || tenant[3] != "C-1" || tenant[4] != "テナント" || tenant[7] != "30" || tenant[8] != "1" || tenant[9] != "2" || tenant[12] != "0" || tenant[15] != "0" {
		t.Errorf("tenants = %v", tenant)
	}
}

func TestBuildFileHeaderOnly(t *testing.T) {
	// 訂正がなければヘッダーだけ（課金レコード §7.1）。
	_, armored := newKey(t, curveConfig())
	pub, _, err := ParsePublicKey(armored, jst(12, 1, 6, 0))
	if err != nil {
		t.Fatal(err)
	}
	res, err := BuildFile(t.Context(), &fakeSource{}, november, 1, 1, pub, jst(12, 1, 6, 0))
	if err != nil || res.LineCount != 0 || res.AdjCount != 0 {
		t.Errorf("empty = %+v, %v", res.LineCount, err)
	}
}

func TestParsePublicKey(t *testing.T) {
	now := jst(11, 1, 0, 0)
	keyErr := func(t *testing.T, err error, want string) {
		t.Helper()
		e, ok := errors.AsType[*KeyError](err)
		if !ok || !strings.Contains(e.Message, want) {
			t.Errorf("err = %v, want %q", err, want)
		}
	}
	_, _, err := ParsePublicKey("これは鍵ではありません", now)
	keyErr(t, err, "読み取れません")

	// RSA は 3072 ビット以上。
	_, weak := newKey(t, &packet.Config{Algorithm: packet.PubKeyAlgoRSA, RSABits: 2048})
	_, _, err = ParsePublicKey(weak, now)
	keyErr(t, err, "3072")
	_, strong := newKey(t, &packet.Config{Algorithm: packet.PubKeyAlgoRSA, RSABits: 3072})
	if _, info, err := ParsePublicKey(strong, now); err != nil || info.Algorithm != "RSA 3072" {
		t.Errorf("rsa 3072 = %+v, %v", info, err)
	}

	// 秘密鍵は断る。
	ent, _ := newKey(t, curveConfig())
	var b bytes.Buffer
	w, _ := armor.Encode(&b, openpgp.PrivateKeyType, nil)
	if err := ent.SerializePrivate(w, nil); err != nil {
		t.Fatal(err)
	}
	w.Close()
	_, _, err = ParsePublicKey(b.String(), now)
	keyErr(t, err, "秘密鍵")

	// 鍵は 1 つだけ。
	_, a := newKey(t, curveConfig())
	_, c := newKey(t, curveConfig())
	_, _, err = ParsePublicKey(a+"\n"+c, now)
	keyErr(t, err, "1 つだけ")

	// 期限つきの鍵は期限を読み、切れていたら断る。作った時刻は試験の時刻（現在）なので、十分先の時刻で確かめる。
	cfg := curveConfig()
	cfg.KeyLifetimeSecs = 3600 * 24 * 365 * 3
	cfg.Time = func() time.Time { return now }
	_, expiring := newKey(t, cfg)
	if _, info, err := ParsePublicKey(expiring, now); err != nil || !info.ExpiresAt.Equal(now.Add(3*365*24*time.Hour)) {
		t.Errorf("expiring = %+v, %v", info, err)
	}
	_, _, err = ParsePublicKey(expiring, now.AddDate(4, 0, 0))
	keyErr(t, err, "有効な鍵がありません")
}

func TestEvaluateKey(t *testing.T) {
	now := jst(11, 10, 0, 0)
	if s := EvaluateKey(now, nil, 0); s.Usable() || s.Warning != "" {
		t.Errorf("no key before the 25th = %+v", s)
	}
	if s := EvaluateKey(jst(11, 25, 0, 0), nil, 0); s.Usable() || s.Warning == "" {
		t.Errorf("no key on the 25th = %+v", s)
	}
	if s := EvaluateKey(now, nil, 2); s.Warning == "" {
		t.Errorf("no key with pending = %+v", s)
	}
	if s := EvaluateKey(now, &db.PGPKey{}, 0); !s.Usable() || s.Warning != "" {
		t.Errorf("no expiry = %+v", s)
	}
	if s := EvaluateKey(now, &db.PGPKey{ExpiresAt: now.AddDate(0, 0, 31)}, 0); !s.Usable() || s.Warning != "" {
		t.Errorf("31 days = %+v", s)
	}
	if s := EvaluateKey(now, &db.PGPKey{ExpiresAt: now.AddDate(0, 0, 30)}, 0); !s.Usable() || s.Warning == "" {
		t.Errorf("30 days = %+v", s)
	}
	if s := EvaluateKey(now, &db.PGPKey{ExpiresAt: now}, 1); s.Usable() || !s.Expired || s.Warning == "" {
		t.Errorf("expired = %+v", s)
	}
}
