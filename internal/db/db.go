// Package db は本BFF 専用の PostgreSQL への接続と、スキーマの移行を扱う（設計概要 §11）。
package db

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"net"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

// migrationLockID は、スキーマの移行を複数のプロセスで同時に行わないための advisory lock の番号（任意の固定値）。
const migrationLockID = 0x05_5b55_0001

// Options は接続の設定。
type Options struct {
	// Addr は「ホスト:ポート」。
	Addr     string
	Name     string
	User     string
	Password string
	MaxConns int32
}

// DB は接続プール。
type DB struct {
	Pool *pgxpool.Pool
}

// Open は接続プールを作り、接続できることを確かめる。
func Open(ctx context.Context, opts Options) (*DB, error) {
	host, port, err := net.SplitHostPort(opts.Addr)
	if err != nil {
		return nil, fmt.Errorf("db address %q: %w", opts.Addr, err)
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("db port %q: %w", port, err)
	}
	// 接続文字列にはせず、項目ごとに渡す（パスワードの記号をエスケープしなくてよいように）。
	cfg, err := pgxpool.ParseConfig("")
	if err != nil {
		return nil, err
	}
	cc := cfg.ConnConfig
	cc.Host, cc.Port, cc.Database, cc.User, cc.Password = host, uint16(p), opts.Name, opts.User, opts.Password
	cc.ConnectTimeout = 10 * time.Second
	// 同じホストの compose のネットワークの中だけで使うので、TLS は使わない。
	cc.TLSConfig = nil
	cc.Fallbacks = nil
	cc.RuntimeParams["application_name"] = "eapaka-ossbss"
	cc.RuntimeParams["timezone"] = "UTC"
	if opts.MaxConns > 0 {
		cfg.MaxConns = opts.MaxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect db %s/%s: %w", opts.Addr, opts.Name, err)
	}
	return &DB{Pool: pool}, nil
}

// Close は接続プールを閉じる。
func (d *DB) Close() { d.Pool.Close() }

// Ping は接続できることを確かめる。
func (d *DB) Ping(ctx context.Context) error { return d.Pool.Ping(ctx) }

// migration は 1 つの移行（migrations/NNNN_名前.sql）。
type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations は埋め込んだ移行を番号の順に返す。
func loadMigrations() ([]migration, error) {
	files, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, f := range files {
		base := strings.TrimSuffix(path.Base(f), ".sql")
		num, name, ok := strings.Cut(base, "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil || v < 1 {
			return nil, fmt.Errorf("migration file name %q: must be NNNN_name.sql", f)
		}
		b, err := migrations.ReadFile(f)
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: v, name: name, sql: string(b)})
	}
	slices.SortFunc(out, func(a, b migration) int { return a.version - b.version })
	for i := 1; i < len(out); i++ {
		if out[i].version == out[i-1].version {
			return nil, fmt.Errorf("duplicate migration version %d", out[i].version)
		}
	}
	return out, nil
}

// Migrate は、まだ適用していない移行を番号の順に適用し、適用した番号を返す。
// 全体を 1 つのトランザクションで行い、advisory lock で他のプロセスの移行と重ならないようにする
// （途中で失敗したら、何も適用しなかった状態に戻る）。DB にあってこのプログラムが知らない番号があれば、
// 新しい版で移行した DB を古い版で使おうとしているので、エラーにする。
func (d *DB) Migrate(ctx context.Context) ([]int, error) {
	ms, err := loadMigrations()
	if err != nil {
		return nil, err
	}
	applied := []int{}
	err = pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(migrationLockID)); err != nil {
			return fmt.Errorf("lock: %w", err)
		}
		if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
			version    integer     PRIMARY KEY,
			name       text        NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
			return fmt.Errorf("create schema_migrations: %w", err)
		}
		rows, err := tx.Query(ctx, "SELECT version FROM schema_migrations")
		if err != nil {
			return err
		}
		done, err := pgx.CollectRows(rows, pgx.RowTo[int])
		if err != nil {
			return err
		}
		if latest := ms[len(ms)-1].version; len(done) > 0 && slices.Max(done) > latest {
			return fmt.Errorf("the database schema (version %d) is newer than this program (version %d)", slices.Max(done), latest)
		}
		for _, m := range ms {
			if slices.Contains(done, m.version) {
				continue
			}
			if _, err := tx.Exec(ctx, m.sql); err != nil {
				return fmt.Errorf("migration %04d_%s: %w", m.version, m.name, err)
			}
			if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version, name) VALUES ($1, $2)", m.version, m.name); err != nil {
				return err
			}
			applied = append(applied, m.version)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return applied, nil
}

// SchemaVersion は、適用済みの移行のうち最も新しい番号を返す（なければ 0）。
func (d *DB) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := d.Pool.QueryRow(ctx, "SELECT coalesce(max(version), 0) FROM schema_migrations").Scan(&v)
	if err != nil {
		return 0, err
	}
	return v, nil
}
