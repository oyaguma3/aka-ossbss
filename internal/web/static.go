package web

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"strings"
)

// staticFiles は static/ のファイルを配信する。
// embed のファイルには更新時刻がないので、内容のハッシュを ETag にして再検証させる。
type staticFiles struct {
	files    http.Handler
	etags    map[string]string
	notFound http.Handler
}

func newStaticFiles(notFound http.Handler) (*staticFiles, error) {
	sub, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, err
	}
	etags := map[string]string{}
	err = fs.WalkDir(sub, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(sub, name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		etags[name] = `"` + hex.EncodeToString(sum[:16]) + `"`
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &staticFiles{files: http.FileServerFS(sub), etags: etags, notFound: notFound}, nil
}

func (s *staticFiles) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/static/")
	etag, ok := s.etags[name]
	if !ok {
		// ディレクトリの一覧は返さない。
		s.notFound.ServeHTTP(w, r)
		return
	}
	// http.FileServer は ETag ヘッダーがあれば If-None-Match を見て 304 を返す。
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/" + name
	s.files.ServeHTTP(w, r2)
}
