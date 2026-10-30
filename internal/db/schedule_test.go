package db

import (
	"testing"
	"time"
)

func TestScheduledRuns(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2031, 3, 10, 4, 0, 0, 0, time.UTC)
	id, err := d.InsertScheduledRun(ctx, "reconcile", t0)
	if err != nil {
		t.Fatal(err)
	}
	// 実行中は数える（重ねて動かさない）。中断にすると数えない（動かし直す）。
	if ok, err := d.ScheduledRunSince(ctx, "reconcile", t0); !ok || err != nil {
		t.Errorf("since (running) = %v, %v", ok, err)
	}
	if err := d.InterruptRunningScheduled(ctx, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if ok, err := d.ScheduledRunSince(ctx, "reconcile", t0); ok || err != nil {
		t.Errorf("since (interrupted) = %v, %v", ok, err)
	}
	id2, err := d.InsertScheduledRun(ctx, "reconcile", t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.FinishScheduledRun(ctx, id2, ScheduleFailed, "届きません", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if ok, _ := d.ScheduledRunSince(ctx, "reconcile", t0); !ok {
		t.Error("a failed run must not be repeated")
	}
	if ok, _ := d.ScheduledRunSince(ctx, "reconcile", t0.Add(3*time.Hour)); ok {
		t.Error("run before the slot was counted")
	}
	if _, err := d.InsertScheduledRun(ctx, "retention", t0); err != nil {
		t.Fatal(err)
	}
	latest, err := d.LatestScheduledRuns(ctx)
	if err != nil || len(latest) != 2 || latest[0].Name != "reconcile" || latest[0].ID != id2 || latest[0].Status != ScheduleFailed ||
		latest[0].Detail != "届きません" || latest[1].Name != "retention" || !latest[1].FinishedAt.IsZero() {
		t.Errorf("latest = %+v, %v (first run %d)", latest, err, id)
	}
}

func TestNamedLock(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	release, ok, err := d.TryNamedLock(ctx, "a")
	if err != nil || !ok {
		t.Fatalf("lock = %v, %v", ok, err)
	}
	if _, ok, err := d.TryNamedLock(ctx, "a"); ok || err != nil {
		t.Errorf("second lock = %v, %v", ok, err)
	}
	r2, ok, err := d.TryNamedLock(ctx, "b")
	if err != nil || !ok {
		t.Fatalf("other name = %v, %v", ok, err)
	}
	r2()
	release()
	r3, ok, err := d.TryNamedLock(ctx, "a")
	if err != nil || !ok {
		t.Fatalf("after release = %v, %v", ok, err)
	}
	r3()
}

func TestRetentionCutoff(t *testing.T) {
	jst := time.FixedZone("JST", 9*60*60)
	for _, tt := range []struct{ now, want time.Time }{
		{time.Date(2031, 11, 15, 12, 0, 0, 0, jst), time.Date(2030, 10, 15, 12, 0, 0, 0, jst)},
		// UTC では前の日でも、日本時間の暦で数える。
		{time.Date(2031, 11, 14, 20, 0, 0, 0, time.UTC), time.Date(2030, 10, 15, 5, 0, 0, 0, jst)},
	} {
		if got := RetentionCutoff(tt.now); !got.Equal(tt.want) {
			t.Errorf("RetentionCutoff(%v) = %v, want %v", tt.now, got, tt.want)
		}
	}
}

func TestPurgeOld(t *testing.T) {
	d := openTestDB(t)
	ctx := t.Context()
	if _, err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC)
	before := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := d.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	// 古い監査ログを 1,000 件より多く（まとめて消すことの確かめ）と、新しいものを 1 件。
	exec("INSERT INTO audit_log (time, action) SELECT $1, 'x' FROM generate_series(1, 2500)", old)
	exec("INSERT INTO audit_log (time, action) VALUES ($1, 'keep')", before.Add(time.Hour))
	job := func(state string, at time.Time) {
		t.Helper()
		var id int64
		if err := d.Pool.QueryRow(ctx, `INSERT INTO bulk_jobs (kind, state, file_name, total, created_by, created_at, finished_at)
			VALUES ('radius.delete', $1, 'a.csv', 1, 'd', $2, $2) RETURNING id`, state, at).Scan(&id); err != nil {
			t.Fatal(err)
		}
		exec("INSERT INTO bulk_job_rows (job_id, line, imsi, result) VALUES ($1, 2, '001010000000001', 'failed')", id)
	}
	job(JobCompleted, old)
	job(JobInterrupted, before.Add(time.Hour))
	job(JobQueued, old) // 開いているジョブは古くても消さない
	exec("INSERT INTO reconcile_runs (trigger, requested_by, state, started_at) VALUES ('schedule', 'system', 'completed', $1)", old)
	exec("INSERT INTO reconcile_findings (run_id, imsi, kind, side, status) SELECT id, '001010000000001', 'state_diff', 'vecgen', 'fixed' FROM reconcile_runs")
	exec("INSERT INTO reconcile_runs (trigger, requested_by, state, started_at) VALUES ('schedule', 'system', 'running', $1)", old)
	exec("INSERT INTO scheduled_runs (name, started_at, status) VALUES ('a', $1, 'ok'), ('a', $1, 'running'), ('a', $2, 'ok')", old, before.Add(time.Hour))

	c, err := d.PurgeOld(ctx, before)
	if err != nil {
		t.Fatal(err)
	}
	if c != (PurgeCounts{Audit: 2500, Jobs: 1, Reconcile: 1, Scheduled: 1}) || c.Total() != 2503 {
		t.Errorf("purged = %+v", c)
	}
	count := func(sql string) (n int) {
		t.Helper()
		if err := d.Pool.QueryRow(ctx, sql).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for sql, want := range map[string]int{
		"SELECT count(*) FROM audit_log":          1,
		"SELECT count(*) FROM bulk_jobs":          2,
		"SELECT count(*) FROM bulk_job_rows":      2,
		"SELECT count(*) FROM reconcile_runs":     1,
		"SELECT count(*) FROM reconcile_findings": 0,
		"SELECT count(*) FROM scheduled_runs":     2,
	} {
		if got := count(sql); got != want {
			t.Errorf("%s = %d, want %d", sql, got, want)
		}
	}
}
