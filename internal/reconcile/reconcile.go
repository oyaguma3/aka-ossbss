package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/oyaguma3/aka-ossbss/internal/audit"
	"github.com/oyaguma3/aka-ossbss/internal/authz"
	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/ledger"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
	"github.com/oyaguma3/aka-ossbss/internal/trace"
)

const (
	// Operator は自動の修正の操作者（状態の事実と監査ログに書く。Y8）。
	Operator = "system"
	// ReasonFix は自動の修正の補いの事実の理由、ReasonImport は取り込みの補いの事実の理由。
	ReasonFix    = "突き合わせ"
	ReasonImport = "突き合わせで取り込み"

	// pageSize は下流の一覧の 1 回の読み取りの数（下流の上限）、ledgerPage は台帳の 1 回の読み取りの数。
	pageSize   = 500
	ledgerPage = 1000
	// confirmBatch は候補を読み直す 1 回の数（一括取得の上限）。
	confirmBatch = provisioner.MaxBatchGet
	// maxFindings は 1 回の実行で行として残すずれの数（総数は別に数える。Y7）。
	maxFindings = 10000
	// findingsFlush はずれをまとめて書く数。progressEvery は読んだ行の数を書く間隔（行の数）。
	findingsFlush = 500
	progressEvery = 5000
	// checkLimit は 1 回の確かめで読み直す印の数（Y10）。
	checkLimit = 100
	// runLock は実行を重ねないロックの名前。
	runLock = "reconcile-run"
)

// ErrRunning は突き合わせが動いていること。
var ErrRunning = errors.New("reconcile is running")

// ErrUnreadable は下流を読めなかったこと。
var ErrUnreadable = errors.New("cannot read the downstreams")

// Store は Runner が使う DB の操作。*db.DB が満たす。
type Store interface {
	ledger.DB
	audit.Inserter
	ReconcileEntries(ctx context.Context, after string, limit int) ([]db.Entry, error)
	SubscribersByIMSI(ctx context.Context, imsis []string) (map[string]db.Entry, error)
	CreateReconcileRun(ctx context.Context, trigger, by, reason string) (db.ReconcileRun, error)
	UpdateReconcileProgress(ctx context.Context, id int64, ledger, vecgen, radius int) error
	FinishReconcileRun(ctx context.Context, r db.ReconcileRun) (db.ReconcileRun, error)
	InterruptRunningReconciles(ctx context.Context) error
	AddFindings(ctx context.Context, runID int64, fs []db.Finding) error
	ResolveFindings(ctx context.Context, runID int64, imsi string, side db.Side, status, by string) (int64, error)
	CheckNeededIMSIs(ctx context.Context, limit int) ([]string, error)
	TryNamedLock(ctx context.Context, name string) (release func(), ok bool, err error)
}

// Provisioner は Runner が使う aka-provisioner の口。*provisioner.Client が満たす。
type Provisioner interface {
	ledger.Provisioner
	ListVecgenSubscribers(ctx context.Context, cursor string, limit int) (provisioner.VecgenList, error)
	ListRadiusSubscribers(ctx context.Context, cursor string, limit int) (provisioner.RadiusList, error)
	RadiusSubscriber(ctx context.Context, imsi string) (provisioner.RadiusSubscriber, error)
	UpdateRadiusSubscriber(ctx context.Context, imsi string, u provisioner.RadiusSubscriberUpdate, idempotencyKey string) (provisioner.RadiusSubscriber, error)
	SetState(ctx context.Context, d provisioner.Downstream, imsi string, ch provisioner.StateChange, idempotencyKey string) (string, error)
	DeleteSubscriber(ctx context.Context, d provisioner.Downstream, imsi, idempotencyKey string) error
}

// Options は Runner の設定。
type Options struct {
	Log   *slog.Logger
	DB    Store
	PV    Provisioner
	Audit *audit.Recorder
	// MaxFixes は自動で直す数の上限（超えたら保留にする。Y4）。
	MaxFixes int
	// Base は手動の実行のコンテキスト（プロセスの終わりまで）。
	Base context.Context
}

// Runner は突き合わせを動かす。
type Runner struct {
	opts    Options
	ops     *ledger.Ops
	running atomic.Bool
	wg      sync.WaitGroup
}

// New は Runner を作る。
func New(opts Options) *Runner {
	if opts.Base == nil {
		opts.Base = context.Background()
	}
	return &Runner{opts: opts, ops: &ledger.Ops{DB: opts.DB, PV: opts.PV, Log: opts.Log}}
}

// Recover は、実行中のまま残っている実行を中断にする（起動のとき）。
func (r *Runner) Recover(ctx context.Context) error { return r.opts.DB.InterruptRunningReconciles(ctx) }

// begin は実行を始める（重ねない）。
func (r *Runner) begin(ctx context.Context, trigger, by, reason string) (db.ReconcileRun, func(), error) {
	if !r.running.CompareAndSwap(false, true) {
		return db.ReconcileRun{}, nil, ErrRunning
	}
	release, ok, err := r.opts.DB.TryNamedLock(ctx, runLock)
	if err != nil || !ok {
		r.running.Store(false)
		if err == nil {
			err = ErrRunning
		}
		return db.ReconcileRun{}, nil, err
	}
	run, err := r.opts.DB.CreateReconcileRun(ctx, trigger, by, reason)
	if err != nil {
		release()
		r.running.Store(false)
		return db.ReconcileRun{}, nil, err
	}
	return run, func() {
		release()
		r.running.Store(false)
	}, nil
}

// RunScheduled は定刻の実行（終わるまで戻らない）。手動の実行が動いていれば飛ばす。
func (r *Runner) RunScheduled(ctx context.Context) (string, error) {
	run, end, err := r.begin(ctx, db.TriggerSchedule, Operator, "")
	if errors.Is(err, ErrRunning) {
		return "ほかの実行が動いているので飛ばしました。", nil
	}
	if err != nil {
		return "", err
	}
	defer end()
	return r.execute(ctx, run)
}

// Start は手動の実行か承認の実行を始めてすぐに戻る。動いていれば ErrRunning。
func (r *Runner) Start(ctx context.Context, trigger, by, reason string) (db.ReconcileRun, error) {
	run, end, err := r.begin(ctx, trigger, by, reason)
	if err != nil {
		return db.ReconcileRun{}, err
	}
	r.wg.Go(func() {
		defer end()
		_, _ = r.execute(r.opts.Base, run)
	})
	return run, nil
}

// Wait は手動の実行が終わるのを待つ（シャットダウンのときと試験に使う）。
func (r *Runner) Wait() { r.wg.Wait() }

// actor は実行の主体（定刻は system、手動はシステム管理者）。
func actor(by string) authz.Role {
	if by == Operator {
		return ""
	}
	return authz.SystemAdmin
}

// traceOf は実行のトレースID。
func traceOf(run db.ReconcileRun) string { return "reconcile-" + strconv.FormatInt(run.ID, 10) }

// record は実行の監査ログを書く。
func (r *Runner) record(ctx context.Context, run db.ReconcileRun, action, target string, tenant int64, detail map[string]any) {
	r.opts.Audit.Record(ctx, db.AuditEntry{ActorEmail: run.RequestedBy, ActorRole: actor(run.RequestedBy), TenantID: tenant, Action: action,
		Target: target, Detail: detail, TraceID: traceOf(run)})
}

// runDetail は実行の結果の文と監査ログの内容。
func runDetail(run db.ReconcileRun, pending int) (string, map[string]any) {
	text := fmt.Sprintf("読んだ行: 台帳 %d・vecgen 側 %d・radius 側 %d。直した数 %d、未対応 %d、見つけたずれ %d。",
		run.LedgerRows, run.VecgenRows, run.RadiusRows, run.FixedCount, pending, run.FindingsTotal)
	return text, map[string]any{"trigger": run.Trigger, "state": run.State, "ledgerRows": run.LedgerRows, "vecgenRows": run.VecgenRows,
		"radiusRows": run.RadiusRows, "fixable": run.FixableCount, "fixed": run.FixedCount, "pending": pending, "findings": run.FindingsTotal}
}

// execute は実行の本体。読み取り（併合 → 読み直し）を終えるまで何も直さない。
func (r *Runner) execute(ctx context.Context, run db.ReconcileRun) (string, error) {
	log := r.opts.Log.With("reconcile_run", run.ID)
	ctx = trace.With(ctx, traceOf(run))
	r.record(ctx, run, audit.ActionReconcileStart, "", 0, map[string]any{"trigger": run.Trigger})
	st := &runState{r: r, run: run}

	finish := func(state, errText string, runErr error) (string, error) {
		st.run.State, st.run.Error = state, errText
		wctx := context.WithoutCancel(ctx)
		if err := st.flush(wctx, true); err != nil {
			log.Warn("reconcile: write findings", "event_id", "RECONCILE_FAILED", "error", err)
		}
		done, err := r.opts.DB.FinishReconcileRun(wctx, st.run)
		if err != nil {
			log.Error("reconcile: finish", "event_id", "RECONCILE_FAILED", "error", err)
			done = st.run
		}
		detail, auditDetail := runDetail(done, st.pending)
		if errText != "" {
			auditDetail["error"] = errText
		}
		action := audit.ActionReconcileFinish
		if state == db.RunHeld {
			action = audit.ActionReconcileHold
		}
		r.record(wctx, done, action, "", 0, auditDetail)
		switch {
		case state == db.RunFailed:
			log.Error("reconcile failed", "event_id", "RECONCILE_FAILED", "error", errText)
		case state == db.RunHeld || done.FixedCount > 0 || st.pending > 0:
			log.Warn("reconcile found drift", "event_id", "RECONCILE_DRIFT", "state", state, "fixed", done.FixedCount, "fixable", done.FixableCount, "pending", st.pending)
		default:
			log.Info("reconcile finished", "state", state, "ledger_rows", done.LedgerRows)
		}
		return detail, runErr
	}
	fail := func(err error) (string, error) {
		if ctx.Err() != nil {
			return finish(db.RunInterrupted, "", ctx.Err())
		}
		text := "下流か台帳を読めなかったので、ここで止めました: " + err.Error()
		return finish(db.RunFailed, text, errors.New(text))
	}

	candidates, err := st.scan(ctx)
	if err != nil {
		return fail(err)
	}
	fixable, err := st.confirm(ctx, candidates)
	if err != nil {
		return fail(err)
	}
	st.run.FixableCount = len(fixable)
	if run.Trigger != db.TriggerApproved && len(fixable) > r.opts.MaxFixes {
		// 安全弁: 何も直さず、システム管理者の判断を待つ（Y4）。
		for _, f := range fixable {
			f.Status = db.FindingHeld
			st.add(f)
		}
		return finish(db.RunHeld, "", nil)
	}
	if err := st.fix(ctx, fixable); err != nil {
		return fail(err)
	}
	return finish(db.RunCompleted, "", nil)
}

// runState は 1 回の実行の途中の値。
type runState struct {
	r       *Runner
	run     db.ReconcileRun
	buf     []db.Finding
	stored  int
	pending int
}

// add はずれを数えて書く列に足す（行として残すのは maxFindings 件まで）。
func (s *runState) add(f db.Finding) {
	s.run.FindingsTotal++
	if f.Status == db.FindingPending {
		s.pending++
	}
	if s.stored < maxFindings {
		s.stored++
		s.buf = append(s.buf, f)
	}
}

// flush はたまったずれを書く。
func (s *runState) flush(ctx context.Context, force bool) error {
	if len(s.buf) == 0 || (!force && len(s.buf) < findingsFlush) {
		return nil
	}
	if err := s.r.opts.DB.AddFindings(ctx, s.run.ID, s.buf); err != nil {
		return err
	}
	s.buf = nil
	return nil
}

// pager はページごとに読む列。
type pager[T any] struct {
	items []T
	i     int
	next  string
	done  bool
	key   func(T) string
	fetch func(cursor string) (items []T, next string, err error)
}

// peek は先頭の行の IMSI を返す（なければ空）。
func (p *pager[T]) peek() (string, error) {
	for p.i >= len(p.items) {
		if p.done {
			return "", nil
		}
		items, next, err := p.fetch(p.next)
		if err != nil {
			return "", err
		}
		p.items, p.i, p.next = items, 0, next
		p.done = next == ""
	}
	return p.key(p.items[p.i]), nil
}

// take は先頭の行の IMSI が imsi なら、その行を取り出す。
func (p *pager[T]) take(imsi string) (T, bool) {
	var zero T
	if p.i < len(p.items) && p.key(p.items[p.i]) == imsi {
		p.i++
		return p.items[p.i-1], true
	}
	return zero, false
}

// scan は台帳と両側の一覧を IMSI の順に併合して、ずれのありそうな IMSI を返す（全件をメモリーに持たない）。
func (s *runState) scan(ctx context.Context) ([]string, error) {
	opts := s.r.opts
	entries := &pager[db.Entry]{key: func(e db.Entry) string { return e.IMSI }, fetch: func(cursor string) ([]db.Entry, string, error) {
		items, err := opts.DB.ReconcileEntries(ctx, cursor, ledgerPage)
		if err != nil || len(items) < ledgerPage {
			return items, "", err
		}
		return items, items[len(items)-1].IMSI, nil
	}}
	vecgen := &pager[provisioner.VecgenSubscriber]{key: func(v provisioner.VecgenSubscriber) string { return v.IMSI },
		fetch: func(cursor string) ([]provisioner.VecgenSubscriber, string, error) {
			l, err := opts.PV.ListVecgenSubscribers(ctx, cursor, pageSize)
			return l.Items, l.NextCursor, err
		}}
	radius := &pager[provisioner.RadiusSubscriber]{key: func(v provisioner.RadiusSubscriber) string { return v.IMSI },
		fetch: func(cursor string) ([]provisioner.RadiusSubscriber, string, error) {
			l, err := opts.PV.ListRadiusSubscribers(ctx, cursor, pageSize)
			return l.Items, l.NextCursor, err
		}}
	var candidates []string
	rows := 0
	for {
		a, err := entries.peek()
		if err != nil {
			return nil, err
		}
		b, err := vecgen.peek()
		if err != nil {
			return nil, err
		}
		c, err := radius.peek()
		if err != nil {
			return nil, err
		}
		imsi := minNonEmpty(a, b, c)
		if imsi == "" {
			break
		}
		e, exists := entries.take(imsi)
		v, hasV := vecgen.take(imsi)
		rad, hasR := radius.take(imsi)
		var vv, rv view
		if exists {
			s.run.LedgerRows++
		}
		if hasV {
			s.run.VecgenRows++
			vv = vecgenView(v)
		}
		if hasR {
			s.run.RadiusRows++
			rv = radiusView(rad)
		}
		if len(compare(e, exists, vv, rv, imsi)) > 0 {
			candidates = append(candidates, imsi)
		}
		if rows++; rows%progressEvery == 0 {
			if err := opts.DB.UpdateReconcileProgress(ctx, s.run.ID, s.run.LedgerRows, s.run.VecgenRows, s.run.RadiusRows); err != nil {
				return nil, err
			}
		}
	}
	return candidates, nil
}

// minNonEmpty は空でない最小の文字列を返す（なければ空）。
func minNonEmpty(vs ...string) string {
	out := ""
	for _, v := range vs {
		if v != "" && (out == "" || v < out) {
			out = v
		}
	}
	return out
}

// confirm は候補を一括取得で読み直して絞る（読み取りの間に変わったものを除く）。システム管理者が決めるずれと報告を書く列に足し、
// 自動で直すずれを返す。
func (s *runState) confirm(ctx context.Context, candidates []string) ([]db.Finding, error) {
	opts := s.r.opts
	var fixable []db.Finding
	for start := 0; start < len(candidates); start += confirmBatch {
		batch := candidates[start:min(start+confirmBatch, len(candidates))]
		entries, err := opts.DB.SubscribersByIMSI(ctx, batch)
		if err != nil {
			return nil, err
		}
		vb, err := opts.PV.VecgenBatchGet(ctx, batch)
		if err != nil {
			return nil, err
		}
		rb, err := opts.PV.RadiusBatchGet(ctx, batch)
		if err != nil {
			return nil, err
		}
		vm := make(map[string]view, len(vb.Items))
		for _, x := range vb.Items {
			vm[x.IMSI] = vecgenView(x)
		}
		rm := make(map[string]view, len(rb.Items))
		for _, x := range rb.Items {
			rm[x.IMSI] = radiusView(x)
		}
		for _, imsi := range batch {
			e, exists := entries[imsi]
			found := compare(e, exists, vm[imsi], rm[imsi], imsi)
			if needsLockCheck(found) {
				// 処理中の操作（下流には書いたが、台帳はまだ）を「台帳にない」と記録しないよう、ロックを取って台帳を見直す。
				absent, err := s.stillAbsent(ctx, imsi)
				if err != nil {
					return nil, err
				}
				if !absent {
					continue // 次の回で見る
				}
			}
			for _, f := range found {
				switch {
				case Fixable(f.Kind):
					fixable = append(fixable, f)
				case f.Kind == db.FindingRuleTenant:
					f.Status = db.FindingInfo
					s.add(f)
				default:
					f.Status = db.FindingPending
					s.add(f)
				}
			}
		}
		if err := s.flush(ctx, false); err != nil {
			return nil, err
		}
	}
	return fixable, nil
}

// needsLockCheck は、システム管理者が決めるずれがあるか。
func needsLockCheck(fs []db.Finding) bool {
	return slices.ContainsFunc(fs, func(f db.Finding) bool {
		return f.Kind == db.FindingDownstreamOnly || f.Kind == db.FindingDeletedPresent
	})
}

// stillAbsent は、ロックを取った後も IMSI が台帳にない（または削除済み）かを返す。
func (s *runState) stillAbsent(ctx context.Context, imsi string) (bool, error) {
	absent := false
	err := s.r.opts.DB.WithSubscriber(ctx, imsi, db.Meta{Operator: s.run.RequestedBy, TraceID: traceOf(s.run)}, func(l db.Ledger) error {
		e, exists := l.Entry()
		absent = !exists || e.Deleted()
		return nil
	})
	return absent, err
}

// fix は IMSI ごとのロックの中で読み直して、台帳を下流に合わせる。
func (s *runState) fix(ctx context.Context, fixable []db.Finding) error {
	opts := s.r.opts
	meta := db.Meta{Operator: s.run.RequestedBy, TraceID: traceOf(s.run)}
	for i := 0; i < len(fixable); {
		imsi := fixable[i].IMSI
		for i < len(fixable) && fixable[i].IMSI == imsi {
			i++
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// 行の修正は取り消さない（下流を読んだ結果を台帳に書き損じないように）。
		rowCtx := context.WithoutCancel(ctx)
		var fixed []db.Finding
		var tenant int64
		err := opts.DB.WithSubscriber(rowCtx, imsi, meta, func(l db.Ledger) error {
			e, exists := l.Entry()
			if !exists || e.Deleted() {
				return nil // 読み直しの後に変わった（次の回で見る）
			}
			vo, ro, ok := s.r.ops.Observe(rowCtx, imsi)
			if !ok {
				return ErrUnreadable
			}
			for _, f := range compare(e, exists, observationView(vo), observationView(ro), imsi) {
				if Fixable(f.Kind) {
					fixed = append(fixed, f)
				}
			}
			if len(fixed) == 0 {
				return nil
			}
			tenant = e.TenantID
			return l.Apply(db.Change{Kind: db.ChangeObserve, Vecgen: vo, Radius: ro, Reason: ReasonFix})
		})
		if err != nil {
			return err
		}
		if len(fixed) == 0 {
			continue
		}
		detail := make([]map[string]any, 0, len(fixed))
		for _, f := range fixed {
			f.Status = db.FindingFixed
			s.add(f)
			s.run.FixedCount++
			detail = append(detail, map[string]any{"kind": f.Kind, "side": string(f.Side), "ledger": f.LedgerVal, "downstream": f.DownstreamVal})
		}
		s.r.record(rowCtx, s.run, audit.ActionSubscriberReconcile, imsi, tenant, map[string]any{"findings": detail})
		if err := s.flush(rowCtx, false); err != nil {
			return err
		}
	}
	return nil
}

// CheckMarked は結果を確かめる印の立った行を読み直して台帳を直す（5 分ごと。Y10）。下流を読めなければその回はやめる。
func (r *Runner) CheckMarked(ctx context.Context) (string, error) {
	imsis, err := r.opts.DB.CheckNeededIMSIs(ctx, checkLimit)
	if err != nil {
		return "", err
	}
	const traceID = "check-needed"
	ctx = trace.With(ctx, traceID)
	meta := db.Meta{Operator: Operator, TraceID: traceID}
	fixed := 0
	for _, imsi := range imsis {
		if ctx.Err() != nil {
			break
		}
		rowCtx := context.WithoutCancel(ctx)
		tenant, ok := r.ops.Recheck(rowCtx, meta, imsi)
		if !ok {
			break
		}
		fixed++
		r.opts.Audit.Record(rowCtx, db.AuditEntry{ActorEmail: Operator, TenantID: tenant, Action: audit.ActionSubscriberCheck, Target: imsi, TraceID: traceID})
	}
	if fixed == 0 {
		return "", nil
	}
	return fmt.Sprintf("結果を確かめる印の %d 件を読み直しました。", fixed), nil
}

// Import は IMSI を台帳に取り込む（Y6）。実行のときにロックの中で読み直し、解消していれば db.FindingGone を返す。
// ルールのテナントが tenant と違えば、ルールを書き換えてから台帳を書く（解約のものは書き換えられないので、そのまま取り込む）。
// ctx には操作者とトレースID を入れておく。
func (r *Runner) Import(ctx context.Context, f db.Finding, tenant int64, by string) (string, error) {
	status := db.FindingGone
	meta := db.Meta{Operator: by, TraceID: trace.From(ctx)}
	err := r.opts.DB.WithSubscriber(ctx, f.IMSI, meta, func(l db.Ledger) error {
		e, exists := l.Entry()
		if exists && !e.Deleted() {
			return nil
		}
		vo, ro, ok := r.ops.Observe(ctx, f.IMSI)
		if !ok {
			return ErrUnreadable
		}
		if !vo.Present && !ro.Present {
			return nil
		}
		if ro.Present && ro.State != db.StateTerminated {
			if err := r.rewriteRules(ctx, f, tenant); err != nil {
				return err
			}
		}
		if err := l.Apply(db.Change{Kind: db.ChangeObserve, TenantID: tenant, Vecgen: vo, Radius: ro, Reason: ReasonImport}); err != nil {
			return err
		}
		status = db.FindingImported
		return nil
	})
	if err != nil {
		return "", err
	}
	if _, err := r.opts.DB.ResolveFindings(ctx, f.RunID, f.IMSI, "", status, by); err != nil {
		return "", err
	}
	return status, nil
}

// rewriteRules は、ルールのテナントが tenant と違うものがあれば書き換える。キーにテナントを入れる（別のテナントでやり直せるように）。
func (r *Runner) rewriteRules(ctx context.Context, f db.Finding, tenant int64) error {
	rs, err := r.opts.PV.RadiusSubscriber(ctx, f.IMSI)
	if err != nil {
		return err
	}
	changed := false
	for i := range rs.Rules {
		if rs.Rules[i].TenantID != tenant {
			rs.Rules[i].TenantID, changed = tenant, true
		}
	}
	if !changed {
		return nil
	}
	key := fmt.Sprintf("reconcile-%d-rules-%d", f.ID, tenant)
	_, err = r.opts.PV.UpdateRadiusSubscriber(ctx, f.IMSI, provisioner.RadiusSubscriberUpdate{Rules: rs.Rules}, key)
	return err
}

// Delete は下流のその側から削除する（Y6）。Active は止めてから消す。実行のときにロックの中で読み直し、解消していれば db.FindingGone を返す。
// 台帳に行がないので、台帳と状態の事実は書かない。ctx には操作者とトレースID を入れておく。
func (r *Runner) Delete(ctx context.Context, f db.Finding, by string) (string, error) {
	status := db.FindingGone
	meta := db.Meta{Operator: by, TraceID: trace.From(ctx)}
	err := r.opts.DB.WithSubscriber(ctx, f.IMSI, meta, func(l db.Ledger) error {
		e, exists := l.Entry()
		if exists && !e.Deleted() && e.Present(f.Side) {
			return nil
		}
		vo, ro, ok := r.ops.Observe(ctx, f.IMSI)
		if !ok {
			return ErrUnreadable
		}
		o := vo
		if f.Side == db.SideRadius {
			o = ro
		}
		if !o.Present {
			return nil
		}
		d := provisioner.Downstream(f.Side)
		if o.State == db.StateActive {
			ch := provisioner.StateChange{State: db.StateInactive, Reason: "突き合わせで削除"}
			if _, err := r.opts.PV.SetState(ctx, d, f.IMSI, ch, fmt.Sprintf("reconcile-%d-state", f.ID)); err != nil {
				return err
			}
		}
		if err := r.opts.PV.DeleteSubscriber(ctx, d, f.IMSI, fmt.Sprintf("reconcile-%d-delete", f.ID)); err != nil {
			return err
		}
		status = db.FindingDeleted
		return nil
	})
	if err != nil {
		return "", err
	}
	if _, err := r.opts.DB.ResolveFindings(ctx, f.RunID, f.IMSI, f.Side, status, by); err != nil {
		return "", err
	}
	return status, nil
}
