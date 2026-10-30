package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/oyaguma3/aka-ossbss/internal/audit"
	"github.com/oyaguma3/aka-ossbss/internal/authz"
	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/reconcile"
)

// 突き合わせの画面（設計概要 §9・§15。段階 3-3 の Y6・Y12）。見るのは運用担当とシステム管理者、手動の実行と対応はシステム管理者。
// 対応できるのは最新の実行のずれだけ（Y7）。

const (
	// reconcileRunsPerPage は実行の履歴の件数、findingsPerPage はずれの一覧の 1 ページの件数。
	reconcileRunsPerPage = 30
	findingsPerPage      = 50
	// maxFindingActions は 1 回に対応できるずれの数（Y6）。
	maxFindingActions = 100
	// maxActionErrors は対応の結果に出す誤りの数。
	maxActionErrors = 20
)

var findingKindLabels = map[string]string{
	db.FindingLedgerOnly:     "台帳にあって下流にない",
	db.FindingStateDiff:      "状態が違う",
	db.FindingSourceDiff:     "取得先が違う",
	db.FindingSideAbsent:     "その側だけ台帳にない",
	db.FindingDownstreamOnly: "IMSI が台帳にない",
	db.FindingDeletedPresent: "台帳で削除済みなのに下流にある",
	db.FindingRuleTenant:     "ルールのテナントが所属と違う",
}

func findingKindLabel(k string) string {
	if l, ok := findingKindLabels[k]; ok {
		return l
	}
	return k
}

var findingStatusLabels = map[string]string{
	db.FindingFixed:    "直した",
	db.FindingHeld:     "保留",
	db.FindingPending:  "対応待ち",
	db.FindingInfo:     "報告だけ",
	db.FindingImported: "取り込んだ",
	db.FindingDeleted:  "下流から削除した",
	db.FindingGone:     "解消済み",
}

func findingStatusLabel(s string) string {
	if l, ok := findingStatusLabels[s]; ok {
		return l
	}
	return s
}

var runStateLabels = map[string]string{
	db.RunRunning:     "実行中",
	db.RunCompleted:   "完了",
	db.RunHeld:        "保留",
	db.RunFailed:      "失敗",
	db.RunInterrupted: "中断",
}

func runStateLabel(s string) string {
	if l, ok := runStateLabels[s]; ok {
		return l
	}
	return s
}

func runTriggerLabel(s string) string {
	switch s {
	case db.TriggerSchedule:
		return "定刻"
	case db.TriggerManual:
		return "手動"
	case db.TriggerApproved:
		return "保留の承認"
	}
	return s
}

func scheduleNameLabel(s string) string {
	switch s {
	case "reconcile":
		return "突き合わせ"
	case "check-needed":
		return "結果を確かめる印の読み直し"
	case "retention":
		return "13 か月より古い記録の削除"
	case "job-sweep":
		return "確定しないジョブの取り消し"
	case "audit-files":
		return "監査ログのファイルの整理"
	}
	return s
}

func scheduleStatusLabel(s string) string {
	switch s {
	case db.ScheduleRunning:
		return "実行中"
	case db.ScheduleOK:
		return "成功"
	case db.ScheduleFailed:
		return "失敗"
	case db.ScheduleInterrupted:
		return "中断"
	}
	return s
}

// actionable は、システム管理者が対応するずれか。
func actionable(f db.Finding) bool {
	return f.Status == db.FindingPending && (f.Kind == db.FindingDownstreamOnly || f.Kind == db.FindingDeletedPresent)
}

// ---- ダッシュボード ----

// reconcileSummary はダッシュボードの突き合わせの欄の値。
type reconcileSummary struct {
	// Last は最後の実行、Finished はずれの一覧がそろった最新の実行、Held はそれが保留のときの実行。
	Last, Finished, Held *db.ReconcileRun
	// Pending は対応を待つずれの数。
	Pending int
	// FailedSchedules は最新の実行が失敗の定期の処理。
	FailedSchedules []db.ScheduledRun
}

// loadReconcileSummary はダッシュボードの欄の値を読む（読めなければ nil）。
func (h *Handler) loadReconcileSummary(ctx context.Context) *reconcileSummary {
	s := &reconcileSummary{}
	runs, err := h.db.ReconcileRuns(ctx, 1)
	if err != nil {
		h.log.Warn("home: reconcile runs", "error", err)
		return nil
	}
	if len(runs) > 0 {
		s.Last = &runs[0]
	}
	finished, ok, err := h.db.LatestFinishedReconcileRun(ctx)
	if err != nil {
		h.log.Warn("home: latest reconcile run", "error", err)
		return nil
	}
	if ok {
		s.Finished = &finished
		if finished.State == db.RunHeld {
			s.Held = &finished
		}
		counts, err := h.db.CountFindings(ctx, finished.ID)
		if err != nil {
			h.log.Warn("home: count findings", "error", err)
			return nil
		}
		for _, c := range counts {
			if c.Status == db.FindingPending {
				s.Pending += c.Count
			}
		}
	}
	schedules, err := h.db.LatestScheduledRuns(ctx)
	if err != nil {
		h.log.Warn("home: scheduled runs", "error", err)
		return nil
	}
	for _, r := range schedules {
		if r.Status == db.ScheduleFailed {
			s.FailedSchedules = append(s.FailedSchedules, r)
		}
	}
	return s
}

// ---- 履歴 ----

type reconcileListData struct {
	Runs      []db.ReconcileRun
	Schedules []db.ScheduledRun
	// CanManage は手動の実行のボタンを出すか。Active は定期の読み込みをするか。
	CanManage, Active bool
	Error             string
}

func (h *Handler) reconcileList(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.ReconcileView) {
		return
	}
	h.renderReconcileList(w, r, http.StatusOK, "")
}

func (h *Handler) renderReconcileList(w http.ResponseWriter, r *http.Request, status int, msg string) {
	ctx, cancel := dbContext(r)
	defer cancel()
	runs, err := h.db.ReconcileRuns(ctx, reconcileRunsPerPage)
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	schedules, err := h.db.LatestScheduledRuns(ctx)
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	d := reconcileListData{Runs: runs, Schedules: schedules, CanManage: can(r, authz.ReconcileManage), Error: msg}
	d.Active = len(runs) > 0 && runs[0].State == db.RunRunning
	if msg != "" {
		h.render(w, r, status, "reconcile_list", "突き合わせ", d)
		return
	}
	h.renderPart(w, r, status, "reconcile_list", "reconcile-table", "突き合わせ", d)
}

// reconcileRun は手動で実行する（システム管理者）。
func (h *Handler) reconcileRun(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.ReconcileManage) {
		return
	}
	run, err := h.reconciler.Start(context.WithoutCancel(r.Context()), db.TriggerManual, userFrom(r.Context()).Email, "")
	switch {
	case errors.Is(err, reconcile.ErrRunning):
		h.renderReconcileList(w, r, http.StatusConflict, "突き合わせが動いています。終わってからもう一度実行してください。")
		return
	case err != nil:
		h.dbUnavailable(w, r, err)
		return
	}
	seeOther(w, r, fmt.Sprintf("/reconcile/%d", run.ID))
}

// ---- 実行の詳細 ----

type reconcileDetailData struct {
	Run db.ReconcileRun
	// Latest は、対応できる最新の実行か。CanManage は対応のボタンを出すか。
	Latest, CanManage bool
	Counts            []db.FindingCount
	Pending           int
	// Findings はずれの一覧。Kind と Status は絞り込み、NextURL は次のページ。
	Findings     []db.Finding
	Kind, Status string
	NextURL      string
	Tenants      tenantInfo
	// HasActions は、このページに対応できるずれがあるか。
	HasActions     bool
	Reason         string
	Message, Error string
	// ActionErrors は対応の失敗の一覧。
	ActionErrors []string
}

// Selectable は、そのずれを画面で選べるか。
func (d reconcileDetailData) Selectable(f db.Finding) bool {
	return d.Latest && d.CanManage && actionable(f)
}

// FilterURL は絞り込みの URL を返す。
func (d reconcileDetailData) FilterURL(kind, status string) string {
	v := url.Values{}
	if kind != "" {
		v.Set("kind", kind)
	}
	if status != "" {
		v.Set("status", status)
	}
	u := fmt.Sprintf("/reconcile/%d", d.Run.ID)
	if len(v) > 0 {
		u += "?" + v.Encode()
	}
	return u
}

// Stored は行として残したずれの数。
func (d reconcileDetailData) Stored() int {
	n := 0
	for _, c := range d.Counts {
		n += c.Count
	}
	return n
}

// loadRun は URL の実行を読む。なければ 404 を返して false。
func (h *Handler) loadRun(w http.ResponseWriter, r *http.Request) (db.ReconcileRun, bool) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		h.notFound(w, r)
		return db.ReconcileRun{}, false
	}
	ctx, cancel := dbContext(r)
	defer cancel()
	run, err := h.db.ReconcileRun(ctx, id)
	switch {
	case errors.Is(err, db.ErrNotFound):
		h.renderErrorLink(w, r, http.StatusNotFound, "突き合わせの実行が見つかりません。", "/reconcile", "突き合わせの一覧へ")
		return db.ReconcileRun{}, false
	case err != nil:
		h.dbUnavailable(w, r, err)
		return db.ReconcileRun{}, false
	}
	return run, true
}

// isLatestRun は、対応できる最新の実行か。
func (h *Handler) isLatestRun(ctx context.Context, run db.ReconcileRun) (bool, error) {
	latest, ok, err := h.db.LatestFinishedReconcileRun(ctx)
	return ok && latest.ID == run.ID, err
}

func (h *Handler) reconcileDetail(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.ReconcileView) {
		return
	}
	run, ok := h.loadRun(w, r)
	if !ok {
		return
	}
	// 定期の読み込みで状態が変わったら、画面全体を読み直す。
	if isHTMX(r) && r.URL.Query().Get("state") != run.State {
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
		return
	}
	d, ok := h.newReconcileDetail(w, r, run)
	if !ok {
		return
	}
	h.renderPart(w, r, http.StatusOK, "reconcile", "reconcile-progress", fmt.Sprintf("突き合わせ %d", run.ID), d)
}

// newReconcileDetail は詳細の画面の値を作る。
func (h *Handler) newReconcileDetail(w http.ResponseWriter, r *http.Request, run db.ReconcileRun) (reconcileDetailData, bool) {
	q := r.URL.Query()
	d := reconcileDetailData{Run: run, CanManage: can(r, authz.ReconcileManage), Kind: q.Get("kind"), Status: q.Get("status")}
	if _, ok := findingKindLabels[d.Kind]; !ok {
		d.Kind = ""
	}
	if _, ok := findingStatusLabels[d.Status]; !ok {
		d.Status = ""
	}
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	t, err := h.tenants(r)
	if err != nil {
		h.dbUnavailable(w, r, err)
		return d, false
	}
	d.Tenants = t
	ctx, cancel := dbContext(r)
	defer cancel()
	if d.Latest, err = h.isLatestRun(ctx, run); err != nil {
		h.dbUnavailable(w, r, err)
		return d, false
	}
	if d.Counts, err = h.db.CountFindings(ctx, run.ID); err != nil {
		h.dbUnavailable(w, r, err)
		return d, false
	}
	for _, c := range d.Counts {
		if c.Status == db.FindingPending {
			d.Pending += c.Count
		}
	}
	rows, err := h.db.ReconcileFindings(ctx, db.FindingQuery{RunID: run.ID, Kind: d.Kind, Status: d.Status, AfterID: after, Limit: findingsPerPage + 1})
	if err != nil {
		h.dbUnavailable(w, r, err)
		return d, false
	}
	if len(rows) > findingsPerPage {
		rows = rows[:findingsPerPage]
		next, _ := url.Parse(d.FilterURL(d.Kind, d.Status))
		v := next.Query()
		v.Set("after", strconv.FormatInt(rows[len(rows)-1].ID, 10))
		next.RawQuery = v.Encode()
		d.NextURL = next.String()
	}
	d.Findings = rows
	for _, f := range rows {
		d.HasActions = d.HasActions || d.Selectable(f)
	}
	return d, true
}

// reconcileApprove は保留の実行を承認して、読み直して直す実行を始める（理由が必須。Y4）。
func (h *Handler) reconcileApprove(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.ReconcileManage) {
		return
	}
	run, ok := h.loadRun(w, r)
	if !ok {
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	reason := strings.TrimSpace(r.PostForm.Get("reason"))
	fail := func(status int, msg string) {
		d, ok := h.newReconcileDetail(w, r, run)
		if !ok {
			return
		}
		d.Error, d.Reason = msg, reason
		h.render(w, r, status, "reconcile", fmt.Sprintf("突き合わせ %d", run.ID), d)
	}
	ctx, cancel := dbContext(r)
	latest, err := h.isLatestRun(ctx, run)
	cancel()
	switch {
	case err != nil:
		h.dbUnavailable(w, r, err)
		return
	case run.State != db.RunHeld || !latest:
		fail(http.StatusConflict, "この実行は保留の最新の実行ではありません。")
		return
	case reason == "" || len([]rune(reason)) > maxReasonLen:
		fail(http.StatusBadRequest, "承認の理由を 500 文字までで入力してください。")
		return
	}
	next, err := h.reconciler.Start(context.WithoutCancel(r.Context()), db.TriggerApproved, userFrom(r.Context()).Email, reason)
	switch {
	case errors.Is(err, reconcile.ErrRunning):
		fail(http.StatusConflict, "突き合わせが動いています。終わってからもう一度押してください。")
		return
	case err != nil:
		h.dbUnavailable(w, r, err)
		return
	}
	h.record(r, audit.ActionReconcileApprove, fmt.Sprintf("reconcile-%d", run.ID), authz.NoTenant,
		map[string]any{"fixable": run.FixableCount, "reason": reason, "run": next.ID})
	seeOther(w, r, fmt.Sprintf("/reconcile/%d", next.ID))
}

// reconcileFindings は選んだずれに対応する（取り込むか、下流から削除する。Y6）。
func (h *Handler) reconcileFindings(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.ReconcileManage) {
		return
	}
	run, ok := h.loadRun(w, r)
	if !ok {
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	pf := r.PostForm
	op, reason := pf.Get("op"), strings.TrimSpace(pf.Get("reason"))
	render := func(status int, message, errText string, actionErrs []string) {
		d, ok := h.newReconcileDetail(w, r, run)
		if !ok {
			return
		}
		d.Message, d.Error, d.ActionErrors, d.Reason = message, errText, actionErrs, reason
		h.render(w, r, status, "reconcile", fmt.Sprintf("突き合わせ %d", run.ID), d)
	}
	ids := pf["finding"]
	switch {
	case op != "import" && op != "delete":
		render(http.StatusBadRequest, "", "操作の指定が正しくありません。", nil)
		return
	case len(ids) == 0:
		render(http.StatusBadRequest, "", "対応するずれを選んでください。", nil)
		return
	case len(ids) > maxFindingActions:
		render(http.StatusBadRequest, "", fmt.Sprintf("1 回に対応できるのは %d 件までです。", maxFindingActions), nil)
		return
	case len([]rune(reason)) > maxReasonLen:
		render(http.StatusBadRequest, "", "理由は 500 文字までです。", nil)
		return
	}
	t, err := h.tenants(r)
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	var chosen int64
	if v := pf.Get("tenant"); v != "" && op == "import" {
		id, ok, live := readTenantChoice(v, t)
		if !ok || !live {
			render(http.StatusBadRequest, "", "選んだテナントはありません。", nil)
			return
		}
		chosen = id
	}
	ctx, cancel := dbContext(r)
	latest, err := h.isLatestRun(ctx, run)
	cancel()
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	if !latest {
		render(http.StatusConflict, "", "対応できるのは最新の実行のずれだけです。", nil)
		return
	}
	// 先に全部を読んで確かめる（誤りが 1 件でもあれば何もしない）。
	findings := make([]db.Finding, 0, len(ids))
	for _, v := range ids {
		id, err := parseID(v)
		if err != nil {
			render(http.StatusBadRequest, "", "ずれの指定が正しくありません。", nil)
			return
		}
		ctx, cancel := dbContext(r)
		f, err := h.db.ReconcileFinding(ctx, id)
		cancel()
		switch {
		case errors.Is(err, db.ErrNotFound) || (err == nil && f.RunID != run.ID):
			render(http.StatusBadRequest, "", "ずれの指定が正しくありません。", nil)
			return
		case err != nil:
			h.dbUnavailable(w, r, err)
			return
		}
		findings = append(findings, f)
	}
	for _, f := range findings {
		if op == "import" && f.Kind == db.FindingDeletedPresent && reason == "" {
			render(http.StatusBadRequest, "", "削除済みの IMSI を取り込むには、理由を入力してください。", nil)
			return
		}
		if op == "import" && chosen == 0 && f.TenantID == 0 {
			render(http.StatusBadRequest, "", "テナントの候補のない IMSI（"+f.IMSI+"）があります。取り込むテナントを選んでください。", nil)
			return
		}
	}

	u := userFrom(r.Context())
	done, gone := 0, 0
	var actionErrs []string
	failed := 0
	seen := map[string]bool{}
	for _, f := range findings {
		// 取り込みは IMSI ごと（両側をまとめて取り込む）。
		if op == "import" && seen[f.IMSI] {
			continue
		}
		seen[f.IMSI] = true
		if !actionable(f) {
			gone++
			continue
		}
		actx := context.WithoutCancel(r.Context())
		tenant := chosen
		if tenant == 0 {
			tenant = f.TenantID
		}
		var status string
		var err error
		if op == "import" {
			status, err = h.reconciler.Import(actx, f, tenant, u.Email)
		} else {
			status, err = h.reconciler.Delete(actx, f, u.Email)
		}
		switch {
		case err != nil:
			failed++
			h.log.Warn("reconcile action", "op", op, "imsi", f.IMSI, "error", err)
			if len(actionErrs) < maxActionErrors {
				actionErrs = append(actionErrs, f.IMSI+": "+describeReconcileError(err, f.Side))
			}
		case status == db.FindingGone:
			gone++
		case op == "import":
			done++
			h.record(r, audit.ActionReconcileImport, f.IMSI, tenant, map[string]any{"kind": f.Kind, "reason": reason})
		default:
			done++
			h.record(r, audit.ActionReconcileDelete, f.IMSI, f.TenantID, map[string]any{"kind": f.Kind, "side": string(f.Side), "reason": reason})
		}
	}
	word := "取り込み"
	if op == "delete" {
		word = "下流からの削除"
	}
	msg := fmt.Sprintf("%s %d 件、解消済み %d 件。", word, done, gone)
	errText := ""
	if failed > 0 {
		errText = fmt.Sprintf("%d 件は失敗しました（対応待ちのままです）。", failed)
	}
	render(http.StatusOK, msg, errText, actionErrs)
}

// describeReconcileError は対応の失敗を文にする。
func describeReconcileError(err error, side db.Side) string {
	switch {
	case errors.Is(err, reconcile.ErrUnreadable):
		return "下流を読めませんでした。"
	case errors.Is(err, db.ErrTenantNotFound):
		return "テナントがありません。"
	}
	return DescribeProvisionerError(err, side)
}
