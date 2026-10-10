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

var funcs = template.FuncMap{}

func parsePages() (pages, error) {
	base, err := template.New("layout.html").Funcs(funcs).Option("missingkey=zero").ParseFS(assets, "templates/layout.html")
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
	Data      any
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
	var buf bytes.Buffer
	pd := pageData{Title: title, Version: h.version, Dev: h.ident.Dev(), Data: data}
	if !pd.Dev {
		pd.LogoutURL = accessLogoutURL
	}
	if u := userFrom(r.Context()); u.Email != "" {
		pd.User = &u
	}
	if err := t.ExecuteTemplate(&buf, "layout", pd); err != nil {
		h.log.ErrorContext(r.Context(), "render", "page", page, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

// errorData はエラーの画面に出す値。
type errorData struct {
	Status  int
	Message string
	// Relogin は、別のメールアドレスでログインし直すリンク（Access のログアウト）を出すか（登録のない利用者の 403）。
	Relogin bool
}

// renderError はエラーの画面を返す。
func (h *Handler) renderError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	h.render(w, r, status, "error", http.StatusText(status), errorData{Status: status, Message: msg})
}
