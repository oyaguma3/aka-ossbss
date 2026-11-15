package audit

// RadSec の受け口の監査ログの操作の名前（設計概要 §13。段階 4-3 の OB19）。
const (
	ActionEdgeCreate        = "radsec.edge.create"
	ActionEdgeUpdate        = "radsec.edge.update"
	ActionEdgeDelete        = "radsec.edge.delete"
	ActionEdgeRestart       = "radsec.edge.restart"
	ActionRadsecEnable      = "radsec.enable"
	ActionRadsecSources     = "radsec.sources"
	ActionRadsecMatch       = "radsec.match"
	ActionRadsecApply       = "radsec.apply"
	ActionRadsecCADisable   = "radsec.ca.disable"
	ActionPlacementAdd      = "radsec.placement.add"
	ActionPlacementRemove   = "radsec.placement.remove"
	ActionPlacementMove     = "radsec.placement.move"
	ActionPlacementMoveDone = "radsec.placement.move_finish"
	ActionPlacementMoveStop = "radsec.placement.move_cancel"
	ActionAPCreate          = "radsec.ap.create"
	ActionAPRenew           = "radsec.ap.renew"
	ActionAPRevoke          = "radsec.ap.revoke"
	ActionAPDelete          = "radsec.ap.delete"
	ActionBroughtCreate     = "radsec.brought.create"
	ActionBroughtSource     = "radsec.brought.source"
	ActionBroughtUploadCRL  = "radsec.brought.crl_upload"
	ActionBroughtRefreshCRL = "radsec.brought.crl_refresh"
	ActionBroughtRemove     = "radsec.brought.remove"
	ActionRadsecRetry       = "radsec.apply.retry"
	ActionEdgeOrphanDelete  = "radsec.edge.orphan_delete"
	// 反映し直し（段階 6-0 の U6-8）。
	ActionRolloutStart  = "radsec.rollout.start"
	ActionRolloutResume = "radsec.rollout.resume"
	ActionRolloutCancel = "radsec.rollout.cancel"

	// 中央の整合のループが書くもの（操作者は system。段階 4-4 の OB19）。
	ActionLoopDeclare     = "radsec.declare"
	ActionLoopApplyStart  = "radsec.apply.start"
	ActionLoopApplyResult = "radsec.apply.result"
	ActionLoopCertPlace   = "radsec.cert.place"
	ActionLoopCertRevoke  = "radsec.cert.revoke"
	ActionLoopCRLRefresh  = "radsec.crl.refresh"
	ActionLoopRemoved     = "radsec.placement.removed"
	ActionLoopBroughtOff  = "radsec.brought.disabled"
	// 反映し直しの 1 件の始まりと終わり、反映し直しの終わりと止まり（段階 6-0 の U6-8）。
	ActionLoopRolloutItem   = "radsec.rollout.item"
	ActionLoopRolloutFinish = "radsec.rollout.finish"
	ActionLoopRolloutStop   = "radsec.rollout.stop"
)
