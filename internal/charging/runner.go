package charging

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oyaguma3/aka-ossbss/internal/audit"
	"github.com/oyaguma3/aka-ossbss/internal/authz"
	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
)

const (
	// Operator は定刻の実行の操作者。
	Operator = "system"
	// usageDays は取り込みで読み直す前の日の数（aka-radius が持つ 7 日。Z6）。
	usageDays = 7
	// computePage は月次の計算の 1 回の読み取りの数、lineFlush は行をまとめて書く数。
	computePage = 5000
	lineFlush   = 5000
	// runLock は取り込み・締め・作り直しを重ねないロックの名前。
	runLock = "charging-run"
	// waitInterval は定刻の実行がほかの実行の終わりを確かめる間隔。
	waitInterval = time.Second
	// 実行の記録の名前（定期の処理と同じ表。手動の実行も残す）。
	JobUsageImport = "usage-import"
	JobClose       = "charging-close"
	JobRebuild     = "charging-rebuild"
)

// ErrRunning は取り込み・締め・作り直しのどれかが動いていること。
var ErrRunning = errors.New("charging job is running")

// Store は Runner が使う DB の操作。*db.DB が満たす。
type Store interface {
	audit.Inserter
	EnsureUsagePartition(ctx context.Context, date time.Time) error
	UpsertUsage(ctx context.Context, date time.Time, rows []db.UsageRow) (int64, error)
	RecordUsageImport(ctx context.Context, date time.Time, errText string, at time.Time) error
	UsageImportedDays(ctx context.Context, start, end time.Time) ([]time.Time, error)
	MonthUsage(ctx context.Context, start, end time.Time, afterIMSI string, afterTenant int64, limit int) ([]db.UsageRow, error)
	MonthLineEvents(ctx context.Context, start, end time.Time, afterIMSI string, afterID int64, limit int) ([]db.ChargingEvent, error)
	MonthTenantEvents(ctx context.Context, start, end time.Time) ([]db.ChargingEvent, error)
	OldestChargingEvent(ctx context.Context) (time.Time, bool, error)
	OpenLines(ctx context.Context, month time.Time, afterIMSI string, limit int) ([]db.OpenLine, error)
	ChargingTenantsOf(ctx context.Context, month time.Time) ([]db.ChargingTenant, error)
	LatestChargingPeriod(ctx context.Context) (db.ChargingPeriod, bool, error)
	WriteChargingPeriod(ctx context.Context, c db.ChargingClose, fn func(w db.ChargingWriter) error) (db.ChargingPeriod, error)
	InsertScheduledRun(ctx context.Context, name string, startedAt time.Time) (int64, error)
	FinishScheduledRun(ctx context.Context, id int64, status, detail string, finishedAt time.Time) error
	TryNamedLock(ctx context.Context, name string) (release func(), ok bool, err error)
}

// Provisioner は Runner が使う aka-provisioner の口。*provisioner.Client が満たす。
type Provisioner interface {
	RadiusUsage(ctx context.Context, date, cursor string, limit int) (provisioner.UsageList, error)
}

// Options は Runner の設定。
type Options struct {
	Log   *slog.Logger
	DB    Store
	PV    Provisioner
	Audit *audit.Recorder
	// Now は時計（既定 time.Now）。Base は手動の実行のコンテキスト（プロセスの終わりまで）。
	Now  func() time.Time
	Base context.Context
}

// Runner は利用の集計の取り込み、月の締め、ファイルの生成を動かす。
type Runner struct {
	opts    Options
	running atomic.Bool
	wg      sync.WaitGroup
}

// New は Runner を作る。
func New(opts Options) *Runner {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Base == nil {
		opts.Base = context.Background()
	}
	return &Runner{opts: opts}
}

// Busy は動いているか。
func (r *Runner) Busy() bool { return r.running.Load() }

// Wait は手動の実行が終わるのを待つ（シャットダウンのときと試験に使う）。
func (r *Runner) Wait() { r.wg.Wait() }

// begin は実行を始める（重ねない）。
func (r *Runner) begin(ctx context.Context) (func(), error) {
	if !r.running.CompareAndSwap(false, true) {
		return nil, ErrRunning
	}
	release, ok, err := r.opts.DB.TryNamedLock(ctx, runLock)
	if err != nil || !ok {
		r.running.Store(false)
		if err == nil {
			err = ErrRunning
		}
		return nil, err
	}
	return func() {
		release()
		r.running.Store(false)
	}, nil
}

// record は監査ログを書く。
func (r *Runner) record(ctx context.Context, by, action, target string, detail map[string]any) {
	role := authz.SystemAdmin
	if by == Operator {
		role = ""
	}
	r.opts.Audit.Record(ctx, db.AuditEntry{ActorEmail: by, ActorRole: role, Action: action, Target: target, Detail: detail})
}

// beginWait は、ほかの実行が終わるのを待ってから実行を始める。定刻の実行は飛ばさない（止まっていた後の起動のときは、取り込みと締めが
// 同時に定刻を迎える。飛ばして成功と記録すると、その日は締まらない）。
func (r *Runner) beginWait(ctx context.Context) (func(), error) {
	for {
		end, err := r.begin(ctx)
		if !errors.Is(err, ErrRunning) {
			return end, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(waitInterval):
		}
	}
}

// RunImport は定刻の取り込み（終わるまで戻らない）。
func (r *Runner) RunImport(ctx context.Context) (string, error) {
	end, err := r.beginWait(ctx)
	if err != nil {
		return "", err
	}
	defer end()
	return r.importUsage(ctx, Operator)
}

// RunClose は定刻の締め（終わるまで戻らない）。
func (r *Runner) RunClose(ctx context.Context) (string, error) {
	end, err := r.beginWait(ctx)
	if err != nil {
		return "", err
	}
	defer end()
	return r.closeDue(ctx, Operator)
}

// startJob は手動の実行を裏で動かす。結果は定期の処理と同じ記録に残す。
func (r *Runner) startJob(ctx context.Context, name string, run func(ctx context.Context) (string, error)) error {
	end, err := r.begin(ctx)
	if err != nil {
		return err
	}
	r.wg.Go(func() {
		defer end()
		base := r.opts.Base
		log := r.opts.Log.With("job", name)
		id, err := r.opts.DB.InsertScheduledRun(base, name, r.opts.Now())
		if err != nil {
			log.Error("charging: record", "event_id", "SCHEDULE_ERR", "error", err)
			return
		}
		detail, runErr := run(base)
		status := db.ScheduleOK
		switch {
		case runErr != nil && base.Err() != nil:
			status, detail = db.ScheduleInterrupted, "aka-ossbss が止まるので中断しました。"
		case runErr != nil:
			status, detail = db.ScheduleFailed, runErr.Error()
			log.Error("charging job failed", "event_id", "SCHEDULE_ERR", "error", runErr)
		}
		wctx, cancel := context.WithTimeout(context.WithoutCancel(base), 10*time.Second)
		defer cancel()
		if err := r.opts.DB.FinishScheduledRun(wctx, id, status, detail, r.opts.Now()); err != nil {
			log.Warn("charging: record", "event_id", "SCHEDULE_ERR", "error", err)
		}
	})
	return nil
}

// StartImport は手動の取り込みを始めてすぐに戻る。動いていれば ErrRunning。
func (r *Runner) StartImport(ctx context.Context, by string) error {
	return r.startJob(ctx, JobUsageImport, func(ctx context.Context) (string, error) { return r.importUsage(ctx, by) })
}

// StartClose は手動の締めを始めてすぐに戻る。動いていれば ErrRunning。
func (r *Runner) StartClose(ctx context.Context, by string) error {
	return r.startJob(ctx, JobClose, func(ctx context.Context) (string, error) { return r.closeDue(ctx, by) })
}

// ---- 取り込み ----

// importUsage は前の 7 日分を読み直す。失敗した日があっても残りを続ける（Z6）。
func (r *Runner) importUsage(ctx context.Context, by string) (string, error) {
	now := r.opts.Now()
	n := now.In(db.JST)
	today := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, db.JST)
	var changed int64
	var failed []string
	for i := usageDays; i >= 1; i-- {
		day := today.AddDate(0, 0, -i)
		written, err := r.importDay(ctx, day)
		changed += written
		errText := ""
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			errText = err.Error()
			failed = append(failed, day.Format(time.DateOnly))
			r.opts.Log.Error("usage import failed", "event_id", "USAGE_IMPORT_ERR", "date", day.Format(time.DateOnly), "error", err)
		}
		if err := r.opts.DB.RecordUsageImport(ctx, day, errText, r.opts.Now()); err != nil {
			return "", err
		}
	}
	text := fmt.Sprintf("前の %d 日分を読みました（書いた行 %d）。", usageDays, changed)
	detail := map[string]any{"days": usageDays, "changed": changed}
	if len(failed) > 0 {
		detail["failed"] = failed
	}
	r.record(ctx, by, audit.ActionChargingImport, "", detail)
	if len(failed) > 0 {
		return text, fmt.Errorf("利用の集計を読めなかった日があります: %s", strings.Join(failed, ", "))
	}
	return text, nil
}

// importDay は 1 日分を読む。途中で読めなくなったら、それまでに読んだ行を残して誤りを返す。
func (r *Runner) importDay(ctx context.Context, day time.Time) (int64, error) {
	if err := r.opts.DB.EnsureUsagePartition(ctx, day); err != nil {
		return 0, err
	}
	date := day.Format(time.DateOnly)
	cursor := ""
	var written int64
	for {
		l, err := r.opts.PV.RadiusUsage(ctx, date, cursor, provisioner.MaxUsagePage)
		if err != nil {
			return written, err
		}
		rows := make([]db.UsageRow, len(l.Items))
		for i, u := range l.Items {
			rows[i] = db.UsageRow{IMSI: u.IMSI, TenantID: u.TenantID, Usage: db.Usage{AuthOK: u.AuthOK, Sessions: u.Sessions, Seconds: u.Seconds, InOctets: u.InOctets, OutOctets: u.OutOctets}}
		}
		n, err := r.opts.DB.UpsertUsage(ctx, day, rows)
		written += n
		if err != nil {
			return written, err
		}
		if l.NextCursor == "" {
			return written, nil
		}
		cursor = l.NextCursor
	}
}

// ---- 締め ----

// closeDue は締めていない過去の月を古い順に締める（Z8）。取り込みに失敗しても締める
// （欠けは usage_complete で示し、翌月の訂正で直す）。
func (r *Runner) closeDue(ctx context.Context, by string) (string, error) {
	var notes []string
	if _, err := r.importUsage(ctx, by); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		notes = append(notes, "取り込み: "+err.Error())
	}
	months, err := r.dueMonths(ctx)
	if err != nil {
		return "", err
	}
	for _, m := range months {
		p, err := r.closeMonth(ctx, m, by)
		if err != nil {
			return strings.Join(notes, " "), err
		}
		notes = append(notes, fmt.Sprintf("%s を締めました（回線 %d、テナント %d、事実 %d、訂正 %d）。", m.Label(), p.LineCount, p.TenantCount, p.EventCount, p.AdjCount))
	}
	if len(months) == 0 {
		notes = append(notes, "締める月はありません。")
	}
	return strings.Join(notes, " "), nil
}

// dueMonths は締める月を古い順に返す。対象は最も古い事実の月から、今の月の前の月まで（事実のない月も）。
func (r *Runner) dueMonths(ctx context.Context) ([]Period, error) {
	var next time.Time
	latest, ok, err := r.opts.DB.LatestChargingPeriod(ctx)
	if err != nil {
		return nil, err
	}
	if ok {
		next = latest.Period.AddDate(0, 1, 0)
	} else {
		oldest, found, err := r.opts.DB.OldestChargingEvent(ctx)
		if err != nil || !found {
			return nil, err
		}
		next = db.MonthOf(oldest)
	}
	current := db.MonthOf(r.opts.Now())
	var out []Period
	for ; next.Before(current); next = next.AddDate(0, 1, 0) {
		out = append(out, NewPeriod(next))
	}
	return out, nil
}

// missingOf はその月の取り込めなかった日のビットを返す。
func (r *Runner) missingOf(ctx context.Context, p Period) (uint32, error) {
	days, err := r.opts.DB.UsageImportedDays(ctx, p.Start, p.End)
	if err != nil {
		return 0, err
	}
	return p.MissingDays(days), nil
}

// closeMonth は 1 つの月を締める（1 つのトランザクションで、途中で止まれば何も残らない）。
func (r *Runner) closeMonth(ctx context.Context, p Period, by string) (db.ChargingPeriod, error) {
	missing, err := r.missingOf(ctx, p)
	if err != nil {
		return db.ChargingPeriod{}, err
	}
	prevMissing, err := r.missingOf(ctx, p.Prev())
	if err != nil {
		return db.ChargingPeriod{}, err
	}
	out, err := r.opts.DB.WriteChargingPeriod(ctx, db.ChargingClose{Month: p.Start, By: by, At: r.opts.Now(), MissingDays: countBits(missing), PrevMissing: prevMissing},
		func(w db.ChargingWriter) error { return r.compute(ctx, p, missing, w) })
	if err != nil {
		r.opts.Log.Error("charging close failed", "event_id", "CHARGING_CLOSE_ERR", "period", p.Label(), "error", err)
		return db.ChargingPeriod{}, err
	}
	r.record(ctx, by, audit.ActionChargingClose, p.Label(), map[string]any{"sequence": out.Sequence, "lines": out.LineCount, "tenants": out.TenantCount,
		"events": out.EventCount, "adjustments": out.AdjCount, "missingDays": out.MissingDays})
	r.opts.Log.Info("charging period closed", "period", p.Label(), "sequence", out.Sequence, "lines", out.LineCount, "tenants", out.TenantCount)
	return out, nil
}

func countBits(v uint32) int {
	n := 0
	for ; v != 0; v &= v - 1 {
		n++
	}
	return n
}

// pager はページごとに読む IMSI の順の列（20 万回線でも全件をメモリーに持たない）。
type pager[T any] struct {
	items []T
	i     int
	last  *T
	done  bool
	key   func(T) string
	fetch func(last *T) ([]T, error)
	size  int
}

// peek は先頭の行の IMSI を返す（なければ空）。
func (p *pager[T]) peek() (string, error) {
	for p.i >= len(p.items) {
		if p.done {
			return "", nil
		}
		items, err := p.fetch(p.last)
		if err != nil {
			return "", err
		}
		p.items, p.i = items, 0
		p.done = len(items) < p.size
		if len(items) > 0 {
			p.last = &items[len(items)-1]
		}
	}
	return p.key(p.items[p.i]), nil
}

// take は先頭から IMSI が imsi の行を全部取り出す（ページをまたいでも）。
func (p *pager[T]) take(imsi string) ([]T, error) {
	var out []T
	for {
		k, err := p.peek()
		if err != nil {
			return nil, err
		}
		if k == "" || k != imsi {
			return out, nil
		}
		out = append(out, p.items[p.i])
		p.i++
	}
}

// compute は月初の状態・事実・利用の集計を IMSI の順に併合して、回線とテナントの行を書く（Z3）。
func (r *Runner) compute(ctx context.Context, p Period, missing uint32, w db.ChargingWriter) error {
	store := r.opts.DB
	prev := p.Prev()
	open := &pager[db.OpenLine]{key: func(o db.OpenLine) string { return o.IMSI }, size: computePage, fetch: func(last *db.OpenLine) ([]db.OpenLine, error) {
		after := ""
		if last != nil {
			after = last.IMSI
		}
		return store.OpenLines(ctx, prev.Start, after, computePage)
	}}
	events := &pager[db.ChargingEvent]{key: func(e db.ChargingEvent) string { return e.IMSI }, size: computePage, fetch: func(last *db.ChargingEvent) ([]db.ChargingEvent, error) {
		after, id := "", int64(0)
		if last != nil {
			after, id = last.IMSI, last.ID
		}
		return store.MonthLineEvents(ctx, p.Start, p.End, after, id, computePage)
	}}
	usage := &pager[db.UsageRow]{key: func(u db.UsageRow) string { return u.IMSI }, size: computePage, fetch: func(last *db.UsageRow) ([]db.UsageRow, error) {
		after, tenant := "", int64(0)
		if last != nil {
			after, tenant = last.IMSI, last.TenantID
		}
		return store.MonthUsage(ctx, p.Start, p.End, after, tenant, computePage)
	}}
	counter := NewTenantCounter()
	buf := make([]db.ChargingLine, 0, lineFlush)
	for {
		a, err := open.peek()
		if err != nil {
			return err
		}
		b, err := events.peek()
		if err != nil {
			return err
		}
		c, err := usage.peek()
		if err != nil {
			return err
		}
		imsi := minNonEmpty(a, b, c)
		if imsi == "" {
			break
		}
		starts, err := open.take(imsi)
		if err != nil {
			return err
		}
		evs, err := events.take(imsi)
		if err != nil {
			return err
		}
		us, err := usage.take(imsi)
		if err != nil {
			return err
		}
		var start *db.OpenLine
		if len(starts) > 0 {
			start = &starts[0]
		}
		res := ComputeLine(p, imsi, start, evs)
		counter.Add(res)
		buf = append(buf, AttachUsage(res.Rows, imsi, us, missing)...)
		if len(buf) >= lineFlush {
			if err := w.AddLines(buf); err != nil {
				return err
			}
			buf = buf[:0]
		}
	}
	if len(buf) > 0 {
		if err := w.AddLines(buf); err != nil {
			return err
		}
	}
	tenants, err := r.computeTenants(ctx, p, counter)
	if err != nil {
		return err
	}
	if len(tenants) > 0 {
		return w.AddTenants(tenants)
	}
	return nil
}

// minNonEmpty は空でない値のうち最も小さいものを返す（全部空なら空）。
func minNonEmpty(vals ...string) string {
	out := ""
	for _, v := range vals {
		if v != "" && (out == "" || v < out) {
			out = v
		}
	}
	return out
}

// computeTenants はテナントの月次の行を作る。対象は前の月の月末に存在していたテナントと、その月に事実のあるテナント。
func (r *Runner) computeTenants(ctx context.Context, p Period, counter *TenantCounter) ([]db.ChargingTenant, error) {
	prev, err := r.opts.DB.ChargingTenantsOf(ctx, p.Prev().Start)
	if err != nil {
		return nil, err
	}
	facts, err := r.opts.DB.MonthTenantEvents(ctx, p.Start, p.End)
	if err != nil {
		return nil, err
	}
	starts := map[int64]*db.ChargingTenant{}
	ids := []int64{}
	for i := range prev {
		if prev[i].LiveEnd {
			starts[prev[i].TenantID] = &prev[i]
			ids = append(ids, prev[i].TenantID)
		}
	}
	byTenant := map[int64][]db.ChargingEvent{}
	for _, ev := range facts {
		if _, ok := byTenant[ev.TenantID]; !ok && starts[ev.TenantID] == nil {
			ids = append(ids, ev.TenantID)
		}
		byTenant[ev.TenantID] = append(byTenant[ev.TenantID], ev)
	}
	slices.Sort(ids)
	var out []db.ChargingTenant
	for _, id := range ids {
		t, ok := ComputeTenant(p, id, starts[id], byTenant[id])
		if !ok {
			continue
		}
		counter.Fill(&t)
		out = append(out, t)
	}
	return out, nil
}
