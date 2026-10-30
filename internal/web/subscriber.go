package web

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/oyaguma3/aka-ossbss/internal/authz"
	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/ledger"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
)

// 加入者の一覧と詳細（設計概要 §7.3。段階 3-1 の W5）と、状態・削除・変更の操作（§8。W6〜W8）。
// 一覧・件数・絞り込みは台帳で行い、そのページの IMSI だけを両側の一括取得で補う。詳細は vecgen 側と radius 側を並べる。

// subscribersPerPage は一覧の 1 ページの件数。
const subscribersPerPage = 50

// fetchTimeout は、一覧と詳細で下流を読む上限時間（読めなくても台帳は出す）。
const fetchTimeout = 10 * time.Second

// ---- 一覧 ----

// listQuery は一覧の絞り込み（画面の値のまま）。
type listQuery struct {
	Prefix, Tenant, VecgenState, RadiusState, Source string
	Mismatch, Deleted, CheckNeeded                   bool
}

// URL は、この絞り込みで cursor から始まる一覧の URL を返す。
func (q listQuery) URL(cursor string) string {
	v := url.Values{}
	for k, s := range map[string]string{"prefix": q.Prefix, "tenant": q.Tenant, "vecgen": q.VecgenState, "radius": q.RadiusState,
		"source": q.Source, "cursor": cursor} {
		if s != "" {
			v.Set(k, s)
		}
	}
	for k, b := range map[string]bool{"mismatch": q.Mismatch, "deleted": q.Deleted, "check": q.CheckNeeded} {
		if b {
			v.Set(k, "1")
		}
	}
	if len(v) == 0 {
		return "/subscribers"
	}
	return "/subscribers?" + v.Encode()
}

// subscriberRow は一覧の 1 行（台帳と、一括取得で読んだ下流）。
type subscriberRow struct {
	db.Entry
	// Vecgen と Radius は一括取得で読んだもの（台帳でない側・読めなかったときは nil）。
	Vecgen *provisioner.VecgenSubscriber
	Radius *provisioner.RadiusSubscriber
	// VecgenMissing と RadiusMissing は、台帳にあるのに下流になかったこと。
	VecgenMissing, RadiusMissing bool
}

type subscriberListData struct {
	// Staff は、テナント・AMF・SQN の列と、テナントの絞り込みを出すか（C・D）。
	Staff                      bool
	CanVecgenNew, CanRadiusNew bool
	Query                      listQuery
	Filter                     tenantFilter
	Tenants                    tenantInfo
	Sources                    []string
	States                     []string
	Rows                       []subscriberRow
	Total                      int64
	Cursor, Next               string
	// FetchError は、下流を読めず一部の列を出せなかったときの文。
	FetchError string
	Error      string
}

// readListQuery は一覧の絞り込みを読み、DB の絞り込みにする。値が正しくなければ文を返す。
func readListQuery(q url.Values, f tenantFilter) (listQuery, db.SubscriberFilter, string) {
	lq := listQuery{Prefix: strings.TrimSpace(q.Get("prefix")), Tenant: f.Value, VecgenState: q.Get("vecgen"), RadiusState: q.Get("radius"),
		Source: q.Get("source"), Mismatch: q.Get("mismatch") == "1", Deleted: q.Get("deleted") == "1", CheckNeeded: q.Get("check") == "1"}
	df := db.SubscriberFilter{TenantID: f.Tenant, Prefix: lq.Prefix, VecgenState: lq.VecgenState, RadiusState: lq.RadiusState,
		SourceID: lq.Source, Mismatch: lq.Mismatch, Deleted: lq.Deleted, CheckNeeded: lq.CheckNeeded}
	switch {
	case lq.Prefix != "" && !prefixDigits.MatchString(lq.Prefix):
		return lq, df, "IMSI の前方一致は 15 桁までの数字で指定してください。"
	case lq.VecgenState != "" && !slices.Contains(filterStates, lq.VecgenState),
		lq.RadiusState != "" && !slices.Contains(filterStates, lq.RadiusState):
		return lq, df, "状態の指定が正しくありません。"
	case lq.Source != "" && !sourceIDPattern.MatchString(lq.Source):
		return lq, df, "取得先の指定が正しくありません。"
	}
	return lq, df, ""
}

// filterStates は一覧の状態の絞り込みの選択肢。
var filterStates = []string{db.StateActive, db.StateInactive, db.StateTerminated, db.FilterAbsent}

func (h *Handler) subscribers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	d := subscriberListData{Staff: isStaff(r), CanVecgenNew: can(r, authz.VecgenRegister), CanRadiusNew: can(r, authz.RadiusRegister),
		Cursor: q.Get("cursor"), States: filterStates}
	t, err := h.tenants(r)
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	d.Tenants = t
	f, ok := readTenantFilter(r, t, false)
	d.Filter = f
	lq, df, msg := readListQuery(q, f)
	d.Query = lq
	switch {
	case !ok:
		d.Error = "テナントの指定が正しくありません。"
	case msg != "":
		d.Error = msg
	case d.Cursor != "" && !imsiPattern.MatchString(d.Cursor):
		d.Error = "ページの位置の指定が正しくありません。"
	}
	if d.Error != "" {
		h.render(w, r, http.StatusBadRequest, "subscribers", "加入者", d)
		return
	}

	ctx, cancel := dbContext(r)
	rows, err := h.db.ListSubscribers(ctx, df, d.Cursor, subscribersPerPage+1)
	if err == nil {
		d.Total, err = h.db.CountSubscribers(ctx, df)
	}
	if err == nil {
		d.Sources, err = h.db.SubscriberSources(ctx, f.Tenant)
	}
	cancel()
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	if len(rows) > subscribersPerPage {
		rows = rows[:subscribersPerPage]
		d.Next = rows[len(rows)-1].IMSI
	}
	d.Rows = make([]subscriberRow, len(rows))
	for i, e := range rows {
		d.Rows[i] = subscriberRow{Entry: e}
	}
	d.FetchError = h.fillRows(r, d.Rows, d.Staff)
	h.render(w, r, http.StatusOK, "subscribers", "加入者", d)
}

// fillRows は、一覧のページの IMSI を両側の一括取得で読んで行に入れる（W5）。vecgen 側（AMF・SQN）は C・D だけ読む。
// 読めなければ、その文を返す（台帳の列は出す）。
func (h *Handler) fillRows(r *http.Request, rows []subscriberRow, staff bool) string {
	var vImsis, rImsis []string
	for _, row := range rows {
		if row.VecgenPresent {
			vImsis = append(vImsis, row.IMSI)
		}
		if row.RadiusPresent {
			rImsis = append(rImsis, row.IMSI)
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), fetchTimeout)
	defer cancel()
	var errs []string
	if staff && len(vImsis) > 0 {
		b, err := h.pv.VecgenBatchGet(ctx, vImsis)
		if err != nil {
			h.log.Warn("list: vecgen batch-get", "error", err)
			errs = append(errs, "aka-vecgen")
		} else {
			got := map[string]*provisioner.VecgenSubscriber{}
			for i := range b.Items {
				got[b.Items[i].IMSI] = &b.Items[i]
			}
			for i := range rows {
				if rows[i].VecgenPresent {
					rows[i].Vecgen, rows[i].VecgenMissing = got[rows[i].IMSI], got[rows[i].IMSI] == nil
				}
			}
		}
	}
	if len(rImsis) > 0 {
		b, err := h.pv.RadiusBatchGet(ctx, rImsis)
		if err != nil {
			h.log.Warn("list: radius batch-get", "error", err)
			errs = append(errs, "aka-radius")
		} else {
			got := map[string]*provisioner.RadiusSubscriber{}
			for i := range b.Items {
				got[b.Items[i].IMSI] = &b.Items[i]
			}
			for i := range rows {
				if rows[i].RadiusPresent {
					rows[i].Radius, rows[i].RadiusMissing = got[rows[i].IMSI], got[rows[i].IMSI] == nil
				}
			}
		}
	}
	if len(errs) == 0 {
		return ""
	}
	return strings.Join(errs, "・") + " を読めませんでした。台帳の値だけを出しています（「-」の列）。"
}

// ---- 詳細 ----

// subscriberPerm は詳細の画面で出す操作。
type subscriberPerm struct {
	State, Reinstate, Delete, VecgenEdit, SourceEdit, PolicyEdit, Move, VecgenNew, RadiusNew, Detail bool
}

type subscriberData struct {
	Entry      db.Entry
	TenantName string
	Staff      bool
	Perm       subscriberPerm
	// Vecgen と Radius は下流から読んだもの（ない・読めなかったときは nil）。VecgenErr・RadiusErr は読めなかったときの文。
	Vecgen               *provisioner.VecgenSubscriber
	Radius               *provisioner.RadiusSubscriber
	VecgenErr, RadiusErr string
	// Drifts は台帳と下流の違いの説明。
	Drifts []string
	// Clients は aka-vecgen の AVクライアント（許可クライアントの名前と選択肢。C・D）。Sources は取得先（C・D）。
	Clients []provisioner.VecgenClient
	Sources []provisioner.Source
	Tenants tenantInfo
	// Key は、この画面のフォームの Idempotency-Key（操作ごとに 1 つのキーを、側ごとに sideKey で分ける）。
	Key            string
	Message, Error string
	Errors         fieldErrors
	// Form は vecgen 側の変更の入力（誤りのとき入力を残す）。
	Form vecgenEditForm
}

// vecgenEditForm は vecgen 側の変更の入力。
type vecgenEditForm struct {
	AMF, SQN, SQNType string
	Clients           []int64
	Set               bool
}

// Sides は詳細の両側（テンプレートで並べる）。
func (d subscriberData) Sides() []db.Side { return db.Sides }

// BothOps は「両方を止める・使う」を出すか（両側にあり、どちらも解約でない）。
func (d subscriberData) BothOps() bool {
	e := d.Entry
	return d.Perm.State && e.VecgenPresent && e.RadiusPresent && e.VecgenState != db.StateTerminated && e.RadiusState != db.StateTerminated
}

// ClientName は AVクライアントの名前を返す（なければ番号）。
func (d subscriberData) ClientName(id int64) string {
	for _, c := range d.Clients {
		if c.ID == id {
			return c.Name
		}
	}
	return "#" + formatID(id)
}

// loadSubscriber は詳細に出す値を集める（台帳 e は範囲を確かめたもの）。
func (h *Handler) loadSubscriber(r *http.Request, e db.Entry) subscriberData {
	a := userFrom(r.Context()).Actor()
	allowed := func(op authz.Op) bool { return authz.Allowed(a, op, e.TenantID) }
	d := subscriberData{Entry: e, Staff: isStaff(r), Key: newIdemKey(), Errors: fieldErrors{},
		Perm: subscriberPerm{State: allowed(authz.SubscriberStateChange), Reinstate: allowed(authz.SubscriberReinstate),
			Delete: allowed(authz.SubscriberDelete), VecgenEdit: allowed(authz.VecgenUpdate), SourceEdit: allowed(authz.SourceUpdate),
			PolicyEdit: allowed(authz.PolicyUpdate), Move: allowed(authz.TenantMove), VecgenNew: allowed(authz.VecgenRegister),
			RadiusNew: allowed(authz.RadiusRegister), Detail: allowed(authz.SubscriberDetailView)}}
	t, err := h.tenants(r)
	if err != nil {
		h.log.Error("tenants", "error", err)
	}
	d.Tenants, d.TenantName = t, t.Name(e.TenantID)

	ctx, cancel := context.WithTimeout(r.Context(), fetchTimeout)
	defer cancel()
	if e.VecgenPresent || e.CheckNeeded {
		v, err := h.pv.VecgenSubscriber(ctx, e.IMSI)
		switch {
		case err == nil:
			d.Vecgen = &v
		case provisioner.CauseOf(err) == provisioner.CauseUserNotFound:
		default:
			h.log.Warn("get vecgen subscriber", "imsi", e.IMSI, "error", err)
			d.VecgenErr = h.apiError(r, err, db.SideVecgen).Message
		}
	}
	if e.RadiusPresent || e.CheckNeeded {
		rs, err := h.pv.RadiusSubscriber(ctx, e.IMSI)
		switch {
		case err == nil:
			d.Radius = &rs
		case provisioner.CauseOf(err) == provisioner.CauseUserNotFound:
		default:
			h.log.Warn("get radius subscriber", "imsi", e.IMSI, "error", err)
			d.RadiusErr = h.apiError(r, err, db.SideRadius).Message
		}
	}
	d.Drifts = drifts(e, d)
	if d.Perm.Detail {
		if c, err := h.pv.VecgenClients(ctx); err == nil {
			d.Clients = c
		}
	}
	if d.Perm.SourceEdit && e.RadiusPresent {
		if s, err := h.pv.RadiusSources(ctx); err == nil {
			d.Sources = s
		}
	}
	return d
}

// drifts は、台帳と下流の違いを説明する文を返す（印が立っているときは、確かめている途中なので出さない）。
func drifts(e db.Entry, d subscriberData) []string {
	if e.CheckNeeded {
		return nil
	}
	var out []string
	check := func(side db.Side, present bool, state string, got bool, gotState, errMsg string) {
		switch {
		case errMsg != "":
		case present && !got:
			out = append(out, sideLabel(side)+"は台帳では「"+stateLabel(state)+"」ですが、下流にありません（aka-ossbss の外で削除された可能性があります）。")
		case !present && got:
			out = append(out, sideLabel(side)+"は台帳にはありませんが、下流にあります（aka-ossbss の外で登録された可能性があります）。")
		case present && state != gotState:
			out = append(out, sideLabel(side)+"の状態が、台帳（"+stateLabel(state)+"）と下流（"+stateLabel(gotState)+"）で違います。")
		}
	}
	vState, rState := "", ""
	if d.Vecgen != nil {
		vState = d.Vecgen.State
	}
	if d.Radius != nil {
		rState = d.Radius.State
	}
	check(db.SideVecgen, e.VecgenPresent, e.VecgenState, d.Vecgen != nil, vState, d.VecgenErr)
	check(db.SideRadius, e.RadiusPresent, e.RadiusState, d.Radius != nil, rState, d.RadiusErr)
	if len(out) > 0 {
		out = append(out, "台帳は毎日の突き合わせで下流に合わせます。")
	}
	return out
}

func (h *Handler) subscriber(w http.ResponseWriter, r *http.Request) {
	imsi, ok := h.pathIMSI(w, r)
	if !ok {
		return
	}
	e, ok := h.subscriberScope(w, r, imsi, authz.SubscriberView)
	if !ok {
		return
	}
	var msg string
	if e.CheckNeeded && h.recheck(r, imsi) {
		// 結果が分からなかった操作を、開いたときに読み直して台帳を直した（W7）。
		if e, ok = h.subscriberScope(w, r, imsi, authz.SubscriberView); !ok {
			return
		}
		msg = "結果が分からなかった操作を読み直し、台帳を下流に合わせました。"
	}
	d := h.loadSubscriber(r, e)
	d.Message = msg
	switch r.URL.Query().Get("created") {
	case string(db.SideVecgen), string(db.SideRadius):
		d.Message = sideLabel(db.Side(r.URL.Query().Get("created"))) + "に登録しました。"
	}
	title := "加入者 " + imsi
	h.render(w, r, http.StatusOK, "subscriber", title, d)
}

// renderSubscriber は操作の結果を詳細（htmx では詳細の部分だけ）で返す。台帳はもう一度読む（範囲も確かめ直す）。
func (h *Handler) renderSubscriber(w http.ResponseWriter, r *http.Request, imsi string, status int, mutate func(*subscriberData)) {
	e, ok := h.subscriberScope(w, r, imsi, authz.SubscriberView)
	if !ok {
		return
	}
	d := h.loadSubscriber(r, e)
	if mutate != nil {
		mutate(&d)
	}
	h.renderPart(w, r, status, "subscriber", "subscriber-detail", "加入者 "+imsi, d)
}

// opStart は詳細の操作の共通の前処理（IMSI、範囲と権限、フォーム）。
func (h *Handler) opStart(w http.ResponseWriter, r *http.Request, op authz.Op) (db.Entry, bool) {
	imsi, ok := h.pathIMSI(w, r)
	if !ok {
		return db.Entry{}, false
	}
	e, ok := h.subscriberScope(w, r, imsi, op)
	if !ok {
		return db.Entry{}, false
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return db.Entry{}, false
	}
	return e, true
}

// formSide はフォームの側（vecgen / radius）を読む。
func formSide(r *http.Request) (db.Side, bool) {
	s := db.Side(r.PostForm.Get("side"))
	return s, s == db.SideVecgen || s == db.SideRadius
}

// sideResult は、1 つの側の操作の結果を文にする（成功なら ok の文）。
func (h *Handler) sideResult(r *http.Request, side db.Side, o ledger.Outcome, err error, what, ok string) (msg string, failed bool) {
	switch {
	case err != nil:
		return sideLabel(side) + "の" + what + "は済みましたが、台帳を書けませんでした（結果を確かめる印を付けました）。", true
	case o.Refused != "":
		return o.Refused, true
	case o.Unknown:
		return unknownMessage(sideLabel(side)+"の"+what, o), true
	case o.Err != nil:
		return sideLabel(side) + "の" + what + "に失敗しました: " + h.apiError(r, o.Err, side).Message, true
	}
	return ok, false
}

// ---- 状態の変更（W6） ----

// subscriberState は状態を変える。side は vecgen / radius / both（両方を止める・使う）。
// 両方のときは 1 つずつ呼び、取り消さない（F5）。止めるときは radius 側を先、使うときは vecgen 側を先にする。
func (h *Handler) subscriberState(w http.ResponseWriter, r *http.Request) {
	e, ok := h.opStart(w, r, authz.SubscriberStateChange)
	if !ok {
		return
	}
	pf := r.PostForm
	state, reason := pf.Get("state"), strings.TrimSpace(pf.Get("reason"))
	var sides []db.Side
	switch pf.Get("side") {
	case string(db.SideVecgen), string(db.SideRadius):
		sides = []db.Side{db.Side(pf.Get("side"))}
	case "both":
		sides = []db.Side{db.SideRadius, db.SideVecgen}
		if state == db.StateActive {
			sides = []db.Side{db.SideVecgen, db.SideRadius}
		}
	}
	if len(sides) == 0 || !db.ValidState(state) || len([]rune(reason)) > maxReasonLen {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。画面を開き直してください。")
		return
	}
	key := formIdemKey(r, "idem")
	var msgs []string
	failed := false
	for _, side := range sides {
		o, err := h.runLedger(r, e.IMSI, ledger.Step{
			Check: func(cur db.Entry, exists bool) string {
				switch {
				case !exists || !cur.Present(side):
					return sideLabel(side) + "には登録されていません。"
				case cur.State(side) == db.StateTerminated:
					return sideLabel(side) + "は解約です。戻すのはシステム管理者の「戻す」です。"
				}
				return ""
			},
			Call: func(ctx context.Context, _ db.Entry) error {
				_, err := h.pv.SetState(ctx, provisioner.Downstream(side), e.IMSI, provisioner.StateChange{State: state, Reason: reason}, sideKey(key, side))
				return err
			},
			Change: func(db.Entry, bool) db.Change {
				return db.Change{Kind: db.ChangeState, Side: side, State: state, Reason: reason}
			},
		})
		msg, f := h.sideResult(r, side, o, err, "状態の変更", sideLabel(side)+"を「"+stateLabel(state)+"」にしました。")
		msgs, failed = append(msgs, msg), failed || f
		if !f || o.Unknown {
			h.record(r, auditStateChange, e.IMSI, e.TenantID, map[string]any{"side": string(side), "state": state, "reason": reason,
				"result": resultWord(o, err)})
		}
	}
	h.renderOpResult(w, r, e.IMSI, msgs, failed, key)
}

// resultWord は監査ログに残す結果（ok / unknown）。
func resultWord(o ledger.Outcome, err error) string {
	if o.Unknown || err != nil {
		return "unknown"
	}
	return "ok"
}

// renderOpResult は操作の結果の文を詳細に出して返す。失敗したら、同じキーで送り直せるよう残す。
func (h *Handler) renderOpResult(w http.ResponseWriter, r *http.Request, imsi string, msgs []string, failed bool, key string) {
	status := http.StatusOK
	if failed {
		status = http.StatusConflict
	}
	h.renderSubscriber(w, r, imsi, status, func(d *subscriberData) {
		if failed {
			d.Error, d.Key = strings.Join(msgs, " "), baseKey(key)
			return
		}
		d.Message = strings.Join(msgs, " ")
	})
}

// subscriberReinstate は解約（Terminated）の側を停止中に戻す（システム管理者だけ。理由が必須）。
func (h *Handler) subscriberReinstate(w http.ResponseWriter, r *http.Request) {
	e, ok := h.opStart(w, r, authz.SubscriberReinstate)
	if !ok {
		return
	}
	side, ok := formSide(r)
	reason := strings.TrimSpace(r.PostForm.Get("reason"))
	if !ok {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。画面を開き直してください。")
		return
	}
	if reason == "" || len([]rune(reason)) > maxReasonLen {
		h.renderSubscriber(w, r, e.IMSI, http.StatusBadRequest, func(d *subscriberData) {
			d.Error = "戻す理由を 500 文字までで入力してください。"
			d.Errors["reinstate_"+string(side)] = "理由を入力してください。"
		})
		return
	}
	key := formIdemKey(r, "idem")
	o, err := h.runLedger(r, e.IMSI, ledger.Step{
		Check: func(cur db.Entry, exists bool) string {
			if !exists || cur.State(side) != db.StateTerminated {
				return sideLabel(side) + "は解約ではありません。画面を開き直してください。"
			}
			return ""
		},
		Call: func(ctx context.Context, _ db.Entry) error {
			_, err := h.pv.Reinstate(ctx, provisioner.Downstream(side), e.IMSI, reason, sideKey(key, side))
			return err
		},
		Change: func(db.Entry, bool) db.Change {
			return db.Change{Kind: db.ChangeState, Side: side, State: db.StateInactive, Reason: reason}
		},
	})
	msg, failed := h.sideResult(r, side, o, err, "戻し", sideLabel(side)+"を解約から停止中に戻しました。")
	if !failed || o.Unknown {
		h.record(r, auditReinstate, e.IMSI, e.TenantID, map[string]any{"side": string(side), "reason": reason, "result": resultWord(o, err)})
	}
	h.renderOpResult(w, r, e.IMSI, []string{msg}, failed, key)
}

// subscriberDelete はその側を物理削除する（運用担当・システム管理者。Active は不可）。両側を消したら台帳は削除済み。
func (h *Handler) subscriberDelete(w http.ResponseWriter, r *http.Request) {
	e, ok := h.opStart(w, r, authz.SubscriberDelete)
	if !ok {
		return
	}
	side, ok := formSide(r)
	if !ok {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。画面を開き直してください。")
		return
	}
	key := formIdemKey(r, "idem")
	var state string
	o, err := h.runLedger(r, e.IMSI, ledger.Step{
		Check: func(cur db.Entry, exists bool) string {
			switch {
			case !exists || !cur.Present(side):
				return sideLabel(side) + "には登録されていません。"
			case cur.State(side) == db.StateActive:
				return "利用中（Active）の加入者は物理削除できません。先に止めるか解約してください。"
			}
			state = cur.State(side)
			return ""
		},
		Call: func(ctx context.Context, _ db.Entry) error {
			return h.pv.DeleteSubscriber(ctx, provisioner.Downstream(side), e.IMSI, sideKey(key, side))
		},
		Change: func(db.Entry, bool) db.Change { return db.Change{Kind: db.ChangeDelete, Side: side} },
	})
	done := sideLabel(side) + "から物理削除しました。"
	if o.Entry.Deleted() {
		done += "両側ともなくなったので、台帳は削除済みにしました（登録し直すにはシステム管理者の承認が要ります）。"
	}
	msg, failed := h.sideResult(r, side, o, err, "物理削除", done)
	if !failed || o.Unknown {
		h.record(r, auditSubscriberDelete, e.IMSI, e.TenantID, map[string]any{"side": string(side), "state": state, "result": resultWord(o, err)})
	}
	h.renderOpResult(w, r, e.IMSI, []string{msg}, failed, key)
}

// ---- 変更（vecgen 側・取得先） ----

// vecgenUpdate は vecgen 側の AMF・SQN・SQN の方式・許可クライアントを変える（システム管理者だけ）。変わった項目だけを送る。
func (h *Handler) vecgenUpdate(w http.ResponseWriter, r *http.Request) {
	e, ok := h.opStart(w, r, authz.VecgenUpdate)
	if !ok {
		return
	}
	pf := r.PostForm
	f := vecgenEditForm{AMF: normalizeHex(pf.Get("amf")), SQN: normalizeHex(pf.Get("sqn")), SQNType: pf.Get("sqn_type"), Set: true}
	errs := fieldErrors{}
	for _, v := range pf["clients"] {
		id, err := parseID(v)
		errs.check(err == nil, "clients", "許可クライアントの指定が正しくありません。")
		f.Clients = append(f.Clients, id)
	}
	errs.check(amfPattern.MatchString(f.AMF), "amf", "AMF は 16 進 4 桁で入力してください。")
	errs.check(sqnPattern.MatchString(f.SQN), "sqn", "SQN は 16 進 12 桁で入力してください。")
	errs.check(slices.Contains(sqnTypes, f.SQNType), "sqnType", "SQN の方式を選んでください。")
	key := formIdemKey(r, "idem")
	if len(errs) > 0 {
		h.renderSubscriber(w, r, e.IMSI, http.StatusBadRequest, func(d *subscriberData) {
			d.Errors, d.Error, d.Form, d.Key = errs, "入力を確かめてください。", f, baseKey(key)
		})
		return
	}
	cur, err := h.pv.VecgenSubscriber(r.Context(), e.IMSI)
	if err != nil {
		h.renderSubscriber(w, r, e.IMSI, h.apiError(r, err, db.SideVecgen).Status, func(d *subscriberData) {
			d.Error = h.apiError(r, err, db.SideVecgen).Message
		})
		return
	}
	var u provisioner.VecgenSubscriberUpdate
	var changed []string
	// SQN は変えたときだけ送る（送らなければ、認証で進んだ SQN に触れない）。
	if f.SQN != cur.SQN {
		u.SQN, changed = f.SQN, append(changed, "sqn")
	}
	if f.AMF != cur.AMF {
		u.AMF, changed = f.AMF, append(changed, "amf")
	}
	if f.SQNType != cur.SQNType {
		u.SQNType, changed = f.SQNType, append(changed, "sqnType")
	}
	slices.Sort(f.Clients)
	f.Clients = slices.Compact(f.Clients)
	// 選択肢を出せなかったフォーム（AVクライアントを読めなかった）からは、許可クライアントを変えない。
	if pf.Get("clients_shown") == "1" && !slices.Equal(f.Clients, cur.AllowedClientIDs) {
		u.AllowedClientIDs, changed = new(slices.Clone(f.Clients)), append(changed, "allowedClientIds")
		if *u.AllowedClientIDs == nil {
			*u.AllowedClientIDs = []int64{}
		}
	}
	if len(changed) == 0 {
		h.renderSubscriber(w, r, e.IMSI, http.StatusOK, func(d *subscriberData) { d.Message = "変更はありません。" })
		return
	}
	o, err := h.runLedger(r, e.IMSI, ledger.Step{
		Check: func(cur db.Entry, exists bool) string {
			if !exists || !cur.VecgenPresent {
				return sideLabel(db.SideVecgen) + "には登録されていません。"
			}
			return ""
		},
		Call: func(ctx context.Context, _ db.Entry) error {
			_, err := h.pv.UpdateVecgenSubscriber(ctx, e.IMSI, u, key)
			return err
		},
	})
	msg, failed := h.sideResult(r, db.SideVecgen, o, err, "変更", sideLabel(db.SideVecgen)+"を変更しました（"+strings.Join(changed, "、")+"）。")
	if !failed || o.Unknown {
		h.record(r, auditVecgenUpdate, e.IMSI, e.TenantID, map[string]any{"fields": changed, "result": resultWord(o, err)})
	}
	h.renderOpResult(w, r, e.IMSI, []string{msg}, failed, key)
}

// sourceUpdate は radius 側の取得先を変える（運用担当・システム管理者）。
func (h *Handler) sourceUpdate(w http.ResponseWriter, r *http.Request) {
	e, ok := h.opStart(w, r, authz.SourceUpdate)
	if !ok {
		return
	}
	source := r.PostForm.Get("source")
	if !sourceIDPattern.MatchString(source) {
		h.renderSubscriber(w, r, e.IMSI, http.StatusBadRequest, func(d *subscriberData) { d.Error = "取得先を選んでください。" })
		return
	}
	key := formIdemKey(r, "idem")
	var from string
	o, err := h.runLedger(r, e.IMSI, ledger.Step{
		Check: func(cur db.Entry, exists bool) string {
			switch {
			case !exists || !cur.RadiusPresent:
				return sideLabel(db.SideRadius) + "には登録されていません。"
			case cur.RadiusState == db.StateTerminated:
				return "解約（Terminated）の加入者は変更できません。"
			case cur.RadiusSourceID == source:
				return "取得先は既に " + source + " です。"
			}
			from = cur.RadiusSourceID
			return ""
		},
		Call: func(ctx context.Context, _ db.Entry) error {
			_, err := h.pv.UpdateRadiusSubscriber(ctx, e.IMSI, provisioner.RadiusSubscriberUpdate{SourceID: source}, key)
			return err
		},
		Change: func(db.Entry, bool) db.Change { return db.Change{Kind: db.ChangeSource, SourceID: source} },
	})
	msg, failed := h.sideResult(r, db.SideRadius, o, err, "取得先の変更", "取得先を "+source+" にしました。")
	if !failed || o.Unknown {
		h.record(r, auditSourceUpdate, e.IMSI, e.TenantID, map[string]any{"sourceId": map[string]any{"from": from, "to": source},
			"result": resultWord(o, err)})
	}
	h.renderOpResult(w, r, e.IMSI, []string{msg}, failed, key)
}

// ---- 所属のテナントの付け替え（W8） ----

// tenantMove は所属のテナントを付け替える（システム管理者だけ）。radius 側があれば、ルールのテナントを新しいテナントに
// 書き換えてから台帳を書く（F4）。
func (h *Handler) tenantMove(w http.ResponseWriter, r *http.Request) {
	e, ok := h.opStart(w, r, authz.TenantMove)
	if !ok {
		return
	}
	t, err := h.tenants(r)
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	to, chosen, exists := readTenantChoice(r.PostForm.Get("tenant"), t)
	if !chosen || !exists || to == e.TenantID {
		h.renderSubscriber(w, r, e.IMSI, http.StatusBadRequest, func(d *subscriberData) {
			d.Error = "付け替え先のテナントを、今と違うものから選んでください。"
		})
		return
	}
	key := formIdemKey(r, "idem")
	from := e.TenantID
	o, err := h.runLedger(r, e.IMSI, ledger.Step{
		Check: func(cur db.Entry, exists bool) string {
			switch {
			case !exists || cur.Deleted() || cur.Pending():
				return "台帳に登録のない加入者は付け替えられません。"
			case cur.TenantID != from:
				return "所属は他の操作で変わりました。画面を開き直してください。"
			case cur.RadiusState == db.StateTerminated:
				return "radius 側が解約（Terminated）なので、ルールのテナントを書き換えられません。先に戻してください。"
			}
			return ""
		},
		Call: func(ctx context.Context, cur db.Entry) error {
			if !cur.RadiusPresent {
				return nil
			}
			rs, err := h.pv.RadiusSubscriber(ctx, cur.IMSI)
			if err != nil {
				return err
			}
			rules := slices.Clone(rs.Rules)
			for i := range rules {
				rules[i].TenantID = to
			}
			_, err = h.pv.UpdateRadiusSubscriber(ctx, cur.IMSI, provisioner.RadiusSubscriberUpdate{Rules: rules}, key)
			return err
		},
		Change: func(db.Entry, bool) db.Change { return db.Change{Kind: db.ChangeMove, TenantID: to} },
	})
	msg, failed := h.sideResult(r, db.SideRadius, o, err, "ルールのテナントの書き換え", "所属を「"+t.Name(to)+"」に付け替えました。")
	if !failed || o.Unknown {
		h.recordMove(r, auditTenantMove, e.IMSI, from, to, map[string]any{"tenant": map[string]any{"from": from, "to": to},
			"result": resultWord(o, err)})
	}
	h.renderOpResult(w, r, e.IMSI, []string{msg}, failed, key)
}
