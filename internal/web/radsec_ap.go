package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/oyaguma3/aka-ossbss/internal/audit"
	"github.com/oyaguma3/aka-ossbss/internal/authz"
	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
)

// AP とグループの画面（段階 4-3 の OB5・OB8・OB14）。発行は同期で、Idempotency-Key をフォームに持たせる（送り直しても証明書は増えない）。
// 台帳に足すと版が上がるが、反映は人が求める（OB1）。

// certExpiryWarn は証明書の期限の警告を出す残り日数（概要 §9.3）。
const certExpiryWarn = 30 * 24 * time.Hour

// radsecCSRPattern は CSR の入力の目印。形は radsec-ca が確かめる。
const pemCSRMarker = "CERTIFICATE REQUEST"

// purposeNotes は用途ごとの設定の短い注意（概要 §9.5）。手順書の本文は別に用意する。
var purposeNotes = map[string]string{
	"box":     "Box は 1 台ごとにグループを 1 つ登録します。AP と Box の間の RADIUS はインターネットを通さないでください。",
	"unifi":   "UniFi Network 8.4 以上の RADIUS のプロファイルに、証明書・鍵・サーバーCA を PEM で入れます。",
	"extreme": "ExtremeCloud IQ の RADIUS サーバーの定義（Trust Point）に、証明書と鍵を入れます。",
	"aruba":   "AP グループで 1 組の証明書になり、ほかの RadSec の宛先と共用になります。差し替えが効くか受入試験で確かめてください。",
	"mist":    "組織の全 AP が同じ証明書になります。",
	"other":   "2083 以外のポートを設定できるかを最初に確かめてください。",
}

// PurposeOption は用途の選択肢。
type PurposeOption struct{ Value, Label string }

var purposeOptions = []PurposeOption{{"unifi", "UniFi"}, {"extreme", "Extreme"}, {"aruba", "Aruba"}, {"mist", "Mist"}, {"box", "Box"}, {"other", "その他"}}

// apRow は AP の一覧の 1 行。
type apRow struct {
	db.AP
	// Latest は失効していない一番新しい証明書。ValidCount は失効していない証明書の数。Expiring は期限が近いか。
	Latest     db.APCert
	ValidCount int
	Expiring   bool
}

func apRowOf(a db.AP, now time.Time) apRow {
	row := apRow{AP: a}
	for _, c := range a.Certs {
		if c.Valid() {
			if row.ValidCount == 0 {
				row.Latest = c
			}
			row.ValidCount++
		}
	}
	row.Expiring = row.ValidCount > 0 && row.Latest.NotAfter.Before(now.Add(certExpiryWarn))
	return row
}

type apsData struct {
	Tenant  db.TenantSummary
	Enabled bool
	Radsec  db.TenantRadsec
	Rows    []apRow
	// フォーム（登録）。
	Name, Kind, Purpose, CSR string
	Errors                   fieldErrors
	IdemKey                  string
	Purposes                 []PurposeOption
	NoCRL, CanManage         bool
	Message, Error           string
}

func (h *Handler) renderAPs(w http.ResponseWriter, r *http.Request, id int64, status int, mutate func(*apsData)) {
	ctx, cancel := dbContext(r)
	defer cancel()
	t, err := h.db.TenantSummary(ctx, id)
	switch {
	case errors.Is(err, db.ErrNotFound):
		h.renderError(w, r, http.StatusNotFound, msgTenantNotFound)
		return
	case err != nil:
		h.dbUnavailable(w, r, err)
		return
	}
	a := userFrom(r.Context()).Actor()
	d := apsData{Tenant: t, Kind: db.KindAP, Errors: fieldErrors{}, IdemKey: newIdemKey(), Purposes: purposeOptions, CanManage: authz.Allowed(a, authz.RadsecAPManage, id)}
	rad, err := h.radsec.TenantRadsec(ctx, id)
	switch {
	case errors.Is(err, db.ErrNotFound):
	case err != nil:
		h.dbUnavailable(w, r, err)
		return
	default:
		d.Enabled, d.Radsec = true, rad
		list, err := h.radsec.APs(ctx, id)
		if err != nil {
			h.dbUnavailable(w, r, err)
			return
		}
		now := time.Now()
		for _, ap := range list {
			d.Rows = append(d.Rows, apRowOf(ap, now))
		}
		brought, err := h.radsec.BroughtCAs(ctx, id)
		if err != nil {
			h.dbUnavailable(w, r, err)
			return
		}
		d.NoCRL = slices.ContainsFunc(brought, func(b db.BroughtCA) bool { return !b.Removing() && !b.HasCRL })
	}
	if mutate != nil {
		mutate(&d)
	}
	h.renderPart(w, r, status, "aps", "aps-section", "AP・グループ "+t.Name, d)
}

func (h *Handler) apList(w http.ResponseWriter, r *http.Request) {
	id, ok := h.radsecScope(w, r, authz.RadsecView)
	if !ok {
		return
	}
	h.renderAPs(w, r, id, http.StatusOK, nil)
}

// issueAPCert はプロファイルを選んで AP かグループの証明書を発行する。
func (h *Handler) issueAPCert(r *http.Request, slug, name, kind, csr, key string) (provisioner.Cert, error) {
	i := provisioner.CertIssue{Profile: provisioner.ProfileAPClient, Tenant: slug, AP: name, CSR: csr}
	if kind == db.KindGroup {
		i = provisioner.CertIssue{Profile: provisioner.ProfileGroupClient, Tenant: slug, Group: name, CSR: csr}
	}
	return h.pv.IssueCert(r.Context(), i, key)
}

// apNameError は台帳の誤りの文を返す。
func apNameError(err error) string {
	switch {
	case errors.Is(err, db.ErrConflict):
		return "この名前の AP かグループが、このテナントに既にあります。"
	case errors.Is(err, db.ErrNASIDUsed):
		return "NAS-Identifier（<slug>-<名前>）が別のテナントのものと重なるので、別の名前にしてください。"
	case errors.Is(err, db.ErrNameRetired):
		return "CRL のないテナントでは、削除した名前を使い回せません（失効させた古い証明書がまたつながってしまうため）。別の名前にしてください。"
	case errors.Is(err, db.ErrAPLimit):
		return fmt.Sprintf("AP とグループは 1 つのテナントに %d 件までです。", db.MaxAPs)
	}
	return ""
}

func (h *Handler) apCreate(w http.ResponseWriter, r *http.Request) {
	id, ok := h.radsecScope(w, r, authz.RadsecAPManage)
	if !ok {
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	pf := r.PostForm
	name, kind, purpose, csr := strings.TrimSpace(pf.Get("name")), pf.Get("kind"), pf.Get("purpose"), strings.TrimSpace(pf.Get("csr"))
	errs := fieldErrors{}
	errs.check(slugPattern.MatchString(name), "name", "名前は "+msgSlugForm)
	errs.check(name != "probe", "name", "probe はヘルスチェックに予約しているので使えません。")
	errs.check(kind == db.KindAP || kind == db.KindGroup, "kind", "種類を選んでください。")
	if kind == db.KindGroup {
		errs.check(purposeNotes[purpose] != "", "purpose", "用途を選んでください。")
	} else {
		purpose = ""
	}
	errs.check(strings.Contains(csr, pemCSRMarker), "csr", "CSR を PEM で貼り付けてください。")
	key := formIdemKey(r, "idem")
	failed := func(status int, msg string, key string) {
		h.renderAPs(w, r, id, status, func(d *apsData) {
			d.Name, d.Kind, d.Purpose, d.CSR, d.Errors, d.Error = name, kind, purpose, csr, errs, msg
			d.IdemKey = key
		})
	}
	if len(errs) > 0 {
		failed(http.StatusBadRequest, "", key)
		return
	}
	ctx, cancel := dbContext(r)
	rad, err := h.radsec.TenantRadsec(ctx, id)
	nasID := rad.Slug + "-" + name
	if err == nil {
		err = h.radsec.CheckAPName(ctx, id, name, nasID)
	}
	cancel()
	if msg := apNameError(err); msg != "" {
		errs["name"] = msg
		failed(http.StatusConflict, "", key)
		return
	}
	if h.radsecDBError(w, r, err) {
		return
	}
	cert, err := h.issueAPCert(r, rad.Slug, name, kind, csr, key)
	if err != nil {
		failed(http.StatusBadGateway, h.radsecFailure(r, err), keepIdemKey(err, key))
		return
	}
	by := userFrom(r.Context()).Email
	ctx, cancel = dbContext(r)
	ap, err := h.radsec.CreateAP(ctx, db.AP{TenantID: id, Name: name, Kind: kind, Purpose: purpose, NASID: nasID, CreatedBy: by}, db.APCert{Serial: cert.Serial, NotAfter: cert.NotAfter, IssuedBy: by})
	cancel()
	if msg := apNameError(err); msg != "" {
		errs["name"] = msg
		failed(http.StatusConflict, "", key)
		return
	}
	if h.radsecDBError(w, r, err) {
		return
	}
	h.record(r, audit.ActionAPCreate, nasID, id, map[string]any{"name": name, "kind": kind, "purpose": purpose, "serial": cert.Serial})
	seeOther(w, r, fmt.Sprintf("/tenants/%d/aps/%d?created=1", id, ap.ID))
}

type apData struct {
	Tenant db.TenantSummary
	Radsec db.TenantRadsec
	AP     apRow
	// Domain は DNS のドメイン、SlotNames は AP に設定する名前。
	Domain      string
	SlotNames   []string
	PurposeNote string
	CSR         string
	CSRError    string
	IdemKey     string
	NoCRL       bool
	CanManage   bool
	Message     string
	Error       string
}

func (h *Handler) renderAP(w http.ResponseWriter, r *http.Request, id int64, status int, mutate func(*apData)) {
	apID, err := parseID(r.PathValue("ap"))
	if err != nil {
		h.notFound(w, r)
		return
	}
	ctx, cancel := dbContext(r)
	defer cancel()
	t, err := h.db.TenantSummary(ctx, id)
	var rad db.TenantRadsec
	var ap db.AP
	if err == nil {
		rad, err = h.radsec.TenantRadsec(ctx, id)
	}
	if err == nil {
		ap, err = h.radsec.APByID(ctx, id, apID)
	}
	var brought []db.BroughtCA
	var ps []db.Placement
	if err == nil {
		brought, err = h.radsec.BroughtCAs(ctx, id)
	}
	if err == nil {
		ps, err = h.radsec.Placements(ctx, id)
	}
	if errors.Is(err, db.ErrNotFound) {
		h.renderErrorLink(w, r, http.StatusNotFound, "AP・グループが見つかりません。", fmt.Sprintf("/tenants/%d/aps", id), "一覧へ")
		return
	}
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	a := userFrom(r.Context()).Actor()
	d := apData{Tenant: t, Radsec: rad, AP: apRowOf(ap, time.Now()), PurposeNote: purposeNotes[ap.Purpose], IdemKey: newIdemKey(),
		NoCRL:     slices.ContainsFunc(brought, func(b db.BroughtCA) bool { return !b.Removing() && !b.HasCRL }),
		CanManage: authz.Allowed(a, authz.RadsecAPManage, id)}
	if r.URL.Query().Get("created") == "1" {
		d.Message = "登録して証明書を発行しました。つながるようにするには、RadSec の画面で反映してください。"
	}
	if st, ok := h.radsecStatus(r); ok {
		d.Domain = dnsDomain(st)
	}
	if d.Domain != "" {
		seen := map[string]bool{}
		for _, s := range db.RadsecSlots() {
			for _, p := range ps {
				if p.Slot == s && !p.Removing() && !seen[s] {
					seen[s] = true
					d.SlotNames = append(d.SlotNames, slotDNSName(s, rad.Slug, d.Domain))
				}
			}
		}
	}
	if mutate != nil {
		mutate(&d)
	}
	h.renderPart(w, r, status, "ap", "ap-detail", ap.Name, d)
}

func (h *Handler) ap(w http.ResponseWriter, r *http.Request) {
	id, ok := h.radsecScope(w, r, authz.RadsecView)
	if !ok {
		return
	}
	h.renderAP(w, r, id, http.StatusOK, nil)
}

// apOf はテナントの AP を読む（操作の共通の手順）。なければ応えて false。
func (h *Handler) apOf(w http.ResponseWriter, r *http.Request, id int64) (db.AP, db.TenantRadsec, bool) {
	apID, err := parseID(r.PathValue("ap"))
	if err != nil {
		h.notFound(w, r)
		return db.AP{}, db.TenantRadsec{}, false
	}
	ctx, cancel := dbContext(r)
	defer cancel()
	rad, err := h.radsec.TenantRadsec(ctx, id)
	var ap db.AP
	if err == nil {
		ap, err = h.radsec.APByID(ctx, id, apID)
	}
	if errors.Is(err, db.ErrNotFound) {
		h.renderErrorLink(w, r, http.StatusNotFound, "AP・グループが見つかりません。", fmt.Sprintf("/tenants/%d/aps", id), "一覧へ")
		return db.AP{}, db.TenantRadsec{}, false
	}
	if err != nil {
		h.dbUnavailable(w, r, err)
		return db.AP{}, db.TenantRadsec{}, false
	}
	return ap, rad, true
}

// apRenew は同じ名前で証明書を発行し直す（新旧が両方有効な間はどちらでもつながる。古い証明書の失効は、顧客が入れ替えた後で人が押す）。
func (h *Handler) apRenew(w http.ResponseWriter, r *http.Request) {
	id, ok := h.radsecScope(w, r, authz.RadsecAPManage)
	if !ok {
		return
	}
	ap, rad, ok := h.apOf(w, r, id)
	if !ok {
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	csr, key := strings.TrimSpace(r.PostForm.Get("csr")), formIdemKey(r, "idem")
	if !strings.Contains(csr, pemCSRMarker) {
		h.renderAP(w, r, id, http.StatusBadRequest, func(d *apData) {
			d.CSR, d.CSRError, d.IdemKey = csr, "CSR を PEM で貼り付けてください。", key
		})
		return
	}
	cert, err := h.issueAPCert(r, rad.Slug, ap.Name, ap.Kind, csr, key)
	if err != nil {
		h.renderAP(w, r, id, http.StatusBadGateway, func(d *apData) { d.CSR, d.Error, d.IdemKey = csr, h.radsecFailure(r, err), keepIdemKey(err, key) })
		return
	}
	ctx, cancel := dbContext(r)
	err = h.radsec.AddAPCert(ctx, id, ap.ID, db.APCert{Serial: cert.Serial, NotAfter: cert.NotAfter, IssuedBy: userFrom(r.Context()).Email})
	cancel()
	if h.radsecDBError(w, r, err) {
		return
	}
	h.record(r, audit.ActionAPRenew, ap.NASID, id, map[string]any{"serial": cert.Serial})
	h.renderAP(w, r, id, http.StatusOK, func(d *apData) {
		d.Message = "新しい証明書を発行しました。AP に入れ替えた後で、古い証明書を失効させてください。"
	})
}

// revokeAndMark は証明書を失効させて台帳に記録する。radsec-ca が失効済みなら、そのまま成功する。
func (h *Handler) revokeAndMark(r *http.Request, id int64, ap db.AP, serial, reason string) error {
	if _, err := h.pv.RevokeCert(r.Context(), serial, reason, "revoke-"+serial+"-"+reason); err != nil {
		return err
	}
	ctx, cancel := dbContext(r)
	defer cancel()
	return h.radsec.MarkAPCertRevoked(ctx, id, ap.ID, serial, reason)
}

// apRevoke は 1 枚の証明書を失効させる。理由は入れ替えの後（superseded）と鍵の漏えい（keyCompromise）。
func (h *Handler) apRevoke(w http.ResponseWriter, r *http.Request) {
	id, ok := h.radsecScope(w, r, authz.RadsecAPManage)
	if !ok {
		return
	}
	ap, _, ok := h.apOf(w, r, id)
	if !ok {
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	serial, reason := r.PathValue("serial"), r.PostForm.Get("reason")
	if reason != provisioner.RevokeSuperseded && reason != provisioner.RevokeKeyCompromise {
		h.renderError(w, r, http.StatusBadRequest, "失効の理由が正しくありません。")
		return
	}
	if !slices.ContainsFunc(ap.Certs, func(c db.APCert) bool { return c.Serial == serial && c.Valid() }) {
		h.renderAP(w, r, id, http.StatusConflict, func(d *apData) {
			d.Error = "この証明書は既に失効させたか、この AP のものではありません。"
		})
		return
	}
	if err := h.revokeAndMark(r, id, ap, serial, reason); err != nil {
		if provisioner.IsUnavailable(err) || provisioner.StatusOf(err) > 0 {
			h.renderAP(w, r, id, http.StatusBadGateway, func(d *apData) { d.Error = h.radsecFailure(r, err) })
			return
		}
		if h.radsecDBError(w, r, err) {
			return
		}
	}
	h.record(r, audit.ActionAPRevoke, ap.NASID, id, map[string]any{"serial": serial, "reason": reason})
	h.renderAP(w, r, id, http.StatusOK, func(d *apData) {
		d.Message = "証明書を失効させました。各エッジに CRL の取り直しを送ります。"
	})
}

// apDelete は AP を削除する。失効させてから台帳から消す（途中で失敗しても、もう一度押せば続きから進む）。
func (h *Handler) apDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := h.radsecScope(w, r, authz.RadsecAPManage)
	if !ok {
		return
	}
	ap, _, ok := h.apOf(w, r, id)
	if !ok {
		return
	}
	for _, c := range ap.Certs {
		if !c.Valid() {
			continue
		}
		if err := h.revokeAndMark(r, id, ap, c.Serial, provisioner.RevokeCessationOfOperation); err != nil {
			if provisioner.IsUnavailable(err) || provisioner.StatusOf(err) > 0 {
				h.renderAP(w, r, id, http.StatusBadGateway, func(d *apData) {
					d.Error = h.radsecFailure(r, err) + " 証明書を失効できなかったので、削除していません。"
				})
				return
			}
			if h.radsecDBError(w, r, err) {
				return
			}
		}
	}
	ctx, cancel := dbContext(r)
	requested, err := h.radsec.DeleteAP(ctx, id, ap.ID)
	cancel()
	if h.radsecDBError(w, r, err) {
		return
	}
	h.record(r, audit.ActionAPDelete, ap.NASID, id, map[string]any{"name": ap.Name, "kind": ap.Kind, "applyRequested": requested})
	h.renderAPs(w, r, id, http.StatusOK, func(d *apsData) {
		d.Message = ap.Name + " を削除しました（証明書は失効させました）。"
		if requested {
			d.Message += "CRL のないテナントなので、すぐ各エッジに反映します。"
		}
	})
}

// pemResponse は PEM をファイルとして返す。
func pemResponse(w http.ResponseWriter, name, data string) {
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Write([]byte(data))
}

// apCertPEM は AP の証明書を取り出す（本体は radsec-ca から読む）。
func (h *Handler) apCertPEM(w http.ResponseWriter, r *http.Request) {
	id, ok := h.radsecScope(w, r, authz.RadsecAPManage)
	if !ok {
		return
	}
	ap, _, ok := h.apOf(w, r, id)
	if !ok {
		return
	}
	serial := r.PathValue("serial")
	if !slices.ContainsFunc(ap.Certs, func(c db.APCert) bool { return c.Serial == serial }) {
		h.notFound(w, r)
		return
	}
	cert, err := h.pv.GetCert(r.Context(), serial)
	if err != nil {
		h.renderAP(w, r, id, http.StatusBadGateway, func(d *apData) { d.Error = h.radsecFailure(r, err) })
		return
	}
	pemResponse(w, ap.NASID+".pem", cert.Certificate)
}

// radsecServerCA はサーバーCA の証明書を取り出す（AP に入れる）。
func (h *Handler) radsecServerCA(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.radsecScope(w, r, authz.RadsecAPManage); !ok {
		return
	}
	ca, err := h.pv.GetCA(r.Context(), "server")
	if err != nil {
		h.renderError(w, r, http.StatusBadGateway, h.radsecFailure(r, err))
		return
	}
	pemResponse(w, "radsec-server-ca.pem", ca.Certificate)
}

// seenRow は接続の状態の 1 行。
type seenRow struct {
	Slot string
	Name string
	provisioner.EdgeSeenAP
}

// sharedRow は共通のブロックの接続の 1 行。
type sharedRow struct {
	Slot string
	provisioner.EdgeSeenShared
}

type apSeenData struct {
	Rows   []seenRow
	Shared []sharedRow
	// Unreachable は読めなかったスロット。
	Unreachable []string
}

// apSeen は配置した各エッジから接続の状態を読む（遅れて読み込む部品。OB14）。エッジの ID は見せず、スロットで出す。
func (h *Handler) apSeen(w http.ResponseWriter, r *http.Request) {
	id, ok := h.radsecScope(w, r, authz.RadsecView)
	if !ok {
		return
	}
	ctx, cancel := dbContext(r)
	ps, err := h.radsec.Placements(ctx, id)
	cancel()
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	d := apSeenData{}
	for _, p := range ps {
		if p.Removing() {
			continue
		}
		wg.Go(func() {
			// 届かないエッジを長く待たない（画面の部品なので、読めなければ読めなかったと出す）。
			ctx, cancel := context.WithTimeout(r.Context(), radsecStatusTimeout)
			defer cancel()
			seen, err := h.pv.EdgeTenantLastSeen(ctx, p.EdgeID, id)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				// まだ置いていないエッジ（反映待ち）は届かないのと同じ扱いにする。
				d.Unreachable = append(d.Unreachable, p.Slot)
				return
			}
			for _, a := range seen.APs {
				d.Rows = append(d.Rows, seenRow{Slot: p.Slot, Name: a.Name, EdgeSeenAP: a})
			}
			for _, s := range seen.Shared {
				d.Shared = append(d.Shared, sharedRow{Slot: p.Slot, EdgeSeenShared: s})
			}
		})
	}
	wg.Wait()
	slices.SortFunc(d.Rows, func(a, b seenRow) int { return strings.Compare(a.Name+"\x00"+a.Slot, b.Name+"\x00"+b.Slot) })
	slices.SortFunc(d.Shared, func(a, b sharedRow) int { return strings.Compare(a.IP+"\x00"+a.Slot, b.IP+"\x00"+b.Slot) })
	slices.Sort(d.Unreachable)
	h.renderBlock(w, r, http.StatusOK, "aps", "ap-seen", d)
}
