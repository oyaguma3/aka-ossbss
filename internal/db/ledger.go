package db

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// 加入者の台帳（設計概要 §7）と、課金レコードの状態の事実（課金レコード §5.1）。
// 台帳は aka-provisioner を通した操作が成功したときだけ書き、状態の事実と同じトランザクションで書く。
// 同じ IMSI の操作は、IMSI ごとの advisory lock で順に行う（画面と一括投入のジョブが同じ IMSI を同時に触らないように）。
// 台帳の移り方（Transition）は DB を使わない関数にして、画面の試験の偽物からも使う。

// Side は加入者のデータの側（aka-vecgen / aka-radius）。
type Side string

const (
	SideVecgen Side = "vecgen"
	SideRadius Side = "radius"
)

// Sides は両側（画面に出す順）。
var Sides = []Side{SideVecgen, SideRadius}

// 加入者の状態（aka-vecgen・aka-radius 共通）。
const (
	StateActive     = "active"
	StateInactive   = "inactive"
	StateTerminated = "terminated"
)

// ValidState は状態の値か。
func ValidState(s string) bool {
	return s == StateActive || s == StateInactive || s == StateTerminated
}

// Entry は台帳の 1 行。
type Entry struct {
	IMSI           string
	TenantID       int64
	VecgenPresent  bool
	VecgenState    string
	RadiusPresent  bool
	RadiusState    string
	RadiusSourceID string
	ExternalLineID string
	RegisteredAt   time.Time
	RegisteredBy   string
	UpdatedAt      time.Time
	// DeletedAt は両側とも物理削除した日時（ゼロ値は削除していない）。
	DeletedAt   time.Time
	CheckNeeded bool
}

// Deleted は削除済みか。
func (e Entry) Deleted() bool { return !e.DeletedAt.IsZero() }

// Present はその側にあるか。
func (e Entry) Present(s Side) bool {
	if s == SideVecgen {
		return e.VecgenPresent
	}
	return e.RadiusPresent
}

// State はその側の状態（なければ空）。
func (e Entry) State(s Side) string {
	if s == SideVecgen {
		return e.VecgenState
	}
	return e.RadiusState
}

// Pending は、登録の結果を確かめている行（両側ともなく、削除済みでもない）か。
func (e Entry) Pending() bool { return !e.VecgenPresent && !e.RadiusPresent && !e.Deleted() }

// Mismatch は、片側だけにあるか、両側の状態が違うか（設計概要 §7.3 の食い違い）。
func (e Entry) Mismatch() bool {
	if e.VecgenPresent != e.RadiusPresent {
		return true
	}
	return e.VecgenPresent && e.VecgenState != e.RadiusState
}

func (e *Entry) setSide(s Side, present bool, state string) {
	if s == SideVecgen {
		e.VecgenPresent, e.VecgenState = present, state
		return
	}
	e.RadiusPresent, e.RadiusState = present, state
	if !present {
		e.RadiusSourceID = ""
	}
}

// 状態の事実の種類（課金レコード §7.7 の event）。
const (
	EventTenantCreated    = "tenant_created"
	EventTenantUpdated    = "tenant_updated"
	EventTenantDeleted    = "tenant_deleted"
	EventLineRegistered   = "line_registered"
	EventLineStateChanged = "line_state_changed"
	EventLineTenantMoved  = "line_tenant_moved"
	EventLineDeleted      = "line_deleted"
	EventReconciled       = "reconciled"
)

// ChargingEvent は状態の事実の 1 件（charging_events の 1 行）。
type ChargingEvent struct {
	ID    int64
	Time  time.Time
	Event string
	// Side は回線の事実のときの側（付け替えとテナントの事実では空）。
	Side     Side
	TenantID int64
	// IMSI は回線の事実のときの IMSI（テナントの事実では空）。
	IMSI     string
	From, To string
	Operator string
	JobID    string
	TraceID  string
	Reason   string
}

// Meta は操作の主体（状態の事実に書く）。
type Meta struct {
	Operator string
	TraceID  string
	// JobID は一括投入のジョブ（段階 3-2。画面の操作では空）。
	JobID string
}

// ChangeKind は台帳の変え方。
type ChangeKind int

const (
	// ChangeRegister はその側の登録（Side、State、radius では SourceID。台帳にない・削除済みなら TenantID も）。
	ChangeRegister ChangeKind = iota + 1
	// ChangeState はその側の状態の変更（Side、State、Reason。Terminated からの戻しを含む）。
	ChangeState
	// ChangeDelete はその側の物理削除（Side）。両側ともなくなったら削除済みにする。
	ChangeDelete
	// ChangeSource は radius 側の取得先の変更（SourceID）。
	ChangeSource
	// ChangeMove は所属のテナントの付け替え（TenantID）。
	ChangeMove
	// ChangeCheckNeeded は結果を確かめる印を立てる（台帳にない IMSI の登録なら TenantID で行を作る）。
	ChangeCheckNeeded
	// ChangeObserve は、下流から読んだ両側（Vecgen・Radius）に台帳を合わせ、違いを補いの事実として記録し、印を下ろす
	// （Reason は補いの理由。台帳にない IMSI なら TenantID で行を作る）。
	ChangeObserve
)

// Observation は下流から読んだ 1 つの側。
type Observation struct {
	Present  bool
	State    string
	SourceID string
}

// Change は台帳の変更の内容。
type Change struct {
	Kind     ChangeKind
	Side     Side
	TenantID int64
	State    string
	SourceID string
	Reason   string
	// Vecgen と Radius は ChangeObserve で読んだ両側。
	Vecgen, Radius Observation
}

var (
	// ErrLedgerConflict は、台帳の今の値ではその変更ができないこと（既に登録済み、ない側の操作など）。
	ErrLedgerConflict = errors.New("ledger conflict")
	// ErrTenantNotFound は、属させるテナントがない・削除済みであること。
	ErrTenantNotFound = errors.New("tenant not found")
)

// Result は Transition の結果。
type Result struct {
	Entry  Entry
	Events []ChargingEvent
	// Remove は行を消すこと（登録の結果を確かめたら、どちらの側にもなかった）。
	Remove bool
	// Changed は台帳の行が変わったか（変わらなければ書かない）。
	Changed bool
}

// Transition は、台帳の今の行（exists が false なら、まだない）に変更を当てた結果を返す。DB を使わない。
// 状態の事実の時刻と台帳の更新の時刻は now にする。
func Transition(cur Entry, exists bool, ch Change, meta Meta, now time.Time) (Result, error) {
	next := cur
	var events []ChargingEvent
	event := func(name string, side Side, from, to, reason string) {
		events = append(events, ChargingEvent{Time: now, Event: name, Side: side, TenantID: next.TenantID, IMSI: next.IMSI,
			From: from, To: to, Operator: meta.Operator, JobID: meta.JobID, TraceID: meta.TraceID, Reason: reason})
	}
	// fresh は、台帳にない・削除済みで、新しく登録を始める行にする（所属のテナントを選び直す）。
	fresh := func(tenant int64) error {
		if tenant < 1 {
			return fmt.Errorf("%w: tenant is required for %s", ErrLedgerConflict, cur.IMSI)
		}
		next = Entry{IMSI: cur.IMSI, TenantID: tenant, ExternalLineID: cur.ExternalLineID, RegisteredAt: now,
			RegisteredBy: meta.Operator, CheckNeeded: exists && cur.CheckNeeded}
		return nil
	}
	sideOK := func() error {
		if ch.Side != SideVecgen && ch.Side != SideRadius {
			return fmt.Errorf("%w: unknown side %q", ErrLedgerConflict, ch.Side)
		}
		return nil
	}

	switch ch.Kind {
	case ChangeRegister:
		if err := sideOK(); err != nil {
			return Result{}, err
		}
		if !ValidState(ch.State) || (ch.Side == SideRadius && ch.SourceID == "") {
			return Result{}, fmt.Errorf("%w: invalid registration", ErrLedgerConflict)
		}
		switch {
		case !exists || cur.Deleted():
			if err := fresh(ch.TenantID); err != nil {
				return Result{}, err
			}
		case cur.Present(ch.Side):
			return Result{}, fmt.Errorf("%w: %s is already registered on %s", ErrLedgerConflict, cur.IMSI, ch.Side)
		case ch.TenantID != 0 && ch.TenantID != cur.TenantID:
			return Result{}, fmt.Errorf("%w: %s belongs to tenant %d", ErrLedgerConflict, cur.IMSI, cur.TenantID)
		case cur.Pending():
			// 登録の結果を確かめている行に、登録が成功した（最初の登録として扱う）。
			next.RegisteredAt, next.RegisteredBy = now, meta.Operator
		}
		next.setSide(ch.Side, true, ch.State)
		if ch.Side == SideRadius {
			next.RadiusSourceID = ch.SourceID
		}
		event(EventLineRegistered, ch.Side, "", ch.State, ch.Reason)

	case ChangeState:
		if err := sideOK(); err != nil {
			return Result{}, err
		}
		if !exists || !cur.Present(ch.Side) || !ValidState(ch.State) {
			return Result{}, fmt.Errorf("%w: %s is not registered on %s", ErrLedgerConflict, cur.IMSI, ch.Side)
		}
		from := cur.State(ch.Side)
		if from == ch.State {
			return Result{Entry: cur}, nil
		}
		next.setSide(ch.Side, true, ch.State)
		event(EventLineStateChanged, ch.Side, from, ch.State, ch.Reason)

	case ChangeDelete:
		if err := sideOK(); err != nil {
			return Result{}, err
		}
		if !exists || !cur.Present(ch.Side) {
			return Result{}, fmt.Errorf("%w: %s is not registered on %s", ErrLedgerConflict, cur.IMSI, ch.Side)
		}
		event(EventLineDeleted, ch.Side, cur.State(ch.Side), "", ch.Reason)
		next.setSide(ch.Side, false, "")
		if !next.VecgenPresent && !next.RadiusPresent {
			next.DeletedAt = now
		}

	case ChangeSource:
		if !exists || !cur.RadiusPresent || ch.SourceID == "" {
			return Result{}, fmt.Errorf("%w: %s is not registered on radius", ErrLedgerConflict, cur.IMSI)
		}
		if cur.RadiusSourceID == ch.SourceID {
			return Result{Entry: cur}, nil
		}
		next.RadiusSourceID = ch.SourceID

	case ChangeMove:
		if !exists || cur.Deleted() || ch.TenantID < 1 {
			return Result{}, fmt.Errorf("%w: %s cannot be moved", ErrLedgerConflict, cur.IMSI)
		}
		if cur.TenantID == ch.TenantID {
			return Result{Entry: cur}, nil
		}
		next.TenantID = ch.TenantID
		event(EventLineTenantMoved, "", strconv.FormatInt(cur.TenantID, 10), strconv.FormatInt(ch.TenantID, 10), ch.Reason)

	case ChangeCheckNeeded:
		if !exists {
			if err := fresh(ch.TenantID); err != nil {
				return Result{}, err
			}
		}
		if exists && cur.CheckNeeded {
			return Result{Entry: cur}, nil
		}
		next.CheckNeeded = true

	case ChangeObserve:
		seen := ch.Vecgen.Present || ch.Radius.Present
		switch {
		case !exists && !seen:
			return Result{Entry: cur}, nil
		case !exists || (cur.Deleted() && seen):
			if err := fresh(cmp.Or(ch.TenantID, cur.TenantID)); err != nil {
				return Result{}, err
			}
		}
		wasPresent := next.VecgenPresent || next.RadiusPresent
		for _, s := range Sides {
			o := ch.Vecgen
			if s == SideRadius {
				o = ch.Radius
			}
			if o.Present && !ValidState(o.State) {
				return Result{}, fmt.Errorf("%w: invalid observed state %q", ErrLedgerConflict, o.State)
			}
			if o.Present != next.Present(s) || o.State != next.State(s) {
				event(EventReconciled, s, next.State(s), o.State, ch.Reason)
				next.setSide(s, o.Present, o.State)
			}
		}
		if ch.Radius.Present {
			next.RadiusSourceID = ch.Radius.SourceID
		}
		next.CheckNeeded = false
		if !seen {
			if !wasPresent && !next.Deleted() {
				// 登録の結果を確かめたら、どちらの側にもなかった（登録は行われなかった）。
				return Result{Entry: next, Events: events, Remove: true, Changed: true}, nil
			}
			if !next.Deleted() {
				next.DeletedAt = now
			}
		} else {
			next.DeletedAt = time.Time{}
		}

	default:
		return Result{}, fmt.Errorf("%w: unknown change %d", ErrLedgerConflict, ch.Kind)
	}
	changed := !exists || next != cur
	if changed {
		next.UpdatedAt = now
	}
	return Result{Entry: next, Events: events, Changed: changed}, nil
}

// Ledger は 1 つの IMSI の台帳の操作（WithSubscriber の中）。
type Ledger interface {
	// Entry は台帳の今の行を返す（なければ false）。
	Entry() (Entry, bool)
	// Apply は変更を当て、台帳と状態の事実を書く。
	Apply(ch Change) error
}

// subscriberLockClass は、IMSI ごとの advisory lock の 1 つ目の番号（任意の固定値。2 つ目は IMSI のハッシュ）。
const subscriberLockClass = 0x0553_0002

// WithSubscriber は、IMSI ごとの advisory lock を取ったトランザクションの中で fn を呼ぶ。fn は台帳の行を読み、
// 下流を呼び、成功したら Apply で台帳と状態の事実を書く。fn がエラーを返したら何も書かない（ロールバック）。
// 要求の取り消しで下流の成功を書き損じないよう、呼び出し側は取り消されないコンテキストを渡す。
func (d *DB) WithSubscriber(ctx context.Context, imsi string, meta Meta, fn func(Ledger) error) error {
	return pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1::int, hashtext($2))", subscriberLockClass, imsi); err != nil {
			return fmt.Errorf("lock subscriber: %w", err)
		}
		l := &ledgerTx{ctx: ctx, tx: tx, meta: meta}
		e, err := scanEntry(tx.QueryRow(ctx, entrySelect+" WHERE imsi = $1 FOR UPDATE", imsi))
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			l.cur = Entry{IMSI: imsi}
		case err != nil:
			return fmt.Errorf("read subscriber: %w", err)
		default:
			l.cur, l.exists = e, true
		}
		return fn(l)
	})
}

// ledgerTx は WithSubscriber のトランザクションの中の台帳。
type ledgerTx struct {
	ctx    context.Context
	tx     pgx.Tx
	meta   Meta
	cur    Entry
	exists bool
}

func (l *ledgerTx) Entry() (Entry, bool) { return l.cur, l.exists }

func (l *ledgerTx) Apply(ch Change) error {
	var now time.Time
	if err := l.tx.QueryRow(l.ctx, "SELECT now()").Scan(&now); err != nil {
		return err
	}
	res, err := Transition(l.cur, l.exists, ch, l.meta, now)
	if err != nil {
		return err
	}
	// 行を新しくテナントに属させるとき（登録、削除済みからの登録、付け替え）は、テナントが削除されていないことを確かめ、
	// 行のロック（FOR KEY SHARE）でテナントの削除と順にする（DeleteTenant が数えた後に属させない）。
	if e := res.Entry; res.Changed && !res.Remove && !e.Deleted() && (!l.exists || l.cur.Deleted() || l.cur.TenantID != e.TenantID) {
		var live bool
		err := l.tx.QueryRow(l.ctx, "SELECT deleted_at IS NULL FROM tenants WHERE id = $1 FOR KEY SHARE", e.TenantID).Scan(&live)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !live) {
			return fmt.Errorf("%w: tenant %d", ErrTenantNotFound, e.TenantID)
		}
		if err != nil {
			return err
		}
	}
	if err := writeEntry(l.ctx, l.tx, res, l.exists); err != nil {
		return err
	}
	if err := insertChargingEvents(l.ctx, l.tx, res.Events); err != nil {
		return err
	}
	l.cur, l.exists = res.Entry, !res.Remove
	if res.Remove {
		l.cur = Entry{IMSI: l.cur.IMSI}
	}
	return nil
}

// nullText は空文字列を NULL にする。
func nullText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// nullTime はゼロ値を NULL にする。
func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// writeEntry は Transition の結果の行を書く。
func writeEntry(ctx context.Context, tx pgx.Tx, res Result, existed bool) error {
	e := res.Entry
	var err error
	switch {
	case res.Remove:
		_, err = tx.Exec(ctx, "DELETE FROM subscribers WHERE imsi = $1", e.IMSI)
	case !res.Changed:
		return nil
	case !existed:
		_, err = tx.Exec(ctx, `
			INSERT INTO subscribers (imsi, tenant_id, vecgen_present, vecgen_state, radius_present, radius_state, radius_source_id,
				external_line_id, registered_at, registered_by, updated_at, deleted_at, check_needed)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
			e.IMSI, e.TenantID, e.VecgenPresent, nullText(e.VecgenState), e.RadiusPresent, nullText(e.RadiusState),
			nullText(e.RadiusSourceID), e.ExternalLineID, e.RegisteredAt, e.RegisteredBy, e.UpdatedAt, nullTime(e.DeletedAt), e.CheckNeeded)
	default:
		_, err = tx.Exec(ctx, `
			UPDATE subscribers SET tenant_id = $2, vecgen_present = $3, vecgen_state = $4, radius_present = $5, radius_state = $6,
				radius_source_id = $7, external_line_id = $8, registered_at = $9, registered_by = $10, updated_at = $11,
				deleted_at = $12, check_needed = $13
			WHERE imsi = $1`,
			e.IMSI, e.TenantID, e.VecgenPresent, nullText(e.VecgenState), e.RadiusPresent, nullText(e.RadiusState),
			nullText(e.RadiusSourceID), e.ExternalLineID, e.RegisteredAt, e.RegisteredBy, e.UpdatedAt, nullTime(e.DeletedAt), e.CheckNeeded)
	}
	if err != nil {
		return fmt.Errorf("write subscriber %s: %w", e.IMSI, err)
	}
	return nil
}

// insertChargingEvents は状態の事実を書く（台帳と同じトランザクションで）。
func insertChargingEvents(ctx context.Context, tx pgx.Tx, events []ChargingEvent) error {
	for _, ev := range events {
		_, err := tx.Exec(ctx, `
			INSERT INTO charging_events (time, event, side, tenant_id, imsi, from_val, to_val, operator, job_id, trace_id, reason)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			ev.Time, ev.Event, nullText(string(ev.Side)), ev.TenantID, nullText(ev.IMSI), ev.From, ev.To, ev.Operator,
			ev.JobID, ev.TraceID, ev.Reason)
		if err != nil {
			return fmt.Errorf("insert charging event %s: %w", ev.Event, err)
		}
	}
	return nil
}

// ChargingEvents は状態の事実を番号の順に返す（after より後ろから limit 件。imsi が空でなければその IMSI だけ）。
// 試験と、段階 3-4 の月次の計算の前の確かめに使う。
func (d *DB) ChargingEvents(ctx context.Context, imsi string, after int64, limit int) ([]ChargingEvent, error) {
	rows, err := d.Pool.Query(ctx, `
		SELECT id, time, event, coalesce(side, ''), tenant_id, coalesce(imsi, ''), from_val, to_val, operator, job_id, trace_id, reason
		FROM charging_events WHERE ($1 = '' OR imsi = $1) AND id > $2 ORDER BY id LIMIT $3`, imsi, after, limit)
	if err != nil {
		return nil, fmt.Errorf("charging events: %w", err)
	}
	var out []ChargingEvent
	var ev ChargingEvent
	if _, err := pgx.ForEachRow(rows, []any{&ev.ID, &ev.Time, &ev.Event, &ev.Side, &ev.TenantID, &ev.IMSI, &ev.From, &ev.To,
		&ev.Operator, &ev.JobID, &ev.TraceID, &ev.Reason}, func() error {
		out = append(out, ev)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("charging events: %w", err)
	}
	return out, nil
}

// ---- 読み出し ----

// entrySelect は台帳の行を読む SELECT。scanEntry で読む。
const entrySelect = `
	SELECT imsi, tenant_id, vecgen_present, coalesce(vecgen_state, ''), radius_present, coalesce(radius_state, ''),
		coalesce(radius_source_id, ''), external_line_id, registered_at, registered_by, updated_at, deleted_at, check_needed
	FROM subscribers`

func scanEntry(row pgx.Row) (Entry, error) {
	var e Entry
	var deleted *time.Time
	if err := row.Scan(&e.IMSI, &e.TenantID, &e.VecgenPresent, &e.VecgenState, &e.RadiusPresent, &e.RadiusState, &e.RadiusSourceID,
		&e.ExternalLineID, &e.RegisteredAt, &e.RegisteredBy, &e.UpdatedAt, &deleted, &e.CheckNeeded); err != nil {
		return Entry{}, err
	}
	if deleted != nil {
		e.DeletedAt = *deleted
	}
	return e, nil
}

// Subscriber は IMSI の台帳の行を返す（削除済みを含む）。なければ ErrNotFound。
func (d *DB) Subscriber(ctx context.Context, imsi string) (Entry, error) {
	e, err := scanEntry(d.Pool.QueryRow(ctx, entrySelect+" WHERE imsi = $1", imsi))
	if errors.Is(err, pgx.ErrNoRows) {
		return Entry{}, ErrNotFound
	}
	if err != nil {
		return Entry{}, fmt.Errorf("subscriber: %w", err)
	}
	return e, nil
}

// 一覧の状態の絞り込みの値（状態の値のほかに）。
const (
	// FilterAbsent はその側にないもの。
	FilterAbsent = "absent"
)

// SubscriberFilter は台帳の一覧の絞り込み（設計概要 §7.3。W5）。ゼロ値の項目は絞らない。
type SubscriberFilter struct {
	// TenantID が 0 でなければそのテナントだけ。
	TenantID int64
	// Prefix は IMSI の前方一致。
	Prefix string
	// VecgenState と RadiusState は状態（active / inactive / terminated）か FilterAbsent。
	VecgenState, RadiusState string
	// SourceID は radius 側の取得先。
	SourceID string
	// Mismatch は食い違い（片側だけ・状態が違う）だけ。
	Mismatch bool
	// Deleted は削除済みだけ。偽なら削除済みを除く。
	Deleted bool
	// CheckNeeded は結果を確かめている印のあるものだけ。
	CheckNeeded bool
}

// where は絞り込みの WHERE 句と引数を作る。
func (f SubscriberFilter) where() (string, []any) {
	var conds []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		conds = append(conds, strings.ReplaceAll(cond, "$?", "$"+strconv.Itoa(len(args))))
	}
	if f.TenantID != 0 {
		add("tenant_id = $?", f.TenantID)
	}
	if f.Prefix != "" {
		add("imsi LIKE $?", likePrefix(f.Prefix))
	}
	for _, s := range []struct{ col, v string }{{"vecgen", f.VecgenState}, {"radius", f.RadiusState}} {
		switch s.v {
		case "":
		case FilterAbsent:
			conds = append(conds, "NOT "+s.col+"_present")
		default:
			add(s.col+"_state = $?", s.v)
		}
	}
	if f.SourceID != "" {
		add("radius_source_id = $?", f.SourceID)
	}
	if f.Mismatch {
		conds = append(conds, "(vecgen_present <> radius_present OR (vecgen_present AND vecgen_state <> radius_state))")
	}
	if f.Deleted {
		conds = append(conds, "deleted_at IS NOT NULL")
	} else {
		conds = append(conds, "deleted_at IS NULL")
	}
	if f.CheckNeeded {
		conds = append(conds, "check_needed")
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// likePrefix は前方一致の LIKE のパターンを作る（IMSI の前方一致は数字だけなので、エスケープの要る文字は含まないが念のため除く）。
func likePrefix(prefix string) string {
	return strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(prefix) + "%"
}

// ListSubscribers は台帳の行を IMSI の順に、after より後ろから limit 件まで返す。
func (d *DB) ListSubscribers(ctx context.Context, f SubscriberFilter, after string, limit int) ([]Entry, error) {
	where, args := f.where()
	args = append(args, after, limit)
	rows, err := d.Pool.Query(ctx, fmt.Sprintf("%s%s AND imsi > $%d ORDER BY imsi LIMIT $%d", entrySelect, where, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, fmt.Errorf("list subscribers: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Entry, error) { return scanEntry(row) })
	if err != nil {
		return nil, fmt.Errorf("list subscribers: %w", err)
	}
	return out, nil
}

// CountSubscribers は絞り込みに当たる台帳の行の数を返す。
func (d *DB) CountSubscribers(ctx context.Context, f SubscriberFilter) (int64, error) {
	where, args := f.where()
	var n int64
	if err := d.Pool.QueryRow(ctx, "SELECT count(*) FROM subscribers"+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count subscribers: %w", err)
	}
	return n, nil
}

// SubscriberSources は台帳にある radius 側の取得先の ID を名前の順に返す（一覧の絞り込みの選択肢）。
func (d *DB) SubscriberSources(ctx context.Context, tenantID int64) ([]string, error) {
	rows, err := d.Pool.Query(ctx, `
		SELECT DISTINCT radius_source_id FROM subscribers
		WHERE radius_source_id IS NOT NULL AND ($1 = 0 OR tenant_id = $1) ORDER BY 1`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("subscriber sources: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("subscriber sources: %w", err)
	}
	return out, nil
}

// LedgerCounts は台帳の件数（ダッシュボード。設計概要 §7.3）。削除済みは Deleted だけに数える。
type LedgerCounts struct {
	Total int64
	// Vecgen と Radius は側ごとの状態の数（キーは状態と FilterAbsent）。
	Vecgen, Radius map[string]int64
	Mismatch       int64
	CheckNeeded    int64
	Deleted        int64
}

// CountLedger は台帳の件数を数える（tenantID が 0 でなければそのテナントだけ）。
func (d *DB) CountLedger(ctx context.Context, tenantID int64) (LedgerCounts, error) {
	c := LedgerCounts{Vecgen: map[string]int64{}, Radius: map[string]int64{}}
	var va, vi, vt, vn, ra, ri, rt, rn int64
	err := d.Pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE deleted_at IS NULL),
			count(*) FILTER (WHERE deleted_at IS NULL AND vecgen_state = 'active'),
			count(*) FILTER (WHERE deleted_at IS NULL AND vecgen_state = 'inactive'),
			count(*) FILTER (WHERE deleted_at IS NULL AND vecgen_state = 'terminated'),
			count(*) FILTER (WHERE deleted_at IS NULL AND NOT vecgen_present),
			count(*) FILTER (WHERE deleted_at IS NULL AND radius_state = 'active'),
			count(*) FILTER (WHERE deleted_at IS NULL AND radius_state = 'inactive'),
			count(*) FILTER (WHERE deleted_at IS NULL AND radius_state = 'terminated'),
			count(*) FILTER (WHERE deleted_at IS NULL AND NOT radius_present),
			count(*) FILTER (WHERE deleted_at IS NULL AND (vecgen_present <> radius_present OR (vecgen_present AND vecgen_state <> radius_state))),
			count(*) FILTER (WHERE check_needed),
			count(*) FILTER (WHERE deleted_at IS NOT NULL)
		FROM subscribers WHERE $1 = 0 OR tenant_id = $1`, tenantID).
		Scan(&c.Total, &va, &vi, &vt, &vn, &ra, &ri, &rt, &rn, &c.Mismatch, &c.CheckNeeded, &c.Deleted)
	if err != nil {
		return LedgerCounts{}, fmt.Errorf("count ledger: %w", err)
	}
	c.Vecgen[StateActive], c.Vecgen[StateInactive], c.Vecgen[StateTerminated], c.Vecgen[FilterAbsent] = va, vi, vt, vn
	c.Radius[StateActive], c.Radius[StateInactive], c.Radius[StateTerminated], c.Radius[FilterAbsent] = ra, ri, rt, rn
	return c, nil
}
