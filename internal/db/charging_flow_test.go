package db_test

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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
