package db

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// 一括投入と一括の物理削除のジョブ（設計概要 §10・§14。段階 3-2 の X2・X9）。ここではジョブの読み書きだけを行う
// （CSV の読み取りと実行は internal/bulk）。

// ジョブの状態。
const (
	JobValidated   = "validated"
	JobQueued      = "queued"
	JobRunning     = "running"
	JobCompleted   = "completed"
	JobInterrupted = "interrupted"
	JobCanceled    = "canceled"
)

// 削除済みの IMSI の選択（設計概要 §10.2。X7）。
const (
	DeletedInclude = "include"
	DeletedExclude = "exclude"
)

// 成功以外の行の結果。
const (
	RowExists      = "exists"
	RowAbsent      = "absent"
	RowFailed      = "failed"
	RowUnprocessed = "unprocessed"
	RowExcluded    = "excluded"
)

// JobCounts はジョブの結果の件数。
type JobCounts struct {
	OK, Exists, Absent, Failed, Unprocessed, Excluded int
}

// Done は処理の済んだ行の数（未処理を除く）。
func (c JobCounts) Done() int { return c.OK + c.Exists + c.Absent + c.Failed + c.Excluded }

// Job はジョブの 1 行（暗号化したファイルは持たない。Sealed は StartJob などで返すときだけ）。
type Job struct {
	ID   int64
	Kind string
	// Target は待ち行列（vecgen / radius）。
	Target   Side
	State    string
	Settings []byte
	FileName string
	// HasFile は暗号化したファイルがあるか。Sealed はその中身（StartJob・TakeQueuedJob のときだけ）。
	HasFile bool
	Sealed  []byte
	KEKID   string
	Total   int
	// PlanExists と PlanAbsent は検証のときの「既にある」「台帳にない」の数。Deleted は検証のときの削除済みの数。
	PlanExists, PlanAbsent, Deleted int
	DeletedChoice, DeletedReason    string
	JobCounts
	InterruptBy string
	Reason      string
	CreatedBy   string
	CreatedAt   time.Time
	QueuedAt    time.Time
	StartedAt   time.Time
	FinishedAt  time.Time
}

// JobID は台帳の事実と監査ログの job_id に書く文字列。
func (j Job) JobID() string { return strconv.FormatInt(j.ID, 10) }

// Active は待ちか実行中か。
func (j Job) Active() bool { return j.State == JobQueued || j.State == JobRunning }

// JobCreate はジョブの登録の内容。
type JobCreate struct {
	Kind         string
	Settings     []byte
	FileName     string
	KEKID        string
	Total        int
	PlanExists   int
	PlanAbsent   int
	DeletedIMSIs []string
	CreatedBy    string
}

// JobRow はジョブの成功以外の行。
type JobRow struct {
	Line   int
	IMSI   string
	Result string
	Reason string
	// Unknown は下流の結果が分からなかった行。
	Unknown bool
}

const jobColumns = `id, kind, target, state, settings, file_name, file_enc IS NOT NULL, kek_id, total, plan_exists, plan_absent,
	cardinality(deleted_imsis), deleted_choice, deleted_reason, ok_count, exists_count, absent_count, failed_count, unprocessed_count,
	excluded_count, interrupt_by, reason, created_by, created_at, queued_at, started_at, finished_at`

func scanJob(row pgx.Row, extra ...any) (Job, error) {
	var j Job
	var target string
	var queued, started, finished *time.Time
	dest := append([]any{&j.ID, &j.Kind, &target, &j.State, &j.Settings, &j.FileName, &j.HasFile, &j.KEKID, &j.Total, &j.PlanExists, &j.PlanAbsent,
		&j.Deleted, &j.DeletedChoice, &j.DeletedReason, &j.OK, &j.Exists, &j.Absent, &j.Failed, &j.Unprocessed, &j.Excluded,
		&j.InterruptBy, &j.Reason, &j.CreatedBy, &j.CreatedAt, &queued, &started, &finished}, extra...)
	if err := row.Scan(dest...); err != nil {
		return Job{}, err
	}
	j.Target = Side(target)
	if queued != nil {
		j.QueuedAt = *queued
	}
	if started != nil {
		j.StartedAt = *started
	}
	if finished != nil {
		j.FinishedAt = *finished
	}
	return j, nil
}

// CreateJob はジョブを検証済みとして登録する。seal は、決まったジョブの ID で（AAD に使う）ファイルを暗号化して返す。
func (d *DB) CreateJob(ctx context.Context, c JobCreate, seal func(id int64) ([]byte, error)) (Job, error) {
	var j Job
	err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		var id int64
		deleted := c.DeletedIMSIs
		if deleted == nil {
			deleted = []string{}
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO bulk_jobs (kind, settings, file_name, kek_id, total, plan_exists, plan_absent, deleted_imsis, created_by)
			VALUES ($1, $2::jsonb, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
			c.Kind, string(c.Settings), c.FileName, c.KEKID, c.Total, c.PlanExists, c.PlanAbsent, deleted, c.CreatedBy).Scan(&id); err != nil {
			return err
		}
		sealed, err := seal(id)
		if err != nil {
			return err
		}
		j, err = scanJob(tx.QueryRow(ctx, "UPDATE bulk_jobs SET file_enc = $2 WHERE id = $1 RETURNING "+jobColumns, id, sealed))
		return err
	})
	if err != nil {
		return Job{}, fmt.Errorf("create job: %w", err)
	}
	return j, nil
}

// Job はジョブを返す。なければ ErrNotFound。
func (d *DB) Job(ctx context.Context, id int64) (Job, error) {
	j, err := scanJob(d.Pool.QueryRow(ctx, "SELECT "+jobColumns+" FROM bulk_jobs WHERE id = $1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("job: %w", err)
	}
	return j, nil
}

func collectJobs(rows pgx.Rows) ([]Job, error) {
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Job, error) { return scanJob(row) })
}

// ListJobs はジョブを返す（確定を待つもの・待ち・実行中を先に、あとは新しい順に limit 件まで）。
func (d *DB) ListJobs(ctx context.Context, limit int) ([]Job, error) {
	rows, err := d.Pool.Query(ctx, "SELECT "+jobColumns+` FROM bulk_jobs
		ORDER BY (state IN ('validated', 'queued', 'running')) DESC, id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	out, err := collectJobs(rows)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	return out, nil
}

// ActiveJobs は待ちと実行中のジョブを、実行中を先に、確定した順で返す（ダッシュボード）。
func (d *DB) ActiveJobs(ctx context.Context) ([]Job, error) {
	rows, err := d.Pool.Query(ctx, "SELECT "+jobColumns+` FROM bulk_jobs WHERE state IN ('queued', 'running')
		ORDER BY state = 'running' DESC, queued_at, id`)
	if err != nil {
		return nil, fmt.Errorf("active jobs: %w", err)
	}
	out, err := collectJobs(rows)
	if err != nil {
		return nil, fmt.Errorf("active jobs: %w", err)
	}
	return out, nil
}

// JobDeletedIMSIs は検証のときに削除済みだった IMSI を昇順で返す。
func (d *DB) JobDeletedIMSIs(ctx context.Context, id int64) ([]string, error) {
	var out []string
	err := d.Pool.QueryRow(ctx, "SELECT array(SELECT unnest(deleted_imsis) ORDER BY 1) FROM bulk_jobs WHERE id = $1", id).Scan(&out)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("job deleted imsis: %w", err)
	}
	return out, nil
}

// jobMove はジョブの状態を移す UPDATE を実行し、移した行を返す。当たらなければ、ジョブがあれば ErrConflict、なければ ErrNotFound。
func (d *DB) jobMove(ctx context.Context, id int64, set string, from []string, args ...any) (Job, error) {
	var sealed []byte
	sql := fmt.Sprintf("UPDATE bulk_jobs SET %s WHERE id = $1 AND state = ANY($2) RETURNING %s, old.file_enc",
		set, jobColumns)
	j, err := scanJob(d.Pool.QueryRow(ctx, sql, append([]any{id, from}, args...)...), &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err := d.Job(ctx, id); err != nil {
			return Job{}, err
		}
		return Job{}, ErrConflict
	}
	if err != nil {
		return Job{}, fmt.Errorf("job %d: %w", id, err)
	}
	j.Sealed = sealed
	return j, nil
}

// ConfirmJob は検証済みのジョブを待ち行列に入れる（確定した順に動く）。
func (d *DB) ConfirmJob(ctx context.Context, id int64, deletedChoice, deletedReason string) (Job, error) {
	return d.jobMove(ctx, id, "state = 'queued', queued_at = now(), deleted_choice = $3, deleted_reason = $4",
		[]string{JobValidated}, deletedChoice, deletedReason)
}

// CancelJob は検証済みのジョブを取り消す（ファイルを消す）。
func (d *DB) CancelJob(ctx context.Context, id int64, reason string) (Job, error) {
	return d.jobMove(ctx, id, "state = 'canceled', finished_at = now(), file_enc = NULL, reason = $3", []string{JobValidated}, reason)
}

// NextQueuedJob は対象の待ち行列の先頭のジョブを返す（なければ false）。
func (d *DB) NextQueuedJob(ctx context.Context, target Side) (Job, bool, error) {
	j, err := scanJob(d.Pool.QueryRow(ctx, "SELECT "+jobColumns+` FROM bulk_jobs
		WHERE state = 'queued' AND target = $1 ORDER BY queued_at, id LIMIT 1`, string(target)))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, fmt.Errorf("next queued job: %w", err)
	}
	return j, true, nil
}

// StartJob は待ちのジョブを実行中にし、暗号化したファイルとともに返す。
func (d *DB) StartJob(ctx context.Context, id int64) (Job, error) {
	return d.jobMove(ctx, id, "state = 'running', started_at = now()", []string{JobQueued})
}

// TakeQueuedJob は待ちのジョブを中断にして（ファイルを消す）、暗号化していたファイルとともに返す。
func (d *DB) TakeQueuedJob(ctx context.Context, id int64, by, reason string) (Job, error) {
	return d.jobMove(ctx, id, "state = 'interrupted', finished_at = now(), file_enc = NULL, interrupt_by = $3, reason = $4, unprocessed_count = total",
		[]string{JobQueued}, by, reason)
}

// RequestJobInterrupt は実行中のジョブの中断を求める（実行の側が次の書き込みで気づく）。
func (d *DB) RequestJobInterrupt(ctx context.Context, id int64, by string) (Job, error) {
	return d.jobMove(ctx, id, "interrupt_by = $3", []string{JobRunning}, by)
}

// FlushJobProgress は結果の件数を足し、成功以外の行を書く。中断を求めた利用者を返す（求められていなければ空）。
func (d *DB) FlushJobProgress(ctx context.Context, id int64, delta JobCounts, rows []JobRow) (interruptBy string, err error) {
	err = pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			UPDATE bulk_jobs SET ok_count = ok_count + $2, exists_count = exists_count + $3, absent_count = absent_count + $4,
				failed_count = failed_count + $5, unprocessed_count = unprocessed_count + $6, excluded_count = excluded_count + $7
			WHERE id = $1 RETURNING interrupt_by`,
			id, delta.OK, delta.Exists, delta.Absent, delta.Failed, delta.Unprocessed, delta.Excluded).Scan(&interruptBy); err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		_, err := tx.CopyFrom(ctx, pgx.Identifier{"bulk_job_rows"}, []string{"job_id", "line", "imsi", "result", "reason", "unknown"},
			pgx.CopyFromSlice(len(rows), func(i int) ([]any, error) {
				r := rows[i]
				return []any{id, int32(r.Line), r.IMSI, r.Result, r.Reason, r.Unknown}, nil
			}))
		return err
	})
	if err != nil {
		return "", fmt.Errorf("flush job progress: %w", err)
	}
	return interruptBy, nil
}

// FinishJob は実行中のジョブを終える（完了か中断。ファイルを消す）。
func (d *DB) FinishJob(ctx context.Context, id int64, state, reason string) (Job, error) {
	return d.jobMove(ctx, id, "state = $3, finished_at = now(), file_enc = NULL, reason = $4", []string{JobRunning}, state, reason)
}

// InterruptRunningJobs は、実行中のまま残っているジョブ（aka-ossbss が止まった）を中断にする（起動のとき。X9）。
// 処理の済まなかった行の数は、全体から処理した数を引いて求める（行の一覧は残らない）。
func (d *DB) InterruptRunningJobs(ctx context.Context, reason string) ([]Job, error) {
	rows, err := d.Pool.Query(ctx, `
		UPDATE bulk_jobs SET state = 'interrupted', finished_at = now(), file_enc = NULL, reason = $1,
			unprocessed_count = greatest(total - ok_count - exists_count - absent_count - failed_count - excluded_count, 0)
		WHERE state = 'running' RETURNING `+jobColumns, reason)
	if err != nil {
		return nil, fmt.Errorf("interrupt running jobs: %w", err)
	}
	out, err := collectJobs(rows)
	if err != nil {
		return nil, fmt.Errorf("interrupt running jobs: %w", err)
	}
	return out, nil
}

// ExpireValidatedJobs は before より前に検証したまま確定していないジョブを取り消す（ファイルを消す。X6）。
func (d *DB) ExpireValidatedJobs(ctx context.Context, before time.Time, reason string) ([]Job, error) {
	rows, err := d.Pool.Query(ctx, `
		UPDATE bulk_jobs SET state = 'canceled', finished_at = now(), file_enc = NULL, reason = $2
		WHERE state = 'validated' AND created_at < $1 RETURNING `+jobColumns, before, reason)
	if err != nil {
		return nil, fmt.Errorf("expire jobs: %w", err)
	}
	out, err := collectJobs(rows)
	if err != nil {
		return nil, fmt.Errorf("expire jobs: %w", err)
	}
	return out, nil
}

// JobRows はジョブの成功以外の行を行番号の順に、afterLine より後ろから limit 件まで返す。
func (d *DB) JobRows(ctx context.Context, id int64, afterLine, limit int) ([]JobRow, error) {
	rows, err := d.Pool.Query(ctx, `SELECT line, imsi, result, reason, unknown FROM bulk_job_rows
		WHERE job_id = $1 AND line > $2 ORDER BY line LIMIT $3`, id, afterLine, limit)
	if err != nil {
		return nil, fmt.Errorf("job rows: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[JobRow])
	if err != nil {
		return nil, fmt.Errorf("job rows: %w", err)
	}
	return out, nil
}

// SubscribersByIMSI は台帳の行を IMSI で引く（1 回の問い合わせ。ファイルの検証に使う。X6）。ない IMSI は結果に入らない。
func (d *DB) SubscribersByIMSI(ctx context.Context, imsis []string) (map[string]Entry, error) {
	rows, err := d.Pool.Query(ctx, entrySelect+" WHERE imsi = ANY($1)", imsis)
	if err != nil {
		return nil, fmt.Errorf("subscribers by imsi: %w", err)
	}
	entries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Entry, error) { return scanEntry(row) })
	if err != nil {
		return nil, fmt.Errorf("subscribers by imsi: %w", err)
	}
	out := make(map[string]Entry, len(entries))
	for _, e := range entries {
		out[e.IMSI] = e
	}
	return out, nil
}

// jobLockClass は、対象ごとの待ち行列の advisory lock の 1 つ目の番号（任意の固定値。2 つ目は対象のハッシュ）。
const jobLockClass = 0x0553_0003

// TryJobLock は対象の待ち行列のロックを取る（aka-ossbss は 1 台だが念のため。X9）。取れたら解放の関数を返す。
// ロックは接続に結び付くので、解放するまで接続を 1 つ持つ。
func (d *DB) TryJobLock(ctx context.Context, target Side) (release func(), ok bool, err error) {
	return d.tryLock(ctx, jobLockClass, string(target))
}
