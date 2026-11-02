package web

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/oyaguma3/aka-ossbss/internal/audit"
	"github.com/oyaguma3/aka-ossbss/internal/authz"
	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
)

// 持ち込みの CA の画面（段階 4-3 の OB6・OB15）。見るのは C・D、登録と外すのは D だけ（受ける範囲が広がるため）。
// radsec-ca が正本で、CRL の状態は radsec-ca から読む。Meraki の API キーは radsec-ca に渡すだけで、DB・ログ・監査ログ・画面の再表示に残さない。

// 共通のブロックの照合の形の検査（radsec-edge の rsconf.CheckMatch と同じ。送る前に日本語で示す）。
var (
	sharedMatchAttrs   = []string{"CN", "SubjectAltName:URI", "SubjectAltName:DNS"}
	sharedPatternChars = regexp.MustCompile(`^[A-Za-z0-9^$.*+?()\[\]{}|\\:_@=-]+$`)
	sharedPatternEsc   = regexp.MustCompile(`\\[A-Za-z0-9]`)
)

const maxSharedPattern = 128

// checkSharedMatch は照合の形を確かめ、誤りの文を返す。
func checkSharedMatch(attr, pattern string) string {
	if !slices.Contains(sharedMatchAttrs, attr) {
		return "照合の属性は " + strings.Join(sharedMatchAttrs, "・") + " のどれかにしてください。"
	}
	if len(pattern) > maxSharedPattern || !sharedPatternChars.MatchString(pattern) {
		return fmt.Sprintf(`照合の正規表現は %d 文字までで、英数字と ^$.*+?()[]{}|\:_@=- だけにしてください。`, maxSharedPattern)
	}
	if !strings.HasPrefix(pattern, "^") || !strings.HasSuffix(pattern, "$") {
		return "照合の正規表現は ^ で始めて $ で終えてください（部分一致を防ぐため）。"
	}
	if strings.Contains(pattern, "(?") || sharedPatternEsc.MatchString(pattern) {
		return `照合の正規表現に (? と \d などの英数字のエスケープは使えません（POSIX の拡張正規表現で書いてください）。`
	}
	if _, err := regexp.Compile(pattern); err != nil {
		return "照合の正規表現が読めません。"
	}
	return ""
}

// broughtForm は持ち込みの CA のフォームの値と誤り。API キーは持たない（誤りで描き直しても画面に出さない）。
type broughtForm struct {
	Name, CertPEM, Source, URL, MerakiOrgID, CRLPEM, MatchAttr, MatchPattern string
	TenantOnly, NoCRL                                                        bool
	Errors                                                                   fieldErrors
}

// broughtRow は一覧の 1 行（DB の行と radsec-ca の状態）。
type broughtRow struct {
	db.BroughtCA
	CA provisioner.CA
	// Known は radsec-ca の状態を読めたか。
	Known bool
}

type broughtData struct {
	Tenant  db.TenantSummary
	Radsec  db.TenantRadsec
	Rows    []broughtRow
	Form    broughtForm
	IdemKey string
	// MatchAttrs は照合の属性の選択肢。Attr・Pattern・MatchError は共通のブロックの照合の形のフォーム。
	MatchAttrs                []string
	Attr, Pattern, MatchError string
	CanManage                 bool
	Message, Error            string
}

func (h *Handler) renderBrought(w http.ResponseWriter, r *http.Request, id int64, status int, mutate func(*broughtData)) {
	ctx, cancel := dbContext(r)
	defer cancel()
	t, err := h.db.TenantSummary(ctx, id)
	var rad db.TenantRadsec
	var list []db.BroughtCA
	if err == nil {
		rad, err = h.radsec.TenantRadsec(ctx, id)
	}
	if err == nil {
		list, err = h.radsec.BroughtCAs(ctx, id)
	}
	if errors.Is(err, db.ErrNotFound) {
		h.renderErrorLink(w, r, http.StatusNotFound, "このテナントは RadSec を有効にしていません。", fmt.Sprintf("/tenants/%d/radsec", id), "RadSec の画面へ")
		return
	}
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	d := broughtData{Tenant: t, Radsec: rad, IdemKey: newIdemKey(), MatchAttrs: sharedMatchAttrs, Attr: rad.MatchAttr, Pattern: rad.MatchPattern,
		Form: broughtForm{Source: provisioner.CRLSourceURL, Errors: fieldErrors{}}, CanManage: can(r, authz.BroughtCAManage)}
	cas, err := h.pv.TenantCAs(r.Context(), rad.Slug)
	if err != nil {
		d.Error = "radsec-ca から CRL の状態を読めません。" + h.radsecFailure(r, err)
	}
	for _, b := range list {
		row := broughtRow{BroughtCA: b}
		if i := slices.IndexFunc(cas, func(c provisioner.CA) bool { return c.ID == b.CAID }); i >= 0 {
			row.CA, row.Known = cas[i], true
		}
		d.Rows = append(d.Rows, row)
	}
	if mutate != nil {
		mutate(&d)
	}
	h.renderPart(w, r, status, "brought_cas", "brought-section", "持ち込みの CA "+t.Name, d)
}

func (h *Handler) broughtList(w http.ResponseWriter, r *http.Request) {
	id, ok := h.radsecScope(w, r, authz.RadsecConfig)
	if !ok {
		return
	}
	h.renderBrought(w, r, id, http.StatusOK, nil)
}

// readCRLSource はフォームの取得先を読む。API キーはここでだけ読み、呼び出しに渡す。
func readCRLSource(r *http.Request) provisioner.CRLSource {
	pf := r.PostForm
	s := provisioner.CRLSource{Source: pf.Get("source")}
	switch s.Source {
	case provisioner.CRLSourceURL:
		s.URL = strings.TrimSpace(pf.Get("url"))
	case provisioner.CRLSourceMeraki:
		s.MerakiOrgID, s.MerakiAPIKey = strings.TrimSpace(pf.Get("merakiOrgId")), strings.TrimSpace(pf.Get("merakiApiKey"))
	case provisioner.CRLSourceUpload:
		s.PEM = strings.TrimSpace(pf.Get("crlPem"))
	}
	return s
}

// checkCRLSource は取得先の必須の項目を確かめる。keyOptional は Meraki の API キーを省けるか（組織の ID だけを変えるとき）。
func checkCRLSource(s provisioner.CRLSource, errs fieldErrors, keyOptional bool) {
	switch s.Source {
	case provisioner.CRLSourceURL:
		errs.check(strings.HasPrefix(s.URL, "http://") || strings.HasPrefix(s.URL, "https://"), "url", "CRL の URL を http か https で入力してください。")
	case provisioner.CRLSourceMeraki:
		errs.check(s.MerakiOrgID != "", "merakiOrgId", "組織の ID を入力してください。")
		errs.check(s.MerakiAPIKey != "" || keyOptional, "merakiApiKey", "API キーを入力してください（顧客が発行した読み取り専用のキー）。")
	case provisioner.CRLSourceUpload:
		errs.check(strings.Contains(s.PEM, "X509 CRL"), "crlPem", "CRL を PEM で貼り付けてください。")
	case provisioner.CRLSourceNone:
	default:
		errs["source"] = "CRL の取得先を選んでください。"
	}
}

// parseCACert は PEM の CA の証明書を読む。
func parseCACert(v string) (*x509.Certificate, bool) {
	b, rest := pem.Decode([]byte(v))
	if b == nil || b.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, false
	}
	c, err := x509.ParseCertificate(b.Bytes)
	return c, err == nil
}

// sameSubjectCA はテナントの CA に同じ Subject のものがあれば、その ID を返す（束に入れると片方の正当な AP が拒否される。E7）。
func sameSubjectCA(cert *x509.Certificate, rad db.TenantRadsec, list []db.BroughtCA) string {
	same := func(id, certPEM string) string {
		if c, ok := parseCACert(certPEM); ok && bytes.Equal(c.RawSubject, cert.RawSubject) {
			return id
		}
		return ""
	}
	if id := same("tenant."+rad.Slug, rad.CACertificate); id != "" {
		return id
	}
	for _, b := range list {
		if b.Removing() {
			continue
		}
		if id := same(b.CAID, b.Certificate); id != "" {
			return id
		}
	}
	return ""
}

func (h *Handler) broughtCreate(w http.ResponseWriter, r *http.Request) {
	id, ok := h.radsecScope(w, r, authz.BroughtCAManage)
	if !ok {
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	pf := r.PostForm
	src := readCRLSource(r)
	f := broughtForm{Name: strings.TrimSpace(pf.Get("name")), CertPEM: strings.TrimSpace(pf.Get("certPem")), Source: src.Source, URL: src.URL, MerakiOrgID: src.MerakiOrgID, CRLPEM: src.PEM,
		MatchAttr: pf.Get("matchAttr"), MatchPattern: strings.TrimSpace(pf.Get("matchPattern")),
		TenantOnly: pf.Get("tenantOnly") == "1", NoCRL: pf.Get("noCrl") == "1", Errors: fieldErrors{}}
	key := formIdemKey(r, "idem")
	failed := func(status int, msg, key string) {
		h.renderBrought(w, r, id, status, func(d *broughtData) { d.Form, d.Error, d.IdemKey = f, msg, key })
	}
	f.Errors.check(slugPattern.MatchString(f.Name), "name", "名前は "+msgSlugForm)
	f.Errors.check(f.TenantOnly, "tenantOnly", "このテナント専用の CA であることを確かめて、印を付けてください。")
	checkCRLSource(src, f.Errors, false)
	cert, certOK := parseCACert(f.CertPEM)
	f.Errors.check(certOK, "certPem", "CA の証明書を PEM で 1 つ貼り付けてください。")

	ctx, cancel := dbContext(r)
	rad, err := h.radsec.TenantRadsec(ctx, id)
	var list []db.BroughtCA
	if err == nil {
		list, err = h.radsec.BroughtCAs(ctx, id)
	}
	cancel()
	if h.radsecDBError(w, r, err) {
		return
	}
	if certOK {
		if same := sameSubjectCA(cert, rad, list); same != "" {
			f.Errors["certPem"] = "同じ Subject の CA（" + same + "）があるので並べられません。片方の正当な AP が拒否されるためです。一度に切り替えてください。"
		}
	}
	matchAttr, matchPattern := "", ""
	switch {
	case src.Source == provisioner.CRLSourceNone:
		f.Errors.check(f.NoCRL, "noCrl", "CRL がなく業者の AP の失効が効かないことを了承して、印を付けてください。")
		if f.MatchPattern == "" && rad.MatchPattern == "" {
			f.Errors["matchPattern"] = "CRL のない CA には、業者の証明書の形（照合の形）が必須です。"
		}
		fallthrough
	case f.MatchPattern != "":
		if f.MatchPattern != "" {
			if msg := checkSharedMatch(f.MatchAttr, f.MatchPattern); msg != "" {
				f.Errors["matchPattern"] = msg
			}
			matchAttr, matchPattern = f.MatchAttr, f.MatchPattern
		}
	}
	if len(f.Errors) > 0 {
		failed(http.StatusBadRequest, "", key)
		return
	}
	ca, err := h.pv.CreateBroughtCA(r.Context(), provisioner.BroughtCACreate{Tenant: rad.Slug, Name: f.Name, CertPEM: f.CertPEM, CRL: src,
		TenantOnlyConfirmed: true, NoCRLAccepted: src.Source == provisioner.CRLSourceNone}, key)
	if err != nil {
		// radsec-ca が 4xx を覚えるので、入力を直して送り直すときは新しいキーにする（keepIdemKey。CA4）。
		failed(http.StatusBadGateway, h.radsecFailure(r, err), keepIdemKey(err, key))
		return
	}
	ctx, cancel = dbContext(r)
	err = h.radsec.AddBroughtCA(ctx, db.BroughtCA{CAID: ca.ID, Name: f.Name, TenantID: id, Certificate: f.CertPEM, Subject: ca.Subject, HasCRL: ca.HasCRL, CreatedBy: userFrom(r.Context()).Email}, matchAttr, matchPattern)
	cancel()
	if errors.Is(err, db.ErrConflict) {
		failed(http.StatusConflict, "この名前の持ち込みの CA は既にあります。", newIdemKey())
		return
	}
	if h.radsecDBError(w, r, err) {
		return
	}
	// 秘密の値は残さない（取得先の種類と組織の ID だけ）。
	h.record(r, audit.ActionBroughtCreate, ca.ID, id, map[string]any{"name": f.Name, "source": src.Source, "merakiOrgId": src.MerakiOrgID, "tenantOnlyConfirmed": true, "selfSigned": ca.SelfSigned})
	h.renderBrought(w, r, id, http.StatusOK, func(d *broughtData) {
		d.Message = "持ち込みの CA を登録しました。受けるようにするには、RadSec の画面で反映してください。"
	})
}

// broughtOf はテナントの持ち込みの CA を読む。なければ応えて false。
func (h *Handler) broughtOf(w http.ResponseWriter, r *http.Request, id int64) (db.BroughtCA, bool) {
	ctx, cancel := dbContext(r)
	list, err := h.radsec.BroughtCAs(ctx, id)
	cancel()
	if err != nil {
		h.dbUnavailable(w, r, err)
		return db.BroughtCA{}, false
	}
	caID := r.PathValue("ca")
	i := slices.IndexFunc(list, func(b db.BroughtCA) bool { return b.CAID == caID })
	if i < 0 {
		h.renderErrorLink(w, r, http.StatusNotFound, "持ち込みの CA が見つかりません。", fmt.Sprintf("/tenants/%d/brought-cas", id), "一覧へ")
		return db.BroughtCA{}, false
	}
	return list[i], true
}

// broughtSource は CRL の取得先を変える。none との間で替えると CRLCheck が変わるので、すぐ反映を求める（CA6）。
func (h *Handler) broughtSource(w http.ResponseWriter, r *http.Request) {
	id, ok := h.radsecScope(w, r, authz.BroughtCAManage)
	if !ok {
		return
	}
	b, ok := h.broughtOf(w, r, id)
	if !ok {
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	pf := r.PostForm
	src := readCRLSource(r)
	errs := fieldErrors{}
	checkCRLSource(src, errs, true)
	noCRL, attr, pattern := pf.Get("noCrl") == "1", pf.Get("matchAttr"), strings.TrimSpace(pf.Get("matchPattern"))
	ctx, cancel := dbContext(r)
	rad, err := h.radsec.TenantRadsec(ctx, id)
	cancel()
	if h.radsecDBError(w, r, err) {
		return
	}
	if src.Source == provisioner.CRLSourceNone {
		errs.check(noCRL, "noCrl", "CRL がなく業者の AP の失効が効かないことを了承して、印を付けてください。")
		errs.check(pattern != "" || rad.MatchPattern != "", "matchPattern", "CRL のない CA には、業者の証明書の形（照合の形）が必須です。")
	}
	if pattern != "" {
		if msg := checkSharedMatch(attr, pattern); msg != "" {
			errs["matchPattern"] = msg
		}
	} else {
		attr = ""
	}
	if len(errs) > 0 {
		msgs := make([]string, 0, len(errs))
		for _, m := range errs {
			msgs = append(msgs, m)
		}
		slices.Sort(msgs)
		h.renderBrought(w, r, id, http.StatusBadRequest, func(d *broughtData) { d.Error = strings.Join(msgs, " ") })
		return
	}
	ca, err := h.pv.ChangeCRLSource(r.Context(), b.CAID, provisioner.CRLSourceChange{CRL: src, NoCRLAccepted: src.Source == provisioner.CRLSourceNone}, newIdemKey())
	if err != nil {
		h.renderBrought(w, r, id, http.StatusBadGateway, func(d *broughtData) { d.Error = h.radsecFailure(r, err) })
		return
	}
	ctx, cancel = dbContext(r)
	err = h.radsec.SetBroughtHasCRL(ctx, id, b.CAID, ca.HasCRL, attr, pattern)
	cancel()
	if h.radsecDBError(w, r, err) {
		return
	}
	h.record(r, audit.ActionBroughtSource, b.CAID, id, map[string]any{"source": src.Source, "merakiOrgId": src.MerakiOrgID})
	h.renderBrought(w, r, id, http.StatusOK, func(d *broughtData) {
		d.Message = "CRL の取得先を変更しました。"
		if ca.HasCRL != b.HasCRL {
			d.Message += "CRL の確認の設定が変わるので、各エッジに順に反映します（接続がいったん切れます）。"
		}
	})
}

// broughtUpload は取得先が upload の CA に CRL を置く。
func (h *Handler) broughtUpload(w http.ResponseWriter, r *http.Request) {
	id, ok := h.radsecScope(w, r, authz.BroughtCAManage)
	if !ok {
		return
	}
	b, ok := h.broughtOf(w, r, id)
	if !ok {
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	crl := strings.TrimSpace(r.PostForm.Get("crlPem"))
	if !strings.Contains(crl, "X509 CRL") {
		h.renderBrought(w, r, id, http.StatusBadRequest, func(d *broughtData) { d.Error = "CRL を PEM で貼り付けてください。" })
		return
	}
	if _, err := h.pv.UploadCRL(r.Context(), b.CAID, crl, newIdemKey()); err != nil {
		h.renderBrought(w, r, id, http.StatusBadGateway, func(d *broughtData) { d.Error = h.radsecFailure(r, err) })
		return
	}
	h.record(r, audit.ActionBroughtUploadCRL, b.CAID, id, nil)
	h.renderBrought(w, r, id, http.StatusOK, func(d *broughtData) {
		d.Message = "CRL を置きました。エッジは次の取得で読みます（5 分以内）。"
	})
}

// broughtRefresh は CRL をすぐ取りに行く。
func (h *Handler) broughtRefresh(w http.ResponseWriter, r *http.Request) {
	id, ok := h.radsecScope(w, r, authz.BroughtCAManage)
	if !ok {
		return
	}
	b, ok := h.broughtOf(w, r, id)
	if !ok {
		return
	}
	if _, err := h.pv.RefreshCACRL(r.Context(), b.CAID, newIdemKey()); err != nil {
		h.renderBrought(w, r, id, http.StatusBadGateway, func(d *broughtData) { d.Error = h.radsecFailure(r, err) })
		return
	}
	h.record(r, audit.ActionBroughtRefreshCRL, b.CAID, id, nil)
	h.renderBrought(w, r, id, http.StatusOK, func(d *broughtData) {
		d.Message = "CRL を取り直しました。エッジは次の取得で読みます（5 分以内）。"
	})
}

// broughtRemove は持ち込みの CA を外す（OB6）。全配置が反映したら、ループが radsec-ca で止める。
func (h *Handler) broughtRemove(w http.ResponseWriter, r *http.Request) {
	id, ok := h.radsecScope(w, r, authz.BroughtCAManage)
	if !ok {
		return
	}
	b, ok := h.broughtOf(w, r, id)
	if !ok {
		return
	}
	ctx, cancel := dbContext(r)
	err := h.radsec.StartRemoveBrought(ctx, id, b.CAID)
	cancel()
	if h.radsecDBError(w, r, err) {
		return
	}
	h.record(r, audit.ActionBroughtRemove, b.CAID, id, nil)
	h.renderBrought(w, r, id, http.StatusOK, func(d *broughtData) {
		d.Message = "持ち込みの CA を外しました。各エッジに順に反映し、全部のエッジから外し終えたら radsec-ca で止めます。"
	})
}

// broughtMatch は共通のブロックの照合の形を変える。
func (h *Handler) broughtMatch(w http.ResponseWriter, r *http.Request) {
	id, ok := h.radsecScope(w, r, authz.BroughtCAManage)
	if !ok {
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	attr, pattern := r.PostForm.Get("matchAttr"), strings.TrimSpace(r.PostForm.Get("matchPattern"))
	if pattern == "" {
		attr = ""
	} else if msg := checkSharedMatch(attr, pattern); msg != "" {
		h.renderBrought(w, r, id, http.StatusBadRequest, func(d *broughtData) { d.Attr, d.Pattern, d.MatchError = attr, pattern, msg })
		return
	}
	ctx, cancel := dbContext(r)
	err := h.radsec.SetSharedMatch(ctx, id, attr, pattern)
	cancel()
	if errors.Is(err, db.ErrInUse) {
		h.renderBrought(w, r, id, http.StatusConflict, func(d *broughtData) {
			d.MatchError = "CRL のない持ち込みの CA があるので、照合の形は消せません。"
		})
		return
	}
	if h.radsecDBError(w, r, err) {
		return
	}
	h.record(r, audit.ActionRadsecMatch, formatID(id), id, map[string]any{"attr": attr, "pattern": pattern})
	h.renderBrought(w, r, id, http.StatusOK, func(d *broughtData) {
		d.Message = "照合の形を変更しました。効かせるには RadSec の画面で反映してください。"
	})
}
