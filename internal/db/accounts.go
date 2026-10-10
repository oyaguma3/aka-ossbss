package db

import (
	"context"
	"errors"
	"fmt"

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
}

// Actor は権限の判定に使う形にする。
func (a Account) Actor() authz.Actor {
	return authz.Actor{Email: a.Email, Role: a.Role, TenantID: a.TenantID}
}

// AccountByEmail はメールアドレス（正規化したもの）のアカウントを返す。なければ ErrNotFound。
func (d *DB) AccountByEmail(ctx context.Context, email string) (Account, error) {
	var a Account
	var tenantID *int64
	var tenantName *string
	err := d.Pool.QueryRow(ctx, `
		SELECT a.id, a.email, a.role, a.tenant_id, t.name, a.disabled
		FROM accounts a LEFT JOIN tenants t ON t.id = a.tenant_id
		WHERE a.email = $1`, email).Scan(&a.ID, &a.Email, &a.Role, &tenantID, &tenantName, &a.Disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("account by email: %w", err)
	}
	if tenantID != nil {
		a.TenantID = *tenantID
	}
	if tenantName != nil {
		a.TenantName = *tenantName
	}
	return a, nil
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
