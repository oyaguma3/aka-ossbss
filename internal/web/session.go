package web

import (
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/oyaguma3/aka-ossbss/internal/authz"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
)

// セッションの画面（設計概要 §11。段階 3-1 の W9）。aka-radius のセッションを、テナント（刻印のテナント）か IMSI で絞って出す。
// テナントユーザーとテナント管理者は自分のテナントのもの。運用担当とシステム管理者は、テナントか IMSI（カンマ区切り 100 件まで）を
// 選ぶ（どちらかが要る）。加入者の詳細からは、その IMSI で開く。読み出しだけで、セッションは切らない。

// sessionsPerPage はセッションの一覧の 1 ページの件数。
const sessionsPerPage = 100

type sessionListData struct {
	// Staff はテナントの選択と、テナントの列を出すか（C・D）。
	Staff   bool
	IMSIs   string
	Filter  tenantFilter
	Tenants tenantInfo
	Rows    []provisioner.Session
	// Queried は条件を指定して読んだか（C・D は条件がなければ読まない）。
	Queried      bool
	Cursor, Next string
	Error        string
}

// NextURL は次のページの URL。
func (d sessionListData) NextURL() string {
	v := url.Values{}
	if d.IMSIs != "" {
		v.Set("imsi", d.IMSIs)
	}
	if d.Filter.Value != "" {
		v.Set("tenant", d.Filter.Value)
	}
	v.Set("cursor", d.Next)
	return "/sessions?" + v.Encode()
}

// readIMSIList はカンマ区切りの IMSI を読む（重なりを除く）。正しくなければ文を返す。
func readIMSIList(v string) ([]string, string) {
	var out []string
	for s := range strings.SplitSeq(v, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !imsiPattern.MatchString(s) {
			return nil, "IMSI は 15 桁の数字をカンマで区切って入力してください。"
		}
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	if len(out) > provisioner.MaxSessionIMSIs {
		return nil, "IMSI は 100 件までです。"
	}
	return out, ""
}

func (h *Handler) sessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	d := sessionListData{Staff: isStaff(r), IMSIs: strings.TrimSpace(q.Get("imsi")), Cursor: q.Get("cursor")}
	t, err := h.tenants(r)
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	d.Tenants = t
	f, ok := readTenantFilter(r, t, false)
	d.Filter = f
	imsis, msg := readIMSIList(d.IMSIs)
	switch {
	case !ok:
		d.Error = "テナントの指定が正しくありません。"
	case msg != "":
		d.Error = msg
	case d.Cursor != "" && !cursorPattern.MatchString(d.Cursor):
		d.Error = "ページの位置の指定が正しくありません。"
	case d.Staff && len(imsis) > 0 && !f.All:
		d.Error = "テナントか IMSI のどちらか一方で絞ってください。"
	}
	if d.Error != "" {
		h.render(w, r, http.StatusBadRequest, "sessions", "セッション", d)
		return
	}
	a := userFrom(r.Context()).Actor()
	p := provisioner.SessionParams{Cursor: d.Cursor, Limit: sessionsPerPage}
	switch {
	case len(imsis) > 0:
		// IMSI で絞る。A・B は自分のテナントの加入者だけ（範囲の外は存在しないときと同じ 404）。
		if a.Role.TenantScoped() {
			if len(imsis) > 1 {
				d.Error = "IMSI は 1 件だけ指定してください。"
				h.render(w, r, http.StatusBadRequest, "sessions", "セッション", d)
				return
			}
			if _, ok := h.subscriberScope(w, r, imsis[0], authz.SessionView); !ok {
				return
			}
		}
		p.IMSIs = imsis
	case !f.All:
		p.TenantID = f.Tenant
	default:
		// C・D で条件がない: 読まずに選ばせる。
		h.render(w, r, http.StatusOK, "sessions", "セッション", d)
		return
	}
	l, err := h.pv.Sessions(r.Context(), p)
	if err != nil {
		h.log.Warn("list sessions", "error", err)
		fail := h.apiError(r, err, "")
		d.Error = fail.Message
		h.render(w, r, fail.Status, "sessions", "セッション", d)
		return
	}
	d.Queried, d.Next = true, l.NextCursor
	for _, s := range l.Items {
		// A・B には、自分のテナントの刻印のセッションだけを出す（IMSI で絞ったときも、他のテナントのネットワークからの接続は出さない）。
		if a.Role.TenantScoped() && s.TenantID != a.TenantID {
			continue
		}
		d.Rows = append(d.Rows, s)
	}
	h.render(w, r, http.StatusOK, "sessions", "セッション", d)
}
