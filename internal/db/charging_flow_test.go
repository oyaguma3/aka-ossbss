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
