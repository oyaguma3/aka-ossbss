package web

import (
	"bytes"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/oyaguma3/aka-ossbss/internal/authz"
	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
)

// pages はページごとのテンプレート（layout.html と部品に、ページのファイルを重ねたもの）。
type pages map[string]*template.Template

var funcs = template.FuncMap{
	// asset は静的ファイルの URL（内容のハッシュつき）。
	"asset": assetURL,
	// stateLabel は加入者の状態の名前、sideLabel は側の名前、downstreamLabel は下流の名前。
	"stateLabel":      stateLabel,
	"sideLabel":       sideLabel,
	"downstreamLabel": downstreamLabel,
	// octets は通信量を単位つきで表示する。datetime は日時を表示する。
	"octets":   formatOctets,
	"datetime": datetime,
	// dict は「名前, 値, …」から部品に渡すマップを作る。
	"dict": func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			if k, ok := kv[i].(string); ok {
				m[k] = kv[i+1]
			}
		}
		return m
	},
	// add1 は 0 からの番号を 1 からの番号にする。
	"add1": func(i int) int { return i + 1 },
	// actionLabel と detailText は監査ログの操作の名前と内容。
	"actionLabel": actionLabel,
	"detailText":  detailText,
	// list は引数を並べたスライスを作る。
	"list": func(v ...string) []string { return v },
	// hasID は ID の並びに id があるか（許可クライアントの選択）。
	"hasID": slices.Contains[[]int64],
	// joinIDs と joinStrings は並びを「, 」でつなぐ。
	"joinIDs": func(ids []int64) string {
		var out []string
		for _, id := range ids {
			out = append(out, strconv.FormatInt(id, 10))
		}
		return strings.Join(out, ", ")
	},
	"joinStrings": func(v []string) string { return strings.Join(v, ", ") },
	// jobKind・jobState・jobRow はジョブの種類・状態・行の結果の名前、percent は進み具合の率。
	"jobKind":  jobKindLabel,
	"jobState": jobStateLabel,
	"jobRow":   jobRowLabel,
	"percent":  jobPercent,
	// 突き合わせの名前。
	"findingKind":    findingKindLabel,
	"findingStatus":  findingStatusLabel,
	"runState":       runStateLabel,
	"runTrigger":     runTriggerLabel,
	"scheduleName":   scheduleNameLabel,
	"scheduleStatus": scheduleStatusLabel,
	// node は下流の /status の主な項目（読めなければ nil）。
	"node": func(d provisioner.DownstreamStatus) *provisioner.NodeStatus {
		if n, ok := d.Node(); ok {
			return &n
		}
		return nil
	},
}

func parsePages() (pages, error) {
	base, err := template.New("layout.html").Funcs(funcs).Option("missingkey=zero").ParseFS(assets, "templates/layout.html", "templates/partials/*.html")
	if err != nil {
		return nil, err
	}
	files, err := fs.Glob(assets, "templates/pages/*.html")
	if err != nil {
		return nil, err
	}
	p := pages{}
	for _, file := range files {
		t, err := template.Must(base.Clone()).ParseFS(assets, file)
		if err != nil {
			return nil, err
		}
		p[strings.TrimSuffix(path.Base(file), ".html")] = t
	}
	return p, nil
}

// pageData はレイアウトに渡す値。
type pageData struct {
	Title   string
	Version string
	// User は認証した利用者（401・403 などの画面では nil）。
	User *db.Account
	// Dev は開発用の設定で動いているか。
	Dev bool
	// LogoutURL はログアウトのリンク（Cloudflare Access のログアウト。開発用の設定では空）。
	LogoutURL string
	// Section はナビゲーションで強調する項目（パスの最初の要素。例 subscribers）。
	Section string
	// Nav は利用者の役割で出すナビゲーションの項目。
	Nav  []navItem
	Data any
}

// navItem はナビゲーションの 1 項目。
type navItem struct {
	Label, Href string
	// Sections は、この項目を強調するパスの最初の要素。
	Sections []string
}

// Active は、section の画面でこの項目を強調するか。
func (n navItem) Active(section string) bool { return slices.Contains(n.Sections, section) }

// navItems は利用者の役割で出すナビゲーションの項目を返す。
func navItems(a authz.Actor) []navItem {
	items := []navItem{
		{"ダッシュボード", "/", []string{""}},
		{"加入者", "/subscribers", []string{"subscribers"}},
		{"セッション", "/sessions", []string{"sessions"}},
	}
	target := a.TenantID // A・B は自分のテナントのものとして判定する
	if authz.Allowed(a, authz.JobView, target) {
		items = append(items, navItem{"一括投入", "/jobs", []string{"jobs"}})
	}
	if authz.Allowed(a, authz.ReconcileView, target) {
		items = append(items, navItem{"突き合わせ", "/reconcile", []string{"reconcile"}})
	}
	if authz.Allowed(a, authz.TenantManage, target) {
		items = append(items, navItem{"テナント", "/tenants", []string{"tenants"}})
	}
	if authz.Allowed(a, authz.AccountManage, target) {
		items = append(items, navItem{"アカウント", "/accounts", []string{"accounts"}})
	}
	if authz.Allowed(a, authz.AuditView, target) {
		items = append(items, navItem{"監査ログ", "/audit", []string{"audit"}})
	}
	return items
}

// accessLogoutURL は Cloudflare Access のログアウト（公開ホスト名のパス。Access が処理する）。
const accessLogoutURL = "/cdn-cgi/access/logout"

// render はページを描いて返す。描くのに失敗したら 500 を返す（途中まで書き出さないよう、先にバッファーに描く）。
func (h *Handler) render(w http.ResponseWriter, r *http.Request, status int, page, title string, data any) {
	t, ok := h.pages[page]
	if !ok {
		h.log.Error("unknown page", "page", page)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	h.execute(w, r, status, t, "layout", h.pageData(r, title, data))
}

// pageData はレイアウトに渡す値を作る。
func (h *Handler) pageData(r *http.Request, title string, data any) pageData {
	pd := pageData{Title: title, Version: h.version, Dev: h.ident.Dev(), Data: data}
	if !pd.Dev {
		pd.LogoutURL = accessLogoutURL
	}
	if u := userFrom(r.Context()); u.Email != "" {
		pd.User = &u
		pd.Nav = navItems(u.Actor())
	}
	pd.Section, _, _ = strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	return pd
}

// renderBlock はページの中の 1 つのテンプレート（htmx で差し替える部分）だけを描いて返す。
func (h *Handler) renderBlock(w http.ResponseWriter, r *http.Request, status int, page, block string, data any) {
	t, ok := h.pages[page]
	if !ok {
		h.log.Error("unknown page", "page", page)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	h.execute(w, r, status, t, block, data)
}

// renderPart は、htmx の要求なら block だけを、そうでなければページ全体を返す。
func (h *Handler) renderPart(w http.ResponseWriter, r *http.Request, status int, page, block, title string, data any) {
	if isHTMX(r) {
		h.renderBlock(w, r, status, page, block, data)
		return
	}
	h.render(w, r, status, page, title, data)
}

// isHTMX は htmx が送った要求か。
func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// execute はテンプレートを描いて返す。描くのに失敗したら 500 を返す（途中まで書き出さないよう、先にバッファーに描く）。
func (h *Handler) execute(w http.ResponseWriter, r *http.Request, status int, t *template.Template, name string, data any) {
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		h.log.ErrorContext(r.Context(), "render", "template", name, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

// seeOther は別の画面へ移動させる（htmx の要求では HX-Redirect で移動させる）。
func seeOther(w http.ResponseWriter, r *http.Request, to string) {
	if isHTMX(r) {
		w.Header().Set("HX-Redirect", to)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// errorData はエラーの画面に出す値。
type errorData struct {
	Status  int
	Message string
	// Relogin は、別のメールアドレスでログインし直すリンク（Access のログアウト）を出すか（登録のない利用者の 403）。
	Relogin bool
	// Link と LinkLabel は案内のリンク（一覧へ戻るなど。空なら出さない）。
	Link, LinkLabel string
}

// renderError はエラーの画面を返す。
func (h *Handler) renderError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	h.renderErrorData(w, r, errorData{Status: status, Message: msg})
}

// renderErrorLink はエラーの画面に案内のリンクを付けて返す。
func (h *Handler) renderErrorLink(w http.ResponseWriter, r *http.Request, status int, msg, link, linkLabel string) {
	h.renderErrorData(w, r, errorData{Status: status, Message: msg, Link: link, LinkLabel: linkLabel})
}

// renderFailure は aka-provisioner の呼び出しの失敗をエラーの画面で返す。
func (h *Handler) renderFailure(w http.ResponseWriter, r *http.Request, f apiFailure, link, linkLabel string) {
	h.renderErrorData(w, r, errorData{Status: f.Status, Message: f.Message, Link: link, LinkLabel: linkLabel})
}

// renderErrorData はエラーの画面を返す。htmx の要求では、差し替え先によらず本文全体をエラーの表示に置き換える。
func (h *Handler) renderErrorData(w http.ResponseWriter, r *http.Request, data errorData) {
	title := http.StatusText(data.Status)
	if isHTMX(r) {
		w.Header().Set("HX-Retarget", "main")
		w.Header().Set("HX-Reswap", "innerHTML")
		h.renderBlock(w, r, data.Status, "error", "content", h.pageData(r, title, data))
		return
	}
	h.render(w, r, data.Status, "error", title, data)
}

// forbidden は、範囲の中のものに対して役割にない操作をしたときの 403 を返す（もとの画面仕様 §1.2）。
func (h *Handler) forbidden(w http.ResponseWriter, r *http.Request) {
	h.renderError(w, r, http.StatusForbidden, "この操作の権限がありません。")
}
