package web

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/oyaguma3/aka-ossbss/internal/audit"
	"github.com/oyaguma3/aka-ossbss/internal/authz"
	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
)

// RadSec の受け口の画面（設計概要 §13。段階 4-3 の OB11〜OB13、OB21）。画面は DB の望む状態を書いて版を上げるだけで、エッジには触らない（OB3）。
// エッジへ送るのは中央の整合のループ（internal/radsec）の役目。例外は読み出し（ログ、最後の接続）と再起動。radsec-ca は発行の結果をすぐ渡すので同期で呼ぶ。

// RadsecStore は RadSec の画面が使う DB の操作。*db.DB が満たす。
type RadsecStore interface {
	Edges(ctx context.Context) ([]db.Edge, error)
	EdgeByID(ctx context.Context, id string) (db.Edge, error)
	CreateEdge(ctx context.Context, e db.Edge, by string) error
	UpdateEdge(ctx context.Context, e db.Edge) (db.Edge, error)
	DeleteEdge(ctx context.Context, id string) error
	TenantRadsec(ctx context.Context, tenant int64) (db.TenantRadsec, error)
	ReleasedRadsec(ctx context.Context, tenant int64) (db.TenantRadsec, error)
	SlugUsed(ctx context.Context, slug string) (bool, error)
	EnableRadsec(ctx context.Context, tenant int64, slug, caCertificate, by string) (db.TenantRadsec, error)
	SetRadsecSources(ctx context.Context, tenant int64, cidrs []string) ([]string, error)
	SetSharedMatch(ctx context.Context, tenant int64, attr, pattern string) error
	RequestApply(ctx context.Context, tenant int64) (int64, error)
	MarkRadsecCADisabled(ctx context.Context, tenant int64) error
	Placements(ctx context.Context, tenant int64) ([]db.Placement, error)
	EdgePlacements(ctx context.Context, edge string) ([]db.Placement, error)
	AddPlacement(ctx context.Context, tenant int64, edge, slot, by string) (db.Placement, error)
	RemovePlacement(ctx context.Context, tenant, id int64) (db.Placement, error)
	StartMove(ctx context.Context, tenant, id int64, edge, by string) (db.Placement, error)
	FinishMove(ctx context.Context, tenant, id int64) (db.Placement, error)
	CancelMove(ctx context.Context, tenant, id int64) (db.Placement, error)
	RadsecOverviews(ctx context.Context) ([]db.RadsecOverview, error)
	APs(ctx context.Context, tenant int64) ([]db.AP, error)
	APByID(ctx context.Context, tenant, id int64) (db.AP, error)
	CheckAPName(ctx context.Context, tenant int64, name, nasID string) error
	CreateAP(ctx context.Context, a db.AP, cert db.APCert) (db.AP, error)
	AddAPCert(ctx context.Context, tenant, ap int64, cert db.APCert) error
	MarkAPCertRevoked(ctx context.Context, tenant, ap int64, serial, reason string) error
	DeleteAP(ctx context.Context, tenant, id int64) (bool, error)
	NASIDs(ctx context.Context, tenant int64) ([]string, error)
	BroughtCAs(ctx context.Context, tenant int64) ([]db.BroughtCA, error)
	AddBroughtCA(ctx context.Context, b db.BroughtCA, matchAttr, matchPattern string) error
	SetBroughtHasCRL(ctx context.Context, tenant int64, caID string, has bool, matchAttr, matchPattern string) error
	StartRemoveBrought(ctx context.Context, tenant int64, caID string) error
}

// slugPattern はテナントの slug・エッジの ID・AP の名前の形（radsec-edge §8.1）。
var slugPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

const (
	msgSlugForm    = "小文字の英数字と - で、32 文字まで（先頭と末尾は英数字）にしてください。"
	maxEdgeNameLen = 100
	// maxRadsecCIDRs は送信元の範囲の数の上限（エッジの宣言の上限）。
	maxRadsecCIDRs = 32
	// radsecStatusTimeout は aka-provisioner の状態を読む上限（DNS のドメインと接続の状態）。
	radsecStatusTimeout = 10 * time.Second
)

// wakeAfter はハンドラーの後に中央の整合のループを起こす（断った要求でも起こすだけで害はない）。
func (h *Handler) wakeAfter(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		next(w, r)
		if h.radsecLoop != nil {
			h.radsecLoop.Wake()
		}
	}
}

// radsecFailure は radsec-ca とエッジの呼び出しの誤りを画面の文にする。下流の 4xx の詳しい文（radsec-ca は日本語で返す）があれば、それを使う。
func (h *Handler) radsecFailure(r *http.Request, err error) string {
	if e, ok := errors.AsType[*provisioner.Error](err); ok && e.Status >= 400 && e.Status < 500 && e.Problem.Detail != "" && e.Problem.Downstream != "" {
		return downstreamLabel(e.Problem.Downstream) + ": " + e.Problem.Detail
	}
	return h.apiError(r, err, "").Message
}

// radsecStatus は aka-provisioner の状態を読む（画面が待ちすぎないよう上限を短くする）。読めなければ false。
func (h *Handler) radsecStatus(r *http.Request) (provisioner.Status, bool) {
	ctx, cancel := context.WithTimeout(r.Context(), radsecStatusTimeout)
	defer cancel()
	st, err := h.pv.Status(ctx)
	if err != nil {
		h.log.Warn("provisioner status", "error", err)
		return provisioner.Status{}, false
	}
	return st, true
}

// dnsDomain は名前のドメインを radsec-ca の /status から読む（サーバー証明書の SAN を作るのは radsec-ca なので、設定を別に持たない。OB13）。
func dnsDomain(st provisioner.Status) string {
	for _, d := range st.Downstreams {
		if d.Name != provisioner.DownstreamCA || len(d.Status) == 0 {
			continue
		}
		var s provisioner.CAStatus
		if json.Unmarshal(d.Status, &s) == nil {
			return s.DNSDomain
		}
	}
	return ""
}

// edgeDNSName はエッジの DNS の名前（A レコード）、slotDNSName はスロットの DNS の名前（エッジの名前への CNAME）。
func edgeDNSName(edge, domain string) string { return edge + "." + domain }
func slotDNSName(slot, slug, domain string) string {
	return slot + "." + slug + "." + domain
}

// dnsRecord は DNS に登録する 1 件。
type dnsRecord struct {
	Name, Type, Value, Note string
}

// slotRecords は配置の DNS のレコードを返す。移動の途中は、新しい配置の行で書き換えの先を示す（古い配置の行は出さない）。
func slotRecords(slug, domain string, ps []db.Placement) []dnsRecord {
	var out []dnsRecord
	movedAway := map[int64]bool{}
	for _, p := range ps {
		if p.Replaces != 0 {
			movedAway[p.Replaces] = true
		}
	}
	for _, p := range ps {
		if p.Removing() || movedAway[p.ID] {
			continue
		}
		rec := dnsRecord{Name: slotDNSName(p.Slot, slug, domain), Type: "CNAME", Value: edgeDNSName(p.EdgeID, domain)}
		if p.Replaces != 0 {
			rec.Note = "移動の途中です。新しいエッジが反映してから、この向け先に書き換えてください。"
		}
		out = append(out, rec)
	}
	return out
}

// ---- エッジの台帳 ----

// edgeForm はエッジのフォームの値と誤り。
type edgeForm struct {
	ID, Name, Capacity, PublicAddr, AuthUpstream, AcctUpstream string
	Errors                                                     fieldErrors
}

func (f edgeForm) edge() db.Edge {
	capacity, _ := strconv.Atoi(f.Capacity)
	return db.Edge{ID: f.ID, Name: f.Name, Capacity: capacity, PublicAddr: f.PublicAddr, AuthUpstream: f.AuthUpstream, AcctUpstream: f.AcctUpstream}
}

func formOfEdge(e db.Edge) edgeForm {
	return edgeForm{ID: e.ID, Name: e.Name, Capacity: strconv.Itoa(e.Capacity), PublicAddr: e.PublicAddr, AuthUpstream: e.AuthUpstream, AcctUpstream: e.AcctUpstream, Errors: fieldErrors{}}
}

// readEdgeForm はエッジのフォームを読んで確かめる（ID は作るときだけ）。
func readEdgeForm(r *http.Request, withID bool) edgeForm {
	pf := r.PostForm
	f := edgeForm{ID: strings.TrimSpace(pf.Get("id")), Name: strings.TrimSpace(pf.Get("name")), Capacity: strings.TrimSpace(pf.Get("capacity")),
		PublicAddr: strings.TrimSpace(pf.Get("publicAddr")), AuthUpstream: strings.TrimSpace(pf.Get("authUpstream")), AcctUpstream: strings.TrimSpace(pf.Get("acctUpstream")),
		Errors: fieldErrors{}}
	if withID && !slugPattern.MatchString(f.ID) {
		f.Errors["id"] = "ID は " + msgSlugForm
	}
	if f.Name == "" || utf8.RuneCountInString(f.Name) > maxEdgeNameLen {
		f.Errors["name"] = fmt.Sprintf("名前は 1〜%d 文字で入力してください。", maxEdgeNameLen)
	}
	if n, err := strconv.Atoi(f.Capacity); err != nil || n < 1 || n > 2000 {
		f.Errors["capacity"] = "容量は 1〜2000 の数で入力してください。"
	}
	if _, err := netip.ParseAddr(f.PublicAddr); err != nil {
		f.Errors["publicAddr"] = "公開のアドレスは IP アドレスで入力してください。"
	}
	for name, v := range map[string]string{"authUpstream": f.AuthUpstream, "acctUpstream": f.AcctUpstream} {
		if _, err := netip.ParseAddrPort(v); err != nil {
			f.Errors[name] = "上流は IP アドレス:ポートで入力してください（ホスト名は使えません）。"
		}
	}
	return f
}

type edgesData struct {
	Edges     []db.Edge
	Form      edgeForm
	Domain    string
	Status    map[string]provisioner.DownstreamStatus
	Message   string
	Error     string
	CanManage bool
}

// edgeStatuses は aka-provisioner の状態から、エッジごとの状態を引く表を作る（キーはエッジの ID）。
func edgeStatuses(st provisioner.Status) map[string]provisioner.DownstreamStatus {
	out := map[string]provisioner.DownstreamStatus{}
	for _, d := range st.Downstreams {
		if id, ok := strings.CutPrefix(string(d.Name), "edge:"); ok {
			out[id] = d
		}
	}
	return out
}

// defaultEdgeForm は新しいエッジのフォームの初期値（上流は今あるエッジと同じ値を入れておく）。
func (h *Handler) defaultEdgeForm(edges []db.Edge) edgeForm {
	f := edgeForm{Capacity: strconv.Itoa(h.edgeCapacity), Errors: fieldErrors{}}
	if len(edges) > 0 {
		f.AuthUpstream, f.AcctUpstream = edges[0].AuthUpstream, edges[0].AcctUpstream
	}
	return f
}

func (h *Handler) renderEdges(w http.ResponseWriter, r *http.Request, status int, d edgesData) {
	ctx, cancel := dbContext(r)
	edges, err := h.radsec.Edges(ctx)
	cancel()
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	d.Edges, d.CanManage = edges, can(r, authz.EdgeManage)
	if d.Form.Errors == nil {
		d.Form = h.defaultEdgeForm(edges)
	}
	if st, ok := h.radsecStatus(r); ok {
		d.Status, d.Domain = edgeStatuses(st), dnsDomain(st)
	}
	h.renderPart(w, r, status, "edges", "edges-section", "エッジ", d)
}

func (h *Handler) edgeList(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.EdgeView) {
		return
	}
	d := edgesData{}
	if r.URL.Query().Get("deleted") == "1" {
		d.Message = "エッジを台帳から消しました。"
	}
	h.renderEdges(w, r, http.StatusOK, d)
}

func (h *Handler) edgeCreate(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.EdgeManage) {
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	f := readEdgeForm(r, true)
	if len(f.Errors) > 0 {
		h.renderEdges(w, r, http.StatusBadRequest, edgesData{Form: f})
		return
	}
	// ID は aka-provisioner の登録簿にある値だけを受ける（綴りの誤りを防ぐ。読めないときも断る。OB11）。
	st, ok := h.radsecStatus(r)
	if !ok {
		h.renderEdges(w, r, http.StatusBadGateway, edgesData{Form: f, Error: "aka-provisioner に接続できないので、登録簿を確かめられません。"})
		return
	}
	if _, found := edgeStatuses(st)[f.ID]; !found {
		f.Errors["id"] = "aka-provisioner の登録簿にこの ID のエッジがありません。先に登録簿に足して SIGHUP を送ってください（aka-provisioner の運用ガイド 3.6）。"
		h.renderEdges(w, r, http.StatusBadRequest, edgesData{Form: f})
		return
	}
	ctx, cancel := dbContext(r)
	err := h.radsec.CreateEdge(ctx, f.edge(), userFrom(r.Context()).Email)
	cancel()
	switch {
	case errors.Is(err, db.ErrConflict):
		f.Errors["id"] = "この ID のエッジは既に台帳にあります。"
		h.renderEdges(w, r, http.StatusConflict, edgesData{Form: f})
		return
	case err != nil:
		h.dbUnavailable(w, r, err)
		return
	}
	h.record(r, audit.ActionEdgeCreate, f.ID, authz.NoTenant, map[string]any{"name": f.Name, "capacity": f.edge().Capacity, "publicAddr": f.PublicAddr})
	h.renderEdges(w, r, http.StatusOK, edgesData{Message: "エッジ " + f.ID + " を台帳に足しました。"})
}

type edgeData struct {
	Edge       db.Edge
	Form       edgeForm
	Status     provisioner.DownstreamStatus
	Registered bool
	Domain     string
	Tenants    []edgeTenantRow
	Message    string
	Error      string
	CanManage  bool
}

// edgeTenantRow はエッジに置いたテナントの 1 行。
type edgeTenantRow struct {
	db.Placement
	TenantName string
}

func (h *Handler) renderEdge(w http.ResponseWriter, r *http.Request, id string, status int, mutate func(*edgeData)) {
	ctx, cancel := dbContext(r)
	defer cancel()
	e, err := h.radsec.EdgeByID(ctx, id)
	switch {
	case errors.Is(err, db.ErrNotFound):
		h.renderErrorLink(w, r, http.StatusNotFound, "エッジが見つかりません。", "/edges", "エッジの一覧へ")
		return
	case err != nil:
		h.dbUnavailable(w, r, err)
		return
	}
	ps, err := h.radsec.EdgePlacements(ctx, id)
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	t, err := h.tenants(r)
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	d := edgeData{Edge: e, Form: formOfEdge(e), CanManage: can(r, authz.EdgeManage)}
	for _, p := range ps {
		d.Tenants = append(d.Tenants, edgeTenantRow{Placement: p, TenantName: t.Name(p.TenantID)})
	}
	if st, ok := h.radsecStatus(r); ok {
		d.Status, d.Registered = edgeStatuses(st)[id]
		d.Domain = dnsDomain(st)
	}
	if mutate != nil {
		mutate(&d)
	}
	h.renderPart(w, r, status, "edge", "edge-detail", "エッジ "+id, d)
}

func (h *Handler) edge(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.EdgeView) {
		return
	}
	h.renderEdge(w, r, r.PathValue("id"), http.StatusOK, nil)
}

func (h *Handler) edgeUpdate(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.EdgeManage) {
		return
	}
	id := r.PathValue("id")
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	f := readEdgeForm(r, false)
	f.ID = id
	if len(f.Errors) > 0 {
		h.renderEdge(w, r, id, http.StatusBadRequest, func(d *edgeData) { d.Form = f })
		return
	}
	ctx, cancel := dbContext(r)
	before, err := h.radsec.UpdateEdge(ctx, f.edge())
	cancel()
	switch {
	case errors.Is(err, db.ErrNotFound):
		h.renderErrorLink(w, r, http.StatusNotFound, "エッジが見つかりません。", "/edges", "エッジの一覧へ")
		return
	case errors.Is(err, db.ErrCapacity):
		h.renderEdge(w, r, id, http.StatusConflict, func(d *edgeData) {
			f.Errors["capacity"] = fmt.Sprintf("今の配置の数（%d）より小さくできません。", d.Edge.Placements)
			d.Form = f
		})
		return
	case err != nil:
		h.dbUnavailable(w, r, err)
		return
	}
	h.record(r, audit.ActionEdgeUpdate, id, authz.NoTenant, map[string]any{"capacity": f.edge().Capacity, "publicAddr": f.PublicAddr,
		"upstreamChanged": before.AuthUpstream != f.AuthUpstream || before.AcctUpstream != f.AcctUpstream})
	msg := "エッジの設定を変更しました。"
	if before.AuthUpstream != f.AuthUpstream || before.AcctUpstream != f.AcctUpstream {
		msg += "上流を変えたので、このエッジに置いたテナントに反映します。"
	}
	if before.PublicAddr != f.PublicAddr {
		msg += "公開のアドレスを変えたら、DNS の A レコードも書き換えてください。"
	}
	h.renderEdge(w, r, id, http.StatusOK, func(d *edgeData) { d.Message = msg })
}

func (h *Handler) edgeDelete(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.EdgeManage) {
		return
	}
	id := r.PathValue("id")
	ctx, cancel := dbContext(r)
	err := h.radsec.DeleteEdge(ctx, id)
	cancel()
	switch {
	case errors.Is(err, db.ErrNotFound):
		h.renderErrorLink(w, r, http.StatusNotFound, "エッジが見つかりません。", "/edges", "エッジの一覧へ")
		return
	case errors.Is(err, db.ErrInUse):
		h.renderEdge(w, r, id, http.StatusConflict, func(d *edgeData) {
			d.Error = "このエッジにテナントの配置が残っているので消せません（消している途中の配置を含みます）。先に配置を外してください。"
		})
		return
	case err != nil:
		h.dbUnavailable(w, r, err)
		return
	}
	h.record(r, audit.ActionEdgeDelete, id, authz.NoTenant, nil)
	seeOther(w, r, "/edges?deleted=1")
}

// ---- 全テナントの状態 ----

type radsecListData struct {
	Rows   []radsecRow
	Domain string
	// EdgeRecords はエッジの A レコード、SlotRecords はスロットの CNAME（全テナント）。
	EdgeRecords, SlotRecords []dnsRecord
}

// radsecRow は全テナントの一覧の 1 行。
type radsecRow struct {
	db.RadsecOverview
	State radsecState
}

func (h *Handler) radsecList(w http.ResponseWriter, r *http.Request) {
	if !h.requireOp(w, r, authz.RadsecConfig) {
		return
	}
	ctx, cancel := dbContext(r)
	rows, err := h.radsec.RadsecOverviews(ctx)
	var edges []db.Edge
	if err == nil {
		edges, err = h.radsec.Edges(ctx)
	}
	cancel()
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	d := radsecListData{}
	if st, ok := h.radsecStatus(r); ok {
		d.Domain = dnsDomain(st)
	}
	for _, row := range rows {
		d.Rows = append(d.Rows, radsecRow{RadsecOverview: row, State: evaluateRadsec(row.TenantRadsec, row.Placements)})
		if d.Domain != "" {
			d.SlotRecords = append(d.SlotRecords, slotRecords(row.Slug, d.Domain, row.Placements)...)
		}
	}
	if d.Domain != "" {
		for _, e := range edges {
			d.EdgeRecords = append(d.EdgeRecords, dnsRecord{Name: edgeDNSName(e.ID, d.Domain), Type: "A", Value: e.PublicAddr})
		}
	}
	h.render(w, r, http.StatusOK, "radsec", "RadSec", d)
}

// ---- テナントの RadSec の設定と配置 ----

// radsecState はテナントの反映の状態の見出し。
type radsecState struct {
	// Unrequested は反映を求めていない変更があるか。Waiting は求めた版にまだ届いていないエッジがあるか。Failed は反映に失敗したエッジがあるか。
	Unrequested, Waiting, Failed bool
}

// Label は状態の短い名前。
func (s radsecState) Label() string {
	switch {
	case s.Failed:
		return "反映の失敗あり"
	case s.Waiting:
		return "反映待ち"
	case s.Unrequested:
		return "未反映の変更あり"
	}
	return "反映済み"
}

// applyFailed は反映の結果が失敗か。
func applyFailed(result string) bool { return result == "failed" || result == "rolled_back" }

// evaluateRadsec はテナントの反映の状態を求める。
func evaluateRadsec(t db.TenantRadsec, ps []db.Placement) radsecState {
	s := radsecState{Unrequested: t.RequestedRevision < t.Revision}
	for _, p := range ps {
		if p.Removing() {
			continue
		}
		if applyFailed(p.ApplyResult) {
			s.Failed = true
		}
		if p.AppliedRevision == 0 || p.AppliedRevision < t.RequestedRevision {
			s.Waiting = true
		}
	}
	return s
}

// placementRow は配置の 1 行。
type placementRow struct {
	db.Placement
	// Label は状態の文。MovedBy はこの配置を置き換えている配置の ID（移動の途中）。
	Label   string
	MovedBy int64
}

// Moving は移動の途中か（どちらの側でも）。
func (p placementRow) Moving() bool { return p.Replaces != 0 || p.MovedBy != 0 }

func placementRows(t db.TenantRadsec, ps []db.Placement) []placementRow {
	movedBy := map[int64]int64{}
	for _, p := range ps {
		if p.Replaces != 0 {
			movedBy[p.Replaces] = p.ID
		}
	}
	var out []placementRow
	for _, p := range ps {
		row := placementRow{Placement: p, MovedBy: movedBy[p.ID]}
		switch {
		case p.Removing():
			row.Label = "エッジから消しています"
		case applyFailed(p.ApplyResult):
			row.Label = "反映に失敗しました"
		case p.AppliedRevision == 0:
			row.Label = "反映待ち（最初の反映）"
		case p.AppliedRevision < t.RequestedRevision:
			row.Label = "反映待ち"
		case p.AppliedRevision < t.Revision:
			row.Label = "未反映の変更あり"
		default:
			row.Label = "反映済み"
		}
		out = append(out, row)
	}
	return out
}

type tenantRadsecData struct {
	Tenant db.TenantSummary
	// Enabled は RadSec を有効にしているか。Radsec はその設定。
	Enabled bool
	Radsec  db.TenantRadsec
	State   radsecState
	Rows    []placementRow
	// Edges は配置の先の選択肢。FreeSlots は空いているスロット。
	Edges     []db.Edge
	FreeSlots []string
	// Domain は DNS のドメイン（読めなければ空）。SlotNames は AP に設定する名前、Records は DNS に登録するレコード（C・D）。
	Domain    string
	SlotNames []string
	Records   []dnsRecord
	// NoCRL は CRL のない持ち込みの CA があるか（テナント全体が CRLCheck off になる）。
	NoCRL bool
	// フォームの値と誤り。
	Slug, SlugError, Sources, SourcesError, PlacementError string
	IdemKey                                                string
	Message, Error                                         string
	// CanConfig は設定と配置、CanApply は反映、CanOperate は再起動とログ。Staff はエッジの ID を見せるか（A・B はスロットだけ。OB16）。
	CanConfig, CanApply, CanOperate, CanBrought, Staff bool
}

// radsecScope はテナントの RadSec の画面の範囲を確かめる。範囲の外は 404、役割にない操作は 403。
func (h *Handler) radsecScope(w http.ResponseWriter, r *http.Request, op authz.Op) (int64, bool) {
	id, ok := h.pathTenantID(w, r)
	if !ok {
		return 0, false
	}
	a := userFrom(r.Context()).Actor()
	if !authz.Allowed(a, authz.RadsecView, id) {
		h.renderError(w, r, http.StatusNotFound, msgTenantNotFound)
		return 0, false
	}
	if !authz.Allowed(a, op, id) {
		h.forbidden(w, r)
		return 0, false
	}
	return id, true
}

func (h *Handler) renderTenantRadsec(w http.ResponseWriter, r *http.Request, id int64, status int, mutate func(*tenantRadsecData)) {
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
	d := tenantRadsecData{Tenant: t, IdemKey: newIdemKey(), Staff: !a.Role.TenantScoped(),
		CanConfig: authz.Allowed(a, authz.RadsecConfig, id), CanApply: authz.Allowed(a, authz.RadsecApply, id),
		CanOperate: authz.Allowed(a, authz.EdgeOperate, id), CanBrought: authz.Allowed(a, authz.BroughtCAManage, id)}
	rad, err := h.radsec.TenantRadsec(ctx, id)
	switch {
	case errors.Is(err, db.ErrNotFound):
	case err != nil:
		h.dbUnavailable(w, r, err)
		return
	default:
		d.Enabled, d.Radsec, d.Sources = true, rad, strings.Join(rad.SourceCIDRs, "\n")
		ps, err := h.radsec.Placements(ctx, id)
		if err != nil {
			h.dbUnavailable(w, r, err)
			return
		}
		d.Rows, d.State = placementRows(rad, ps), evaluateRadsec(rad, ps)
		used := map[string]bool{}
		for _, p := range ps {
			if !p.Removing() {
				used[p.Slot] = true
			}
		}
		for _, s := range db.RadsecSlots() {
			if !used[s] {
				d.FreeSlots = append(d.FreeSlots, s)
			}
		}
		brought, err := h.radsec.BroughtCAs(ctx, id)
		if err != nil {
			h.dbUnavailable(w, r, err)
			return
		}
		d.NoCRL = slices.ContainsFunc(brought, func(b db.BroughtCA) bool { return !b.Removing() && !b.HasCRL })
		if d.CanConfig {
			if d.Edges, err = h.radsec.Edges(ctx); err != nil {
				h.dbUnavailable(w, r, err)
				return
			}
		}
		if st, ok := h.radsecStatus(r); ok {
			d.Domain = dnsDomain(st)
		}
		if d.Domain != "" {
			for s := range used {
				d.SlotNames = append(d.SlotNames, s)
			}
			slices.SortFunc(d.SlotNames, func(x, y string) int {
				return cmp.Compare(slices.Index(db.RadsecSlots(), x), slices.Index(db.RadsecSlots(), y))
			})
			for i, s := range d.SlotNames {
				d.SlotNames[i] = slotDNSName(s, rad.Slug, d.Domain)
			}
			if d.Staff {
				d.Records = slotRecords(rad.Slug, d.Domain, ps)
			}
		}
	}
	if mutate != nil {
		mutate(&d)
	}
	h.renderPart(w, r, status, "tenant_radsec", "tenant-radsec", "RadSec "+t.Name, d)
}

func (h *Handler) tenantRadsec(w http.ResponseWriter, r *http.Request) {
	id, ok := h.radsecScope(w, r, authz.RadsecView)
	if !ok {
		return
	}
	h.renderTenantRadsec(w, r, id, http.StatusOK, nil)
}

// radsecEnable はテナントの RadSec を有効にする（C・D。OB12）。テナントの CA を作り、その後で DB に書く。
// 同じ slug の CA があれば読み直す（DB に書く前に止まったときにやり直せるように）。
func (h *Handler) radsecEnable(w http.ResponseWriter, r *http.Request) {
	id, ok := h.radsecScope(w, r, authz.RadsecConfig)
	if !ok {
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	slug := strings.TrimSpace(r.PostForm.Get("slug"))
	failed := func(status int, field, msg string, key string) {
		h.renderTenantRadsec(w, r, id, status, func(d *tenantRadsecData) {
			d.Slug, d.SlugError, d.Error = slug, field, msg
			if key != "" {
				d.IdemKey = key
			}
		})
	}
	if !slugPattern.MatchString(slug) {
		failed(http.StatusBadRequest, "slug は "+msgSlugForm, "", "")
		return
	}
	ctx, cancel := dbContext(r)
	used, err := h.radsec.SlugUsed(ctx, slug)
	cancel()
	if err != nil {
		h.dbUnavailable(w, r, err)
		return
	}
	if used {
		failed(http.StatusConflict, "この slug は使われたことがあります（使い回しません）。", "", "")
		return
	}
	key := formIdemKey(r, "idem")
	ca, err := h.pv.CreateTenantCA(r.Context(), slug, key)
	if provisioner.CauseOf(err) == provisioner.CauseCAExists {
		ca, err = h.pv.GetCA(r.Context(), "tenant."+slug)
	}
	if err != nil {
		failed(http.StatusBadGateway, "", h.radsecFailure(r, err), keepIdemKey(err, key))
		return
	}
	ctx, cancel = dbContext(r)
	rad, err := h.radsec.EnableRadsec(ctx, id, slug, ca.Certificate, userFrom(r.Context()).Email)
	cancel()
	switch {
	case errors.Is(err, db.ErrNotFound):
		h.renderError(w, r, http.StatusNotFound, msgTenantNotFound)
		return
	case errors.Is(err, db.ErrConflict):
		failed(http.StatusConflict, "この slug は使われているか、このテナントは有効にしてあります。画面を開き直してください。", "", "")
		return
	case errors.Is(err, db.ErrNoPort):
		failed(http.StatusConflict, "", "テナントのポートの空きがありません（20000〜21999）。", "")
		return
	case err != nil:
		h.dbUnavailable(w, r, err)
		return
	}
	h.record(r, audit.ActionRadsecEnable, slug, id, map[string]any{"slug": slug, "port": rad.Port})
	h.renderTenantRadsec(w, r, id, http.StatusOK, func(d *tenantRadsecData) {
		d.Message = fmt.Sprintf("RadSec を有効にしました（ポート %d）。次にエッジへ配置してください。", rad.Port)
	})
}

// parseCIDRs は送信元の範囲の入力（改行かカンマ区切り）を読む。
func parseCIDRs(v string) ([]string, string) {
	out := []string{}
	for f := range strings.FieldsFuncSeq(v, func(c rune) bool { return c == '\n' || c == '\r' || c == ',' || c == ' ' }) {
		p, err := netip.ParsePrefix(f)
		if err != nil {
			return nil, "「" + f + "」はアドレスの範囲（例 203.0.113.0/24）ではありません。"
		}
		out = append(out, p.Masked().String())
	}
	if len(out) > maxRadsecCIDRs {
		return nil, fmt.Sprintf("範囲は %d 個までです。", maxRadsecCIDRs)
	}
	slices.Sort(out)
	return slices.Compact(out), ""
}

func (h *Handler) radsecSources(w http.ResponseWriter, r *http.Request) {
	id, ok := h.radsecScope(w, r, authz.RadsecConfig)
	if !ok {
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	raw := r.PostForm.Get("sources")
	cidrs, msg := parseCIDRs(raw)
	if msg != "" {
		h.renderTenantRadsec(w, r, id, http.StatusBadRequest, func(d *tenantRadsecData) { d.Sources, d.SourcesError = raw, msg })
		return
	}
	ctx, cancel := dbContext(r)
	before, err := h.radsec.SetRadsecSources(ctx, id, cidrs)
	cancel()
	if h.radsecDBError(w, r, err) {
		return
	}
	if !slices.Equal(before, cidrs) {
		h.record(r, audit.ActionRadsecSources, formatID(id), id, map[string]any{"from": before, "to": cidrs})
	}
	h.renderTenantRadsec(w, r, id, http.StatusOK, func(d *tenantRadsecData) {
		d.Message = "送信元の範囲を変更しました。各エッジに順に反映します。"
	})
}

// radsecDBError は RadSec の DB の操作の共通の誤りに応える。応えたら true。
func (h *Handler) radsecDBError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, db.ErrNotFound):
		h.renderError(w, r, http.StatusNotFound, "対象が見つかりません。画面を開き直してください。")
	default:
		h.dbUnavailable(w, r, err)
	}
	return true
}

// radsecApply は今の版の反映を求める（B・C・D の「反映」。OB1）。
func (h *Handler) radsecApply(w http.ResponseWriter, r *http.Request) {
	id, ok := h.radsecScope(w, r, authz.RadsecApply)
	if !ok {
		return
	}
	ctx, cancel := dbContext(r)
	rev, err := h.radsec.RequestApply(ctx, id)
	cancel()
	if h.radsecDBError(w, r, err) {
		return
	}
	h.record(r, audit.ActionRadsecApply, formatID(id), id, map[string]any{"revision": rev})
	h.renderTenantRadsec(w, r, id, http.StatusOK, func(d *tenantRadsecData) {
		d.Message = "反映を求めました。配置したエッジに 1 台ずつ順に反映します。設定によっては、そのエッジの AP の接続がいったん切れます。"
	})
}

// radsecPlacementAdd は配置を足す（C・D。OB13）。
func (h *Handler) radsecPlacementAdd(w http.ResponseWriter, r *http.Request) {
	id, ok := h.radsecScope(w, r, authz.RadsecConfig)
	if !ok {
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	edge, slot := r.PostForm.Get("edge"), r.PostForm.Get("slot")
	if !slices.Contains(db.RadsecSlots(), slot) {
		h.renderTenantRadsec(w, r, id, http.StatusBadRequest, func(d *tenantRadsecData) { d.PlacementError = "スロットを選んでください。" })
		return
	}
	ctx, cancel := dbContext(r)
	_, err := h.radsec.AddPlacement(ctx, id, edge, slot, userFrom(r.Context()).Email)
	cancel()
	if msg, status := placementError(err); msg != "" {
		h.renderTenantRadsec(w, r, id, status, func(d *tenantRadsecData) { d.PlacementError = msg })
		return
	}
	if h.radsecDBError(w, r, err) {
		return
	}
	h.record(r, audit.ActionPlacementAdd, edge, id, map[string]any{"edge": edge, "slot": slot})
	h.renderTenantRadsec(w, r, id, http.StatusOK, func(d *tenantRadsecData) {
		d.Message = "配置を足しました。エッジへの反映は自動で進みます。下の DNS のレコードを登録してください。"
	})
}

// placementError は配置の操作の誤りの文を返す。
func placementError(err error) (string, int) {
	switch {
	case errors.Is(err, db.ErrCapacity):
		return "このエッジの容量を超えます。別のエッジを選ぶか、エッジの容量を見直してください。", http.StatusConflict
	case errors.Is(err, db.ErrConflict):
		return "このスロットかエッジは、このテナントで使っています（エッジから消している途中のものを含みます）。", http.StatusConflict
	case errors.Is(err, db.ErrInUse):
		return "スロットの移動の途中です。先に移動を完了するか、やめてください。", http.StatusConflict
	case errors.Is(err, db.ErrNotReady):
		return "新しいエッジがまだ反映していません。反映してから DNS を書き換え、完了を押してください。", http.StatusConflict
	}
	return "", 0
}

// placementAction は配置の操作（外す、移動の完了とやめる）の共通の手順。
func (h *Handler) placementAction(w http.ResponseWriter, r *http.Request, action, message string, fn func(ctx context.Context, tenant, id int64) (db.Placement, error)) {
	id, ok := h.radsecScope(w, r, authz.RadsecConfig)
	if !ok {
		return
	}
	pid, err := parseID(r.PathValue("pid"))
	if err != nil {
		h.notFound(w, r)
		return
	}
	ctx, cancel := dbContext(r)
	p, err := fn(ctx, id, pid)
	cancel()
	if msg, status := placementError(err); msg != "" {
		h.renderTenantRadsec(w, r, id, status, func(d *tenantRadsecData) { d.PlacementError = msg })
		return
	}
	if h.radsecDBError(w, r, err) {
		return
	}
	h.record(r, action, p.EdgeID, id, map[string]any{"edge": p.EdgeID, "slot": p.Slot})
	h.renderTenantRadsec(w, r, id, http.StatusOK, func(d *tenantRadsecData) { d.Message = message })
}

func (h *Handler) radsecPlacementRemove(w http.ResponseWriter, r *http.Request) {
	h.placementAction(w, r, audit.ActionPlacementRemove, "配置を外しました。エッジからの削除は自動で進みます。DNS のレコードも消してください。", h.radsec.RemovePlacement)
}

func (h *Handler) radsecMoveFinish(w http.ResponseWriter, r *http.Request) {
	h.placementAction(w, r, audit.ActionPlacementMoveDone, "移動を完了しました。古いエッジからの削除は自動で進みます。", h.radsec.FinishMove)
}

func (h *Handler) radsecMoveCancel(w http.ResponseWriter, r *http.Request) {
	h.placementAction(w, r, audit.ActionPlacementMoveStop, "移動をやめました。新しいエッジからの削除は自動で進みます。DNS を書き換えていたら戻してください。", h.radsec.CancelMove)
}

// radsecMoveStart はスロットの移動を始める（OB7）。
func (h *Handler) radsecMoveStart(w http.ResponseWriter, r *http.Request) {
	id, ok := h.radsecScope(w, r, authz.RadsecConfig)
	if !ok {
		return
	}
	pid, err := parseID(r.PathValue("pid"))
	if err != nil {
		h.notFound(w, r)
		return
	}
	if err := parseForm(w, r); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "入力を読み取れませんでした。")
		return
	}
	edge := r.PostForm.Get("edge")
	ctx, cancel := dbContext(r)
	p, err := h.radsec.StartMove(ctx, id, pid, edge, userFrom(r.Context()).Email)
	cancel()
	if msg, status := placementError(err); msg != "" {
		h.renderTenantRadsec(w, r, id, status, func(d *tenantRadsecData) { d.PlacementError = msg })
		return
	}
	if h.radsecDBError(w, r, err) {
		return
	}
	h.record(r, audit.ActionPlacementMove, edge, id, map[string]any{"edge": edge, "slot": p.Slot})
	h.renderTenantRadsec(w, r, id, http.StatusOK, func(d *tenantRadsecData) {
		d.Message = "移動を始めました。新しいエッジが反映したら DNS の向け先を書き換え、AP が移り終わってから完了を押してください。"
	})
}

// placementOf はテナントの配置を読む（エッジの運用の操作）。ないなら 404 を返して false。
func (h *Handler) placementOf(w http.ResponseWriter, r *http.Request, tenant int64) (db.Placement, bool) {
	pid, err := parseID(r.PathValue("pid"))
	if err != nil {
		h.notFound(w, r)
		return db.Placement{}, false
	}
	ctx, cancel := dbContext(r)
	ps, err := h.radsec.Placements(ctx, tenant)
	cancel()
	if err != nil {
		h.dbUnavailable(w, r, err)
		return db.Placement{}, false
	}
	i := slices.IndexFunc(ps, func(p db.Placement) bool { return p.ID == pid })
	if i < 0 {
		h.notFound(w, r)
		return db.Placement{}, false
	}
	return ps[i], true
}

// radsecRestart はテナントのコンテナを再起動する（C・D。実行のたびに新しいキー）。
func (h *Handler) radsecRestart(w http.ResponseWriter, r *http.Request) {
	id, ok := h.radsecScope(w, r, authz.EdgeOperate)
	if !ok {
		return
	}
	p, ok := h.placementOf(w, r, id)
	if !ok {
		return
	}
	if err := h.pv.RestartEdgeTenant(r.Context(), p.EdgeID, id, newIdemKey()); err != nil {
		h.renderTenantRadsec(w, r, id, http.StatusBadGateway, func(d *tenantRadsecData) { d.Error = h.radsecFailure(r, err) })
		return
	}
	h.record(r, audit.ActionEdgeRestart, p.EdgeID, id, map[string]any{"edge": p.EdgeID, "slot": p.Slot})
	h.renderTenantRadsec(w, r, id, http.StatusOK, func(d *tenantRadsecData) {
		d.Message = "エッジ " + p.EdgeID + " のコンテナを再起動しました。"
	})
}

type radsecLogsData struct {
	Tenant int64
	Edge   string
	Logs   string
	Error  string
}

// radsecLogLines はログの画面に出す行の数。
const radsecLogLines = 500

// radsecLogs は radsecproxy のログの末尾を出す（C・D）。
func (h *Handler) radsecLogs(w http.ResponseWriter, r *http.Request) {
	id, ok := h.radsecScope(w, r, authz.EdgeOperate)
	if !ok {
		return
	}
	p, ok := h.placementOf(w, r, id)
	if !ok {
		return
	}
	d := radsecLogsData{Tenant: id, Edge: p.EdgeID}
	logs, err := h.pv.EdgeTenantLogs(r.Context(), p.EdgeID, id, radsecLogLines)
	if err != nil {
		d.Error = h.radsecFailure(r, err)
	}
	d.Logs = logs
	h.render(w, r, http.StatusOK, "radsec_logs", "ログ "+p.EdgeID, d)
}

// disableReleasedCA は削除したテナントの CA を止める（OB12。届かなければ後でループがやり直す）。
func (h *Handler) disableReleasedCA(r *http.Request, tenant int64) {
	ctx, cancel := dbContext(r)
	t, err := h.radsec.ReleasedRadsec(ctx, tenant)
	cancel()
	if err != nil {
		if !errors.Is(err, db.ErrNotFound) {
			h.log.Warn("released radsec", "tenant", tenant, "error", err)
		}
		return
	}
	if _, err := h.pv.DisableCA(r.Context(), "tenant."+t.Slug, "disable-tenant-"+formatID(tenant)); err != nil {
		h.log.Warn("cannot disable the ca of the deleted tenant", "tenant", tenant, "error", err)
		return
	}
	ctx, cancel = dbContext(r)
	err = h.radsec.MarkRadsecCADisabled(ctx, tenant)
	cancel()
	if err != nil {
		h.log.Warn("mark radsec ca disabled", "tenant", tenant, "error", err)
		return
	}
	h.record(r, audit.ActionRadsecCADisable, "tenant."+t.Slug, tenant, nil)
}
