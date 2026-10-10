package web

import (
	"context"
	"net/http"
	"time"

	"github.com/oyaguma3/eapaka-ossbss-sample/internal/authz"
	"github.com/oyaguma3/eapaka-ossbss-sample/internal/provisioner"
)

// ダッシュボードの件数（画面仕様 §6）。

const (
	// suspendedScanPage と suspendedScanMax は、停止中の数を数えるときに provisioner の認可ポリシーの一覧を読む 1 回の件数と、
	// 読む件数の上限（§17.5 の 5）。
	suspendedScanPage = 500
	suspendedScanMax  = 10000
	// dashboardTimeout はダッシュボードの provisioner の呼び出しの上限時間（下流が止まっていると時間がかかるため短くする）。
	dashboardTimeout = 15 * time.Second
)

// dashboardCounts はダッシュボードに出す件数（取得できなかったものは -1）。
type dashboardCounts struct {
	// Staff は運用担当・システム管理者の範囲（すべて）か。
	Staff bool
	// Tenants はテナントの数（運用担当・システム管理者だけ）。
	Tenants int64
	// Subscribers は、テナントの利用者は所属の記録の件数、運用担当・システム管理者は provisioner の認可ポリシーの件数。
	Subscribers int64
	// Clients は範囲の RADIUSクライアントの数。
	Clients int64
}

// dashboardCounts は利用者の範囲の件数を集める（停止中の数は dashboardSuspended で後から読み込む）。
func (h *Handler) dashboardCounts(r *http.Request) dashboardCounts {
	a := userFrom(r.Context()).Actor()
	c := dashboardCounts{Staff: !a.Role.TenantScoped(), Tenants: -1, Subscribers: -1, Clients: -1}
	ctx, cancel := context.WithTimeout(r.Context(), dashboardTimeout)
	defer cancel()

	dctx, dcancel := dbContext(r)
	assigned, err := h.db.ClientTenants(dctx)
	if a.Role.TenantScoped() {
		if n, err := h.db.CountTenantSubscribers(dctx, a.TenantID, ""); err == nil {
			c.Subscribers = n
		}
	} else if t, err := h.db.Tenants(dctx); err == nil {
		c.Tenants = int64(len(t))
	}
	dcancel()
	if err != nil {
		h.log.Warn("dashboard: client tenants", "error", err)
	} else if clients, err := h.pv.ListRADIUSClients(ctx); err != nil {
		h.log.Warn("dashboard: list radius clients", "error", err)
	} else {
		c.Clients = 0
		for _, cl := range clients {
			if authz.Allowed(a, authz.ClientView, assigned[cl.ID]) {
				c.Clients++
			}
		}
	}
	if !a.Role.TenantScoped() {
		if l, err := h.pv.ListPolicies(ctx, provisioner.ListParams{Limit: 1}); err != nil {
			h.log.Warn("dashboard: list policies", "error", err)
		} else {
			c.Subscribers = l.Total
		}
	}
	return c
}

type suspendedData struct {
	Count int64
	// Truncated は上限（10,000 件）で数えるのを止めたこと。
	Truncated bool
	Error     bool
}

// dashboardSuspended は、範囲の停止中の加入者の数を数えて返す（ダッシュボードから htmx で読み込む部品）。
// provisioner の認可ポリシーの一覧（状態つき）を 500 件ずつ、10,000 件まで読み、テナントの利用者は所属の記録と突き合わせる。
func (h *Handler) dashboardSuspended(w http.ResponseWriter, r *http.Request) {
	a := userFrom(r.Context()).Actor()
	d := suspendedData{}
	var mine map[string]bool
	if a.Role.TenantScoped() {
		ctx, cancel := dbContext(r)
		imsis, err := h.db.TenantSubscribers(ctx, a.TenantID, "", "", suspendedScanMax)
		cancel()
		if err != nil {
			h.log.Warn("dashboard: tenant subscribers", "error", err)
			d.Error = true
			h.renderBlock(w, r, http.StatusOK, "home", "suspended-count", d)
			return
		}
		d.Truncated = len(imsis) >= suspendedScanMax
		mine = make(map[string]bool, len(imsis))
		for _, imsi := range imsis {
			mine[imsi] = true
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), dashboardTimeout)
	defer cancel()
	cursor, read := "", 0
	for {
		l, err := h.pv.ListPolicies(ctx, provisioner.ListParams{Cursor: cursor, Limit: suspendedScanPage})
		if err != nil {
			h.log.Warn("dashboard: list policies", "error", err)
			d.Error = true
			break
		}
		for _, p := range l.Items {
			if p.Status == provisioner.PolicySuspended && (mine == nil || mine[p.IMSI]) {
				d.Count++
			}
		}
		read += len(l.Items)
		if l.NextCursor == "" {
			break
		}
		if read >= suspendedScanMax {
			d.Truncated = true
			break
		}
		cursor = l.NextCursor
	}
	h.renderBlock(w, r, http.StatusOK, "home", "suspended-count", d)
}
