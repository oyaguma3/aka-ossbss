package web

import (
	"bytes"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/oyaguma3/eapaka-ossbss-sample/internal/db"
)

// pages はページごとのテンプレート（layout.html と部品に、ページのファイルを重ねたもの）。
type pages map[string]*template.Template

var funcs = template.FuncMap{
	// asset は静的ファイルの URL（内容のハッシュつき）。
	"asset": assetURL,
	// issueLabel と issueHelp は provisioner の加入者の食い違い（issues）の名前と対処。
	"issueLabel": issueLabel,
	"issueHelp":  issueHelp,
	// keyStoreLabel は鍵の置き場所の名前。
	"keyStoreLabel": keyStoreLabel,
	// policyStatusLabel は加入者の状態（認可ポリシーの状態）の名前。
	"policyStatusLabel": policyStatusLabel,
	// octets は通信量を単位つきで表示する。datetime は日時を表示する。
	"octets":   formatOctets,
	"datetime": datetime,
	// deref は *bool の値（nil は false）。
	"deref": func(b *bool) bool { return b != nil && *b },
	// keysPlaceholder は、Ki / OPc をまだ表示していない状態の値を作る。secretPlaceholder は共有シークレットの同じもの。
	"keysPlaceholder":   func(imsi string) keysData { return keysData{IMSI: imsi} },
	"secretPlaceholder": func(id int64) secretData { return secretData{ID: id} },
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
	// list は引数を並べたスライスを作る。
	"list": func(v ...string) []string { return v },
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
	Data    any
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
	// OperationID は provisioner の操作の記録の ID（運用担当・システム管理者にだけ入る）。
	OperationID string
}

// renderError はエラーの画面を返す。
func (h *Handler) renderError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	h.renderErrorData(w, r, errorData{Status: status, Message: msg})
}

// renderErrorLink はエラーの画面に案内のリンクを付けて返す。
func (h *Handler) renderErrorLink(w http.ResponseWriter, r *http.Request, status int, msg, link, linkLabel string) {
	h.renderErrorData(w, r, errorData{Status: status, Message: msg, Link: link, LinkLabel: linkLabel})
}

// renderFailure は provisioner の呼び出しの失敗をエラーの画面で返す。
func (h *Handler) renderFailure(w http.ResponseWriter, r *http.Request, f apiFailure, link, linkLabel string) {
	h.renderErrorData(w, r, errorData{Status: f.Status, Message: f.Message, Link: link, LinkLabel: linkLabel, OperationID: f.OperationID})
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

// forbidden は、範囲の中のものに対して役割にない操作をしたときの 403 を返す（画面仕様 §1.2）。
func (h *Handler) forbidden(w http.ResponseWriter, r *http.Request) {
	h.renderError(w, r, http.StatusForbidden, "この操作の権限がありません。")
}
