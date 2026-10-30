// Package charging は課金レコードの月次の計算、ファイルの生成、取り込みと締めの実行を受け持つ
// （aka-mvpset の docs/charging-records.md。段階 3-4 の Z1〜Z16）。
//
// 料金は計算しない。状態の事実（charging_events）と利用の集計（usage_daily）から、回線とテナントの月次の値を作り、
// CSV と manifest を ZIP にまとめて PGP で暗号化する。
package charging

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
