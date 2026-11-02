// Package radsec は中央の整合のループ（設計概要 §13・§16。段階 4-4 の OB2・OB4・OB6・OB18・OB19）。
//
// 望む状態の正本は DB にある（画面が書いて版を上げる）。ループはエッジごとに GET /tenants を 1 回読み、版が違えば宣言を送り、足りない証明書を出し、
// 反映を求めた版に届いていなければ配置の順（primary → secondary → tertiary）に 1 台ずつ反映する。失敗したら残りに送らず、同じ版を自動ではやり直さない。
// 届かない・エッジの画面で止めたエッジは飛ばす。エッジの状態の写しを placements に書き、画面はそれを読む。
package radsec

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oyaguma3/aka-ossbss/internal/audit"
	"github.com/oyaguma3/aka-ossbss/internal/db"
	"github.com/oyaguma3/aka-ossbss/internal/provisioner"
	"github.com/oyaguma3/aka-ossbss/internal/trace"
)

const (
	// Operator はループの操作者（監査ログとエッジの X-Operator-Id。OB19）。
	Operator = "system"

	// busyInterval は反映が動いている間の周回の間隔（OB18）。
	busyInterval = 10 * time.Second
	// maxApplyPerEdge は 1 つのエッジで同時に動かす反映の数（OB18）。
	maxApplyPerEdge = 5
	// renewBefore は証明書を自動で更新し始める期限までの長さ（OB4）。
	renewBefore = 30 * 24 * time.Hour
	// WarnBefore はエッジの証明書が更新できないまま残っていると警告する長さ、APWarnBefore は AP・グループの証明書の警告の長さ（更新は人が行う）。
	WarnBefore   = 14 * 24 * time.Hour
	APWarnBefore = 30 * 24 * time.Hour
	// maxErrorLen は画面に出す誤りの文の長さの上限。
	maxErrorLen = 500
	// passLock は周回を重ねないロックの名前。
	passLock = "radsec-pass"

	stateRunning = "running"
	resultOK     = "ok"
)

// Store は Runner が使う DB の操作。*db.DB が満たす。
type Store interface {
	audit.Inserter
	Edges(ctx context.Context) ([]db.Edge, error)
	RadsecOverviews(ctx context.Context) ([]db.RadsecOverview, error)
	APs(ctx context.Context, tenant int64) ([]db.AP, error)
	BroughtCAs(ctx context.Context, tenant int64) ([]db.BroughtCA, error)
	SetPlacementApply(ctx context.Context, id, applied, failed int64, result, errText string) error
	TouchPlacements(ctx context.Context, edge string) error
	MarkPlacementRemoved(ctx context.Context, id int64) error
	RecordPlacementCert(ctx context.Context, id int64, kind, serial string, notAfter time.Time) error
	PlacementCertPlaced(ctx context.Context, id int64, kind, serial string, notAfter time.Time) error
	BumpCertGeneration(ctx context.Context, id int64) error
	ClearRenew(ctx context.Context, id int64) error
	FinishCertApply(ctx context.Context, id int64, remaining []string) error
	ClearRetry(ctx context.Context, id int64) error
	ClearCRLDirty(ctx context.Context, tenant, revision int64) error
	RemovingBroughtCAs(ctx context.Context) ([]db.BroughtCA, error)
	FinishRemoveBrought(ctx context.Context, caID string) error
	RadsecCADisablePending(ctx context.Context) ([]db.TenantRadsec, error)
	MarkRadsecCADisabled(ctx context.Context, tenant int64) error
	MarkRenewals(ctx context.Context, before time.Time) (int64, error)
	ExpiringAPCerts(ctx context.Context, before time.Time) ([]db.ExpiringAPCert, error)
	TryNamedLock(ctx context.Context, name string) (release func(), ok bool, err error)
}

// Provisioner は Runner が使う aka-provisioner の口。*provisioner.Client が満たす。
type Provisioner interface {
	EdgeTenants(ctx context.Context, edge string) ([]provisioner.EdgeTenantSummary, error)
	EdgeTenant(ctx context.Context, edge string, tenant int64) (provisioner.EdgeTenant, error)
	PutEdgeTenant(ctx context.Context, edge string, tenant int64, d provisioner.EdgeDeclaration, idempotencyKey string) (provisioner.EdgeTenant, error)
	DeleteEdgeTenant(ctx context.Context, edge string, tenant int64, idempotencyKey string) error
	EdgeServerCSR(ctx context.Context, edge string, tenant int64, idempotencyKey string) (string, error)
	PutEdgeServerCert(ctx context.Context, edge string, tenant int64, certificate, idempotencyKey string) (provisioner.EdgeTenant, error)
	EdgeProbeCSR(ctx context.Context, edge string, tenant int64, idempotencyKey string) (string, error)
	PutEdgeProbeCert(ctx context.Context, edge string, tenant int64, certificate, idempotencyKey string) (provisioner.EdgeTenant, error)
	ApplyEdgeTenant(ctx context.Context, edge string, tenant int64, idempotencyKey string) (provisioner.EdgeApply, error)
	RefreshEdgeCRL(ctx context.Context, edge string, tenant int64, idempotencyKey string) (provisioner.EdgeCRLRefresh, error)
	IssueCert(ctx context.Context, i provisioner.CertIssue, idempotencyKey string) (provisioner.Cert, error)
	RevokeCert(ctx context.Context, serial, reason, idempotencyKey string) (provisioner.Cert, error)
	DisableCA(ctx context.Context, id, idempotencyKey string) (provisioner.CA, error)
}

// Options は Runner の設定。
type Options struct {
	Log   *slog.Logger
	DB    Store
	PV    Provisioner
	Audit *audit.Recorder
	// Now は時計（既定 time.Now）。BusyInterval は反映が動いている間の間隔（既定 10 秒。試験で短くする）。
	Now          func() time.Time
	BusyInterval time.Duration
}

// Runner は整合のループを動かす。
type Runner struct {
	opts    Options
	wake    chan struct{}
	running atomic.Bool
	again   atomic.Bool
	busy    atomic.Bool
	// 以下は周回の中だけで使う（周回は重ならない）。ログを変わったときだけ出すために覚える。
	down     map[string]bool
	orphans  map[string]bool
	expiring map[string]bool
	crlFail  map[int64]bool
	// apMu は apExpiring（毎日の処理が使う）を守る。
	apMu       sync.Mutex
	apExpiring map[string]bool
}

// New は Runner を作る。
func New(opts Options) *Runner {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.BusyInterval <= 0 {
		opts.BusyInterval = busyInterval
	}
	return &Runner{opts: opts, wake: make(chan struct{}, 1), down: map[string]bool{}, orphans: map[string]bool{}, expiring: map[string]bool{},
		crlFail: map[int64]bool{}, apExpiring: map[string]bool{}}
}

// Wake はループを起こす（画面の操作の後に呼ぶ。OB18）。
func (r *Runner) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run は ctx が終わるまで、起こされたときと反映が動いている間の周回を動かす（定期の周回はスケジューラーが RunScheduled を呼ぶ）。
func (r *Runner) Run(ctx context.Context) {
	t := time.NewTicker(r.opts.BusyInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		case <-t.C:
			if !r.busy.Load() {
				continue
			}
		}
		if _, err := r.RunScheduled(ctx); err != nil && ctx.Err() == nil {
			r.opts.Log.Warn("radsec: pass", "event_id", "RADSEC_SYNC_ERR", "error", err)
		}
	}
}

// RunScheduled は周回を 1 回動かす。動いていれば飛ばし、終わった後にもう 1 周する。
func (r *Runner) RunScheduled(ctx context.Context) (string, error) {
	if !r.running.CompareAndSwap(false, true) {
		r.again.Store(true)
		return "", nil
	}
	defer func() {
		r.running.Store(false)
		if r.again.Swap(false) {
			r.Wake()
		}
	}()
	release, ok, err := r.opts.DB.TryNamedLock(ctx, passLock)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", nil
	}
	defer release()
	return r.pass(ctx)
}

// RunRenew は毎日の処理。期限が近い証明書に更新の印を立ててループを起こし、更新は人が行う AP・グループの期限を警告する（OB4）。
func (r *Runner) RunRenew(ctx context.Context) (string, error) {
	now := r.opts.Now()
	n, err := r.opts.DB.MarkRenewals(ctx, now.Add(renewBefore))
	if err != nil {
		return "", err
	}
	aps, err := r.opts.DB.ExpiringAPCerts(ctx, now.Add(APWarnBefore))
	if err != nil {
		return "", err
	}
	r.apMu.Lock()
	seen := map[string]bool{}
	for _, c := range aps {
		seen[c.Serial] = true
		if !r.apExpiring[c.Serial] {
			r.opts.Log.Warn("radsec: ap certificate is expiring", "event_id", "RADSEC_CERT_EXPIRING", "tenant", c.TenantID, "ap", c.AP, "kind", c.Kind, "serial", c.Serial, "not_after", c.NotAfter)
		}
	}
	r.apExpiring = seen
	r.apMu.Unlock()
	r.Wake()
	if n == 0 && len(aps) == 0 {
		return "", nil
	}
	return fmt.Sprintf("証明書の更新を求めた配置 %d 件。期限の近い AP・グループ %d 件。", n, len(aps)), nil
}

// ---- 周回 ----

// edgeView は周回の中の 1 つのエッジ。
type edgeView struct {
	row     db.Edge
	down    bool
	tenants map[int64]*provisioner.EdgeTenantSummary
	// running は動いている反映の数（この周回で始めたものを含む）。
	running int
}

// pass は 1 回の周回の状態。
type pass struct {
	r     *Runner
	now   time.Time
	trace string
	edges map[string]*edgeView
	// busy は動いている反映があるか、エッジが忙しいのでまたすぐ確かめるか。
	busy bool
	// 結果の文の材料。
	declared, placed, started, removed int
}

// pass は周回の本体。1 つのテナントの誤りで止めず、次のテナントへ進む。
func (r *Runner) pass(ctx context.Context) (string, error) {
	now := r.opts.Now()
	p := &pass{r: r, now: now, trace: "radsec-" + strconv.FormatInt(now.Unix(), 10), edges: map[string]*edgeView{}}
	ctx = trace.With(provisioner.WithOperator(ctx, Operator), p.trace)
	edges, err := r.opts.DB.Edges(ctx)
	if err != nil {
		return "", err
	}
	tenants, err := r.opts.DB.RadsecOverviews(ctx)
	if err != nil {
		return "", err
	}
	p.readEdges(ctx, edges)
	p.reportOrphans(tenants)
	for _, t := range tenants {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		p.syncTenant(ctx, t)
	}
	p.finishBrought(ctx, tenants)
	p.disableReleased(ctx)
	for id, ev := range p.edges {
		if !ev.down {
			if err := r.opts.DB.TouchPlacements(ctx, id); err != nil {
				r.opts.Log.Warn("radsec: touch placements", "event_id", "RADSEC_SYNC_ERR", "edge", id, "error", err)
			}
		}
	}
	r.busy.Store(p.busy)
	if p.declared+p.placed+p.started+p.removed == 0 {
		return "", ctx.Err()
	}
	return fmt.Sprintf("宣言を送った数 %d、置いた証明書 %d、始めた反映 %d、エッジから消した数 %d。", p.declared, p.placed, p.started, p.removed), ctx.Err()
}

// readEdges は全エッジの一覧を並べて読む（届かないエッジで待たされないように）。
func (p *pass) readEdges(ctx context.Context, edges []db.Edge) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	errs := map[string]error{}
	for _, e := range edges {
		ev := &edgeView{row: e, tenants: map[int64]*provisioner.EdgeTenantSummary{}}
		p.edges[e.ID] = ev
		wg.Go(func() {
			list, err := p.r.opts.PV.EdgeTenants(ctx, e.ID)
			if err != nil {
				mu.Lock()
				errs[e.ID] = err
				mu.Unlock()
				return
			}
			for i := range list {
				s := &list[i]
				id, err := strconv.ParseInt(s.ID, 10, 64)
				if err != nil {
					continue // 管理モードのテナントの ID は数字（EA4）
				}
				ev.tenants[id] = s
				if s.Apply.State == stateRunning {
					ev.running++
					// 動いている間も前回の結果が返るので、読まない（前の失敗をもう一度数えない）。
					s.Apply.LastResult, s.Apply.LastError, s.Apply.LastRevision = "", "", 0
				}
			}
		})
	}
	wg.Wait()
	for _, e := range edges {
		if err, failed := errs[e.ID]; failed {
			p.markDown(p.edges[e.ID], err)
		} else {
			p.markUp(p.edges[e.ID])
		}
	}
	for id := range p.r.down {
		if _, ok := p.edges[id]; !ok {
			delete(p.r.down, id) // 台帳から消した
		}
	}
	p.busy = slices.ContainsFunc(edges, func(e db.Edge) bool { return p.edges[e.ID].running > 0 })
}

// markDown はこの周回ではエッジを飛ばす（ログは変わったときだけ。OB19）。
func (p *pass) markDown(ev *edgeView, err error) {
	ev.down = true
	if !p.r.down[ev.row.ID] {
		p.r.down[ev.row.ID] = true
		p.r.opts.Log.Warn("radsec: edge is unreachable", "event_id", "RADSEC_EDGE_UNREACHABLE", "edge", ev.row.ID, "error", err)
	}
}

func (p *pass) markUp(ev *edgeView) {
	if p.r.down[ev.row.ID] {
		delete(p.r.down, ev.row.ID)
		p.r.opts.Log.Info("radsec: edge is reachable", "event_id", "RADSEC_EDGE_REACHABLE", "edge", ev.row.ID)
	}
}

// reportOrphans はエッジにしかないテナントを報告する（消さない。D が画面で消せる。OB18）。
func (p *pass) reportOrphans(tenants []db.RadsecOverview) {
	// 同じ番号でも slug が違えば、台帳のテナントとは別のものである（DB を戻した後に番号が重なったとき。G11）。
	placed := map[string]string{}
	for _, t := range tenants {
		for _, pl := range t.Placements {
			placed[pl.EdgeID+"/"+strconv.FormatInt(t.TenantID, 10)] = t.Slug
		}
	}
	seen := map[string]bool{}
	for id, ev := range p.edges {
		if ev.down {
			// 届かない間は分からないので、覚えているものを残す。
			for k := range p.r.orphans {
				if strings.HasPrefix(k, id+"/") {
					seen[k] = true
				}
			}
			continue
		}
		for tenant, s := range ev.tenants {
			k := id + "/" + strconv.FormatInt(tenant, 10)
			if placed[k] == s.Slug {
				continue
			}
			seen[k] = true
			if !p.r.orphans[k] {
				p.r.opts.Log.Warn("radsec: tenant exists only on the edge", "event_id", "RADSEC_ORPHAN_TENANT", "edge", id, "tenant", tenant, "slug", s.Slug)
			}
		}
	}
	p.r.orphans = seen
}

// record は監査ログを書く。
func (p *pass) record(ctx context.Context, action, target string, tenant int64, detail map[string]any) {
	p.r.opts.Audit.Record(ctx, db.AuditEntry{ActorEmail: Operator, TenantID: tenant, Action: action, Target: target, Detail: detail, TraceID: p.trace})
}

// placementState は周回の中の 1 つの配置。
type placementState struct {
	pl *db.Placement
	ev *edgeView
	// s はエッジの要約（まだなければ nil）。failed は反映に失敗した版（0 はなし）。
	s      *provisioner.EdgeTenantSummary
	failed int64
	// syncErr はエッジや radsec-ca が断った誤り（ここで順を止め、次の周回でやり直す）。wait はエッジが忙しいので次の周回に回すこと。
	syncErr error
	wait    bool
}

// ok は続けて呼び出してよいか。
func (st *placementState) ok() bool { return !st.ev.down && !st.wait && st.syncErr == nil }

// stopped はエッジの画面でテナントを止めているか（飛ばす。OB2）。
func (st *placementState) stopped() bool { return st.s != nil && !st.s.Enabled }

// edgeErr はエッジの呼び出しの誤りを振り分ける。忙しければ待ち、結果が分からなければこの周回ではエッジを飛ばし、断られたら同期の誤りにする。
func (p *pass) edgeErr(st *placementState, err error) {
	switch {
	case provisioner.CauseOf(err) == provisioner.CauseTenantBusy:
		st.wait, p.busy = true, true
	case provisioner.ResultUnknown(err):
		p.markDown(st.ev, err)
	default:
		st.syncErr = err
	}
}

// lastFailed は最後の反映が失敗か（interrupted は自動でやり直すので失敗に数えない。OB2）。
func lastFailed(a provisioner.EdgeApply) bool {
	return a.LastResult == "failed" || a.LastResult == "rolled_back"
}

// failedRevision は反映に失敗した版を求める。最後の結果は CRL の更新などでも書き換わるので、失敗した版を覚えておく。その版以上に届いたら忘れる。
func failedRevision(pl *db.Placement, s *provisioner.EdgeTenantSummary) int64 {
	f := pl.FailedRevision
	if lastFailed(s.Apply) && s.Apply.LastRevision > s.AppliedRevision {
		f = max(f, s.Apply.LastRevision)
	}
	if f != 0 && s.AppliedRevision >= f {
		f = 0
	}
	return f
}

// syncTenant は 1 つのテナントを進める。宣言と証明書は全配置に送り（反映するまで接続に影響しない）、反映だけを順に 1 台ずつ行う。
func (p *pass) syncTenant(ctx context.Context, t db.RadsecOverview) {
	decl := &declParts{}
	var states []*placementState
	downSeen := false
	for i := range t.Placements {
		pl := &t.Placements[i]
		ev := p.edges[pl.EdgeID]
		if ev == nil {
			continue
		}
		if pl.Removing() {
			p.remove(ctx, t, pl, ev)
			continue
		}
		if ev.down {
			downSeen = true
			continue
		}
		st := &placementState{pl: pl, ev: ev, s: ev.tenants[t.TenantID], failed: pl.FailedRevision}
		states = append(states, st)
		if st.s != nil && st.s.Slug != t.Slug {
			// 別のテナントである。版が同じでも自分のものとして扱わず、人が消すまで何も送らない（刻印の取り違えを防ぐ。G11）。
			st.syncErr = fmt.Errorf("エッジに同じ番号で別のテナント（slug %s）があります。エッジの詳細の「エッジにしかないテナント」で消してください。", st.s.Slug)
			st.s = nil
			continue
		}
		if st.s != nil {
			st.failed = failedRevision(pl, st.s)
		}
		// 止めている・反映が動いているエッジには送らない（動いている間はテナントの排他を待たされる）。動き終わった次の周回で送る。
		if st.stopped() || (st.s != nil && st.s.Apply.State == stateRunning) {
			continue
		}
		if st.s != nil {
			p.finishCert(ctx, t, st)
		}
		p.declare(ctx, t, st, decl)
		p.certs(ctx, t, st)
	}
	p.rollout(ctx, t, states)
	if t.CRLDirty {
		p.refreshCRL(ctx, t, states, downSeen)
	}
	for _, st := range states {
		p.writeState(ctx, t, st)
	}
}

// remove は望む状態から外した配置をエッジから消す。
func (p *pass) remove(ctx context.Context, t db.RadsecOverview, pl *db.Placement, ev *edgeView) {
	if ev.down {
		return
	}
	// 同じ番号で slug の違うテナントは別のものなので消さない（エッジにしかないテナントとして報告する。G11）。
	if s, onEdge := ev.tenants[t.TenantID]; onEdge && s.Slug == t.Slug {
		st := &placementState{pl: pl, ev: ev}
		err := p.r.opts.PV.DeleteEdgeTenant(ctx, pl.EdgeID, t.TenantID, fmt.Sprintf("radsec-p%d-delete-%d", pl.ID, p.now.Unix()))
		if err != nil && provisioner.CauseOf(err) != provisioner.CauseTenantNotFound {
			p.edgeErr(st, err)
			if st.syncErr != nil {
				p.r.opts.Log.Warn("radsec: delete tenant from the edge", "event_id", "RADSEC_SYNC_ERR", "edge", pl.EdgeID, "tenant", t.TenantID, "error", err)
			}
			return
		}
	}
	if err := p.r.opts.DB.MarkPlacementRemoved(ctx, pl.ID); err != nil {
		p.r.opts.Log.Warn("radsec: mark placement removed", "event_id", "RADSEC_SYNC_ERR", "placement", pl.ID, "error", err)
		return
	}
	p.removed++
	p.record(ctx, audit.ActionLoopRemoved, pl.EdgeID, t.TenantID, map[string]any{"edge": pl.EdgeID, "slot": pl.Slot})
}

// finishCert は新しい証明書の反映が済んだ配置の古い証明書を失効させる（OB4）。周回の最初に見るので、この周回で送る宣言の pending と混ざらない。
func (p *pass) finishCert(ctx context.Context, t db.RadsecOverview, st *placementState) {
	pl, s := st.pl, st.s
	if (pl.CertApplyNeeded && s.Pending) || (!pl.CertApplyNeeded && len(pl.SupersededSerials) == 0) {
		return
	}
	var remaining []string
	for _, serial := range pl.SupersededSerials {
		_, err := p.r.opts.PV.RevokeCert(ctx, serial, provisioner.RevokeSuperseded, "radsec-revoke-"+serial)
		switch {
		case err == nil:
			p.record(ctx, audit.ActionLoopCertRevoke, serial, t.TenantID, map[string]any{"edge": pl.EdgeID, "reason": provisioner.RevokeSuperseded})
		case provisioner.CauseOf(err) == provisioner.CauseCertNotFound:
		default:
			remaining = append(remaining, serial)
			p.r.opts.Log.Warn("radsec: revoke the old certificate", "event_id", "RADSEC_SYNC_ERR", "edge", pl.EdgeID, "tenant", t.TenantID, "serial", serial, "error", err)
		}
	}
	if err := p.r.opts.DB.FinishCertApply(ctx, pl.ID, remaining); err != nil {
		p.r.opts.Log.Warn("radsec: finish cert apply", "event_id", "RADSEC_SYNC_ERR", "placement", pl.ID, "error", err)
		return
	}
	pl.CertApplyNeeded, pl.SupersededSerials = false, remaining
}

// declParts はテナントの宣言の材料（版が違う配置があるときだけ読み、周回の中で使い回す）。
type declParts struct {
	loaded  bool
	aps     []db.AP
	brought []db.BroughtCA
}

// Declaration はテナントの望む状態を宣言にする（上流はエッジごと。外している途中の持ち込みの CA は入れない。OB6）。
func Declaration(t db.TenantRadsec, e db.Edge, aps []db.AP, brought []db.BroughtCA, now time.Time) provisioner.EdgeDeclaration {
	d := provisioner.EdgeDeclaration{Revision: t.Revision, Slug: t.Slug, Port: t.Port, AuthUpstream: e.AuthUpstream, AcctUpstream: e.AcctUpstream,
		ClientCAs:   []provisioner.EdgeClientCA{{ID: "tenant." + t.Slug, Kind: "issued", Certificate: t.CACertificate, HasCRL: true}},
		SourceCIDRs: t.SourceCIDRs}
	for _, b := range brought {
		if !b.Removing() {
			d.ClientCAs = append(d.ClientCAs, provisioner.EdgeClientCA{ID: b.CAID, Kind: "brought", Certificate: b.Certificate, HasCRL: b.HasCRL})
		}
	}
	if t.MatchPattern != "" {
		d.SharedMatch = &provisioner.EdgeMatch{Attr: t.MatchAttr, Pattern: t.MatchPattern}
	}
	for _, a := range aps {
		ap := provisioner.EdgeAP{Name: a.Name, Kind: a.Kind, Purpose: a.Purpose}
		// 証明書の情報は表示だけ。有効なものの一番新しいものを入れる（Certs は新しい順）。
		if i := slices.IndexFunc(a.Certs, func(c db.APCert) bool { return c.Valid() && c.NotAfter.After(now) }); i >= 0 {
			ap.CertSerial, ap.CertNotAfter = a.Certs[i].Serial, a.Certs[i].NotAfter
		}
		d.APs = append(d.APs, ap)
	}
	return d
}

// declare は版が違えば宣言を送る。キーは本文のハッシュを含める（OB17）。さらに周回ごとに変える: aka-provisioner とエッジは同じキーの応答を 24 時間覚えるので、
// エッジがデータを失ったときに同じ宣言が再生されて届かない。宣言はまるごとの置き換えなので、結果が分からなくても次の周回で版を比べて送り直せばよい。
func (p *pass) declare(ctx context.Context, t db.RadsecOverview, st *placementState, parts *declParts) {
	if st.s != nil && st.s.Revision == t.Revision {
		return
	}
	if !parts.loaded {
		var err error
		if parts.aps, err = p.r.opts.DB.APs(ctx, t.TenantID); err == nil {
			parts.brought, err = p.r.opts.DB.BroughtCAs(ctx, t.TenantID)
		}
		if err != nil {
			st.syncErr = err
			return
		}
		parts.loaded = true
	}
	d := Declaration(t.TenantRadsec, st.ev.row, parts.aps, parts.brought, p.now)
	body, err := json.Marshal(d, json.Deterministic(true))
	if err != nil {
		st.syncErr = err
		return
	}
	sum := sha256.Sum256(body)
	got, err := p.r.opts.PV.PutEdgeTenant(ctx, st.pl.EdgeID, t.TenantID, d, fmt.Sprintf("radsec-p%d-put-%d-%s-%d", st.pl.ID, t.Revision, hex.EncodeToString(sum[:8]), p.now.Unix()))
	if err != nil {
		p.edgeErr(st, err)
		return
	}
	p.declared++
	st.s = &got.EdgeTenantSummary
	st.failed = failedRevision(st.pl, st.s)
	p.record(ctx, audit.ActionLoopDeclare, st.pl.EdgeID, t.TenantID, map[string]any{"edge": st.pl.EdgeID, "slot": st.pl.Slot, "revision": t.Revision, "aps": len(d.APs), "clientCas": len(d.ClientCAs)})
}

// certs は足りない証明書と期限の近い証明書を出す。エッジのデータを失ったときも同じ道を通る（OB4・OB18）。missing の文は読まず、詳細の項目で判定する。
func (p *pass) certs(ctx context.Context, t db.RadsecOverview, st *placementState) {
	if !st.ok() || st.s == nil {
		return
	}
	pl, s := st.pl, st.s
	var needServer, needProbe bool
	if len(s.Missing) > 0 || s.ProbeProblem != "" || pl.ServerSerial == "" || pl.ProbeSerial == "" {
		d, err := p.r.opts.PV.EdgeTenant(ctx, pl.EdgeID, t.TenantID)
		if err != nil {
			p.edgeErr(st, err)
			return
		}
		// 問題のある証明書は、前に出したものを反映するまで出し直さない（直らない問題で周回ごとに証明書を増やさない）。
		needServer, needProbe = d.ServerCert == nil, d.ProbeCert == nil || (s.ProbeProblem != "" && !pl.CertApplyNeeded)
		p.recordCert(ctx, pl, db.CertKindServer, d.ServerCert, &pl.ServerSerial, &pl.ServerNotAfter)
		p.recordCert(ctx, pl, db.CertKindProbe, d.ProbeCert, &pl.ProbeSerial, &pl.ProbeNotAfter)
	}
	if pl.RenewRequested {
		cut := p.now.Add(renewBefore)
		needServer = needServer || (!pl.ServerNotAfter.IsZero() && pl.ServerNotAfter.Before(cut))
		needProbe = needProbe || (!pl.ProbeNotAfter.IsZero() && pl.ProbeNotAfter.Before(cut))
	}
	if needServer {
		p.place(ctx, t, st, db.CertKindServer)
	}
	if needProbe && st.ok() {
		p.place(ctx, t, st, db.CertKindProbe)
	}
	if st.ok() && pl.RenewRequested {
		if err := p.r.opts.DB.ClearRenew(ctx, pl.ID); err != nil {
			st.syncErr = err
		}
	}
	p.warnExpiring(t, pl)
}

// recordCert はエッジにある証明書の控えが DB にないときに書く。
func (p *pass) recordCert(ctx context.Context, pl *db.Placement, kind string, c *provisioner.EdgeCertInfo, serial *string, notAfter *time.Time) {
	if c == nil || *serial != "" {
		return
	}
	if err := p.r.opts.DB.RecordPlacementCert(ctx, pl.ID, kind, c.Serial, c.NotAfter); err != nil {
		p.r.opts.Log.Warn("radsec: record placement cert", "event_id", "RADSEC_SYNC_ERR", "placement", pl.ID, "error", err)
		return
	}
	*serial, *notAfter = c.Serial, c.NotAfter
}

// place は CSR → 署名 → 配置を行う。キーは世代の番号を含めるので、結果が分からなかったらやり直しで同じものが返る（鍵と証明書を増やさない）。
func (p *pass) place(ctx context.Context, t db.RadsecOverview, st *placementState, kind string) {
	pl := st.pl
	key := func(step string) string {
		return fmt.Sprintf("radsec-p%d-%s-%s-%d", pl.ID, kind, step, pl.CertGeneration)
	}
	profile, serial, notAfter := provisioner.ProfileTenantServer, &pl.ServerSerial, &pl.ServerNotAfter
	csrFn, putFn := p.r.opts.PV.EdgeServerCSR, p.r.opts.PV.PutEdgeServerCert
	if kind == db.CertKindProbe {
		profile, serial, notAfter = provisioner.ProfileProbeClient, &pl.ProbeSerial, &pl.ProbeNotAfter
		csrFn, putFn = p.r.opts.PV.EdgeProbeCSR, p.r.opts.PV.PutEdgeProbeCert
	}
	// 断られたら世代を進める（4xx も覚えられるので、同じキーでは同じ誤りが返り続ける）。
	refused := func(err error) {
		if provisioner.ResultUnknown(err) {
			return
		}
		if err := p.r.opts.DB.BumpCertGeneration(ctx, pl.ID); err != nil {
			p.r.opts.Log.Warn("radsec: bump cert generation", "event_id", "RADSEC_SYNC_ERR", "placement", pl.ID, "error", err)
		}
	}
	csr, err := csrFn(ctx, pl.EdgeID, t.TenantID, key("csr"))
	if err != nil {
		refused(err)
		p.edgeErr(st, err)
		return
	}
	cert, err := p.r.opts.PV.IssueCert(ctx, provisioner.CertIssue{Profile: profile, Tenant: t.Slug, Edge: pl.EdgeID, CSR: csr}, key("issue"))
	if err != nil {
		refused(err)
		st.syncErr = fmt.Errorf("radsec-ca: %w", err)
		return
	}
	got, err := putFn(ctx, pl.EdgeID, t.TenantID, cert.Certificate, key("put"))
	if err != nil {
		refused(err)
		p.edgeErr(st, err)
		return
	}
	if err := p.r.opts.DB.PlacementCertPlaced(ctx, pl.ID, kind, cert.Serial, cert.NotAfter); err != nil {
		st.syncErr = err
		return
	}
	if *serial != "" && *serial != cert.Serial && !slices.Contains(pl.SupersededSerials, *serial) {
		pl.SupersededSerials = append(pl.SupersededSerials, *serial)
	}
	*serial, *notAfter = cert.Serial, cert.NotAfter
	pl.CertGeneration++
	pl.CertApplyNeeded = true
	st.s = &got.EdgeTenantSummary
	p.placed++
	p.record(ctx, audit.ActionLoopCertPlace, cert.Serial, t.TenantID, map[string]any{"edge": pl.EdgeID, "slot": pl.Slot, "kind": kind, "notAfter": cert.NotAfter})
}

// warnExpiring は自動の更新ができないまま期限が近づいた証明書を知らせる（変わったときだけ。OB19）。
func (p *pass) warnExpiring(t db.RadsecOverview, pl *db.Placement) {
	for _, c := range []struct {
		kind, serial string
		notAfter     time.Time
	}{{db.CertKindServer, pl.ServerSerial, pl.ServerNotAfter}, {db.CertKindProbe, pl.ProbeSerial, pl.ProbeNotAfter}} {
		k := fmt.Sprintf("%d/%s", pl.ID, c.kind)
		switch {
		case c.notAfter.IsZero() || c.notAfter.After(p.now.Add(WarnBefore)):
			delete(p.r.expiring, k)
		case !p.r.expiring[k]:
			p.r.expiring[k] = true
			p.r.opts.Log.Warn("radsec: edge certificate is expiring", "event_id", "RADSEC_CERT_EXPIRING", "edge", pl.EdgeID, "tenant", t.TenantID, "kind", c.kind, "serial", c.serial, "not_after", c.notAfter)
		}
	}
}

// applyReason は反映を始める理由を返す（要らなければ空）。OB1 の規則。
func applyReason(t db.TenantRadsec, pl *db.Placement, s *provisioner.EdgeTenantSummary) string {
	switch {
	case pl.RetryRequested:
		return "retry"
	case s.AppliedRevision > t.Revision:
		// DB を古いバックアップに戻したとき。自動で古い状態に合わせると、写しの後に足した AP が切れるので、人が「やり直す」を押すまで反映しない（G1）。
		return ""
	case s.AppliedRevision == 0:
		return "first"
	case s.AppliedRevision < t.RequestedRevision:
		return "requested"
	case s.Pending && s.Revision == t.Revision && t.RequestedRevision >= t.Revision:
		// DB を戻した後に版が同じ番号まで追いつくと、版を比べるだけでは未反映の変更を見分けられない。
		return "requested"
	case pl.CertApplyNeeded && s.Pending:
		return "cert"
	}
	return ""
}

// applyBlocked は失敗した版を自動でやり直さないかを返す（OB2）。人が新しい版の反映を求めるか、「やり直す」を押すと進む。
func applyBlocked(t db.TenantRadsec, st *placementState) bool {
	if st.pl.RetryRequested {
		return false
	}
	if st.failed != 0 && st.failed >= t.RequestedRevision {
		return true
	}
	return lastFailed(st.s.Apply) && st.s.Apply.LastRevision >= t.RequestedRevision
}

// rollout は配置の順に 1 台ずつ反映する。1 台が反映し終わるまで次へ進まず、失敗したら残りに送らない。
func (p *pass) rollout(ctx context.Context, t db.RadsecOverview, states []*placementState) {
	for _, st := range states {
		switch {
		case st.ev.down || st.stopped():
			continue // 飛ばす（OB2）
		case st.syncErr != nil || st.wait || st.s == nil:
			return
		case st.s.Apply.State == stateRunning:
			p.busy = true
			return
		}
		reason := applyReason(t.TenantRadsec, st.pl, st.s)
		if reason == "" {
			continue
		}
		if applyBlocked(t.TenantRadsec, st) {
			return
		}
		if st.ev.running >= maxApplyPerEdge {
			p.busy = true
			return
		}
		p.startApply(ctx, t, st, reason)
		if st.ev.down || st.stopped() {
			continue
		}
		return
	}
}

// startApply は反映を始める。実行のたびに新しいキーを使う（EA11）。
func (p *pass) startApply(ctx context.Context, t db.RadsecOverview, st *placementState, reason string) {
	pl := st.pl
	_, err := p.r.opts.PV.ApplyEdgeTenant(ctx, pl.EdgeID, t.TenantID, fmt.Sprintf("radsec-p%d-apply-%s", pl.ID, rand.Text()))
	if err != nil {
		if provisioner.CauseOf(err) == provisioner.CauseTenantStopped {
			st.s.Enabled = false
			return
		}
		p.edgeErr(st, err)
		return
	}
	if pl.RetryRequested || st.failed != 0 {
		if err := p.r.opts.DB.ClearRetry(ctx, pl.ID); err != nil {
			p.r.opts.Log.Warn("radsec: clear retry", "event_id", "RADSEC_SYNC_ERR", "placement", pl.ID, "error", err)
		}
		pl.RetryRequested, pl.FailedRevision, st.failed = false, 0, 0
	}
	st.ev.running++
	p.started++
	p.busy = true
	// 動き始めたので、前の結果を画面に残さない。
	st.s.Apply = provisioner.EdgeApply{State: stateRunning}
	p.record(ctx, audit.ActionLoopApplyStart, pl.EdgeID, t.TenantID, map[string]any{"edge": pl.EdgeID, "slot": pl.Slot, "revision": t.Revision, "requested": t.RequestedRevision, "reason": reason})
}

// refreshCRL は失効を各エッジにすぐ知らせる。全配置が発行した CA の CRL を取れるまで印を残す（OB18）。まだ反映していない配置は反映の最初に取る。
func (p *pass) refreshCRL(ctx context.Context, t db.RadsecOverview, states []*placementState, downSeen bool) {
	done := !downSeen
	issued := "tenant." + t.Slug
	var edges []string
	var failures []string
	for _, st := range states {
		switch {
		case st.ev.down || st.stopped():
			done = false
			continue
		case st.s == nil || st.s.AppliedRevision == 0:
			continue
		case st.s.Apply.State == stateRunning:
			done, p.busy = false, true
			continue
		}
		r, err := p.r.opts.PV.RefreshEdgeCRL(ctx, st.pl.EdgeID, t.TenantID, "radsec-p"+strconv.FormatInt(st.pl.ID, 10)+"-crl-"+rand.Text())
		if err != nil {
			done = false
			if provisioner.CauseOf(err) == provisioner.CauseTenantBusy {
				p.busy = true
				continue
			}
			if provisioner.ResultUnknown(err) {
				p.markDown(st.ev, err)
				continue
			}
			failures = append(failures, st.pl.EdgeID+": "+err.Error())
			continue
		}
		// 持ち込みの CA の取得の失敗では止めない（印はこちらの発行した CA の失効のためのもの）。
		i := slices.IndexFunc(r.Items, func(c provisioner.EdgeCRLState) bool { return c.CAID == issued })
		if (i >= 0 && !r.Items[i].OK) || (i < 0 && !r.OK) {
			done = false
			msg := r.Error
			if i >= 0 {
				msg = r.Items[i].LastError
			}
			failures = append(failures, st.pl.EdgeID+": "+msg)
			continue
		}
		edges = append(edges, st.pl.EdgeID)
	}
	switch {
	case len(failures) > 0 && !p.r.crlFail[t.TenantID]:
		p.r.crlFail[t.TenantID] = true
		p.r.opts.Log.Warn("radsec: refresh the crl on the edges", "event_id", "RADSEC_SYNC_ERR", "tenant", t.TenantID, "errors", failures)
	case len(failures) == 0:
		delete(p.r.crlFail, t.TenantID)
	}
	if !done {
		return
	}
	if err := p.r.opts.DB.ClearCRLDirty(ctx, t.TenantID, t.Revision); err != nil {
		p.r.opts.Log.Warn("radsec: clear crl dirty", "event_id", "RADSEC_SYNC_ERR", "tenant", t.TenantID, "error", err)
		return
	}
	p.record(ctx, audit.ActionLoopCRLRefresh, t.Slug, t.TenantID, map[string]any{"edges": edges})
}

// writeState はエッジの状態の写しを書く。変わったときだけ書き、失敗をログと監査ログに残す（OB19）。
func (p *pass) writeState(ctx context.Context, t db.RadsecOverview, st *placementState) {
	if st.ev.down || st.wait || (st.s == nil && st.syncErr == nil) {
		return
	}
	pl := st.pl
	applied, result, errText := pl.AppliedRevision, pl.ApplyResult, pl.ApplyError
	if st.s != nil {
		applied, result, errText = st.s.AppliedRevision, st.s.Apply.LastResult, st.s.Apply.LastError
		if st.stopped() {
			result, errText = db.ApplyResultStopped, ""
		}
	}
	if st.syncErr != nil {
		result, errText = db.ApplyResultSyncError, describe(st.syncErr)
	}
	errText = truncate(errText)
	if applied == pl.AppliedRevision && st.failed == pl.FailedRevision && result == pl.ApplyResult && errText == pl.ApplyError {
		return
	}
	if err := p.r.opts.DB.SetPlacementApply(ctx, pl.ID, applied, st.failed, result, errText); err != nil {
		p.r.opts.Log.Warn("radsec: write placement state", "event_id", "RADSEC_SYNC_ERR", "placement", pl.ID, "error", err)
		return
	}
	if applied > t.Revision && applied != pl.AppliedRevision {
		// 版が戻ったのは DB を古いバックアップに戻したとき。自動では反映しない（G1）。
		p.r.opts.Log.Warn("radsec: the edge has a newer revision than the database", "event_id", "RADSEC_SYNC_ERR", "edge", pl.EdgeID, "tenant", t.TenantID, "applied_revision", applied, "revision", t.Revision)
	}
	if result == pl.ApplyResult && errText == pl.ApplyError {
		return
	}
	log := p.r.opts.Log.With("edge", pl.EdgeID, "tenant", t.TenantID, "slot", pl.Slot, "applied_revision", applied, "error_text", errText)
	switch result {
	case "failed", "rolled_back":
		log.Error("radsec: apply failed", "event_id", "RADSEC_APPLY_FAILED", "result", result)
	case db.ApplyResultSyncError:
		log.Warn("radsec: cannot sync the edge", "event_id", "RADSEC_SYNC_ERR")
		return
	case resultOK, "interrupted":
		log.Info("radsec: apply finished", "result", result)
	default:
		return
	}
	p.record(ctx, audit.ActionLoopApplyResult, pl.EdgeID, t.TenantID, map[string]any{"edge": pl.EdgeID, "slot": pl.Slot, "result": result, "appliedRevision": applied})
}

// describe は誤りの文を返す（下流の詳しい文があれば、それを使う）。
func describe(err error) string {
	if e, ok := errors.AsType[*provisioner.Error](err); ok && e.Problem.Detail != "" {
		return e.Problem.Detail
	}
	return err.Error()
}

func truncate(s string) string {
	if r := []rune(s); len(r) > maxErrorLen {
		return string(r[:maxErrorLen])
	}
	return s
}

// finishBrought は全配置が外した版を反映した持ち込みの CA を radsec-ca で止める（OB6）。止まっているエッジを待つ。
func (p *pass) finishBrought(ctx context.Context, tenants []db.RadsecOverview) {
	list, err := p.r.opts.DB.RemovingBroughtCAs(ctx)
	if err != nil {
		p.r.opts.Log.Warn("radsec: removing brought cas", "event_id", "RADSEC_SYNC_ERR", "error", err)
		return
	}
	for _, b := range list {
		i := slices.IndexFunc(tenants, func(t db.RadsecOverview) bool { return t.TenantID == b.TenantID })
		if i < 0 {
			continue
		}
		reached := !slices.ContainsFunc(tenants[i].Placements, func(pl db.Placement) bool {
			return !pl.Removing() && pl.AppliedRevision < b.RemovingRevision
		})
		if !reached {
			continue
		}
		if _, err := p.r.opts.PV.DisableCA(ctx, b.CAID, "radsec-disable-"+b.CAID); err != nil && provisioner.CauseOf(err) != provisioner.CauseCANotFound {
			p.r.opts.Log.Warn("radsec: disable the brought ca", "event_id", "RADSEC_SYNC_ERR", "ca", b.CAID, "tenant", b.TenantID, "error", err)
			continue
		}
		if err := p.r.opts.DB.FinishRemoveBrought(ctx, b.CAID); err != nil {
			p.r.opts.Log.Warn("radsec: finish remove brought", "event_id", "RADSEC_SYNC_ERR", "ca", b.CAID, "error", err)
			continue
		}
		p.record(ctx, audit.ActionLoopBroughtOff, b.CAID, b.TenantID, map[string]any{"revision": b.RemovingRevision})
	}
}

// disableReleased は削除したテナントの CA を止め直す（画面で届かなかったとき。OB12）。
func (p *pass) disableReleased(ctx context.Context) {
	list, err := p.r.opts.DB.RadsecCADisablePending(ctx)
	if err != nil {
		p.r.opts.Log.Warn("radsec: released tenants", "event_id", "RADSEC_SYNC_ERR", "error", err)
		return
	}
	for _, t := range list {
		id := "tenant." + t.Slug
		if _, err := p.r.opts.PV.DisableCA(ctx, id, "disable-tenant-"+strconv.FormatInt(t.TenantID, 10)); err != nil && provisioner.CauseOf(err) != provisioner.CauseCANotFound {
			p.r.opts.Log.Warn("radsec: disable the ca of the deleted tenant", "event_id", "RADSEC_SYNC_ERR", "ca", id, "tenant", t.TenantID, "error", err)
			continue
		}
		if err := p.r.opts.DB.MarkRadsecCADisabled(ctx, t.TenantID); err != nil {
			p.r.opts.Log.Warn("radsec: mark ca disabled", "event_id", "RADSEC_SYNC_ERR", "tenant", t.TenantID, "error", err)
			continue
		}
		p.record(ctx, audit.ActionRadsecCADisable, id, t.TenantID, nil)
	}
}
