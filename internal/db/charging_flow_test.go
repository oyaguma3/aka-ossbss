package db_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"github.com/oyaguma3/aka-ossbss/internal/audit"
	"github.com/oyaguma3/aka-ossbss/internal/charging"
	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
)

// 課金レコードの取り込み・締め・ファイルを本物の PostgreSQL で通す結合の試験（Z16）。時計は差し替える。

func at(y int, m time.Month, d, h, mi int) time.Time {
	return time.Date(y, m, d, h, mi, 0, 0, db.JST)
}

// fakeUsage は aka-provisioner の利用の集計の口の偽物。
type fakeUsage struct {
	mu   sync.Mutex
	days map[string][]provisioner.UsageRecord
	// fail は読めない日。
	fail map[string]bool
	// block があれば、読むときに閉じるまで待つ。entered は読み始めたことを知らせる。
	block, entered chan struct{}
}

func (f *fakeUsage) RadiusUsage(_ context.Context, date, cursor string, limit int) (provisioner.UsageList, error) {
	f.mu.Lock()
	block, entered := f.block, f.entered
	f.entered = nil
	f.mu.Unlock()
	if entered != nil {
		close(entered)
	}
	if block != nil {
		<-block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail[date] {
		return provisioner.UsageList{}, errors.New("届きません")
	}
	rows := f.days[date]
	start := 0
	if cursor != "" {
		start = slices.IndexFunc(rows, func(r provisioner.UsageRecord) bool { return r.IMSI == cursor }) + 1
	}
	end := min(start+1, len(rows)) // 1 件ずつ返してページ送りも通す
	out := provisioner.UsageList{Items: rows[start:end]}
	if end < len(rows) {
		out.NextCursor = rows[end-1].IMSI
	}
	return out, nil
}

// clock は差し替える時計。
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

type flow struct {
	t   *testing.T
	d   *db.DB
	pv  *fakeUsage
	clk *clock
	r   *charging.Runner
}

func newFlow(t *testing.T) *flow {
	t.Helper()
	d := db.OpenTestDB(t)
	if _, err := d.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	f := &flow{t: t, d: d, pv: &fakeUsage{days: map[string][]provisioner.UsageRecord{}, fail: map[string]bool{}}, clk: &clock{}}
	log := slog.New(slog.DiscardHandler)
	f.r = charging.New(charging.Options{Log: log, DB: d, PV: f.pv, Audit: &audit.Recorder{DB: d, Log: log}, Now: f.clk.now, Base: context.WithoutCancel(t.Context())})
	return f
}

func (f *flow) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.d.Pool.Exec(f.t.Context(), sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

// event は時刻を決めて状態の事実を書く。
func (f *flow) event(t time.Time, event string, side db.Side, tenant int64, imsi, from, to string) {
	f.t.Helper()
	var s, i *string
	if side != "" {
		v := string(side)
		s = &v
	}
	if imsi != "" {
		i = &imsi
	}
	f.exec(`INSERT INTO charging_events (time, event, side, tenant_id, imsi, from_val, to_val) VALUES ($1, $2, $3, $4, $5, $6, $7)`, t, event, s, tenant, i, from, to)
}

func (f *flow) tenant(id int64, name string, created time.Time) {
	f.t.Helper()
	f.exec(`INSERT INTO tenants (id, name) OVERRIDING SYSTEM VALUE VALUES ($1, $2)`, id, name)
	f.event(created, db.EventTenantCreated, "", id, "", "", name)
}

func (f *flow) usage(day time.Time, imsi string, tenant, auth, sessions int64) {
	date := day.Format(time.DateOnly)
	f.pv.days[date] = append(f.pv.days[date], provisioner.UsageRecord{Date: date, IMSI: imsi, TenantID: tenant, AuthOK: auth, Sessions: sessions, Seconds: 10, InOctets: 20, OutOctets: 30})
}

// importedAll は [start, end) の日を読み切ったことにする。
func (f *flow) importedAll(start, end time.Time) {
	f.t.Helper()
	for d := start; d.Before(end); d = d.AddDate(0, 0, 1) {
		if err := f.d.RecordUsageImport(f.t.Context(), d, "", end); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *flow) close(t time.Time) string {
	f.t.Helper()
	f.clk.set(t)
	text, err := f.r.RunClose(f.t.Context())
	if err != nil {
		f.t.Fatalf("close: %v (%s)", err, text)
	}
	return text
}

func (f *flow) lines(month time.Time) map[string]db.ChargingLine {
	f.t.Helper()
	rows, err := f.d.ChargingLinesPage(f.t.Context(), month, "", 0, 1000)
	if err != nil {
		f.t.Fatal(err)
	}
	out := map[string]db.ChargingLine{}
	for _, r := range rows {
		out[r.IMSI+"/"+strconv.FormatInt(r.TenantID, 10)] = r
	}
	return out
}

func flowKey(t *testing.T) (*openpgp.Entity, string) {
	t.Helper()
	e, err := openpgp.NewEntity("課金", "", "charging@example.com", &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA, Curve: packet.Curve25519})
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

// openFile はファイルを復号して、ZIP の中のファイルを返す。
func openFile(t *testing.T, ent *openpgp.Entity, data []byte) map[string][][]string {
	t.Helper()
	md, err := openpgp.ReadMessage(bytes.NewReader(data), openpgp.EntityList{ent}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(md.UnverifiedBody)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(plain), int64(len(plain)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][][]string{}
	for _, zf := range zr.File {
		rc, err := zf.Open()
		if err != nil {
			t.Fatal(err)
		}
		rows, err := csv.NewReader(rc).ReadAll()
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		out[zf.Name] = rows
	}
	return out
}

func TestChargingCloseFlow(t *testing.T) {
	f := newFlow(t)
	ctx := t.Context()
	const a, b, c, d = "001010000000001", "001010000000002", "001010000000003", "001010000000004"
	mar, apr := at(2031, 3, 1, 0, 0), at(2031, 4, 1, 0, 0)

	// 事実が何もなければ締める月はない。
	if text := f.close(at(2031, 2, 10, 6, 0)); !strings.Contains(text, "締める月はありません") {
		t.Errorf("no facts = %q", text)
	}

	// 3 月: テナント 2 つ。A は月の途中で付け替え、B は月末の 23 時に登録、C は登録して消す、D は月の途中まで Active。
	f.tenant(1, "テナント1", at(2031, 3, 2, 10, 0))
	f.tenant(2, "テナント2", at(2031, 3, 5, 10, 0))
	f.event(at(2031, 3, 10, 12, 0), db.EventLineRegistered, db.SideRadius, 1, a, "", db.StateActive)
	f.event(at(2031, 3, 10, 12, 1), db.EventLineRegistered, db.SideVecgen, 1, a, "", db.StateActive)
	f.event(at(2031, 3, 20, 0, 0), db.EventLineTenantMoved, "", 2, a, "1", "2")
	f.event(at(2031, 3, 31, 23, 0), db.EventLineRegistered, db.SideRadius, 1, b, "", db.StateActive)
	f.event(at(2031, 3, 3, 0, 0), db.EventLineRegistered, db.SideRadius, 1, c, "", db.StateInactive)
	f.event(at(2031, 3, 6, 0, 0), db.EventLineDeleted, db.SideRadius, 1, c, db.StateInactive, "")
	f.event(at(2031, 3, 15, 0, 0), db.EventLineRegistered, db.SideRadius, 1, d, "", db.StateActive)
	f.event(at(2031, 3, 20, 12, 0), db.EventLineStateChanged, db.SideRadius, 1, d, db.StateActive, db.StateInactive)
	// 取り込めた日と、取り込めない日（3 月 12 日。A の登録の間）。
	f.importedAll(mar, at(2031, 3, 25, 0, 0))
	f.exec(`UPDATE usage_import_days SET imported_at = NULL WHERE date = '2031-03-12'`)
	// 3 月の利用: A は集計の刻印のテナントのまま、刻印なし、登録と違うテナント。
	f.usage(at(2031, 3, 30, 0, 0), a, 2, 5, 1)
	f.usage(at(2031, 3, 30, 0, 0), b, 0, 1, 1)
	f.usage(at(2031, 3, 31, 0, 0), d, 9, 2, 1)

	f.close(at(2031, 4, 1, 6, 0))

	periods, err := f.d.ChargingPeriods(ctx, 10)
	if err != nil || len(periods) != 1 {
		t.Fatalf("periods = %+v, %v", periods, err)
	}
	if p := periods[0]; p.Sequence != 1 || p.State != db.PeriodComputed || !p.Period.Equal(mar) || p.MissingDays != 1 || p.EventCount != 10 || p.TenantCount != 2 {
		t.Errorf("period = %+v", p)
	}
	lines := f.lines(mar)
	if r := lines[a+"/1"]; r.RegisteredDays != 10 || r.RadiusActiveDays != 10 || r.VecgenActiveDays != 10 || r.OpenEnd || r.RadiusEnd != db.StateActive || r.UsageComplete {
		t.Errorf("a in tenant 1 = %+v", r) // 3/12 が欠け
	}
	if r := lines[a+"/2"]; r.RegisteredDays != 12 || !r.OpenEnd || r.AuthOK != 5 || !r.UsageComplete {
		t.Errorf("a in tenant 2 = %+v", r)
	}
	if r := lines[b+"/1"]; r.RegisteredDays != 1 || r.RadiusActiveDays != 1 || !r.OpenEnd || r.AuthOK != 0 {
		t.Errorf("b = %+v", r)
	}
	if r := lines[b+"/0"]; r.RegisteredDays != 0 || r.AuthOK != 1 || r.OpenEnd {
		t.Errorf("b without stamp = %+v", r)
	}
	if r := lines[c+"/1"]; r.RegisteredDays != 3 || r.OpenEnd || r.DeletedAt.IsZero() {
		t.Errorf("c = %+v", r)
	}
	if r := lines[d+"/9"]; r.RegisteredDays != 0 || r.AuthOK != 2 {
		t.Errorf("d in another tenant = %+v", r)
	}
	tenants, err := f.d.ChargingTenantsOf(ctx, mar)
	if err != nil || len(tenants) != 2 {
		t.Fatalf("tenants = %+v, %v", tenants, err)
	}
	// テナント 1 の数: 3/3 に C で 1、3/6 に 0、3/10 に A で 1、3/15 に D で 2、3/20 に A が移って 1、3/31 に B で 2。
	if x := tenants[0]; x.Name != "テナント1" || x.ExistedDays != 30 || x.LinesEnd != 2 || x.LinesMax != 2 || x.RadiusActiveLinesEnd != 1 || x.RadiusActiveLinesMax != 2 || !x.LiveEnd {
		t.Errorf("tenant 1 = %+v", x)
	}
	if x := tenants[1]; x.ExistedDays != 27 || x.LinesEnd != 1 || x.LinesMax != 1 {
		t.Errorf("tenant 2 = %+v", x)
	}

	// 鍵がないのでファイルは保留（決定事項 21）。
	files, err := f.d.ChargingFiles(ctx)
	if err != nil || len(files) != 0 {
		t.Fatalf("files before the key = %+v, %v", files, err)
	}

	// 4 月: 事実は A の状態だけ。3 月 31 日の利用が遅れて増える（3 月の訂正）。3/12 の取り込みが済む（usage_complete の訂正）。
	f.event(at(2031, 4, 2, 9, 0), db.EventLineStateChanged, db.SideVecgen, 2, a, db.StateActive, db.StateInactive)
	f.importedAll(at(2031, 3, 12, 0, 0), at(2031, 3, 13, 0, 0))
	f.clk.set(at(2031, 4, 3, 3, 0))
	f.usage(at(2031, 3, 31, 0, 0), a, 2, 7, 2)
	if _, err := f.r.RunImport(ctx); err != nil {
		t.Fatal(err)
	}
	f.close(at(2031, 5, 1, 6, 0))

	periods, _ = f.d.ChargingPeriods(ctx, 10)
	if len(periods) != 2 || periods[0].Sequence != 2 {
		t.Fatalf("periods = %+v", periods)
	}
	aprLines := f.lines(apr)
	// 月初の状態は 3 月の行の月末の値から取る。
	if r := aprLines[a+"/2"]; r.RegisteredDays != 30 || r.RadiusActiveDays != 30 || r.VecgenActiveDays != 2 || r.RadiusStart != db.StateActive || r.VecgenEnd != db.StateInactive || !r.OpenEnd {
		t.Errorf("a in april = %+v", r)
	}
	if _, ok := aprLines[c+"/1"]; ok {
		t.Error("the deleted line appears in april")
	}
	// 元の行は変えない。訂正の行は全体。
	if r := f.lines(mar)[a+"/2"]; r.AuthOK != 5 {
		t.Errorf("the original row changed = %+v", r)
	}
	adj, err := f.d.ChargingAdjPage(ctx, apr, "", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(adj) != 2 {
		t.Fatalf("adj = %+v", adj)
	}
	if adj[0].IMSI != a || adj[0].TenantID != 1 || !adj[0].UsageComplete || adj[0].RegisteredDays != 10 {
		t.Errorf("adj complete = %+v", adj[0])
	}
	if adj[1].IMSI != a || adj[1].TenantID != 2 || adj[1].AuthOK != 12 || adj[1].Sessions != 3 || adj[1].RegisteredDays != 12 || !adj[1].TargetPeriod.Equal(mar) {
		t.Errorf("adj late usage = %+v", adj[1])
	}

	// 鍵を登録するとすぐ作る（古い月から）。
	ent, armored := flowKey(t)
	f.clk.set(at(2031, 5, 2, 10, 0))
	k, err := f.r.RegisterKey(ctx, armored, "admin@example.com")
	if err != nil {
		t.Fatal(err)
	}
	f.r.Wait()
	files, _ = f.d.ChargingFiles(ctx)
	if len(files) != 2 || files[0].FileName != "charging_203104_000002_v1.zip.pgp" || files[1].FileName != "charging_203103_000001_v1.zip.pgp" || files[0].KeyFingerprint != k.Fingerprint {
		t.Fatalf("files = %+v", files)
	}
	full, err := f.d.ChargingFileData(ctx, files[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	got := openFile(t, ent, full.Data)
	if len(got["lines_adj.csv"]) != 3 || got["lines_adj.csv"][2][21] != "L-203103-"+a+"-2" {
		t.Errorf("lines_adj.csv = %v", got["lines_adj.csv"])
	}
	if len(got["events.csv"]) != 2 {
		t.Errorf("events.csv = %v", got["events.csv"])
	}

	// ファイルのある月は計算し直せない。作り直しは保存した値から版を上げ、record_id は同じ。外部の番号は今の値。
	if _, err := f.d.SetTenantExternalID(ctx, 2, "C-2", db.Meta{Operator: "op@example.com"}); err != nil {
		t.Fatal(err)
	}
	f.exec(`INSERT INTO subscribers (imsi, tenant_id, external_line_id) VALUES ($1, 2, 'L-A')`, a)
	if err := f.r.StartRebuild(ctx, mar, "admin@example.com"); err != nil {
		t.Fatal(err)
	}
	f.r.Wait()
	files, _ = f.d.ChargingFiles(ctx)
	if len(files) != 3 || files[1].FileName != "charging_203103_000001_v2.zip.pgp" {
		t.Fatalf("files after rebuild = %+v", files)
	}
	full, _ = f.d.ChargingFileData(ctx, files[1].ID)
	v2 := openFile(t, ent, full.Data)
	full, _ = f.d.ChargingFileData(ctx, files[2].ID)
	v1 := openFile(t, ent, full.Data)
	if len(v1["lines.csv"]) != len(v2["lines.csv"]) {
		t.Fatalf("rows differ: %d, %d", len(v1["lines.csv"]), len(v2["lines.csv"]))
	}
	for i := range v1["lines.csv"] {
		if v1["lines.csv"][i][0] != v2["lines.csv"][i][0] {
			t.Errorf("record_id differs: %v, %v", v1["lines.csv"][i], v2["lines.csv"][i])
		}
	}
	found := false
	for _, r := range v2["lines.csv"][1:] {
		if r[2] == a && r[3] == "2" {
			found = r[4] == "C-2" && r[5] == "L-A"
		}
	}
	if !found {
		t.Errorf("external ids are not the current values: %v", v2["lines.csv"])
	}
	if _, err := f.d.WriteChargingPeriod(ctx, db.ChargingClose{Month: mar, By: "x", At: mar}, func(db.ChargingWriter) error { return nil }); !errors.Is(err, db.ErrPeriodFiled) {
		t.Errorf("recompute a filed period = %v", err)
	}

	// 事実のない月も締める（5 月）。通し番号は続く。鍵の入れ替えは次のファイルから。
	ent2, armored2 := flowKey(t)
	f.clk.set(at(2031, 5, 3, 10, 0))
	if _, err := f.r.RegisterKey(ctx, armored2, "admin@example.com"); err != nil {
		t.Fatal(err)
	}
	f.r.Wait()
	f.close(at(2031, 6, 1, 6, 0))
	files, _ = f.d.ChargingFiles(ctx)
	if files[0].FileName != "charging_203105_000003_v1.zip.pgp" {
		t.Fatalf("files = %+v", files)
	}
	full, _ = f.d.ChargingFileData(ctx, files[0].ID)
	if got := openFile(t, ent2, full.Data); len(got["tenants.csv"]) != 3 || len(got["events.csv"]) != 1 {
		t.Errorf("may file = %v, %v", got["tenants.csv"], got["events.csv"])
	}
}

func TestChargingRecomputeWithoutFile(t *testing.T) {
	f := newFlow(t)
	ctx := t.Context()
	const a = "001010000000001"
	mar := at(2031, 3, 1, 0, 0)
	f.tenant(1, "テナント1", at(2031, 3, 2, 10, 0))
	f.event(at(2031, 3, 10, 12, 0), db.EventLineRegistered, db.SideRadius, 1, a, "", db.StateActive)
	f.close(at(2031, 4, 1, 6, 0))
	if r := f.lines(mar)[a+"/1"]; r.AuthOK != 0 {
		t.Fatalf("line = %+v", r)
	}
	// 鍵がなければ作り直せない。
	if err := f.r.StartRebuild(ctx, mar, "admin@example.com"); !errors.Is(err, charging.ErrNoKey) {
		t.Errorf("rebuild without a key = %v", err)
	}
	if err := f.r.StartRebuild(ctx, at(2031, 2, 1, 0, 0), "admin@example.com"); !errors.Is(err, charging.ErrNotClosed) {
		t.Errorf("rebuild an open period = %v", err)
	}

	// 鍵の登録より先に集計が変わったら、ファイルのない月は計算し直せる。通し番号は同じ。
	if err := f.d.EnsureUsagePartition(ctx, mar); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.UpsertUsage(ctx, mar, []db.UsageRow{{IMSI: a, TenantID: 1, Usage: db.Usage{AuthOK: 4}}}); err != nil {
		t.Fatal(err)
	}
	// 登録と同時に作り始めないよう、先に鍵の行だけを入れる。
	_, armored := flowKey(t)
	f.clk.set(at(2031, 4, 2, 0, 0))
	_, info, err := charging.ParsePublicKey(armored, f.clk.now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.InsertPGPKey(ctx, db.PGPKey{Armored: armored, Fingerprint: info.Fingerprint, Algorithm: info.Algorithm, CreatedAt: f.clk.now(), CreatedBy: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := f.r.StartRebuild(ctx, mar, "admin@example.com"); err != nil {
		t.Fatal(err)
	}
	f.r.Wait()
	if r := f.lines(mar)[a+"/1"]; r.AuthOK != 4 {
		t.Errorf("recomputed = %+v", r)
	}
	p, err := f.d.ChargingPeriodOf(ctx, mar)
	if err != nil || p.Sequence != 1 || p.State != db.PeriodFiled {
		t.Errorf("period = %+v, %v", p, err)
	}
}

// 定刻の締めは、動いている取り込みが終わるのを待って動く（飛ばさない）。
func TestChargingScheduledRunsWait(t *testing.T) {
	f := newFlow(t)
	ctx := t.Context()
	f.tenant(1, "テナント1", at(2031, 3, 2, 10, 0))
	f.clk.set(at(2031, 4, 1, 6, 0))
	block, entered := make(chan struct{}), make(chan struct{})
	f.pv.block, f.pv.entered = block, entered
	if err := f.r.StartImport(ctx, "admin@example.com"); err != nil {
		t.Fatal(err)
	}
	<-entered
	// 手動の実行は重ねない。
	if err := f.r.StartClose(ctx, "admin@example.com"); !errors.Is(err, charging.ErrRunning) {
		t.Errorf("start while running = %v", err)
	}
	done := make(chan string, 1)
	go func() {
		text, err := f.r.RunClose(context.WithoutCancel(ctx))
		if err != nil {
			t.Errorf("close: %v", err)
		}
		done <- text
	}()
	select {
	case text := <-done:
		t.Fatalf("the scheduled close did not wait: %q", text)
	case <-time.After(200 * time.Millisecond):
	}
	f.pv.mu.Lock()
	f.pv.block = nil
	f.pv.mu.Unlock()
	close(block)
	f.r.Wait()
	if text := <-done; !strings.Contains(text, "2031-03 を締めました") {
		t.Errorf("close = %q", text)
	}
	// 待っている間に止められたら、誤りで戻る（記録は中断になる）。
	release, ok, err := f.d.TryNamedLock(ctx, "charging-run")
	if err != nil || !ok {
		t.Fatalf("lock = %v, %v", ok, err)
	}
	defer release()
	cctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := f.r.RunClose(cctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("close while waiting = %v", err)
	}
}

func TestUsageImport(t *testing.T) {
	f := newFlow(t)
	ctx := t.Context()
	f.clk.set(at(2031, 3, 3, 3, 0))
	// 月をまたぐ 7 日（2/24〜3/2）を読み、パーティションを作る。
	f.usage(at(2031, 2, 24, 0, 0), "001010000000001", 1, 1, 1)
	f.usage(at(2031, 2, 24, 0, 0), "001010000000002", 0, 2, 2)
	f.usage(at(2031, 3, 2, 0, 0), "001010000000001", 1, 3, 3)
	f.pv.fail["2031-02-28"] = true
	text, err := f.r.RunImport(ctx)
	if err == nil || !strings.Contains(err.Error(), "2031-02-28") || !strings.Contains(text, "書いた行 3") {
		t.Errorf("import = %q, %v", text, err)
	}
	days, err := f.d.UsageImportedDays(ctx, at(2031, 2, 1, 0, 0), at(2031, 4, 1, 0, 0))
	if err != nil || len(days) != 6 {
		t.Errorf("imported days = %v, %v", days, err)
	}
	if fails, err := f.d.UsageImportFailures(ctx, 10); err != nil || len(fails) != 1 || !fails[0].Date.Equal(at(2031, 2, 28, 0, 0)) || fails[0].Error == "" {
		t.Errorf("failures = %+v, %v", fails, err)
	}
	// 値が変わらない行は書かない。読めなかった日は読めれば埋まる。
	delete(f.pv.fail, "2031-02-28")
	if text, err = f.r.RunImport(ctx); err != nil || !strings.Contains(text, "書いた行 0") {
		t.Errorf("again = %q, %v", text, err)
	}
	if fails, _ := f.d.UsageImportFailures(ctx, 10); len(fails) != 0 {
		t.Errorf("failures = %+v", fails)
	}
	// 一度読めた日は、後で読めなくても読めた日のまま（値は残っている）。
	f.pv.fail["2031-03-02"] = true
	if _, err = f.r.RunImport(ctx); err == nil {
		t.Error("want an error")
	}
	if days, _ := f.d.UsageImportedDays(ctx, at(2031, 3, 2, 0, 0), at(2031, 3, 3, 0, 0)); len(days) != 1 {
		t.Errorf("imported = %v", days)
	}
	// 月をまたいで読み出せる。
	rows, err := f.d.MonthUsage(ctx, at(2031, 2, 1, 0, 0), at(2031, 3, 1, 0, 0), "", 0, 10)
	if err != nil || len(rows) != 2 || rows[0].AuthOK != 1 || rows[1].TenantID != 0 || rows[1].AuthOK != 2 {
		t.Errorf("february = %+v, %v", rows, err)
	}
}

func TestChargingPurge(t *testing.T) {
	f := newFlow(t)
	ctx := t.Context()
	const a = "001010000000001"
	f.tenant(1, "テナント1", at(2030, 1, 15, 0, 0))
	f.event(at(2030, 1, 16, 0, 0), db.EventLineRegistered, db.SideRadius, 1, a, "", db.StateActive)
	f.event(at(2030, 2, 16, 0, 0), db.EventLineStateChanged, db.SideRadius, 1, a, db.StateActive, db.StateInactive)
	for _, d := range []time.Time{at(2030, 1, 20, 0, 0), at(2030, 2, 20, 0, 0)} {
		if err := f.d.EnsureUsagePartition(ctx, d); err != nil {
			t.Fatal(err)
		}
		if _, err := f.d.UpsertUsage(ctx, d, []db.UsageRow{{IMSI: a, TenantID: 1, Usage: db.Usage{AuthOK: 1}}}); err != nil {
			t.Fatal(err)
		}
		if err := f.d.RecordUsageImport(ctx, d, "", d); err != nil {
			t.Fatal(err)
		}
	}
	f.close(at(2030, 3, 1, 6, 0))

	// 2031-03-15 の 13 か月前は 2030-02-15。その月（2 月）は月の途中で切らずに残し、1 月までを消す。
	before := db.RetentionCutoff(at(2031, 3, 15, 4, 30))
	c, err := f.d.PurgeOld(ctx, before)
	if err != nil {
		t.Fatal(err)
	}
	if c.Events != 2 || c.Charging != 1 || c.UsageDays != 1 || c.UsagePartitions != 1 {
		t.Errorf("purge = %+v", c)
	}
	periods, _ := f.d.ChargingPeriods(ctx, 10)
	if len(periods) != 1 || !periods[0].Period.Equal(at(2030, 2, 1, 0, 0)) {
		t.Errorf("periods = %+v", periods)
	}
	var n int
	if err := f.d.Pool.QueryRow(ctx, "SELECT count(*) FROM usage_daily").Scan(&n); err != nil || n != 1 {
		t.Errorf("usage rows = %d, %v", n, err)
	}
	if err := f.d.Pool.QueryRow(ctx, "SELECT count(*) FROM charging_lines").Scan(&n); err != nil || n != 1 {
		t.Errorf("lines = %d, %v", n, err)
	}
	// 2 回目は消すものがない。
	if c, err = f.d.PurgeOld(ctx, before); err != nil || c.Total() != 0 {
		t.Errorf("second purge = %+v, %v", c, err)
	}
	// 残った月の次の月の月初の状態は、残った行から取れる。
	f.close(at(2030, 4, 1, 6, 0))
	if r := f.lines(at(2030, 3, 1, 0, 0))[a+"/1"]; r.RegisteredDays != 31 || r.RadiusStart != db.StateInactive {
		t.Errorf("next month = %+v", r)
	}
}
