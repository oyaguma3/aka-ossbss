package bulk

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/oyaguma3/aka-ossbss/internal/audit"
	"github.com/oyaguma3/aka-ossbss/internal/authz"
	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/kek"
	"github.com/oyaguma3/aka-ossbss/internal/ledger"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
	"github.com/oyaguma3/aka-ossbss/internal/trace"
)

// Settings はジョブの一律の設定（X5）。bulk_jobs.settings に JSON で置く（監査ログにも同じ内容を出す）。鍵を入れない。
type Settings struct {
	// TenantID は登録のジョブの所属のテナント（ルールのテナントも同じ）。
	TenantID int64  `json:"tenantId,omitzero"`
	State    string `json:"state,omitempty"`
	// AllowedClientIDs と SQNType は vecgen 登録。
	AllowedClientIDs []int64 `json:"allowedClientIds,omitempty"`
	SQNType          string  `json:"sqnType,omitempty"`
	// SourceID と Rules は radius 登録。
	SourceID string             `json:"sourceId,omitempty"`
	Rules    []provisioner.Rule `json:"rules,omitempty"`
}

// AAD はジョブのファイルの暗号化の AAD（X3）。
func AAD(id int64) string { return "aka-ossbss/bulk-job/" + strconv.FormatInt(id, 10) }

// 中断の理由。
const (
	reasonRequested = "システム管理者が中断しました。"
	reasonAuto      = "下流に接続できない誤りが続いたので、自動で中断しました。"
	reasonShutdown  = "aka-ossbss が止まるので中断しました。"
	// ReasonRestart は、実行中に aka-ossbss が止まったジョブの中断の理由。
	ReasonRestart = "aka-ossbss が止まったので中断しました（未処理の行の一覧はありません）。"
	reasonDecrypt = "ファイルを復号できません（KEK が変わった可能性があります）。同じファイルを投入し直してください。"
	reasonTenant  = "所属のテナントが削除されました。"
	reasonBad     = "ジョブの内容を読めません。"
	reasonExpired = "確定しないまま 24 時間たったので取り消しました。"
)

// maxConnFailure は自動で中断するまでの、続けて起きた接続の誤りの数（X8）。
const maxConnFailure = 10

// expireAfter は検証済みのジョブを確定せずに置ける時間（X6）。
const expireAfter = 24 * time.Hour

// Store は Runner が使う DB の操作。*db.DB が満たす。
type Store interface {
	ledger.DB
	audit.Inserter
	Tenants(ctx context.Context) ([]db.Tenant, error)
	NextQueuedJob(ctx context.Context, target db.Side) (db.Job, bool, error)
	StartJob(ctx context.Context, id int64) (db.Job, error)
	TakeQueuedJob(ctx context.Context, id int64, by, reason string) (db.Job, error)
	RequestJobInterrupt(ctx context.Context, id int64, by string) (db.Job, error)
	FlushJobProgress(ctx context.Context, id int64, delta db.JobCounts, rows []db.JobRow) (string, error)
	FinishJob(ctx context.Context, id int64, state, reason string) (db.Job, error)
	InterruptRunningJobs(ctx context.Context, reason string) ([]db.Job, error)
	ExpireValidatedJobs(ctx context.Context, before time.Time, reason string) ([]db.Job, error)
	JobDeletedIMSIs(ctx context.Context, id int64) ([]string, error)
	TryJobLock(ctx context.Context, target db.Side) (release func(), ok bool, err error)
}

// Provisioner は Runner が使う aka-provisioner の口。*provisioner.Client が満たす。
type Provisioner interface {
	ledger.Provisioner
	CreateVecgenSubscriber(ctx context.Context, s provisioner.VecgenSubscriberCreate, idempotencyKey string) (provisioner.VecgenSubscriber, error)
	CreateRadiusSubscriber(ctx context.Context, s provisioner.RadiusSubscriberCreate, idempotencyKey string) (provisioner.RadiusSubscriber, error)
	DeleteSubscriber(ctx context.Context, d provisioner.Downstream, imsi, idempotencyKey string) error
}

// Options は Runner の設定。
type Options struct {
	Log *slog.Logger
	DB  Store
	PV  Provisioner
	KEK *kek.KEK
	// Audit は監査ログ。
	Audit *audit.Recorder
	// Concurrency は同時の呼び出しの数（既定 5）。
	Concurrency int
	// Describe は aka-provisioner の誤りを結果の行の文にする（nil なら誤りの文字列）。
	Describe func(err error, side db.Side) string
	// FlushInterval は進み具合を DB に書く間隔（既定 1 秒）、PollInterval は待ち行列を見に行く間隔（既定 10 秒）。
	FlushInterval, PollInterval time.Duration
	// Now は時計（試験で差し替える。既定 time.Now）。
	Now func() time.Time
}

// Runner はジョブを対象ごとの待ち行列で動かす（設計概要 §10.3・§16。X8・X9）。
type Runner struct {
	opts Options
	ops  *ledger.Ops
	wake map[db.Side]chan struct{}
}

// New は Runner を作る。
func New(opts Options) *Runner {
	if opts.Concurrency < 1 {
		opts.Concurrency = 5
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = time.Second
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 10 * time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Runner{opts: opts, ops: &ledger.Ops{DB: opts.DB, PV: opts.PV, Log: opts.Log},
		wake: map[db.Side]chan struct{}{db.SideVecgen: make(chan struct{}, 1), db.SideRadius: make(chan struct{}, 1)}}
}

// Wake は、ジョブを確定したことを待ち行列に知らせる。
func (r *Runner) Wake(target db.Side) {
	select {
	case r.wake[target] <- struct{}{}:
	default:
	}
}

// Recover は、実行中のまま残っているジョブを中断にする（起動のとき。待ちのジョブは Run が続けて動かす）。
func (r *Runner) Recover(ctx context.Context) error {
	jobs, err := r.opts.DB.InterruptRunningJobs(ctx, ReasonRestart)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		r.opts.Log.Warn("interrupted a job left running", "event_id", "JOB_INTERRUPTED", "job_id", j.ID, "kind", j.Kind)
		r.recordJob(ctx, j, j.CreatedBy, audit.ActionJobInterrupt, resultDetail(j))
	}
	return nil
}

// Sweep は、確定しないまま 24 時間たった検証済みのジョブを取り消し（ファイルを消す）、取り消した数を返す（定期の処理。段階 3-3 の Y2）。
func (r *Runner) Sweep(ctx context.Context) (int, error) {
	jobs, err := r.opts.DB.ExpireValidatedJobs(ctx, r.opts.Now().Add(-expireAfter), reasonExpired)
	if err != nil {
		return 0, err
	}
	for _, j := range jobs {
		r.recordJob(ctx, j, j.CreatedBy, audit.ActionJobCancel, map[string]any{"kind": j.Kind, "file": j.FileName, "reason": j.Reason})
	}
	return len(jobs), nil
}

// Run は待ち行列を動かす。ctx が終わると、処理中の行を終えてジョブを中断にしてから戻る。
func (r *Runner) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, target := range db.Sides {
		wg.Go(func() { r.loop(ctx, target) })
	}
	wg.Wait()
}

func (r *Runner) loop(ctx context.Context, target db.Side) {
	t := time.NewTicker(r.opts.PollInterval)
	defer t.Stop()
	for {
		for ctx.Err() == nil {
			ran, err := r.RunNext(ctx, target)
			if err != nil {
				if ctx.Err() == nil {
					r.opts.Log.Warn("job queue", "event_id", "JOB_DB_ERR", "target", string(target), "error", err)
				}
				break
			}
			if !ran {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-r.wake[target]:
		case <-t.C:
		}
	}
}

// RunNext は対象の待ち行列の先頭のジョブを動かす。動かすものがなければ false。
func (r *Runner) RunNext(ctx context.Context, target db.Side) (bool, error) {
	release, ok, err := r.opts.DB.TryJobLock(ctx, target)
	if err != nil || !ok {
		return false, err
	}
	defer release()
	job, ok, err := r.opts.DB.NextQueuedJob(ctx, target)
	if err != nil || !ok {
		return false, err
	}
	job, err = r.opts.DB.StartJob(ctx, job.ID)
	if errors.Is(err, db.ErrConflict) {
		// 中断されて待ちでなくなった。次を見る。
		return true, nil
	}
	if err != nil {
		return false, err
	}
	r.runJob(ctx, job)
	return true, nil
}

// Interrupt はジョブを中断する（X9）。待ちのジョブはその場で中断にし、実行中のジョブは処理中の行を終えてから止める。
// by は中断した利用者。待ちでも実行中でもなければ db.ErrConflict。
func (r *Runner) Interrupt(ctx context.Context, id int64, by string) (db.Job, error) {
	job, err := r.opts.DB.TakeQueuedJob(ctx, id, by, reasonRequested)
	if errors.Is(err, db.ErrConflict) {
		return r.opts.DB.RequestJobInterrupt(ctx, id, by)
	}
	if err != nil {
		return db.Job{}, err
	}
	// 残りは未処理として記録する（読めなければ件数だけ）。
	if rows, ok := r.openRows(job); ok {
		out := make([]db.JobRow, 0, len(rows))
		for _, row := range rows {
			out = append(out, db.JobRow{Line: row.Line, IMSI: row.IMSI, Result: db.RowUnprocessed})
		}
		if _, err := r.opts.DB.FlushJobProgress(ctx, id, db.JobCounts{}, out); err != nil {
			r.opts.Log.Warn("write unprocessed rows", "event_id", "JOB_DB_ERR", "job_id", id, "error", err)
		}
	}
	r.recordJob(ctx, job, by, audit.ActionJobInterrupt, resultDetail(job))
	return job, nil
}

// openRows はジョブのファイルを復号して読む。
func (r *Runner) openRows(job db.Job) ([]Row, bool) {
	plain, err := r.opts.KEK.Open(job.Sealed, AAD(job.ID))
	if err != nil {
		r.opts.Log.Warn("cannot decrypt the job file", "event_id", "JOB_DECRYPT_ERR", "job_id", job.ID, "kek_id", job.KEKID, "current_kek_id", r.opts.KEK.ID())
		return nil, false
	}
	rows, errs := ParseCSV(Kind(job.Kind), plain)
	clear(plain)
	if len(errs) > 0 {
		r.opts.Log.Error("cannot parse the job file", "event_id", "JOB_FILE_ERR", "job_id", job.ID, "errors", len(errs))
		return nil, false
	}
	return rows, true
}

// jobContext は行の処理のコンテキスト（要求とは別で、シャットダウンでも処理中の行は取り消さない）。
func jobContext(job db.Job) context.Context {
	return provisioner.WithOperator(trace.With(context.Background(), traceID(job)), job.CreatedBy)
}

func traceID(job db.Job) string { return "job-" + job.JobID() }

// runJob は実行中にしたジョブを最後まで動かす（中断を含む）。
func (r *Runner) runJob(ctx context.Context, job db.Job) {
	log := r.opts.Log.With("job_id", job.ID, "kind", job.Kind)
	abort := func(reason string) {
		log.Warn("job interrupted before it started", "event_id", "JOB_INTERRUPTED", "reason", reason)
		r.finish(ctx, job, db.JobCounts{Unprocessed: job.Total}, nil, db.JobInterrupted, reason, job.CreatedBy)
	}
	var settings Settings
	if err := json.Unmarshal(job.Settings, &settings); err != nil {
		abort(reasonBad)
		return
	}
	rows, ok := r.openRows(job)
	if !ok {
		abort(reasonDecrypt)
		return
	}
	kind := Kind(job.Kind)
	if kind.Create() {
		tenants, err := r.opts.DB.Tenants(ctx)
		if err != nil {
			log.Warn("job: read tenants", "event_id", "JOB_DB_ERR", "error", err)
			abort(reasonShutdown)
			return
		}
		if !slices.ContainsFunc(tenants, func(t db.Tenant) bool { return t.ID == settings.TenantID }) {
			abort(reasonTenant)
			return
		}
	}
	var deleted []string
	if job.Deleted > 0 {
		var err error
		if deleted, err = r.opts.DB.JobDeletedIMSIs(ctx, job.ID); err != nil {
			log.Warn("job: read deleted imsis", "event_id", "JOB_DB_ERR", "error", err)
			abort(reasonShutdown)
			return
		}
	}
	r.recordJob(ctx, job, job.CreatedBy, audit.ActionJobStart, map[string]any{"kind": job.Kind, "file": job.FileName, "total": job.Total})
	log.Info("job started", "total", job.Total)

	run := &jobRun{r: r, job: job, kind: kind, settings: settings, deleted: deleted, base: jobContext(job), state: &progress{}}

	// 進み具合を定期的に書く（中断の要求にもここで気づく）。
	flushCtx := context.WithoutCancel(ctx)
	stopFlush := make(chan struct{})
	var flusher sync.WaitGroup
	flusher.Go(func() {
		t := time.NewTicker(r.opts.FlushInterval)
		defer t.Stop()
		for {
			select {
			case <-stopFlush:
				return
			case <-t.C:
				run.flush(flushCtx)
			}
		}
	})

	sem := make(chan struct{}, r.opts.Concurrency)
	var workers sync.WaitGroup
	rest := rows
dispatch:
	for i, row := range rows {
		rest = rows[i:]
		if run.state.stopReason() != "" {
			break
		}
		if ctx.Err() != nil {
			run.state.stop(reasonShutdown, "")
			break
		}
		if job.DeletedChoice == db.DeletedExclude && slices.Contains(deleted, row.IMSI) {
			run.state.add(rowResult{class: db.RowExcluded, row: db.JobRow{Line: row.Line, IMSI: row.IMSI, Result: db.RowExcluded,
				Reason: "削除済みの IMSI なので除きました。"}})
			rest = rows[i+1:]
			continue
		}
		select {
		case <-ctx.Done():
			run.state.stop(reasonShutdown, "")
			break dispatch
		case sem <- struct{}{}:
		}
		// 空きを待つ間に止める理由ができたら、この行は動かさない。
		if run.state.stopReason() != "" {
			<-sem
			break
		}
		rest = rows[i+1:]
		workers.Go(func() {
			defer func() { <-sem }()
			run.state.add(run.process(row))
		})
	}
	workers.Wait()
	close(stopFlush)
	flusher.Wait()

	var left []db.JobRow
	reason, by := run.state.stopped()
	state := db.JobCompleted
	if len(rest) == 0 {
		// 全行を処理した後に止める理由ができた（完了として扱う）。
		reason = ""
	} else {
		state = db.JobInterrupted
		for _, row := range rest {
			left = append(left, db.JobRow{Line: row.Line, IMSI: row.IMSI, Result: db.RowUnprocessed})
		}
	}
	delta, buffered := run.state.take()
	delta.Unprocessed += len(left)
	r.finish(ctx, job, delta, append(buffered, left...), state, reason, cmp.Or(by, job.CreatedBy))
}

// finish は残りの件数を書いてジョブを終え、監査ログを書く。シャットダウンのときも書けるよう、取り消されないコンテキストで行う。
func (r *Runner) finish(ctx context.Context, job db.Job, delta db.JobCounts, rows []db.JobRow, state, reason, by string) {
	ctx = context.WithoutCancel(ctx)
	log := r.opts.Log.With("job_id", job.ID, "kind", job.Kind)
	const attempts = 3
	retry := func(what string, fn func(ctx context.Context) error) error {
		var err error
		for i := range attempts {
			if i > 0 {
				time.Sleep(time.Second)
			}
			c, cancel := context.WithTimeout(ctx, 10*time.Second)
			err = fn(c)
			cancel()
			if err == nil {
				return nil
			}
			log.Warn("job: "+what, "event_id", "JOB_DB_ERR", "error", err)
		}
		return err
	}
	if err := retry("write results", func(c context.Context) error {
		_, err := r.opts.DB.FlushJobProgress(c, job.ID, delta, rows)
		return err
	}); err != nil {
		log.Error("cannot write the job results", "event_id", "JOB_DB_ERR", "error", err)
	}
	var done db.Job
	if err := retry("finish", func(c context.Context) error {
		var err error
		done, err = r.opts.DB.FinishJob(c, job.ID, state, reason)
		return err
	}); err != nil {
		// 次の起動のときに中断の印を付ける。
		log.Error("cannot finish the job", "event_id", "JOB_DB_ERR", "error", err)
		return
	}
	action := audit.ActionJobFinish
	if state == db.JobInterrupted {
		action = audit.ActionJobInterrupt
	}
	r.recordJob(ctx, done, by, action, resultDetail(done))
	log.Info("job finished", "state", state, "ok", done.OK, "exists", done.Exists, "absent", done.Absent, "failed", done.Failed,
		"unprocessed", done.Unprocessed, "excluded", done.Excluded, "reason", reason)
}

// resultDetail はジョブの結果の監査ログの内容。
func resultDetail(j db.Job) map[string]any {
	d := map[string]any{"kind": j.Kind, "file": j.FileName, "total": j.Total, "ok": j.OK, "exists": j.Exists, "absent": j.Absent,
		"failed": j.Failed, "unprocessed": j.Unprocessed, "excluded": j.Excluded}
	if j.Reason != "" {
		d["reason"] = j.Reason
	}
	return d
}

// recordJob はジョブの監査ログを書く。対象のテナントは登録のジョブだけ（削除はテナントをまたぐので、テナントなし）。
func (r *Runner) recordJob(ctx context.Context, j db.Job, by, action string, detail map[string]any) {
	var s Settings
	_ = json.Unmarshal(j.Settings, &s)
	r.opts.Audit.Record(ctx, db.AuditEntry{ActorEmail: by, ActorRole: authz.SystemAdmin, TenantID: s.TenantID, Action: action,
		Target: "job-" + j.JobID(), Detail: detail, TraceID: traceID(j), JobID: j.JobID()})
}

// progress は実行中のジョブの進み具合（処理した行の結果と、止める理由）。
type progress struct {
	mu          sync.Mutex
	delta       db.JobCounts
	rows        []db.JobRow
	consecutive int
	reason, by  string
}

func (p *progress) add(res rowResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch res.class {
	case rowOK:
		p.delta.OK++
	case db.RowExists:
		p.delta.Exists++
	case db.RowAbsent:
		p.delta.Absent++
	case db.RowFailed:
		p.delta.Failed++
	case db.RowExcluded:
		p.delta.Excluded++
	}
	if res.class != rowOK {
		p.rows = append(p.rows, res.row)
	}
	switch {
	case res.connFailure:
		p.consecutive++
		if p.consecutive >= maxConnFailure && p.reason == "" {
			p.reason = reasonAuto
		}
	case res.called:
		// 下流が応答した。下流を呼ばなかった行（既にあるなど）は数え直さない。
		p.consecutive = 0
	}
}

func (p *progress) stop(reason, by string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reason == "" {
		p.reason, p.by = reason, by
	}
}

func (p *progress) stopReason() string {
	r, _ := p.stopped()
	return r
}

func (p *progress) stopped() (reason, by string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reason, p.by
}

// take は書いていない結果を取り出す。
func (p *progress) take() (db.JobCounts, []db.JobRow) {
	p.mu.Lock()
	defer p.mu.Unlock()
	d, rows := p.delta, p.rows
	p.delta, p.rows = db.JobCounts{}, nil
	return d, rows
}

// putBack は書けなかった結果を戻す。
func (p *progress) putBack(d db.JobCounts, rows []db.JobRow) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.delta.OK += d.OK
	p.delta.Exists += d.Exists
	p.delta.Absent += d.Absent
	p.delta.Failed += d.Failed
	p.delta.Excluded += d.Excluded
	p.rows = append(p.rows, rows...)
}

// jobRun は 1 つのジョブの実行。
type jobRun struct {
	r        *Runner
	job      db.Job
	kind     Kind
	settings Settings
	deleted  []string
	base     context.Context
	state    *progress
}

// flush は進み具合を DB に書き、中断の要求を確かめる。
func (j *jobRun) flush(ctx context.Context) {
	d, rows := j.state.take()
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	by, err := j.r.opts.DB.FlushJobProgress(c, j.job.ID, d, rows)
	if err != nil {
		j.r.opts.Log.Warn("job: write progress", "event_id", "JOB_DB_ERR", "job_id", j.job.ID, "error", err)
		j.state.putBack(d, rows)
		return
	}
	if by != "" {
		j.state.stop(reasonRequested, by)
	}
}

// rowOK は成功の行の結果（行は記録しない）。
const rowOK = "ok"

// rowResult は 1 行の処理の結果。
type rowResult struct {
	class string
	row   db.JobRow
	// called は下流を呼んで応答があったか。connFailure は接続の誤りか（X8。続いたら中断）。
	called, connFailure bool
}

// connectionFailure は接続の誤り（届かない・503・タイムアウト）か。
func connectionFailure(err error) bool {
	return provisioner.IsUnavailable(err) || provisioner.StatusOf(err) == 503 || provisioner.StatusOf(err) == 504
}

// describe は aka-provisioner の誤りを結果の行の文にする。
func (j *jobRun) describe(err error) string {
	if j.r.opts.Describe != nil {
		return j.r.opts.Describe(err, j.kind.Side())
	}
	return err.Error()
}

// process は 1 行を処理する（設計概要 §10.3。X8）。行の処理は画面の操作と同じ手順（台帳のロック、成功で台帳と事実、結果が分からなければ
// 印と読み直し）。
func (j *jobRun) process(row Row) rowResult {
	meta := db.Meta{Operator: j.job.CreatedBy, TraceID: traceID(j.job), JobID: j.job.JobID()}
	if j.kind.Create() {
		return j.processCreate(row, meta)
	}
	return j.processDelete(row, meta)
}

// idemKey は行の Idempotency-Key（ジョブの ID と IMSI から作る）。
func (j *jobRun) idemKey(imsi string) string { return traceID(j.job) + "-" + imsi }

// result は結果の行を作る。
func result(row Row, class, reason string) rowResult {
	return rowResult{class: class, row: db.JobRow{Line: row.Line, IMSI: row.IMSI, Result: class, Reason: reason}}
}

// recordRow は行の操作を監査ログに書く（行の操作は 3-1 の画面と同じ名前に job_id）。
func (j *jobRun) recordRow(action, imsi string, tenant int64, detail map[string]any) {
	j.r.opts.Audit.Record(j.base, db.AuditEntry{ActorEmail: j.job.CreatedBy, ActorRole: authz.SystemAdmin, TenantID: tenant, Action: action,
		Target: imsi, Detail: detail, TraceID: traceID(j.job), JobID: j.job.JobID()})
}

func resultWord(o ledger.Outcome) string {
	if o.Unknown {
		return "unknown"
	}
	return "ok"
}

// finishOutcome は手順の結果を行の結果にする。done は、結果が分からなかったときに読み直した台帳で、操作が済んでいたかを判定する。
func (j *jobRun) finishOutcome(row Row, o ledger.Outcome, err error, refused *rowResult, onConflict func(error) (rowResult, bool),
	done func(db.Entry, bool) bool) (rowResult, bool) {
	switch {
	case err != nil:
		res := result(row, db.RowFailed, "台帳を読み書きできませんでした。")
		res.row.Unknown = o.Succeeded
		// DB に届かないまま全行を失敗にしないよう、接続の誤りと同じに数える。台帳の断りは行だけの失敗。
		res.connFailure = !errors.Is(err, db.ErrLedgerConflict) && !errors.Is(err, db.ErrTenantNotFound)
		return res, false
	case o.Refused != "":
		return *refused, false
	case o.Unknown:
		res := rowResult{called: !connectionFailure(o.Err), connFailure: connectionFailure(o.Err)}
		if o.Fixed && done(o.Entry, o.Exists) {
			res.class = rowOK
			return res, true
		}
		msg := "結果が分からず、読み直しもできませんでした（台帳に確かめる印を付けました）: " + j.describe(o.Err)
		if o.Fixed {
			msg = "結果が分からなかったので読み直しましたが、下流の側に反映されていませんでした: " + j.describe(o.Err)
		}
		res.class, res.row = db.RowFailed, db.JobRow{Line: row.Line, IMSI: row.IMSI, Result: db.RowFailed, Reason: msg, Unknown: true}
		return res, true
	case o.Err != nil:
		if res, ok := onConflict(o.Err); ok {
			res.called = true
			return res, false
		}
		res := result(row, db.RowFailed, j.describe(o.Err))
		res.called = true
		return res, false
	}
	return rowResult{class: rowOK, called: true}, true
}

func (j *jobRun) processCreate(row Row, meta db.Meta) rowResult {
	side, tenant := j.kind.Side(), j.settings.TenantID
	var refused rowResult
	refuse := func(class, msg string) string {
		refused = result(row, class, msg)
		return msg
	}
	reregister := false
	o, err := j.r.ops.Run(j.base, meta, row.IMSI, ledger.Step{
		Check: func(e db.Entry, exists bool) string {
			switch {
			case exists && e.CheckNeeded:
				return refuse(db.RowFailed, "検証の後に、下流の結果を確かめる印が付きました。")
			case exists && e.Deleted():
				if _, approved := slices.BinarySearch(j.deleted, row.IMSI); !approved || j.job.DeletedChoice != db.DeletedInclude {
					return refuse(db.RowFailed, "検証の後に削除されました（登録し直すには承認が要ります）。")
				}
				reregister = true
			case exists && e.TenantID != tenant:
				return refuse(db.RowFailed, "検証の後に別のテナントに属しました。")
			case exists && e.Present(side):
				return refuse(db.RowExists, "既にあります。")
			}
			return ""
		},
		Call: func(ctx context.Context, _ db.Entry) error { return j.create(ctx, row) },
		Change: func(e db.Entry, exists bool) db.Change {
			ch := db.Change{Kind: db.ChangeRegister, Side: side, State: j.settings.State, SourceID: j.settings.SourceID}
			if !exists || e.Deleted() {
				ch.TenantID = tenant
			}
			return ch
		},
		Tenant: tenant,
	})
	res, wrote := j.finishOutcome(row, o, err, &refused, func(err error) (rowResult, bool) {
		if provisioner.CauseOf(err) == provisioner.CauseSubscriberExists {
			return result(row, db.RowExists, "下流に既にあります（台帳にはありません。突き合わせで確かめます）。"), true
		}
		return rowResult{}, false
	}, func(e db.Entry, exists bool) bool { return exists && e.Present(side) })
	if wrote {
		j.recordRow(createAction(side), row.IMSI, tenant, j.createDetail(row, o, reregister))
	}
	return res
}

func createAction(side db.Side) string {
	if side == db.SideVecgen {
		return audit.ActionVecgenCreate
	}
	return audit.ActionRadiusCreate
}

// createDetail は行の登録の監査ログの内容（鍵を入れない）。
func (j *jobRun) createDetail(row Row, o ledger.Outcome, reregister bool) map[string]any {
	d := map[string]any{"side": string(j.kind.Side()), "state": j.settings.State, "result": resultWord(o)}
	if j.kind == VecgenCreate {
		d["amf"], d["sqn"], d["sqnType"], d["allowedClientIds"] = row.AMF, row.SQN, j.settings.SQNType, j.clients()
	} else {
		d["sourceId"], d["rules"] = j.settings.SourceID, len(j.settings.Rules)
	}
	if reregister {
		d["reregister"] = true
	}
	return d
}

func (j *jobRun) clients() []int64 {
	if j.settings.AllowedClientIDs == nil {
		return []int64{}
	}
	return j.settings.AllowedClientIDs
}

// create は下流に登録する。本文は構造体から決まった並びで組み立てる。
func (j *jobRun) create(ctx context.Context, row Row) error {
	if j.kind == VecgenCreate {
		_, err := j.r.opts.PV.CreateVecgenSubscriber(ctx, provisioner.VecgenSubscriberCreate{IMSI: row.IMSI, Ki: row.Ki, OPc: row.OPc,
			State: j.settings.State, SQN: row.SQN, AMF: row.AMF, SQNType: j.settings.SQNType, AllowedClientIDs: j.clients()}, j.idemKey(row.IMSI))
		return err
	}
	_, err := j.r.opts.PV.CreateRadiusSubscriber(ctx, provisioner.RadiusSubscriberCreate{IMSI: row.IMSI, State: j.settings.State,
		SourceID: j.settings.SourceID, Rules: j.settings.Rules}, j.idemKey(row.IMSI))
	return err
}

func (j *jobRun) processDelete(row Row, meta db.Meta) rowResult {
	side := j.kind.Side()
	var refused rowResult
	refuse := func(class, msg string) string {
		refused = result(row, class, msg)
		return msg
	}
	var tenant int64
	var state string
	o, err := j.r.ops.Run(j.base, meta, row.IMSI, ledger.Step{
		Check: func(e db.Entry, exists bool) string {
			tenant, state = e.TenantID, e.State(side)
			switch {
			case !exists || e.Deleted() || !e.Present(side):
				return refuse(db.RowAbsent, "台帳にありません。")
			case e.CheckNeeded:
				return refuse(db.RowFailed, "検証の後に、下流の結果を確かめる印が付きました。")
			case e.State(side) == db.StateActive:
				return refuse(db.RowFailed, "検証の後に利用中（Active）になりました。")
			}
			return ""
		},
		Call: func(ctx context.Context, _ db.Entry) error {
			return j.r.opts.PV.DeleteSubscriber(ctx, provisioner.Downstream(side), row.IMSI, j.idemKey(row.IMSI))
		},
		Change: func(db.Entry, bool) db.Change { return db.Change{Kind: db.ChangeDelete, Side: side} },
	})
	res, wrote := j.finishOutcome(row, o, err, &refused, func(err error) (rowResult, bool) {
		if provisioner.StatusOf(err) == 404 {
			return result(row, db.RowAbsent, "下流にありません（台帳にはあります。突き合わせで直します）。"), true
		}
		return rowResult{}, false
	}, func(e db.Entry, exists bool) bool { return !exists || !e.Present(side) })
	if wrote {
		j.recordRow(audit.ActionSubscriberDelete, row.IMSI, tenant, map[string]any{"side": string(side), "state": state, "result": resultWord(o)})
	}
	return res
}
