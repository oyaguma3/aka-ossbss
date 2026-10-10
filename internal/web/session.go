package web

import (
	"net/http"
	"strings"

	"github.com/oyaguma3/eapaka-ossbss-sample/internal/authz"
	"github.com/oyaguma3/eapaka-ossbss-sample/internal/provisioner"
)

// セッションの画面（画面仕様 §5）。provisioner が中継する本PoCのアクティブセッションを、所属で絞って出す。

const (
	// sessionsShown はアクティブセッションの一覧に出す件数の上限（新しい順）。
	sessionsShown = 100
	// sessionsScanned は、所属で絞るときに provisioner から取る件数（新しい順。設計概要 §15 の 7）。
	sessionsScanned = 1000
)

// sessionRow は一覧の 1 行。
type sessionRow struct {
	provisioner.Session
	Tenant int64
}

type sessionListData struct {
	// Staff はテナントの列と絞り込みを出すか（C・D）。
	Staff   bool
	IMSI    string
	Filter  tenantFilter
	Tenants tenantInfo
	Rows    []sessionRow
	// Matched は条件に一致したセッションの数（Rows は sessionsShown 件まで）。
	Matched int64
	// Narrowed は所属で絞ったこと、ScanTruncated はそのとき provisioner のセッションが sessionsScanned 件を超えていたこと。
	Narrowed, ScanTruncated bool
	Error                   string
}

// Truncated は、一致したセッションの一部だけを出しているか。
func (d sessionListData) Truncated() bool { return d.Matched > int64(len(d.Rows)) }

// sessions はアクティブセッションの一覧。全員が使える（A・B は自分のテナントの加入者のものだけ）。
// 読み出しだけで、provisioner と本PoCの監査ログには残らない。
func (h *Handler) sessions(w http.ResponseWriter, r *http.Request) {
	d := sessionListData{Staff: isStaff(r), IMSI: strings.TrimSpace(r.URL.Query().Get("imsi"))}
	t, err := h.tenants(r)
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	d.Tenants = t
	f, ok := readTenantFilter(r, t)
	d.Filter = f
	switch {
	case !ok:
		d.Error = "テナントの指定が正しくありません。"
	case d.IMSI != "" && !imsiPattern.MatchString(d.IMSI):
		d.Error = "IMSI は 15 桁の数字で入力してください。"
	}
	if d.Error != "" {
		h.render(w, r, http.StatusBadRequest, "sessions", "セッション", d)
		return
	}

	params := provisioner.SessionParams{IMSI: d.IMSI, Limit: sessionsShown}
	if d.IMSI != "" {
		// 1 人の加入者のセッション。範囲の外なら 404（加入者の詳細と同じ）。
		tenant, ok := h.subscriberScope(w, r, d.IMSI, authz.SessionView)
		if !ok {
			return
		}
		d.Filter = tenantFilter{Value: f.Value, All: true, Selectable: f.Selectable}
		l, err := h.pv.ListSessions(r.Context(), params)
		if err != nil {
			h.sessionsFailed(w, r, d, err)
			return
		}
		for _, s := range l.Items {
			d.Rows = append(d.Rows, sessionRow{Session: s, Tenant: tenant})
		}
		d.Matched = l.Total
		h.render(w, r, http.StatusOK, "sessions", "セッション", d)
		return
	}

	if !f.All {
		// 所属で絞る。provisioner の新しい 1000 件の中から絞り込む。
		params.Limit, d.Narrowed = sessionsScanned, true
	}
	l, err := h.pv.ListSessions(r.Context(), params)
	if err != nil {
		h.sessionsFailed(w, r, d, err)
		return
	}
	imsis := make([]string, len(l.Items))
	for i, s := range l.Items {
		imsis[i] = s.IMSI
	}
	ctx, cancel := dbContext(r)
	assigned, err := h.db.SubscriberTenants(ctx, imsis)
	cancel()
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	if f.All {
		for _, s := range l.Items {
			d.Rows = append(d.Rows, sessionRow{Session: s, Tenant: assigned[s.IMSI]})
		}
		d.Matched = l.Total
		h.render(w, r, http.StatusOK, "sessions", "セッション", d)
		return
	}
	a := userFrom(r.Context()).Actor()
	for _, s := range l.Items {
		tenant := assigned[s.IMSI]
		if !authz.Allowed(a, authz.SessionView, tenant) || (f.None && tenant != authz.NoTenant) || (!f.None && tenant != f.Tenant) {
			continue
		}
		d.Matched++
		if len(d.Rows) < sessionsShown {
			d.Rows = append(d.Rows, sessionRow{Session: s, Tenant: tenant})
		}
	}
	d.ScanTruncated = l.Total > int64(len(l.Items))
	h.render(w, r, http.StatusOK, "sessions", "セッション", d)
}

func (h *Handler) sessionsFailed(w http.ResponseWriter, r *http.Request, d sessionListData, err error) {
	h.log.Warn("list sessions", "error", err)
	f := h.apiError(r, err, "")
	d.Error = f.Message
	h.render(w, r, f.Status, "sessions", "セッション", d)
}
