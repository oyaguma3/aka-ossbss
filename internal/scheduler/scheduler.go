// Package scheduler は aka-ossbss のプロセスの中で定期の処理を動かす（設計概要 §16。段階 3-3 の Y2）。
//
// 毎日の処理（Daily）と一定の間隔の処理（Every）を持つ。時刻は日本時間に固定し、時計は差し替えられる。処理ごとに advisory lock で
// 重ならないようにし、実行の記録を DB に残す。毎日の処理は、止まっている間に過ぎた分を起動の後に 1 回動かす。失敗は自動ではやり直さず、
// 次の定刻か手で動かす。間隔の処理は、何かしたときと失敗のときだけ記録する（全部を記録すると 1 日に数百行たまるため）。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/oyaguma3/aka-ossbss/internal/db"
)

// jst は毎日の処理の時刻の日本時間（夏時間はないので固定の +09:00 にする）。
var jst = time.FixedZone("JST", 9*60*60)

// Job は定期の処理。
type Job struct {
	// Name は処理の名前（記録とロックの名前）。
	Name string
	// Daily は毎日の時刻（日本時間の HH:MM）。Every は一定の間隔。どちらか一方を与える。
	Daily string
	Every time.Duration
	// Run は処理の本体。detail は記録に残す結果の文（間隔の処理では、何もしなければ空を返す）。
	Run func(ctx context.Context) (detail string, err error)
}

// Store は Scheduler が使う DB の操作。*db.DB が満たす。
type Store interface {
	InsertScheduledRun(ctx context.Context, name string, startedAt time.Time) (int64, error)
	FinishScheduledRun(ctx context.Context, id int64, status, detail string, finishedAt time.Time) error
	ScheduledRunSince(ctx context.Context, name string, since time.Time) (bool, error)
	InterruptRunningScheduled(ctx context.Context, finishedAt time.Time) error
	TryNamedLock(ctx context.Context, name string) (release func(), ok bool, err error)
}

// Options は Scheduler の設定。
type Options struct {
	Log *slog.Logger
	DB  Store
	// Jobs は定期の処理。
	Jobs []Job
	// Tick は処理の時刻を確かめる間隔（既定 1 分）。Now は時計（既定 time.Now）。
	Tick time.Duration
	Now  func() time.Time
}

// entry は 1 つの処理の実行の状態。
type entry struct {
	job Job
	// hour と minute は毎日の時刻。
	hour, minute int
	// running は動いているか。last は間隔の処理の最後の実行の時刻。doneFor は、実行済みと確かめた毎日の定刻。
	running bool
	last    time.Time
	doneFor time.Time
}

// Scheduler は定期の処理を動かす。
type Scheduler struct {
	opts    Options
	mu      sync.Mutex
	entries []*entry
	wg      sync.WaitGroup
}

// New は Scheduler を作る。処理の指定が正しくなければ誤りを返す。
func New(opts Options) (*Scheduler, error) {
	if opts.Tick <= 0 {
		opts.Tick = time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &Scheduler{opts: opts}
	for _, j := range opts.Jobs {
		e := &entry{job: j}
		switch {
		case j.Name == "" || j.Run == nil:
			return nil, errors.New("scheduler: name and run are required")
		case (j.Daily == "") == (j.Every <= 0):
			return nil, fmt.Errorf("scheduler: %s: give either Daily or Every", j.Name)
		case j.Daily != "":
			t, err := time.Parse("15:04", j.Daily)
			if err != nil {
				return nil, fmt.Errorf("scheduler: %s: %w", j.Name, err)
			}
			e.hour, e.minute = t.Hour(), t.Minute()
		}
		s.entries = append(s.entries, e)
	}
	return s, nil
}

// Recover は、実行中のまま残っている記録を中断にする（起動のとき）。
func (s *Scheduler) Recover(ctx context.Context) error {
	return s.opts.DB.InterruptRunningScheduled(ctx, s.opts.Now())
}

// Run は ctx が終わるまで処理を動かし、動いている処理が終わるのを待って戻る。最初の確かめは Tick の後に行う
// （起動のすぐ後は、aka-provisioner や下流がまだ動いていないことがある）。
func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(s.opts.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.wg.Wait()
			return
		case <-t.C:
			s.StartDue(ctx)
		}
	}
}

// Wait は動いている処理が終わるのを待つ（試験に使う）。
func (s *Scheduler) Wait() { s.wg.Wait() }

// StartDue は、今動かす処理を始める。処理は別々の goroutine で動かす（長い突き合わせの間も、短い処理を止めない）。
func (s *Scheduler) StartDue(ctx context.Context) {
	now := s.opts.Now()
	for _, e := range s.entries {
		s.mu.Lock()
		busy := e.running
		s.mu.Unlock()
		if busy {
			continue
		}
		due, slot, err := s.due(ctx, e, now)
		if err != nil {
			if ctx.Err() == nil {
				s.opts.Log.Warn("schedule: check", "event_id", "SCHEDULE_ERR", "job", e.job.Name, "error", err)
			}
			continue
		}
		if !due {
			continue
		}
		s.mu.Lock()
		e.running = true
		s.mu.Unlock()
		s.wg.Go(func() {
			defer func() {
				s.mu.Lock()
				e.running = false
				s.mu.Unlock()
			}()
			s.runJob(ctx, e, now, slot)
		})
	}
}

// LastDue は now 以前で最後の毎日の定刻を返す。
func LastDue(now time.Time, hour, minute int) time.Time {
	n := now.In(jst)
	t := time.Date(n.Year(), n.Month(), n.Day(), hour, minute, 0, 0, jst)
	if t.After(n) {
		t = t.AddDate(0, 0, -1)
	}
	return t
}

// due はその処理を今動かすかを返す。slot は毎日の処理の定刻。
func (s *Scheduler) due(ctx context.Context, e *entry, now time.Time) (bool, time.Time, error) {
	if e.job.Every > 0 {
		s.mu.Lock()
		defer s.mu.Unlock()
		return e.last.IsZero() || now.Sub(e.last) >= e.job.Every, time.Time{}, nil
	}
	slot := LastDue(now, e.hour, e.minute)
	s.mu.Lock()
	done := e.doneFor.Equal(slot)
	s.mu.Unlock()
	if done {
		return false, slot, nil
	}
	ran, err := s.opts.DB.ScheduledRunSince(ctx, e.job.Name, slot)
	if err != nil {
		return false, slot, err
	}
	if ran {
		s.mu.Lock()
		e.doneFor = slot
		s.mu.Unlock()
		return false, slot, nil
	}
	return true, slot, nil
}

// runJob は処理を 1 回動かして記録する。
func (s *Scheduler) runJob(ctx context.Context, e *entry, now, slot time.Time) {
	log := s.opts.Log.With("job", e.job.Name)
	release, ok, err := s.opts.DB.TryNamedLock(ctx, "schedule:"+e.job.Name)
	if err != nil {
		if ctx.Err() == nil {
			log.Warn("schedule: lock", "event_id", "SCHEDULE_ERR", "error", err)
		}
		return
	}
	if !ok {
		log.Warn("schedule: another process is running the job")
		return
	}
	defer release()
	daily := e.job.Every == 0
	var id int64
	if daily {
		if id, err = s.opts.DB.InsertScheduledRun(ctx, e.job.Name, now); err != nil {
			if ctx.Err() == nil {
				log.Warn("schedule: record", "event_id", "SCHEDULE_ERR", "error", err)
			}
			return
		}
	}
	detail, runErr := e.job.Run(ctx)
	status := db.ScheduleOK
	switch {
	case runErr != nil && ctx.Err() != nil:
		status, detail = db.ScheduleInterrupted, "aka-ossbss が止まるので中断しました。"
	case runErr != nil:
		status, detail = db.ScheduleFailed, runErr.Error()
		log.Error("scheduled job failed", "event_id", "SCHEDULE_ERR", "error", runErr)
	}
	// シャットダウンのときも結果を書く。
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	finished := s.opts.Now()
	s.mu.Lock()
	if daily && status != db.ScheduleInterrupted {
		e.doneFor = slot
	}
	if !daily {
		e.last = now
	}
	s.mu.Unlock()
	switch {
	case daily:
		err = s.opts.DB.FinishScheduledRun(wctx, id, status, detail, finished)
	case status != db.ScheduleOK || detail != "":
		// 間隔の処理は、何かしたときと失敗のときだけ記録する。
		if id, err = s.opts.DB.InsertScheduledRun(wctx, e.job.Name, now); err == nil {
			err = s.opts.DB.FinishScheduledRun(wctx, id, status, detail, finished)
		}
	}
	if err != nil {
		log.Warn("schedule: record", "event_id", "SCHEDULE_ERR", "error", err)
	}
}
