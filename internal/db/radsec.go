package db

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// RadSec の受け口の望む状態（設計概要 §13。段階 4-3 の OB10）。ここを書き換えて版（revision）を上げるだけで、エッジには触らない（OB3）。
// エッジへ送るのは中央の整合のループ（段階 4-4）の役目。

const (
	// RadsecPortMin・RadsecPortMax はテナントのポートの範囲（決定事項 9）。
	RadsecPortMin = 20000
	RadsecPortMax = 21999
	// MaxAPs は 1 つのテナントの AP とグループの数の上限（エッジの宣言の上限）。
	MaxAPs = 2000

	// radsecPortLock はポートの割り当てを 1 つずつにするロックの番号。
	radsecPortLock = 7304005
)

var (
	// ErrNoPort はテナントのポートの範囲に空きがないこと。
	ErrNoPort = errors.New("no free tenant port")
	// ErrCapacity はエッジの容量を超えること（OB13）。
	ErrCapacity = errors.New("edge capacity exceeded")
	// ErrAPLimit はテナントの AP とグループの数の上限を超えること。
	ErrAPLimit = errors.New("too many APs in the tenant")
	// ErrNASIDUsed は NAS-Identifier が別のテナントの AP と重なること（OB8）。
	ErrNASIDUsed = errors.New("NAS-Identifier is already used")
	// ErrNameRetired は CRL のないテナントで、削除した名前を使い回そうとしたこと（OB5）。
	ErrNameRetired = errors.New("the name was deleted in a tenant without a CRL")
	// ErrNotReady は移動の完了を、新しい配置が反映する前に求めたこと（OB7）。
	ErrNotReady = errors.New("the new placement has not been applied")
)

// radsecSlots はスロットの順（反映もこの順。OB2）。
var radsecSlots = []string{"primary", "secondary", "tertiary"}

// RadsecSlots はスロットの一覧を返す。
func RadsecSlots() []string { return slices.Clone(radsecSlots) }

// ---- エッジの台帳 ----

// Edge はエッジ。
type Edge struct {
	ID, Name   string
	Capacity   int
	PublicAddr string
	// AuthUpstream・AcctUpstream は上流の認証と課金（IP アドレス:ポート）。
	AuthUpstream, AcctUpstream string
	CreatedAt                  time.Time
	CreatedBy                  string
	// Placements はこのエッジを占めている配置の数。
	Placements int
}

const edgeSelect = `SELECT e.id, e.name, e.capacity, e.public_addr, e.auth_upstream, e.acct_upstream, e.created_at, e.created_by,
	(SELECT count(*) FROM placements p WHERE p.edge_id = e.id AND p.removed_at IS NULL)
	FROM edges e WHERE e.deleted_at IS NULL`

func scanEdge(row pgx.Row) (Edge, error) {
	var e Edge
	err := row.Scan(&e.ID, &e.Name, &e.Capacity, &e.PublicAddr, &e.AuthUpstream, &e.AcctUpstream, &e.CreatedAt, &e.CreatedBy, &e.Placements)
	return e, err
}

// Edges は台帳のエッジを ID の順に返す。
func (d *DB) Edges(ctx context.Context) ([]Edge, error) {
	rows, err := d.Pool.Query(ctx, edgeSelect+" ORDER BY e.id")
	if err != nil {
		return nil, fmt.Errorf("edges: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Edge, error) { return scanEdge(row) })
	if err != nil {
		return nil, fmt.Errorf("edges: %w", err)
	}
	return out, nil
}

// EdgeByID はエッジを返す。ない・消したものなら ErrNotFound。
func (d *DB) EdgeByID(ctx context.Context, id string) (Edge, error) {
	e, err := scanEdge(d.Pool.QueryRow(ctx, edgeSelect+" AND e.id = $1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Edge{}, ErrNotFound
	}
	if err != nil {
		return Edge{}, fmt.Errorf("edge: %w", err)
	}
	return e, nil
}

// CreateEdge はエッジを台帳に足す（消した ID なら行を生き返らせる）。ID が使われていれば ErrConflict。
func (d *DB) CreateEdge(ctx context.Context, e Edge, by string) error {
	tag, err := d.Pool.Exec(ctx, `INSERT INTO edges (id, name, capacity, public_addr, auth_upstream, acct_upstream, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (id) DO UPDATE SET name = $2, capacity = $3, public_addr = $4, auth_upstream = $5, acct_upstream = $6,
			created_by = $7, created_at = now(), updated_at = now(), deleted_at = NULL
		WHERE edges.deleted_at IS NOT NULL`, e.ID, e.Name, e.Capacity, e.PublicAddr, e.AuthUpstream, e.AcctUpstream, by)
	if err != nil {
		return fmt.Errorf("create edge: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrConflict
	}
	return nil
}

// UpdateEdge はエッジの設定を変える。上流が変わったら、置いている全テナントの版を上げて反映を求める（テナント管理者が知る由もない変更なので）。
// 変える前の値を返す。ないなら ErrNotFound、今の配置より容量を減らすなら ErrCapacity。
func (d *DB) UpdateEdge(ctx context.Context, e Edge) (Edge, error) {
	var before Edge
	err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		var err error
		before, err = scanEdge(tx.QueryRow(ctx, edgeSelect+" AND e.id = $1 FOR UPDATE OF e", e.ID))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if e.Capacity < before.Placements {
			return ErrCapacity
		}
		if _, err := tx.Exec(ctx, `UPDATE edges SET name = $2, capacity = $3, public_addr = $4, auth_upstream = $5, acct_upstream = $6, updated_at = now()
			WHERE id = $1`, e.ID, e.Name, e.Capacity, e.PublicAddr, e.AuthUpstream, e.AcctUpstream); err != nil {
			return err
		}
		if before.AuthUpstream == e.AuthUpstream && before.AcctUpstream == e.AcctUpstream {
			return nil
		}
		_, err = tx.Exec(ctx, `UPDATE tenant_radsec SET revision = revision + 1, requested_revision = revision + 1, updated_at = now()
			WHERE tenant_id IN (SELECT tenant_id FROM placements WHERE edge_id = $1 AND deleted_at IS NULL)`, e.ID)
		return err
	})
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrCapacity):
		return Edge{}, err
	case err != nil:
		return Edge{}, fmt.Errorf("update edge: %w", err)
	}
	return before, nil
}

// DeleteEdge はエッジを台帳から消す（行は残す）。ないなら ErrNotFound、配置が残っていれば ErrInUse。
func (d *DB) DeleteEdge(ctx context.Context, id string) error {
	err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		e, err := scanEdge(tx.QueryRow(ctx, edgeSelect+" AND e.id = $1 FOR UPDATE OF e", id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if e.Placements > 0 {
			return ErrInUse
		}
		_, err = tx.Exec(ctx, "UPDATE edges SET deleted_at = now(), updated_at = now() WHERE id = $1", id)
		return err
	})
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrInUse):
		return err
	case err != nil:
		return fmt.Errorf("delete edge: %w", err)
	}
	return nil
}

// ---- テナントの RadSec の設定 ----

// TenantRadsec はテナントの RadSec の設定。
type TenantRadsec struct {
	TenantID                    int64
	Slug                        string
	Port                        int
	CACertificate               string
	SourceCIDRs                 []string
	MatchAttr, MatchPattern     string
	Revision, RequestedRevision int64
	CRLDirty                    bool
	CreatedAt                   time.Time
	CreatedBy                   string
	ReleasedAt, CADisabledAt    time.Time
}

const radsecColumns = `tenant_id, slug, port, ca_certificate, source_cidrs, match_attr, match_pattern, revision, requested_revision, crl_dirty,
	created_at, created_by, released_at, ca_disabled_at`

func scanRadsec(row pgx.Row) (TenantRadsec, error) {
	var t TenantRadsec
	var released, disabled *time.Time
	err := row.Scan(&t.TenantID, &t.Slug, &t.Port, &t.CACertificate, &t.SourceCIDRs, &t.MatchAttr, &t.MatchPattern, &t.Revision, &t.RequestedRevision, &t.CRLDirty,
		&t.CreatedAt, &t.CreatedBy, &released, &disabled)
	if released != nil {
		t.ReleasedAt = *released
	}
	if disabled != nil {
		t.CADisabledAt = *disabled
	}
	return t, err
}

// TenantRadsec はテナントの RadSec の設定を返す。有効にしていない・削除したテナントなら ErrNotFound。
func (d *DB) TenantRadsec(ctx context.Context, tenant int64) (TenantRadsec, error) {
	t, err := scanRadsec(d.Pool.QueryRow(ctx, "SELECT "+radsecColumns+" FROM tenant_radsec WHERE tenant_id = $1 AND released_at IS NULL", tenant))
	if errors.Is(err, pgx.ErrNoRows) {
		return TenantRadsec{}, ErrNotFound
	}
	if err != nil {
		return TenantRadsec{}, fmt.Errorf("tenant radsec: %w", err)
	}
	return t, nil
}

// SlugUsed は slug が使われた（削除したテナントを含む）ことがあるかを返す。
func (d *DB) SlugUsed(ctx context.Context, slug string) (bool, error) {
	var used bool
	if err := d.Pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM tenant_radsec WHERE slug = $1)", slug).Scan(&used); err != nil {
		return false, fmt.Errorf("slug used: %w", err)
	}
	return used, nil
}

// EnableRadsec はテナントの RadSec を有効にする。ポートは一番小さい空きを割り当てる（AP の設定に入るので変えない）。
// テナントがなければ ErrNotFound、slug が使われている・有効にしてあれば ErrConflict、ポートに空きがなければ ErrNoPort。
func (d *DB) EnableRadsec(ctx context.Context, tenant int64, slug, caCertificate, by string) (TenantRadsec, error) {
	var out TenantRadsec
	err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		var id int64
		err := tx.QueryRow(ctx, "SELECT id FROM tenants WHERE id = $1 AND deleted_at IS NULL FOR UPDATE", tenant).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(radsecPortLock)); err != nil {
			return err
		}
		var port int
		err = tx.QueryRow(ctx, `SELECT p FROM generate_series($1::integer, $2::integer) p
			WHERE NOT EXISTS (SELECT 1 FROM tenant_radsec r WHERE r.port = p AND r.released_at IS NULL) ORDER BY p LIMIT 1`, RadsecPortMin, RadsecPortMax).Scan(&port)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoPort
		}
		if err != nil {
			return err
		}
		out, err = scanRadsec(tx.QueryRow(ctx, `INSERT INTO tenant_radsec (tenant_id, slug, port, ca_certificate, created_by)
			VALUES ($1, $2, $3, $4, $5) RETURNING `+radsecColumns, tenant, slug, port, caCertificate, by))
		return err
	})
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrNoPort):
		return TenantRadsec{}, err
	case pgErrorCode(err) == pgUniqueViolation:
		return TenantRadsec{}, ErrConflict
	case err != nil:
		return TenantRadsec{}, fmt.Errorf("enable radsec: %w", err)
	}
	return out, nil
}

// lockRadsec はテナントの設定の行をロックして読む。
func lockRadsec(ctx context.Context, tx pgx.Tx, tenant int64) (TenantRadsec, error) {
	t, err := scanRadsec(tx.QueryRow(ctx, "SELECT "+radsecColumns+" FROM tenant_radsec WHERE tenant_id = $1 AND released_at IS NULL FOR UPDATE", tenant))
	if errors.Is(err, pgx.ErrNoRows) {
		return TenantRadsec{}, ErrNotFound
	}
	return t, err
}

// bumpRevision は望む状態の版を上げる。request なら、その版の反映も求める（OB1）。
func bumpRevision(ctx context.Context, tx pgx.Tx, tenant int64, request bool) error {
	_, err := tx.Exec(ctx, `UPDATE tenant_radsec SET revision = revision + 1, updated_at = now(),
		requested_revision = CASE WHEN $2 THEN revision + 1 ELSE requested_revision END WHERE tenant_id = $1`, tenant, request)
	return err
}

// radsecTx はテナントの設定の行をロックして fn を動かす。
func (d *DB) radsecTx(ctx context.Context, what string, tenant int64, fn func(tx pgx.Tx, t TenantRadsec) error) error {
	err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		t, err := lockRadsec(ctx, tx, tenant)
		if err != nil {
			return err
		}
		return fn(tx, t)
	})
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrConflict), errors.Is(err, ErrInUse), errors.Is(err, ErrCapacity), errors.Is(err, ErrAPLimit),
		errors.Is(err, ErrNASIDUsed), errors.Is(err, ErrNameRetired), errors.Is(err, ErrNotReady):
		return err
	case pgErrorCode(err) == pgUniqueViolation:
		return ErrConflict
	case err != nil:
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

// SetRadsecSources は送信元の範囲を変え、変える前の値を返す。守りの範囲が変わるので、すぐ反映を求める（OB1）。値が同じなら何も書かない。
func (d *DB) SetRadsecSources(ctx context.Context, tenant int64, cidrs []string) ([]string, error) {
	var before []string
	if cidrs == nil {
		cidrs = []string{} // 列は NOT NULL
	}
	err := d.radsecTx(ctx, "set radsec sources", tenant, func(tx pgx.Tx, t TenantRadsec) error {
		before = t.SourceCIDRs
		if slices.Equal(before, cidrs) {
			return nil
		}
		if _, err := tx.Exec(ctx, "UPDATE tenant_radsec SET source_cidrs = $2 WHERE tenant_id = $1", tenant, cidrs); err != nil {
			return err
		}
		return bumpRevision(ctx, tx, tenant, true)
	})
	return before, err
}

// SetSharedMatch は共通のブロックの照合の形を変える。CRL のない持ち込みの CA があるときに形を消すなら ErrInUse。値が同じなら何も書かない。
func (d *DB) SetSharedMatch(ctx context.Context, tenant int64, attr, pattern string) error {
	return d.radsecTx(ctx, "set shared match", tenant, func(tx pgx.Tx, t TenantRadsec) error {
		if t.MatchAttr == attr && t.MatchPattern == pattern {
			return nil
		}
		if pattern == "" {
			var noCRL bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM brought_cas WHERE tenant_id = $1 AND deleted_at IS NULL AND removing_at IS NULL AND NOT has_crl)`, tenant).Scan(&noCRL); err != nil {
				return err
			}
			if noCRL {
				return ErrInUse
			}
		}
		if _, err := tx.Exec(ctx, "UPDATE tenant_radsec SET match_attr = $2, match_pattern = $3 WHERE tenant_id = $1", tenant, attr, pattern); err != nil {
			return err
		}
		return bumpRevision(ctx, tx, tenant, false)
	})
}

// RequestApply は今の版の反映を求める（B・C・D の「反映」。OB1）。求めた版を返す。
func (d *DB) RequestApply(ctx context.Context, tenant int64) (int64, error) {
	var rev int64
	err := d.radsecTx(ctx, "request apply", tenant, func(tx pgx.Tx, t TenantRadsec) error {
		rev = t.Revision
		_, err := tx.Exec(ctx, "UPDATE tenant_radsec SET requested_revision = revision, updated_at = now() WHERE tenant_id = $1", tenant)
		return err
	})
	return rev, err
}

// MarkRadsecCADisabled はテナントの CA を止めたことを記録する。
func (d *DB) MarkRadsecCADisabled(ctx context.Context, tenant int64) error {
	if _, err := d.Pool.Exec(ctx, "UPDATE tenant_radsec SET ca_disabled_at = now() WHERE tenant_id = $1", tenant); err != nil {
		return fmt.Errorf("mark radsec ca disabled: %w", err)
	}
	return nil
}

// ReleasedRadsec は削除したテナントの設定を返す（CA を止めるのを確かめるため）。
func (d *DB) ReleasedRadsec(ctx context.Context, tenant int64) (TenantRadsec, error) {
	t, err := scanRadsec(d.Pool.QueryRow(ctx, "SELECT "+radsecColumns+" FROM tenant_radsec WHERE tenant_id = $1", tenant))
	if errors.Is(err, pgx.ErrNoRows) {
		return TenantRadsec{}, ErrNotFound
	}
	if err != nil {
		return TenantRadsec{}, fmt.Errorf("released radsec: %w", err)
	}
	return t, nil
}

// ---- 配置 ----

// Placement は配置。
type Placement struct {
	ID       int64
	TenantID int64
	EdgeID   string
	Slot     string
	// Replaces は移動の途中で置き換える配置（なければ 0）。
	Replaces        int64
	CreatedAt       time.Time
	CreatedBy       string
	DeletedAt       time.Time
	AppliedRevision int64
	ApplyResult     string
	ApplyError      string
	CheckedAt       time.Time
}

// Removing はエッジから消している途中か。
func (p Placement) Removing() bool { return !p.DeletedAt.IsZero() }

const placementColumns = `id, tenant_id, edge_id, slot, replaces, created_at, created_by, deleted_at, applied_revision, apply_result, apply_error, checked_at`

func scanPlacement(row pgx.Row) (Placement, error) {
	var p Placement
	var replaces *int64
	var deleted, checked *time.Time
	err := row.Scan(&p.ID, &p.TenantID, &p.EdgeID, &p.Slot, &replaces, &p.CreatedAt, &p.CreatedBy, &deleted, &p.AppliedRevision, &p.ApplyResult, &p.ApplyError, &checked)
	if replaces != nil {
		p.Replaces = *replaces
	}
	if deleted != nil {
		p.DeletedAt = *deleted
	}
	if checked != nil {
		p.CheckedAt = *checked
	}
	return p, err
}

// placementOrder はスロットの順（同じスロットでは古いものが先）。
const placementOrder = ` ORDER BY array_position(ARRAY['primary', 'secondary', 'tertiary'], slot), id`

// Placements はテナントの配置（消している途中を含む）をスロットの順に返す。
func (d *DB) Placements(ctx context.Context, tenant int64) ([]Placement, error) {
	rows, err := d.Pool.Query(ctx, "SELECT "+placementColumns+" FROM placements WHERE tenant_id = $1 AND removed_at IS NULL"+placementOrder, tenant)
	if err != nil {
		return nil, fmt.Errorf("placements: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Placement, error) { return scanPlacement(row) })
	if err != nil {
		return nil, fmt.Errorf("placements: %w", err)
	}
	return out, nil
}

// EdgePlacements はエッジに置いているテナントの配置を返す。
func (d *DB) EdgePlacements(ctx context.Context, edge string) ([]Placement, error) {
	rows, err := d.Pool.Query(ctx, "SELECT "+placementColumns+" FROM placements WHERE edge_id = $1 AND removed_at IS NULL ORDER BY tenant_id, id", edge)
	if err != nil {
		return nil, fmt.Errorf("edge placements: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Placement, error) { return scanPlacement(row) })
	if err != nil {
		return nil, fmt.Errorf("edge placements: %w", err)
	}
	return out, nil
}

// reserveEdge はエッジの行をロックし、容量の空きを確かめる（OB13）。
func reserveEdge(ctx context.Context, tx pgx.Tx, edge string) error {
	e, err := scanEdge(tx.QueryRow(ctx, edgeSelect+" AND e.id = $1 FOR UPDATE OF e", edge))
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if e.Placements >= e.Capacity {
		return ErrCapacity
	}
	return nil
}

// AddPlacement は配置を足す。テナントかエッジがなければ ErrNotFound、容量を超えるなら ErrCapacity、スロットかエッジが使われていれば ErrConflict。
func (d *DB) AddPlacement(ctx context.Context, tenant int64, edge, slot, by string) (Placement, error) {
	var out Placement
	err := d.radsecTx(ctx, "add placement", tenant, func(tx pgx.Tx, _ TenantRadsec) error {
		if err := reserveEdge(ctx, tx, edge); err != nil {
			return err
		}
		var err error
		out, err = scanPlacement(tx.QueryRow(ctx, `INSERT INTO placements (tenant_id, edge_id, slot, created_by) VALUES ($1, $2, $3, $4) RETURNING `+placementColumns, tenant, edge, slot, by))
		return err
	})
	return out, err
}

// placementOf はテナントの配置をロックして読む（消した配置は ErrNotFound）。
func placementOf(ctx context.Context, tx pgx.Tx, tenant, id int64) (Placement, error) {
	p, err := scanPlacement(tx.QueryRow(ctx, "SELECT "+placementColumns+" FROM placements WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL FOR UPDATE", id, tenant))
	if errors.Is(err, pgx.ErrNoRows) {
		return Placement{}, ErrNotFound
	}
	return p, err
}

// inMove はその配置が移動の途中か（置き換える側か、置き換えられる側か）。
func inMove(ctx context.Context, tx pgx.Tx, p Placement) (bool, error) {
	if p.Replaces != 0 {
		return true, nil
	}
	var moving bool
	err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM placements WHERE replaces = $1 AND deleted_at IS NULL)", p.ID).Scan(&moving)
	return moving, err
}

// RemovePlacement は配置を望む状態から外す（エッジから消すのはループ）。移動の途中なら ErrInUse。外した配置を返す。
func (d *DB) RemovePlacement(ctx context.Context, tenant, id int64) (Placement, error) {
	var out Placement
	err := d.radsecTx(ctx, "remove placement", tenant, func(tx pgx.Tx, _ TenantRadsec) error {
		p, err := placementOf(ctx, tx, tenant, id)
		if err != nil {
			return err
		}
		moving, err := inMove(ctx, tx, p)
		if err != nil {
			return err
		}
		if moving {
			return ErrInUse
		}
		out = p
		_, err = tx.Exec(ctx, "UPDATE placements SET deleted_at = now() WHERE id = $1", id)
		return err
	})
	return out, err
}

// StartMove はスロットの移動を始める（OB7）。新しいエッジに同じスロットの配置を足し、人が DNS を書き換えて完了を押すまで両方を置く。
func (d *DB) StartMove(ctx context.Context, tenant, id int64, edge, by string) (Placement, error) {
	var out Placement
	err := d.radsecTx(ctx, "start move", tenant, func(tx pgx.Tx, _ TenantRadsec) error {
		p, err := placementOf(ctx, tx, tenant, id)
		if err != nil {
			return err
		}
		moving, err := inMove(ctx, tx, p)
		if err != nil {
			return err
		}
		if moving {
			return ErrInUse
		}
		if err := reserveEdge(ctx, tx, edge); err != nil {
			return err
		}
		out, err = scanPlacement(tx.QueryRow(ctx, `INSERT INTO placements (tenant_id, edge_id, slot, replaces, created_by) VALUES ($1, $2, $3, $4, $5) RETURNING `+placementColumns,
			tenant, edge, p.Slot, p.ID, by))
		return err
	})
	return out, err
}

// FinishMove はスロットの移動を完了する。古い配置を望む状態から外す。新しい配置が反映していなければ ErrNotReady。
func (d *DB) FinishMove(ctx context.Context, tenant, id int64) (Placement, error) {
	var old Placement
	err := d.radsecTx(ctx, "finish move", tenant, func(tx pgx.Tx, _ TenantRadsec) error {
		p, err := placementOf(ctx, tx, tenant, id)
		if err != nil {
			return err
		}
		if p.Replaces == 0 {
			return ErrNotFound
		}
		if p.AppliedRevision == 0 {
			return ErrNotReady
		}
		if old, err = placementOf(ctx, tx, tenant, p.Replaces); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE placements SET deleted_at = now() WHERE id = $1", old.ID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "UPDATE placements SET replaces = NULL WHERE id = $1", id)
		return err
	})
	return old, err
}

// CancelMove はスロットの移動をやめる。新しい配置を望む状態から外す。
func (d *DB) CancelMove(ctx context.Context, tenant, id int64) (Placement, error) {
	var out Placement
	err := d.radsecTx(ctx, "cancel move", tenant, func(tx pgx.Tx, _ TenantRadsec) error {
		p, err := placementOf(ctx, tx, tenant, id)
		if err != nil {
			return err
		}
		if p.Replaces == 0 {
			return ErrNotFound
		}
		out = p
		_, err = tx.Exec(ctx, "UPDATE placements SET deleted_at = now() WHERE id = $1", id)
		return err
	})
	return out, err
}

// ---- 全テナントの状態 ----

// RadsecOverview は RadSec を有効にしたテナント 1 つの状態（/radsec の 1 行）。
type RadsecOverview struct {
	TenantRadsec
	TenantName string
	Placements []Placement
	APs        int
}

// RadsecOverviews は RadSec を有効にした全テナントを名前の順に返す。
func (d *DB) RadsecOverviews(ctx context.Context) ([]RadsecOverview, error) {
	rows, err := d.Pool.Query(ctx, `SELECT r.*, t.name,
			(SELECT count(*) FROM aps a WHERE a.tenant_id = r.tenant_id AND a.deleted_at IS NULL)
		FROM (SELECT `+radsecColumns+` FROM tenant_radsec WHERE released_at IS NULL) r JOIN tenants t ON t.id = r.tenant_id
		ORDER BY t.name, r.tenant_id`)
	if err != nil {
		return nil, fmt.Errorf("radsec overviews: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (RadsecOverview, error) {
		var o RadsecOverview
		var released, disabled *time.Time
		err := row.Scan(&o.TenantID, &o.Slug, &o.Port, &o.CACertificate, &o.SourceCIDRs, &o.MatchAttr, &o.MatchPattern, &o.Revision, &o.RequestedRevision, &o.CRLDirty,
			&o.CreatedAt, &o.CreatedBy, &released, &disabled, &o.TenantName, &o.APs)
		return o, err
	})
	if err != nil {
		return nil, fmt.Errorf("radsec overviews: %w", err)
	}
	rows, err = d.Pool.Query(ctx, "SELECT "+placementColumns+" FROM placements WHERE removed_at IS NULL"+placementOrder)
	if err != nil {
		return nil, fmt.Errorf("radsec overviews: %w", err)
	}
	ps, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Placement, error) { return scanPlacement(row) })
	if err != nil {
		return nil, fmt.Errorf("radsec overviews: %w", err)
	}
	byTenant := map[int64][]Placement{}
	for _, p := range ps {
		byTenant[p.TenantID] = append(byTenant[p.TenantID], p)
	}
	for i := range out {
		out[i].Placements = byTenant[out[i].TenantID]
	}
	return out, nil
}

// ---- AP とグループ ----

// 種類の値。
const (
	KindAP    = "ap"
	KindGroup = "group"
)

// APCert は AP・グループの証明書。
type APCert struct {
	Serial             string
	NotAfter, IssuedAt time.Time
	IssuedBy           string
	RevokedAt          time.Time
	RevokeReason       string
}

// Valid は失効していないか（期限は別に見る）。
func (c APCert) Valid() bool { return c.RevokedAt.IsZero() }

// AP は AP かグループ。
type AP struct {
	ID        int64
	TenantID  int64
	Name      string
	Kind      string
	Purpose   string
	NASID     string
	CreatedAt time.Time
	CreatedBy string
	// Certs は証明書（新しい順）。
	Certs []APCert
}

const apColumns = "id, tenant_id, name, kind, purpose, nas_id, created_at, created_by"

func scanAP(row pgx.Row) (AP, error) {
	var a AP
	err := row.Scan(&a.ID, &a.TenantID, &a.Name, &a.Kind, &a.Purpose, &a.NASID, &a.CreatedAt, &a.CreatedBy)
	return a, err
}

func scanAPCert(row pgx.CollectableRow) (APCert, error) {
	var c APCert
	var revoked *time.Time
	err := row.Scan(&c.Serial, &c.NotAfter, &c.IssuedAt, &c.IssuedBy, &revoked, &c.RevokeReason)
	if revoked != nil {
		c.RevokedAt = *revoked
	}
	return c, err
}

// APs はテナントの AP とグループを名前の順に、証明書つきで返す。
func (d *DB) APs(ctx context.Context, tenant int64) ([]AP, error) {
	rows, err := d.Pool.Query(ctx, "SELECT "+apColumns+" FROM aps WHERE tenant_id = $1 AND deleted_at IS NULL ORDER BY name", tenant)
	if err != nil {
		return nil, fmt.Errorf("aps: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (AP, error) { return scanAP(row) })
	if err != nil {
		return nil, fmt.Errorf("aps: %w", err)
	}
	rows, err = d.Pool.Query(ctx, `SELECT c.ap_id, c.serial, c.not_after, c.issued_at, c.issued_by, c.revoked_at, c.revoke_reason
		FROM ap_certs c JOIN aps a ON a.id = c.ap_id WHERE a.tenant_id = $1 AND a.deleted_at IS NULL ORDER BY c.issued_at DESC, c.serial`, tenant)
	if err != nil {
		return nil, fmt.Errorf("aps: %w", err)
	}
	byAP := map[int64][]APCert{}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var c APCert
		var revoked *time.Time
		if err := rows.Scan(&id, &c.Serial, &c.NotAfter, &c.IssuedAt, &c.IssuedBy, &revoked, &c.RevokeReason); err != nil {
			return nil, fmt.Errorf("aps: %w", err)
		}
		if revoked != nil {
			c.RevokedAt = *revoked
		}
		byAP[id] = append(byAP[id], c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("aps: %w", err)
	}
	for i := range out {
		out[i].Certs = byAP[out[i].ID]
	}
	return out, nil
}

// APByID はテナントの AP を証明書つきで返す。ない・消したものなら ErrNotFound。
func (d *DB) APByID(ctx context.Context, tenant, id int64) (AP, error) {
	a, err := scanAP(d.Pool.QueryRow(ctx, "SELECT "+apColumns+" FROM aps WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL", id, tenant))
	if errors.Is(err, pgx.ErrNoRows) {
		return AP{}, ErrNotFound
	}
	if err != nil {
		return AP{}, fmt.Errorf("ap: %w", err)
	}
	rows, err := d.Pool.Query(ctx, "SELECT serial, not_after, issued_at, issued_by, revoked_at, revoke_reason FROM ap_certs WHERE ap_id = $1 ORDER BY issued_at DESC, serial", id)
	if err != nil {
		return AP{}, fmt.Errorf("ap: %w", err)
	}
	if a.Certs, err = pgx.CollectRows(rows, scanAPCert); err != nil {
		return AP{}, fmt.Errorf("ap: %w", err)
	}
	return a, nil
}

// checkAPName は AP の名前を登録できるかを確かめる。
func checkAPName(ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, tenant int64, name, nasID string) error {
	var live, nasUsed, retired, noCRL bool
	var count int
	err := q.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM aps WHERE tenant_id = $1 AND name = $2 AND deleted_at IS NULL),
		EXISTS (SELECT 1 FROM aps WHERE nas_id = $3 AND deleted_at IS NULL),
		EXISTS (SELECT 1 FROM aps WHERE tenant_id = $1 AND name = $2 AND deleted_at IS NOT NULL),
		EXISTS (SELECT 1 FROM brought_cas WHERE tenant_id = $1 AND deleted_at IS NULL AND removing_at IS NULL AND NOT has_crl),
		(SELECT count(*) FROM aps WHERE tenant_id = $1 AND deleted_at IS NULL)`, tenant, name, nasID).Scan(&live, &nasUsed, &retired, &noCRL, &count)
	switch {
	case err != nil:
		return err
	case live:
		return ErrConflict
	case nasUsed:
		return ErrNASIDUsed
	case retired && noCRL:
		return ErrNameRetired
	case count >= MaxAPs:
		return ErrAPLimit
	}
	return nil
}

// CheckAPName は AP の名前を登録できるかを確かめる（発行の前の確かめ）。
// 使われていれば ErrConflict、NAS-Identifier が重なれば ErrNASIDUsed、CRL のないテナントで削除した名前なら ErrNameRetired、上限なら ErrAPLimit。
func (d *DB) CheckAPName(ctx context.Context, tenant int64, name, nasID string) error {
	if err := checkAPName(ctx, d.Pool, tenant, name, nasID); err != nil {
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrNASIDUsed) || errors.Is(err, ErrNameRetired) || errors.Is(err, ErrAPLimit) {
			return err
		}
		return fmt.Errorf("check ap name: %w", err)
	}
	return nil
}

// CreateAP は AP かグループを証明書つきで台帳に足し、版を上げる（反映は求めない。OB1）。
func (d *DB) CreateAP(ctx context.Context, a AP, cert APCert) (AP, error) {
	var out AP
	err := d.radsecTx(ctx, "create ap", a.TenantID, func(tx pgx.Tx, _ TenantRadsec) error {
		if err := checkAPName(ctx, tx, a.TenantID, a.Name, a.NASID); err != nil {
			return err
		}
		var err error
		out, err = scanAP(tx.QueryRow(ctx, `INSERT INTO aps (tenant_id, name, kind, purpose, nas_id, created_by) VALUES ($1, $2, $3, $4, $5, $6) RETURNING `+apColumns,
			a.TenantID, a.Name, a.Kind, a.Purpose, a.NASID, a.CreatedBy))
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "INSERT INTO ap_certs (serial, ap_id, not_after, issued_by) VALUES ($1, $2, $3, $4)", cert.Serial, out.ID, cert.NotAfter, a.CreatedBy); err != nil {
			return err
		}
		return bumpRevision(ctx, tx, a.TenantID, false)
	})
	return out, err
}

// AddAPCert は AP の更新の証明書を足す。設定は変わらないが、宣言の証明書の情報が変わるので版を上げる（エッジの再起動は起きない）。
func (d *DB) AddAPCert(ctx context.Context, tenant, ap int64, cert APCert) error {
	return d.radsecTx(ctx, "add ap cert", tenant, func(tx pgx.Tx, _ TenantRadsec) error {
		var exists bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM aps WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL)", ap, tenant).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		// 同じ要求の再送では同じ証明書が返るので、2 回目は何もしない。
		tag, err := tx.Exec(ctx, "INSERT INTO ap_certs (serial, ap_id, not_after, issued_by) VALUES ($1, $2, $3, $4) ON CONFLICT (serial) DO NOTHING", cert.Serial, ap, cert.NotAfter, cert.IssuedBy)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		return bumpRevision(ctx, tx, tenant, false)
	})
}

// MarkAPCertRevoked は AP の証明書を失効させたことを記録し、CRL の取り直しの印を立てる。
func (d *DB) MarkAPCertRevoked(ctx context.Context, tenant, ap int64, serial, reason string) error {
	return d.radsecTx(ctx, "mark ap cert revoked", tenant, func(tx pgx.Tx, _ TenantRadsec) error {
		tag, err := tx.Exec(ctx, `UPDATE ap_certs SET revoked_at = now(), revoke_reason = $3 WHERE serial = $1 AND ap_id = $2 AND revoked_at IS NULL
			AND ap_id IN (SELECT id FROM aps WHERE tenant_id = $4)`, serial, ap, reason, tenant)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		if _, err := tx.Exec(ctx, "UPDATE tenant_radsec SET crl_dirty = true WHERE tenant_id = $1", tenant); err != nil {
			return err
		}
		return bumpRevision(ctx, tx, tenant, false)
	})
}

// DeleteAP は AP を台帳から消し、版を上げる。CRL のないテナントでは、失効が TLS で効かず client ブロックを消すしか止める手段がないので、すぐ反映を求める（OB5）。
// ほかのテナントでは失効で止まるので求めない（ブロックは次の反映で消える。OB1）。すぐ反映を求めたかを返す。
func (d *DB) DeleteAP(ctx context.Context, tenant, id int64) (bool, error) {
	var noCRL bool
	err := d.radsecTx(ctx, "delete ap", tenant, func(tx pgx.Tx, _ TenantRadsec) error {
		tag, err := tx.Exec(ctx, "UPDATE aps SET deleted_at = now() WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL", id, tenant)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM brought_cas WHERE tenant_id = $1 AND deleted_at IS NULL AND removing_at IS NULL AND NOT has_crl)`, tenant).Scan(&noCRL); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE tenant_radsec SET crl_dirty = true WHERE tenant_id = $1", tenant); err != nil {
			return err
		}
		return bumpRevision(ctx, tx, tenant, noCRL)
	})
	return noCRL, err
}

// NASIDs はテナントの AP とグループの NAS-Identifier を返す（認可ポリシーの候補）。
func (d *DB) NASIDs(ctx context.Context, tenant int64) ([]string, error) {
	rows, err := d.Pool.Query(ctx, "SELECT nas_id FROM aps WHERE tenant_id = $1 AND deleted_at IS NULL ORDER BY nas_id", tenant)
	if err != nil {
		return nil, fmt.Errorf("nas ids: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("nas ids: %w", err)
	}
	return out, nil
}

// ---- 持ち込みの CA ----

// BroughtCA は持ち込みの CA。
type BroughtCA struct {
	CAID, Name       string
	TenantID         int64
	Certificate      string
	Subject          string
	HasCRL           bool
	CreatedAt        time.Time
	CreatedBy        string
	RemovingAt       time.Time
	RemovingRevision int64
}

// Removing は外している途中か。
func (b BroughtCA) Removing() bool { return !b.RemovingAt.IsZero() }

const broughtColumns = "ca_id, name, tenant_id, certificate, subject, has_crl, created_at, created_by, removing_at, removing_revision"

func scanBrought(row pgx.Row) (BroughtCA, error) {
	var b BroughtCA
	var removing *time.Time
	err := row.Scan(&b.CAID, &b.Name, &b.TenantID, &b.Certificate, &b.Subject, &b.HasCRL, &b.CreatedAt, &b.CreatedBy, &removing, &b.RemovingRevision)
	if removing != nil {
		b.RemovingAt = *removing
	}
	return b, err
}

// BroughtCAs はテナントの持ち込みの CA（外している途中を含む）を名前の順に返す。
func (d *DB) BroughtCAs(ctx context.Context, tenant int64) ([]BroughtCA, error) {
	rows, err := d.Pool.Query(ctx, "SELECT "+broughtColumns+" FROM brought_cas WHERE tenant_id = $1 AND deleted_at IS NULL ORDER BY name", tenant)
	if err != nil {
		return nil, fmt.Errorf("brought cas: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (BroughtCA, error) { return scanBrought(row) })
	if err != nil {
		return nil, fmt.Errorf("brought cas: %w", err)
	}
	return out, nil
}

// AddBroughtCA は持ち込みの CA を台帳に足し、版を上げる。CRL のない CA では照合の形も一緒に置く（必須。OB15）。
// 受ける範囲を広げる操作なので、反映は人が求める（OB1）。同じ ID があれば ErrConflict。
func (d *DB) AddBroughtCA(ctx context.Context, b BroughtCA, matchAttr, matchPattern string) error {
	return d.radsecTx(ctx, "add brought ca", b.TenantID, func(tx pgx.Tx, _ TenantRadsec) error {
		if _, err := tx.Exec(ctx, `INSERT INTO brought_cas (ca_id, name, tenant_id, certificate, subject, has_crl, created_by) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			b.CAID, b.Name, b.TenantID, b.Certificate, b.Subject, b.HasCRL, b.CreatedBy); err != nil {
			return err
		}
		if matchPattern != "" {
			if _, err := tx.Exec(ctx, "UPDATE tenant_radsec SET match_attr = $2, match_pattern = $3 WHERE tenant_id = $1", b.TenantID, matchAttr, matchPattern); err != nil {
				return err
			}
		}
		return bumpRevision(ctx, tx, b.TenantID, false)
	})
}

// SetBroughtHasCRL は持ち込みの CA の CRL の有無を変える。CRLCheck が変わるので、すぐ反映を求める（CA6）。値が同じなら何も書かない。
func (d *DB) SetBroughtHasCRL(ctx context.Context, tenant int64, caID string, has bool, matchAttr, matchPattern string) error {
	return d.radsecTx(ctx, "set brought has crl", tenant, func(tx pgx.Tx, _ TenantRadsec) error {
		tag, err := tx.Exec(ctx, "UPDATE brought_cas SET has_crl = $3 WHERE ca_id = $1 AND tenant_id = $2 AND deleted_at IS NULL AND has_crl <> $3", caID, tenant, has)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		if matchPattern != "" {
			if _, err := tx.Exec(ctx, "UPDATE tenant_radsec SET match_attr = $2, match_pattern = $3 WHERE tenant_id = $1", tenant, matchAttr, matchPattern); err != nil {
				return err
			}
		}
		return bumpRevision(ctx, tx, tenant, true)
	})
}

// StartRemoveBrought は持ち込みの CA を望む状態から外し、すぐ反映を求める（OB6）。全配置が反映したらループが radsec-ca で止める。
// ないなら ErrNotFound。外している途中なら何もしない。
func (d *DB) StartRemoveBrought(ctx context.Context, tenant int64, caID string) error {
	return d.radsecTx(ctx, "start remove brought", tenant, func(tx pgx.Tx, t TenantRadsec) error {
		b, err := scanBrought(tx.QueryRow(ctx, "SELECT "+broughtColumns+" FROM brought_cas WHERE ca_id = $1 AND tenant_id = $2 AND deleted_at IS NULL FOR UPDATE", caID, tenant))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil || b.Removing() {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE brought_cas SET removing_at = now(), removing_revision = $2 WHERE ca_id = $1", caID, t.Revision+1); err != nil {
			return err
		}
		return bumpRevision(ctx, tx, tenant, true)
	})
}

// ---- 課金の数量 ----

// RadsecCount は 1 つのテナントの数量。
type RadsecCount struct {
	APs, Groups, BroughtCAs, Placements int
}

// RadsecCountsAt は時刻 at の時点の数量をテナントごとに返す（S4-12。締めのときに月末の時点の数を数える）。
func (d *DB) RadsecCountsAt(ctx context.Context, at time.Time) (map[int64]RadsecCount, error) {
	out := map[int64]RadsecCount{}
	rows, err := d.Pool.Query(ctx, `SELECT tenant_id, kind, count(*) FROM aps WHERE created_at < $1 AND (deleted_at IS NULL OR deleted_at >= $1) GROUP BY tenant_id, kind`, at)
	if err != nil {
		return nil, fmt.Errorf("radsec counts: %w", err)
	}
	var tenant int64
	var kind string
	var n int
	if _, err := pgx.ForEachRow(rows, []any{&tenant, &kind, &n}, func() error {
		c := out[tenant]
		if kind == KindGroup {
			c.Groups = n
		} else {
			c.APs = n
		}
		out[tenant] = c
		return nil
	}); err != nil {
		return nil, fmt.Errorf("radsec counts: %w", err)
	}
	for _, q := range []struct {
		sql string
		set func(*RadsecCount, int)
	}{
		{`SELECT tenant_id, count(*) FROM brought_cas WHERE created_at < $1 AND (deleted_at IS NULL OR deleted_at >= $1) GROUP BY tenant_id`, func(c *RadsecCount, n int) { c.BroughtCAs = n }},
		{`SELECT tenant_id, count(*) FROM placements WHERE created_at < $1 AND (deleted_at IS NULL OR deleted_at >= $1) GROUP BY tenant_id`, func(c *RadsecCount, n int) { c.Placements = n }},
	} {
		rows, err := d.Pool.Query(ctx, q.sql, at)
		if err != nil {
			return nil, fmt.Errorf("radsec counts: %w", err)
		}
		if _, err := pgx.ForEachRow(rows, []any{&tenant, &n}, func() error {
			c := out[tenant]
			q.set(&c, n)
			out[tenant] = c
			return nil
		}); err != nil {
			return nil, fmt.Errorf("radsec counts: %w", err)
		}
	}
	return out, nil
}
