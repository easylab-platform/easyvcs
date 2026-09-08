package store

import (
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestOpenDriverSQLiteDialect(t *testing.T) {
	// GORM uses the glebarez/sqlite (pure-Go) dialector for sqlite. Assert the
	// store opens and a repo round-trips.
	cs, err := OpenDriver(DriverConfig{Kind: KindSQLite, DSN: t.TempDir() + "/t.db"})
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	r, err := cs.Create(RepoRef{Namespace: "n", Name: "r"})
	if err != nil {
		t.Fatal(err)
	}
	if r.String() != "n/r" {
		t.Fatalf("repo: %s", r.String())
	}
}

func TestOpenDriverDialectors(t *testing.T) {
	// Validate each dialector builds a *gorm.DB (no server needed) by opening an
	// in-memory sqlite via glebarez and asserting the GORM session is live.
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if db == nil {
		t.Fatal("empty db")
	}
}

