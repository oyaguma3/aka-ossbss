package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// 定期の処理の記録（設計概要 §16。段階 3-3 の Y2）と、13 か月より古い記録の削除（Y11）。

// 定期の処理の結果。
const (
	ScheduleRunning     = "running"
	ScheduleOK          = "ok"
	ScheduleFailed      = "failed"
	ScheduleInterrupted = "interrupted"
)

// ScheduledRun は定期の処理の実行の記録。
type ScheduledRun struct {
	ID         int64
	Name       string
	StartedAt  time.Time
	FinishedAt time.Time
	Status     string
	Detail     string
}

// InsertScheduledRun は実行の始まりを記録する。
func (d *DB) InsertScheduledRun(ctx context.Context, name string, startedAt time.Time) (int64, error) {
	var id int64
	err := d.Pool.QueryRow(ctx, "INSERT INTO scheduled_runs (name, started_at, status) VALUES ($1, $2, 'running') RETURNING id",
		name, startedAt).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert scheduled run: %w", err)
	}
	return id, nil
}

// FinishScheduledRun は実行の終わりを記録する。
func (d *DB) FinishScheduledRun(ctx context.Context, id int64, status, detail string, finishedAt time.Time) error {
	_, err := d.Pool.Exec(ctx, "UPDATE scheduled_runs SET status = $2, detail = $3, finished_at = $4 WHERE id = $1", id, status, detail, finishedAt)
	if err != nil {
		return fmt.Errorf("finish scheduled run: %w", err)
	}
	return nil
}

// ScheduledRunSince は、since 以降に始まった実行のうち、やり直さないもの（成功・失敗・実行中）があるかを返す。
// 中断は数えない（起動の後に動かし直す。Y9）。
func (d *DB) ScheduledRunSince(ctx context.Context, name string, since time.Time) (bool, error) {
	var ok bool
	err := d.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM scheduled_runs
		WHERE name = $1 AND started_at >= $2 AND status IN ('ok', 'failed', 'running'))`, name, since).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("scheduled run since: %w", err)
	}
	return ok, nil
}

// InterruptRunningScheduled は、実行中のまま残っている記録を中断にする（起動のとき）。
func (d *DB) InterruptRunningScheduled(ctx context.Context, finishedAt time.Time) error {
	_, err := d.Pool.Exec(ctx, "UPDATE scheduled_runs SET status = 'interrupted', finished_at = $1 WHERE status = 'running'", finishedAt)
	if err != nil {
		return fmt.Errorf("interrupt scheduled runs: %w", err)
	}
	return nil
}

// LatestScheduledRuns は処理の名前ごとの最新の記録を、名前の順に返す。
func (d *DB) LatestScheduledRuns(ctx context.Context) ([]ScheduledRun, error) {
	rows, err := d.Pool.Query(ctx, `SELECT DISTINCT ON (name) id, name, started_at, finished_at, status, detail
		FROM scheduled_runs ORDER BY name, started_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("latest scheduled runs: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ScheduledRun, error) {
		var r ScheduledRun
		var finished *time.Time
		if err := row.Scan(&r.ID, &r.Name, &r.StartedAt, &finished, &r.Status, &r.Detail); err != nil {
			return ScheduledRun{}, err
		}
		if finished != nil {
			r.FinishedAt = *finished
		}
		return r, nil
	})
	if err != nil {
		return nil, fmt.Errorf("latest scheduled runs: %w", err)
	}
	return out, nil
}

// namedLockClass は、名前つきの advisory lock の 1 つ目の番号（任意の固定値。2 つ目は名前のハッシュ）。
const namedLockClass = 0x0553_0004

// TryNamedLock は名前つきのロックを取る。取れたら解放の関数を返す。ロックは接続に結び付くので、解放するまで接続を 1 つ持つ。
func (d *DB) TryNamedLock(ctx context.Context, name string) (release func(), ok bool, err error) {
	return d.tryLock(ctx, namedLockClass, name)
}

// tryLock は advisory lock を取る。
func (d *DB) tryLock(ctx context.Context, class int, name string) (release func(), ok bool, err error) {
	conn, err := d.Pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("lock %s: %w", name, err)
	}
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1::int, hashtext($2))", class, name).Scan(&ok); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("lock %s: %w", name, err)
	}
	if !ok {
		conn.Release()
		return nil, false, nil
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock($1::int, hashtext($2))", class, name); err != nil {
			// 解放できなければ接続を閉じる（閉じればロックも外れる）。
			_ = conn.Conn().Close(ctx)
		}
		conn.Release()
	}, true, nil
}

// ---- 13 か月より古い記録の削除 ----

// retentionMonths は記録を残す月数（課金レコードと同じ）。
const retentionMonths = 13

// RetentionCutoff は now から暦で 13 か月前の時刻（日本時間の暦で数える）を返す。
func RetentionCutoff(now time.Time) time.Time {
	return now.In(time.FixedZone("JST", 9*60*60)).AddDate(0, -retentionMonths, 0)
}

// purgeBatch は 1 回の DELETE で消す行の数（長いトランザクションで画面を止めない）。
const purgeBatch = 1000

// PurgeCounts は消した行の数。
type PurgeCounts struct {
	Audit, Jobs, Reconcile, Scheduled int64
}

// Total は消した行の総数。
func (c PurgeCounts) Total() int64 { return c.Audit + c.Jobs + c.Reconcile + c.Scheduled }

// PurgeOld は before より古い監査ログ、終わったジョブ（行も一緒に）、終わった突き合わせ（ずれも一緒に）、定期の処理の記録を消す。
// 途中で失敗したら、それまでに消した数とともに誤りを返す。課金レコードの表は消さない（段階 3-4）。
func (d *DB) PurgeOld(ctx context.Context, before time.Time) (PurgeCounts, error) {
	var c PurgeCounts
	for _, t := range []struct {
		count *int64
		sql   string
	}{
		{&c.Audit, `DELETE FROM audit_log WHERE id IN (SELECT id FROM audit_log WHERE time < $1 ORDER BY id LIMIT $2)`},
		{&c.Jobs, `DELETE FROM bulk_jobs WHERE id IN (SELECT id FROM bulk_jobs
			WHERE state IN ('completed', 'interrupted', 'canceled') AND coalesce(finished_at, created_at) < $1 ORDER BY id LIMIT $2)`},
		{&c.Reconcile, `DELETE FROM reconcile_runs WHERE id IN (SELECT id FROM reconcile_runs
			WHERE state <> 'running' AND started_at < $1 ORDER BY id LIMIT $2)`},
		{&c.Scheduled, `DELETE FROM scheduled_runs WHERE id IN (SELECT id FROM scheduled_runs
			WHERE status <> 'running' AND started_at < $1 ORDER BY id LIMIT $2)`},
	} {
		for {
			tag, err := d.Pool.Exec(ctx, t.sql, before, purgeBatch)
			if err != nil {
				return c, fmt.Errorf("purge: %w", err)
			}
			*t.count += tag.RowsAffected()
			if tag.RowsAffected() < purgeBatch {
				break
			}
			if err := ctx.Err(); err != nil {
				return c, err
			}
		}
	}
	return c, nil
}
