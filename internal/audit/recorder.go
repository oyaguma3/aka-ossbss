package audit

import (
	"context"
	"log/slog"
	"maps"
	"time"

	"github.com/oyaguma3/aka-ossbss/internal/db"
)

// aka-ossbss の監査ログの操作の名前のうち、画面と一括投入のジョブの両方が書くもの（行ごとの操作）と、ジョブの操作（段階 3-2 の X11）。
const (
	ActionVecgenCreate     = "subscriber.vecgen.create"
	ActionRadiusCreate     = "subscriber.radius.create"
	ActionSubscriberDelete = "subscriber.delete"
	ActionJobCreate        = "job.create"
	ActionJobStart         = "job.start"
	ActionJobFinish        = "job.finish"
	ActionJobCancel        = "job.cancel"
	ActionJobInterrupt     = "job.interrupt"
	ActionJobReregister    = "job.reregister"

	// 突き合わせと結果の確かめ（段階 3-3 の Y8）。
	ActionSubscriberReconcile = "subscriber.reconcile"
	ActionSubscriberCheck     = "subscriber.check"
	ActionReconcileStart      = "reconcile.start"
	ActionReconcileFinish     = "reconcile.finish"
	ActionReconcileHold       = "reconcile.hold"
	ActionReconcileApprove    = "reconcile.approve"
	ActionReconcileImport     = "reconcile.import"
	ActionReconcileDelete     = "reconcile.delete"
)

// insertTimeout は監査ログの書き込み 1 回の上限時間。
const insertTimeout = 5 * time.Second

// Inserter は監査ログを PostgreSQL に書く口。*db.DB が満たす。
type Inserter interface {
	InsertAudit(ctx context.Context, e db.AuditEntry) (int64, time.Time, error)
}

// Recorder は、済んだ操作を監査ログ（PostgreSQL と日ごとのファイル）に残す。
type Recorder struct {
	DB Inserter
	// Files は日ごとのファイル（nil なら書かない）。
	Files *Files
	Log   *slog.Logger
}

// Record は監査ログを書く。Detail に秘密の値（Ki / OPc）を入れてはならない。書き込みに失敗しても操作は済んでいるので、
// ERROR のログを出して続ける。要求が取り消されても（利用者が画面を閉じても）、済んだ操作の記録は残す。
func (r *Recorder) Record(ctx context.Context, e db.AuditEntry) {
	e.Detail = maps.Clone(e.Detail)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), insertTimeout)
	defer cancel()
	id, at, err := r.DB.InsertAudit(ctx, e)
	if err != nil {
		r.Log.Error("audit log write failed", "event_id", "AUDIT_DB_ERR", "action", e.Action, "target", e.Target, "user", e.ActorEmail,
			"trace_id", e.TraceID, "error", err)
		at = time.Now()
	}
	if r.Files == nil {
		return
	}
	if err := r.Files.Append(Entry{ID: id, Time: at, ActorEmail: e.ActorEmail, ActorRole: string(e.ActorRole), TenantID: e.TenantID,
		Action: e.Action, Target: e.Target, Detail: e.Detail, TraceID: e.TraceID, Remote: e.Remote, JobID: e.JobID}); err != nil {
		r.Log.Error("audit log file write failed", "event_id", "AUDIT_FILE_ERR", "action", e.Action, "target", e.Target,
			"trace_id", e.TraceID, "error", err)
	}
}
