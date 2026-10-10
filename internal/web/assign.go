package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/oyaguma3/eapaka-ossbss-sample/internal/authz"
	"github.com/oyaguma3/eapaka-ossbss-sample/internal/db"
)

// 所属の付け替えと、下流にない所属の記録の点検（画面仕様 §7。システム管理者だけ）。
// 所属の記録は本BFF の DB だけにあり、provisioner には送らない。付け替える前と消す前に、provisioner の状態を確かめる。

const (
	// maxBulkSubscribers と maxBulkClients は、一覧からまとめて付け替える 1 回の件数の上限（§17.5 の 1）。
	maxBulkSubscribers = 50
	maxBulkClients     = 200
	// orphanScanLimit は、下流にない所属の記録の点検で 1 回の表示で調べる加入者の記録の数（§17.5 の 2）。
	orphanScanLimit = 200
)

// requireOp は、利用者の役割に op があるかを確かめる（範囲を絞らない操作。なければ 403 を返して false）。
func (h *Handler) requireOp(w http.ResponseWriter, r *http.Request, op authz.Op) bool {
	if !can(r, op) {
		h.forbidden(w, r)
		return false
	}
	return true
}

// assignDetail は所属の付け替えの監査ログの内容。
func assignDetail(kind string, t tenantInfo, from, to int64) map[string]any {
	return map[string]any{"kind": kind, "from": t.Name(from), "to": t.Name(to), "fromTenant": from, "toTenant": to}
}

// まとめての付け替えの誤りの種類（戻り先のクエリ bulk_error に入れ、文は bulkResult で作る。任意の文を出させないため）。
const (
	bulkErrNoneSelected  = "none"
	bulkErrNoTenant      = "tenant"
	bulkErrUnknownTenant = "unknown_tenant"
	bulkErrStopped       = "stopped"
)

// bulkErrorMessages は誤りの種類ごとの文。
var bulkErrorMessages = map[string]string{
	bulkErrNoneSelected:  "所属を変えるものを選んでください。",
	bulkErrNoTenant:      "付け替え先のテナントを選んでください。",
	bulkErrUnknownTenant: "選んだテナントはありません。画面を開き直してください。",
}

// readTargetTenant は付け替え先のテナント（tenant）を読む。正しくなければ誤りの種類を返す。
func readTargetTenant(r *http.Request, t tenantInfo) (int64, string) {
	id, chosen, exists := readTenantChoice(r.PostForm.Get("tenant"), t)
	switch {
	case !chosen:
		return 0, bulkErrNoTenant
	case !exists:
		return 0, bulkErrUnknownTenant
	}
	return id, ""
}

// writeContext は、済んだ操作の記録を書くための上限時間つきのコンテキスト（要求が取り消されても書く）。
func writeContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), dbTimeout)
}

// ---- 詳細からの付け替え ----

// subscriberTenantChange は加入者の所属を変える（加入者の詳細から）。
func (h *Handler) subscriberTenantChange(w http.ResponseWriter, r *http.Request) {
	imsi, ok := h.pathIMSI(w, r)
	if !ok || !h.requireOp(w, r, authz.AssignmentUpdate) {
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	t, err := h.tenants(r)
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	ctx, cancel := dbContext(r)
	from, err := h.db.SubscriberTenant(ctx, imsi)
	cancel()
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	to, code := readTargetTenant(r, t)
	if code != "" {
		h.renderSubscriber(w, r, imsi, from, http.StatusBadRequest, nil, func(d *subscriberData) { d.Error = bulkErrorMessages[code] })
		return
	}
	// 下流にないものに記録を作らない。
	if _, err := h.pv.GetSubscriber(r.Context(), imsi); err != nil {
		h.renderFailure(w, r, h.apiError(r, err, missingMessage(imsi, from)), "/subscribers", "加入者の一覧へ")
		return
	}
	if from != to {
		ctx, cancel := writeContext(r)
		err := h.db.AssignSubscriber(ctx, imsi, to, userFrom(r.Context()).Email)
		cancel()
		if err != nil {
			h.dbUnavailable(w, r, err)
			return
		}
		h.recordMove(r, auditAssignmentUpdate, imsi, from, to, assignDetail("subscriber", t, from, to))
	}
	h.renderSubscriber(w, r, imsi, to, http.StatusOK, nil, func(d *subscriberData) {
		d.Message = "所属を「" + t.Name(to) + "」にしました。"
		if from == to {
			d.Message = "所属は変わりません（「" + t.Name(to) + "」のままです）。"
		}
	})
}

// clientTenantChange は RADIUSクライアントの所属を変える（RADIUSクライアントの詳細から）。
func (h *Handler) clientTenantChange(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathClientID(w, r)
	if !ok || !h.requireOp(w, r, authz.AssignmentUpdate) {
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	t, err := h.tenants(r)
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	ctx, cancel := dbContext(r)
	from, err := h.db.ClientTenant(ctx, id)
	cancel()
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	to, code := readTargetTenant(r, t)
	if code != "" {
		h.renderClient(w, r, id, from, http.StatusBadRequest, nil, func(d *clientData) { d.Error = bulkErrorMessages[code] })
		return
	}
	if _, err := h.pv.GetRADIUSClient(r.Context(), id); err != nil {
		h.renderFailure(w, r, h.apiError(r, err, clientMissingMessage(id, from)), "/clients", "RADIUSクライアントの一覧へ")
		return
	}
	if from != to {
		ctx, cancel := writeContext(r)
		err := h.db.AssignClient(ctx, id, to, userFrom(r.Context()).Email)
		cancel()
		if err != nil {
			h.dbUnavailable(w, r, err)
			return
		}
		h.recordMove(r, auditAssignmentUpdate, clientTarget(id), from, to, assignDetail("client", t, from, to))
	}
	h.renderClient(w, r, id, to, http.StatusOK, nil, func(d *clientData) {
		d.Message = "所属を「" + t.Name(to) + "」にしました。"
		if from == to {
			d.Message = "所属は変わりません（「" + t.Name(to) + "」のままです）。"
		}
	})
}

// ---- 一覧からまとめて付け替える ----

// safeBack は、一覧へ戻る先（back）として安全なパスを返す。prefix で始まる同じサイトのパスだけを使い、それ以外は prefix にする。
func safeBack(back, prefix string) string {
	if back == prefix || strings.HasPrefix(back, prefix+"?") {
		return back
	}
	return prefix
}

// withQuery は、戻り先のパスにクエリの値を足す。
func withQuery(path string, kv ...string) string {
	u, err := url.Parse(path)
	if err != nil {
		return path
	}
	q := u.Query()
	for i := 0; i+1 < len(kv); i += 2 {
		q.Set(kv[i], kv[i+1])
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// bulkDone は、まとめて付け替えた結果を戻り先のクエリにする（文は bulkResult で作る）。
func bulkDone(back string, changed, skipped int, kv ...string) string {
	return withQuery(back, append([]string{"bulk_changed", strconv.Itoa(changed), "bulk_skipped", strconv.Itoa(skipped)}, kv...)...)
}

// subscribersAssign は、一覧で選んだ加入者の所属をまとめて変える（50 件まで）。
func (h *Handler) subscribersAssign(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.AssignmentUpdate) {
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	back := safeBack(r.PostForm.Get("back"), "/subscribers")
	t, err := h.tenants(r)
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	imsis := slices.Compact(slices.Sorted(slices.Values(r.PostForm["imsi"])))
	switch {
	case len(imsis) == 0:
		seeOther(w, r, withQuery(back, "bulk_error", bulkErrNoneSelected))
		return
	case len(imsis) > maxBulkSubscribers || slices.ContainsFunc(imsis, func(s string) bool { return !imsiPattern.MatchString(s) }):
		h.renderErrorLink(w, r, http.StatusBadRequest, "入力を読み取れませんでした。画面を開き直してください。", back, "一覧へ")
		return
	}
	to, code := readTargetTenant(r, t)
	if code != "" {
		seeOther(w, r, withQuery(back, "bulk_error", code))
		return
	}
	// 下流にあるものだけを付け替える（ないものに記録を作らない）。
	exists := make([]bool, len(imsis))
	errs := make([]error, len(imsis))
	forEachConcurrently(len(imsis), fetchConcurrency, func(i int) {
		_, err := h.pv.GetSubscriber(r.Context(), imsis[i])
		switch {
		case err == nil:
			exists[i] = true
		case !isNotFound(err):
			errs[i] = err
		}
	})
	for _, err := range errs {
		if err != nil {
			h.log.Warn("get subscriber", "error", err)
			h.renderFailure(w, r, h.apiError(r, err, ""), back, "一覧へ")
			return
		}
	}
	ctx, cancel := dbContext(r)
	current, err := h.db.SubscriberTenants(ctx, imsis)
	cancel()
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	changed, skipped := 0, 0
	by := userFrom(r.Context()).Email
	for i, imsi := range imsis {
		from := current[imsi]
		switch {
		case !exists[i]:
			skipped++
			continue
		case from == to:
			continue
		}
		ctx, cancel := writeContext(r)
		err := h.db.AssignSubscriber(ctx, imsi, to, by)
		cancel()
		if err != nil {
			h.log.Error("assign subscriber", "imsi", imsi, "error", err)
			seeOther(w, r, bulkDone(back, changed, skipped, "bulk_error", bulkErrStopped, "bulk_stop", imsi))
			return
		}
		h.recordMove(r, auditAssignmentUpdate, imsi, from, to, assignDetail("subscriber", t, from, to))
		changed++
	}
	seeOther(w, r, bulkDone(back, changed, skipped))
}

// clientsAssign は、一覧で選んだ RADIUSクライアントの所属をまとめて変える（200 件まで）。
func (h *Handler) clientsAssign(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.AssignmentUpdate) {
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	back := safeBack(r.PostForm.Get("back"), "/clients")
	t, err := h.tenants(r)
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	var ids []int64
	for _, v := range r.PostForm["id"] {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id < 1 {
			h.renderErrorLink(w, r, http.StatusBadRequest, "入力を読み取れませんでした。画面を開き直してください。", back, "一覧へ")
			return
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	switch {
	case len(ids) == 0:
		seeOther(w, r, withQuery(back, "bulk_error", bulkErrNoneSelected))
		return
	case len(ids) > maxBulkClients:
		h.renderErrorLink(w, r, http.StatusBadRequest, "入力を読み取れませんでした。画面を開き直してください。", back, "一覧へ")
		return
	}
	to, code := readTargetTenant(r, t)
	if code != "" {
		seeOther(w, r, withQuery(back, "bulk_error", code))
		return
	}
	clients, err := h.pv.ListRADIUSClients(r.Context())
	if err != nil {
		h.log.Warn("list radius clients", "error", err)
		h.renderFailure(w, r, h.apiError(r, err, ""), back, "一覧へ")
		return
	}
	existing := map[int64]bool{}
	for _, c := range clients {
		existing[c.ID] = true
	}
	ctx, cancel := dbContext(r)
	current, err := h.db.ClientTenants(ctx)
	cancel()
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	changed, skipped := 0, 0
	by := userFrom(r.Context()).Email
	for _, id := range ids {
		from := current[id]
		switch {
		case !existing[id]:
			skipped++
			continue
		case from == to:
			continue
		}
		ctx, cancel := writeContext(r)
		err := h.db.AssignClient(ctx, id, to, by)
		cancel()
		if err != nil {
			h.log.Error("assign client", "id", id, "error", err)
			seeOther(w, r, bulkDone(back, changed, skipped, "bulk_error", bulkErrStopped, "bulk_stop", clientTarget(id)))
			return
		}
		h.recordMove(r, auditAssignmentUpdate, clientTarget(id), from, to, assignDetail("client", t, from, to))
		changed++
	}
	seeOther(w, r, bulkDone(back, changed, skipped))
}

// bulkStopPattern は、途中で止めた位置（IMSI または #ID）の形式。
var bulkStopPattern = regexp.MustCompile(`^([0-9]{15}|#[0-9]{1,19})$`)

// bulkResult は、まとめての付け替えの結果の文を、戻り先のクエリ（数と誤りの種類）から作る（一覧の上部に出す）。
func bulkResult(r *http.Request) (msg, errMsg string) {
	q := r.URL.Query()
	changed, err1 := strconv.Atoi(q.Get("bulk_changed"))
	skipped, err2 := strconv.Atoi(q.Get("bulk_skipped"))
	if err1 == nil && err2 == nil && changed >= 0 && skipped >= 0 {
		msg = fmt.Sprintf("%d 件の所属を変更しました。", changed)
		if skipped > 0 {
			msg += fmt.Sprintf("%d 件は provisioner にないため飛ばしました。", skipped)
		}
	}
	switch e := q.Get("bulk_error"); {
	case e == bulkErrStopped && bulkStopPattern.MatchString(q.Get("bulk_stop")):
		errMsg = "データベースに接続できず、途中で止めました（" + q.Get("bulk_stop") + " から後ろは変えていません）。"
	default:
		errMsg = bulkErrorMessages[e]
	}
	return msg, errMsg
}

// ---- 下流にない所属の記録 ----

// orphanRow は下流にない所属の記録の 1 行。
type orphanRow struct {
	db.Assignment
	Tenant string
}

type orphansData struct {
	Subscribers []orphanRow
	Clients     []orphanRow
	// Scanned は調べた加入者の記録の数、Last はその最後の IMSI、Next は続きの位置（なければ空）。
	Scanned int
	After   string
	Last    string
	Next    string
	Message string
	Error   string
}

// orphans は下流にない所属の記録を探す（画面仕様 §7.2）。
func (h *Handler) orphans(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.OrphanDelete) {
		return
	}
	q := r.URL.Query()
	d := orphansData{After: q.Get("after")}
	deleted, err1 := strconv.Atoi(q.Get("deleted"))
	kept, err2 := strconv.Atoi(q.Get("kept"))
	if err1 == nil && err2 == nil && deleted >= 0 && kept >= 0 {
		d.Message = fmt.Sprintf("%d 件の所属の記録を消しました。", deleted)
		if kept > 0 {
			d.Message += fmt.Sprintf("%d 件は provisioner にあったため消しませんでした。", kept)
		}
	}
	if d.After != "" && !imsiPattern.MatchString(d.After) {
		h.renderError(w, r, http.StatusBadRequest, "続きの位置の指定が正しくありません。")
		return
	}
	t, err := h.tenants(r)
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	status := h.findOrphans(r, &d, t)
	h.render(w, r, status, "orphans", "下流にない所属の記録", d)
}

// findOrphans は d.After から加入者の記録を 200 件まで調べ、RADIUSクライアントは全件を突き合わせる。
func (h *Handler) findOrphans(r *http.Request, d *orphansData, t tenantInfo) int {
	ctx, cancel := dbContext(r)
	subs, err := h.db.SubscriberAssignments(ctx, d.After, orphanScanLimit+1)
	var clients []db.Assignment
	if err == nil {
		clients, err = h.db.ClientAssignments(ctx)
	}
	cancel()
	if err != nil {
		h.log.Error("assignments", "error", err)
		d.Error = "データベースに接続できません。しばらくしてから開き直してください。"
		return http.StatusServiceUnavailable
	}
	if len(subs) > orphanScanLimit {
		subs = subs[:orphanScanLimit]
		d.Next = subs[len(subs)-1].IMSI
	}
	missing := make([]bool, len(subs))
	errs := make([]error, len(subs))
	forEachConcurrently(len(subs), fetchConcurrency, func(i int) {
		_, err := h.pv.GetSubscriber(r.Context(), subs[i].IMSI)
		switch {
		case isNotFound(err):
			missing[i] = true
		case err != nil:
			errs[i] = err
		}
	})
	for i, a := range subs {
		if errs[i] != nil {
			// 確かめられなかったものから後ろは、次の表示で調べ直す。
			h.log.Warn("get subscriber", "error", errs[i])
			f := h.apiError(r, errs[i], "")
			d.Error, d.Next = f.Message, ""
			return f.Status
		}
		d.Scanned, d.Last = i+1, a.IMSI
		if missing[i] {
			d.Subscribers = append(d.Subscribers, orphanRow{Assignment: a, Tenant: t.Name(a.TenantID)})
		}
	}
	list, err := h.pv.ListRADIUSClients(r.Context())
	if err != nil {
		h.log.Warn("list radius clients", "error", err)
		f := h.apiError(r, err, "")
		d.Error = f.Message
		return f.Status
	}
	existing := map[int64]bool{}
	for _, c := range list {
		existing[c.ID] = true
	}
	for _, a := range clients {
		if !existing[a.ClientID] {
			d.Clients = append(d.Clients, orphanRow{Assignment: a, Tenant: t.Name(a.TenantID)})
		}
	}
	return http.StatusOK
}

// orphansDelete は選んだ下流にない所属の記録を消す。消す直前に provisioner にないことをもう一度確かめる。
func (h *Handler) orphansDelete(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.OrphanDelete) {
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	imsis := slices.Compact(slices.Sorted(slices.Values(r.PostForm["imsi"])))
	var ids []int64
	for _, v := range r.PostForm["client"] {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id < 1 {
			h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。画面を開き直してください。")
			return
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(imsis) > orphanScanLimit || len(ids) > maxBulkClients ||
		slices.ContainsFunc(imsis, func(s string) bool { return !imsiPattern.MatchString(s) }) {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。画面を開き直してください。")
		return
	}
	if len(imsis) == 0 && len(ids) == 0 {
		h.renderErrorLink(w, r, http.StatusBadRequest, "消す記録を選んでください。", "/assignments/orphans", "点検の画面へ")
		return
	}
	// provisioner にないことを確かめ直す（点検の後に、他の操作で作り直されていることがある）。
	missing := make([]bool, len(imsis))
	errs := make([]error, len(imsis))
	forEachConcurrently(len(imsis), fetchConcurrency, func(i int) {
		_, err := h.pv.GetSubscriber(r.Context(), imsis[i])
		switch {
		case isNotFound(err):
			missing[i] = true
		case err != nil:
			errs[i] = err
		}
	})
	for _, err := range errs {
		if err != nil {
			h.log.Warn("get subscriber", "error", err)
			h.renderFailure(w, r, h.apiError(r, err, ""), "/assignments/orphans", "点検の画面へ")
			return
		}
	}
	existing := map[int64]bool{}
	if len(ids) > 0 {
		list, err := h.pv.ListRADIUSClients(r.Context())
		if err != nil {
			h.log.Warn("list radius clients", "error", err)
			h.renderFailure(w, r, h.apiError(r, err, ""), "/assignments/orphans", "点検の画面へ")
			return
		}
		for _, c := range list {
			existing[c.ID] = true
		}
	}
	ctx, cancel := dbContext(r)
	subTenants, err := h.db.SubscriberTenants(ctx, imsis)
	var clientTenants map[int64]int64
	if err == nil {
		clientTenants, err = h.db.ClientTenants(ctx)
	}
	cancel()
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	deleted, kept := 0, 0
	by := userFrom(r.Context()).Email
	for i, imsi := range imsis {
		tenant, recorded := subTenants[imsi]
		if !missing[i] {
			kept++
			continue
		}
		if !recorded {
			continue // 既に消えている
		}
		ctx, cancel := writeContext(r)
		err := h.db.AssignSubscriber(ctx, imsi, authz.NoTenant, by)
		cancel()
		if err != nil {
			h.dbUnavailable(w, r, err)
			return
		}
		h.record(r, auditAssignmentDelete, imsi, tenant, map[string]any{"kind": "subscriber"})
		deleted++
	}
	for _, id := range ids {
		tenant, recorded := clientTenants[id]
		if existing[id] {
			kept++
			continue
		}
		if !recorded {
			continue
		}
		ctx, cancel := writeContext(r)
		err := h.db.AssignClient(ctx, id, authz.NoTenant, by)
		cancel()
		if err != nil {
			h.dbUnavailable(w, r, err)
			return
		}
		h.record(r, auditAssignmentDelete, clientTarget(id), tenant, map[string]any{"kind": "client"})
		deleted++
	}
	seeOther(w, r, withQuery("/assignments/orphans", "deleted", strconv.Itoa(deleted), "kept", strconv.Itoa(kept)))
}
