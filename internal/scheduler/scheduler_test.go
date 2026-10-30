package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/oyaguma3/aka-ossbss/internal/db"
)

// memStore はメモリーの実行の記録。
type memStore struct {
	mu     sync.Mutex
	runs   []db.ScheduledRun
	locked bool
	errs   error
}

func (m *memStore) InsertScheduledRun(_ context.Context, name string, at time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.errs != nil {
		return 0, m.errs
	}
	m.runs = append(m.runs, db.ScheduledRun{ID: int64(len(m.runs) + 1), Name: name, StartedAt: at, Status: db.ScheduleRunning})
	return int64(len(m.runs)), nil
}

func (m *memStore) FinishScheduledRun(_ context.Context, id int64, status, detail string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := &m.runs[id-1]
	r.Status, r.Detail, r.FinishedAt = status, detail, at
	return nil
}

func (m *memStore) ScheduledRunSince(_ context.Context, name string, since time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.errs != nil {
		return false, m.errs
	}
	return slices.ContainsFunc(m.runs, func(r db.ScheduledRun) bool {
		return r.Name == name && !r.StartedAt.Before(since) && r.Status != db.ScheduleInterrupted
	}), nil
}

func (m *memStore) InterruptRunningScheduled(_ context.Context, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.runs {
		if m.runs[i].Status == db.ScheduleRunning {
			m.runs[i].Status, m.runs[i].FinishedAt = db.ScheduleInterrupted, at
		}
	}
	return nil
}

func (m *memStore) TryNamedLock(context.Context, string) (func(), bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.locked {
		return nil, false, nil
	}
	return func() {}, true, nil
}

func (m *memStore) statuses() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, r := range m.runs {
		out = append(out, r.Name+":"+r.Status)
	}
	return out
}

// clock は差し替える時計（Q9。実際の日付に依存しない）。
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

func jstTime(day, hour, minute int) time.Time {
	return time.Date(2031, 3, day, hour, minute, 0, 0, jst)
}

func newTestScheduler(t *testing.T, m *memStore, c *clock, jobs ...Job) *Scheduler {
	t.Helper()
	s, err := New(Options{Log: slog.New(slog.DiscardHandler), DB: m, Jobs: jobs, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func tick(ctx context.Context, s *Scheduler) {
	s.StartDue(ctx)
	s.Wait()
}

func TestNewValidates(t *testing.T) {
	run := func(context.Context) (string, error) { return "", nil }
	for name, j := range map[string]Job{
		"名前なし":  {Daily: "04:00", Run: run},
		"本体なし":  {Name: "a", Daily: "04:00"},
		"時刻なし":  {Name: "a", Run: run},
		"両方":    {Name: "a", Daily: "04:00", Every: time.Minute, Run: run},
		"時刻の誤り": {Name: "a", Daily: "25:00", Run: run},
	} {
		if _, err := New(Options{Jobs: []Job{j}}); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestLastDue(t *testing.T) {
	for _, tt := range []struct{ now, want time.Time }{
		{jstTime(10, 3, 59), jstTime(9, 4, 0)},
		{jstTime(10, 4, 0), jstTime(10, 4, 0)},
		{jstTime(10, 23, 0), jstTime(10, 4, 0)},
		// UTC で前の日でも、日本時間で数える（UTC の 19:00 は日本時間の翌 4:00）。
		{time.Date(2031, 3, 9, 19, 0, 0, 0, time.UTC), jstTime(10, 4, 0)},
		// 月の変わり目。
		{time.Date(2031, 3, 1, 3, 0, 0, 0, jst), time.Date(2031, 2, 28, 4, 0, 0, 0, jst)},
	} {
		if got := LastDue(tt.now, 4, 0); !got.Equal(tt.want) {
			t.Errorf("LastDue(%v) = %v, want %v", tt.now, got, tt.want)
		}
	}
}

func TestDailyRunsOncePerDay(t *testing.T) {
	m := &memStore{}
	c := &clock{t: jstTime(10, 4, 1)}
	runs := 0
	s := newTestScheduler(t, m, c, Job{Name: "daily", Daily: "04:00", Run: func(context.Context) (string, error) {
		runs++
		return "済み", nil
	}})
	ctx := t.Context()
	tick(ctx, s)
	tick(ctx, s)
	c.set(jstTime(10, 23, 0))
	tick(ctx, s)
	if runs != 1 {
		t.Errorf("runs = %d, want 1", runs)
	}
	if m.runs[0].Status != db.ScheduleOK || m.runs[0].Detail != "済み" || !m.runs[0].FinishedAt.Equal(jstTime(10, 4, 1)) {
		t.Errorf("record = %+v", m.runs[0])
	}
	c.set(jstTime(11, 3, 59))
	tick(ctx, s)
	if runs != 1 {
		t.Errorf("runs before the next slot = %d", runs)
	}
	c.set(jstTime(11, 4, 0))
	tick(ctx, s)
	if runs != 2 {
		t.Errorf("runs after the next slot = %d", runs)
	}
}

// 止まっている間に過ぎた分は、起動の後に 1 回動かす（何日分たまっていても 1 回）。記録があれば動かさない。
func TestDailyCatchUp(t *testing.T) {
	m := &memStore{runs: []db.ScheduledRun{{ID: 1, Name: "daily", StartedAt: jstTime(7, 4, 0), Status: db.ScheduleOK}}}
	c := &clock{t: jstTime(10, 12, 0)}
	runs := 0
	job := Job{Name: "daily", Daily: "04:00", Run: func(context.Context) (string, error) { runs++; return "", nil }}
	s := newTestScheduler(t, m, c, job)
	tick(t.Context(), s)
	tick(t.Context(), s)
	if runs != 1 {
		t.Errorf("runs = %d, want 1", runs)
	}
	// 起動し直しても、今日の分は動かさない。
	s2 := newTestScheduler(t, m, c, job)
	tick(t.Context(), s2)
	if runs != 1 {
		t.Errorf("runs after a restart = %d, want 1", runs)
	}
}

// 失敗は自動でやり直さない（次の定刻）。
func TestDailyFailureIsNotRetried(t *testing.T) {
	m := &memStore{}
	c := &clock{t: jstTime(10, 5, 0)}
	runs := 0
	job := Job{Name: "daily", Daily: "04:00", Run: func(context.Context) (string, error) { runs++; return "", errors.New("下流に届きません") }}
	s := newTestScheduler(t, m, c, job)
	tick(t.Context(), s)
	tick(t.Context(), s)
	if runs != 1 || m.runs[0].Status != db.ScheduleFailed || m.runs[0].Detail != "下流に届きません" {
		t.Errorf("runs = %d, record = %+v", runs, m.runs)
	}
	s2 := newTestScheduler(t, m, c, job)
	tick(t.Context(), s2)
	if runs != 1 {
		t.Errorf("runs after a restart = %d", runs)
	}
	c.set(jstTime(11, 4, 0))
	tick(t.Context(), s)
	if runs != 2 {
		t.Errorf("runs on the next day = %d", runs)
	}
}

// シャットダウンで中断した実行と、強制終了で実行中のまま残った実行は、次の起動で動かし直す。
func TestInterruptedRunIsRepeated(t *testing.T) {
	m := &memStore{}
	c := &clock{t: jstTime(10, 5, 0)}
	runs := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newTestScheduler(t, m, c, Job{Name: "daily", Daily: "04:00", Run: func(ctx context.Context) (string, error) {
		runs++
		cancel()
		return "", ctx.Err()
	}})
	tick(ctx, s)
	if got := m.statuses(); !slices.Equal(got, []string{"daily:interrupted"}) {
		t.Fatalf("records = %v", got)
	}
	s2 := newTestScheduler(t, m, c, Job{Name: "daily", Daily: "04:00", Run: func(context.Context) (string, error) { runs++; return "", nil }})
	tick(t.Context(), s2)
	if runs != 2 {
		t.Errorf("runs = %d, want 2", runs)
	}

	// 強制終了: 実行中の記録を起動のときに中断にして、動かし直す。
	m = &memStore{runs: []db.ScheduledRun{{ID: 1, Name: "daily", StartedAt: jstTime(10, 4, 0), Status: db.ScheduleRunning}}}
	runs = 0
	s3 := newTestScheduler(t, m, c, Job{Name: "daily", Daily: "04:00", Run: func(context.Context) (string, error) { runs++; return "", nil }})
	if err := s3.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	tick(t.Context(), s3)
	if runs != 1 || m.runs[0].Status != db.ScheduleInterrupted {
		t.Errorf("runs = %d, records = %+v", runs, m.runs)
	}
}

// 間隔の処理は、何かしたときと失敗のときだけ記録する。
func TestEveryRecordsOnlyWhenSomethingHappened(t *testing.T) {
	m := &memStore{}
	c := &clock{t: jstTime(10, 12, 0)}
	var detail string
	var runErr error
	runs := 0
	s := newTestScheduler(t, m, c, Job{Name: "every", Every: 5 * time.Minute, Run: func(context.Context) (string, error) {
		runs++
		return detail, runErr
	}})
	ctx := t.Context()
	tick(ctx, s) // 最初の確かめで動かす
	tick(ctx, s) // 間隔が過ぎていない
	if runs != 1 || len(m.runs) != 0 {
		t.Errorf("runs = %d, records = %v", runs, m.runs)
	}
	c.set(jstTime(10, 12, 5))
	detail = "3 件を直しました。"
	tick(ctx, s)
	c.set(jstTime(10, 12, 10))
	detail, runErr = "", errors.New("DB に届きません")
	tick(ctx, s)
	if got := m.statuses(); runs != 3 || !slices.Equal(got, []string{"every:ok", "every:failed"}) {
		t.Errorf("runs = %d, records = %v", runs, got)
	}
}

// 長い処理の間も、ほかの処理は動く。同じ処理は重ねない。
func TestJobsDoNotBlockEachOther(t *testing.T) {
	m := &memStore{}
	c := &clock{t: jstTime(10, 12, 0)}
	gate := make(chan struct{})
	entered := make(chan struct{}, 2)
	longRuns, shortRuns := 0, 0
	s := newTestScheduler(t, m, c,
		Job{Name: "long", Every: time.Minute, Run: func(context.Context) (string, error) {
			longRuns++
			entered <- struct{}{}
			<-gate
			return "", nil
		}},
		Job{Name: "short", Every: time.Minute, Run: func(context.Context) (string, error) {
			shortRuns++
			entered <- struct{}{}
			return "", nil
		}})
	ctx := t.Context()
	s.StartDue(ctx)
	<-entered
	<-entered
	c.set(jstTime(10, 12, 1))
	s.StartDue(ctx)
	<-entered
	close(gate)
	s.Wait()
	if longRuns != 1 || shortRuns != 2 {
		t.Errorf("long = %d, short = %d", longRuns, shortRuns)
	}
}

// 別のプロセスがロックを持っていれば動かさない（記録もしない）。
func TestLockHeld(t *testing.T) {
	m := &memStore{locked: true}
	c := &clock{t: jstTime(10, 12, 0)}
	runs := 0
	s := newTestScheduler(t, m, c, Job{Name: "daily", Daily: "04:00", Run: func(context.Context) (string, error) { runs++; return "", nil }})
	tick(t.Context(), s)
	if runs != 0 || len(m.runs) != 0 {
		t.Errorf("runs = %d, records = %v", runs, m.runs)
	}
}

func TestRunStopsWithContext(t *testing.T) {
	m := &memStore{}
	c := &clock{t: jstTime(10, 12, 0)}
	ran := make(chan struct{}, 10)
	s, err := New(Options{Log: slog.New(slog.DiscardHandler), DB: m, Now: c.now, Tick: time.Millisecond,
		Jobs: []Job{{Name: "every", Every: time.Nanosecond, Run: func(context.Context) (string, error) {
			select {
			case ran <- struct{}{}:
			default:
			}
			return "", nil
		}}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()
	<-ran
	cancel()
	<-done
}
