package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/oyaguma3/eapaka-ossbss-sample/internal/authz"
)

// ErrNotFound は対象の行がないことを表す。
var ErrNotFound = errors.New("not found")

// Account は利用者のアカウント。
type Account struct {
	ID    int64
	Email string
	Role  authz.Role
	// TenantID は所属のテナント（A・B だけ。C・D は 0）。TenantName はその名前。
	TenantID   int64
	TenantName string
	Disabled   bool
	CreatedAt  time.Time
	// CreatedBy は作成した利用者のメールアドレス（最初のシステム管理者は initial-admin）。
	CreatedBy string
}

// Actor は権限の判定に使う形にする。
func (a Account) Actor() authz.Actor {
	return authz.Actor{Email: a.Email, Role: a.Role, TenantID: a.TenantID}
}

// AccountByEmail はメールアドレス（正規化したもの）のアカウントを返す。なければ ErrNotFound。
func (d *DB) AccountByEmail(ctx context.Context, email string) (Account, error) {
	a, err := scanAccount(d.Pool.QueryRow(ctx, accountSelect+" WHERE a.email = $1", email))
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("account by email: %w", err)
	}
	return a, nil
}

// accountSelect はアカウント（テナントの名前つき）を読む SELECT。scanAccount で読む。
const accountSelect = `
	SELECT a.id, a.email, a.role, a.tenant_id, t.name, a.disabled, a.created_at, a.created_by
	FROM accounts a LEFT JOIN tenants t ON t.id = a.tenant_id`

// scanAccount は accountSelect の 1 行を読む。
func scanAccount(row pgx.Row) (Account, error) {
	var a Account
	var tenantID *int64
	var tenantName *string
	if err := row.Scan(&a.ID, &a.Email, &a.Role, &tenantID, &tenantName, &a.Disabled, &a.CreatedAt, &a.CreatedBy); err != nil {
		return Account{}, err
	}
	if tenantID != nil {
		a.TenantID = *tenantID
	}
	if tenantName != nil {
		a.TenantName = *tenantName
	}
	return a, nil
}

// AccountFilter はアカウントの一覧の絞り込み。
type AccountFilter struct {
	// All ならすべて、NoTenant ならテナントなし（運用担当・システム管理者）、どちらでもなければ TenantID のもの。
	All, NoTenant bool
	TenantID      int64
}

// Accounts はアカウントの一覧をメールアドレスの順に返す。
func (d *DB) Accounts(ctx context.Context, f AccountFilter) ([]Account, error) {
	q, args := accountSelect+" ORDER BY a.email", []any(nil)
	switch {
	case f.NoTenant:
		q = accountSelect + " WHERE a.tenant_id IS NULL ORDER BY a.email"
	case !f.All:
		q, args = accountSelect+" WHERE a.tenant_id = $1 ORDER BY a.email", []any{f.TenantID}
	}
	rows, err := d.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("accounts: %w", err)
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("accounts: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("accounts: %w", err)
	}
	return out, nil
}

// AccountByID は ID のアカウントを返す。なければ ErrNotFound。
func (d *DB) AccountByID(ctx context.Context, id int64) (Account, error) {
	a, err := scanAccount(d.Pool.QueryRow(ctx, accountSelect+" WHERE a.id = $1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("account by id: %w", err)
	}
	return a, nil
}

// tenantArg は、テナントの ID を DB に渡す値にする（0 は NULL）。
func tenantArg(id int64) *int64 {
	if id == 0 {
		return nil
	}
	return &id
}

// CreateAccount はアカウントを作る（by は作成した利用者）。メールアドレスが既にあれば ErrConflict。
// 役割とテナントの組み合わせ・メールアドレスの形式は呼び出し側で確かめる（DB の CHECK 制約でも守る）。
func (d *DB) CreateAccount(ctx context.Context, email string, role authz.Role, tenantID int64, by string) (int64, error) {
	var id int64
	err := d.Pool.QueryRow(ctx, "INSERT INTO accounts (email, role, tenant_id, created_by) VALUES ($1, $2, $3, $4) RETURNING id",
		email, role, tenantArg(tenantID), by).Scan(&id)
	switch {
	case pgErrorCode(err) == pgUniqueViolation:
		return 0, ErrConflict
	case pgErrorCode(err) == pgForeignKeyViolation:
		return 0, ErrNotFound
	case err != nil:
		return 0, fmt.Errorf("create account: %w", err)
	}
	return id, nil
}

// AccountState はアカウントの役割とテナント。変更のときに、画面を開いた時点の値（変更前）として渡す。
type AccountState struct {
	Role     authz.Role
	TenantID int64
}

// UpdateAccount はアカウントの役割とテナントを from から to に変える。役割・テナントが from と違えば（画面を開いた後に
// 他の操作で変わった）、何もせずに ErrNotFound を返す（権限の判定に使った値のまま変えるため）。
func (d *DB) UpdateAccount(ctx context.Context, id int64, from, to AccountState) error {
	tag, err := d.Pool.Exec(ctx, `
		UPDATE accounts SET role = $4, tenant_id = $5, updated_at = now()
		WHERE id = $1 AND role = $2 AND tenant_id IS NOT DISTINCT FROM $3`,
		id, from.Role, tenantArg(from.TenantID), to.Role, tenantArg(to.TenantID))
	switch {
	case pgErrorCode(err) == pgForeignKeyViolation:
		return ErrNotFound
	case err != nil:
		return fmt.Errorf("update account: %w", err)
	case tag.RowsAffected() == 0:
		return ErrNotFound
	}
	return nil
}

// SetAccountDisabled はアカウントを無効・有効にする。役割・テナントが from と違えば ErrNotFound（UpdateAccount と同じ）。
func (d *DB) SetAccountDisabled(ctx context.Context, id int64, from AccountState, disabled bool) error {
	tag, err := d.Pool.Exec(ctx, `
		UPDATE accounts SET disabled = $4, updated_at = now()
		WHERE id = $1 AND role = $2 AND tenant_id IS NOT DISTINCT FROM $3`,
		id, from.Role, tenantArg(from.TenantID), disabled)
	if err != nil {
		return fmt.Errorf("set account disabled: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteAccount はアカウントを削除する。役割・テナントが from と違えば ErrNotFound（UpdateAccount と同じ）。
func (d *DB) DeleteAccount(ctx context.Context, id int64, from AccountState) error {
	tag, err := d.Pool.Exec(ctx, "DELETE FROM accounts WHERE id = $1 AND role = $2 AND tenant_id IS NOT DISTINCT FROM $3",
		id, from.Role, tenantArg(from.TenantID))
	if err != nil {
		return fmt.Errorf("delete account: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// InitialAdminResult は EnsureInitialAdmin で行ったこと。
type InitialAdminResult string

const (
	InitialAdminCreated   InitialAdminResult = "created"   // 作った
	InitialAdminRestored  InitialAdminResult = "restored"  // 役割・テナント・無効を、システム管理者・有効に戻した
	InitialAdminUnchanged InitialAdminResult = "unchanged" // 既にシステム管理者・有効だった
)

// EnsureInitialAdmin は、最初のシステム管理者（OSSBSS_INITIAL_ADMIN_EMAIL）のアカウントを、システム管理者・有効にする
// （設計概要 §15 の 12。なければ作り、役割が違う・無効なら戻す。締め出されたときに元に戻す経路になる）。
func (d *DB) EnsureInitialAdmin(ctx context.Context, email string) (InitialAdminResult, error) {
	if !authz.ValidEmail(email) {
		return "", fmt.Errorf("initial admin email %q is not valid", email)
	}
	var res InitialAdminResult
	err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		var role authz.Role
		var tenantID *int64
		var disabled bool
		err := tx.QueryRow(ctx, "SELECT role, tenant_id, disabled FROM accounts WHERE email = $1 FOR UPDATE", email).
			Scan(&role, &tenantID, &disabled)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			res = InitialAdminCreated
			_, err = tx.Exec(ctx, `INSERT INTO accounts (email, role, created_by) VALUES ($1, $2, 'initial-admin')
				ON CONFLICT (email) DO NOTHING`, email, authz.SystemAdmin)
			return err
		case err != nil:
			return err
		case role == authz.SystemAdmin && tenantID == nil && !disabled:
			res = InitialAdminUnchanged
			return nil
		}
		res = InitialAdminRestored
		_, err = tx.Exec(ctx, `UPDATE accounts SET role = $2, tenant_id = NULL, disabled = false, updated_at = now()
			WHERE email = $1`, email, authz.SystemAdmin)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("ensure initial admin: %w", err)
	}
	return res, nil
}
