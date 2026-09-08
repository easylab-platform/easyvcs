package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Kinds of metadata backends.
const (
	KindSQLite   = "sqlite"
	KindPostgres = "postgres"
	KindMySQL    = "mysql"
)

// DriverConfig selects the metadata backend. Kind is "sqlite", "postgres", or
// "mysql"/"mariadb". All dialects are pure-Go (no cgo): sqlite via
// glebarez/sqlite (modernc), postgres via pgx, mysql via go-sql-driver.
type DriverConfig struct {
	Kind string
	DSN  string
}

// OpenDriver opens the central store on the configured backend. The semantic
// layer (revision/merge/transfer) is agnostic; only this boundary changes.
// Storage is now GORM-backed so the three dialects share one code path.
func OpenDriver(cfg DriverConfig) (*CentralStore, error) {
	kind := normalizeKind(cfg.Kind)
	if kind == "" {
		return nil, fmt.Errorf("unsupported db driver %q (sqlite|postgres|mysql)", cfg.Kind)
	}
	dsn := cfg.DSN
	if kind == KindSQLite {
		if dsn == "" {
			dsn = DBPath()
		}
		if err := os.MkdirAll(filepath.Dir(dsn), 0o755); err != nil {
			return nil, err
		}
	}

	dialector := dialectorFor(kind, dsn)
	db, err := gorm.Open(dialector, &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	if kind == KindSQLite {
		// modernc/glebarez sqlite: multiple pooled connections see their own
		// PRAGMA state, so WAL + busy_timeout must be baked into the DSN (they
		// are connection-scoped) rather than Exec'd once on one connection.
		// A single connection avoids cross-conn lock churn entirely; writes are
		// serialized by the server layer anyway.
		sqlDB.SetMaxOpenConns(1)
	}

	d := &sqlDialect{db: sqlDB, gdb: db, kind: kind}
	cs := &CentralStore{d: d, root: rootFor(kind, dsn)}
	if err := cs.Init(); err != nil {
		return nil, err
	}
	return cs, nil
}

// normalizeKind maps an env/flag kind string to a canonical Kind, accepting
// "mariadb" as an alias of "mysql".
func normalizeKind(k string) string {
	switch strings.ToLower(strings.TrimSpace(k)) {
	case KindSQLite:
		return KindSQLite
	case KindPostgres, "postgresql":
		return KindPostgres
	case KindMySQL, "mariadb", "maria":
		return KindMySQL
	default:
		return ""
	}
}

// dialectorFor returns the GORM dialector for a backend kind.
func dialectorFor(kind, dsn string) gorm.Dialector {
	switch kind {
	case KindPostgres:
		return postgres.Open(dsn)
	case KindMySQL:
		return mysql.Open(dsn)
	default:
		return sqlite.Open(dsn)
	}
}

func rootFor(kind, dsn string) string {
	switch kind {
	case KindSQLite:
		return filepath.Dir(dsn)
	default:
		// For server dialects the "root" is not a filesystem dir; keep the DSN
		// so callers that inspect it (e.g. registry file layout) degrade.
		return dsn
	}
}

// sqlDialect adapts the storage layer over GORM. `db` is the underlying
// *sql.DB (kept for SetWAL / legacy helpers); `gdb` is the GORM session used
// for all model queries.
type sqlDialect struct {
	db   *sql.DB
	gdb  *gorm.DB
	kind string
}

func (d *sqlDialect) isSQLite() bool { return strings.ToLower(d.kind) == KindSQLite }

// Open opens the central store at the given SQLite file path (legacy helper).
func Open(path string) (*CentralStore, error) {
	return OpenDriver(DriverConfig{Kind: KindSQLite, DSN: path})
}

// OpenDefault opens the central store at the default SQLite path.
func OpenDefault() (*CentralStore, error) {
	return Open(DBPath())
}
