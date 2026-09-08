package store

import (
	"os"
	"testing"
	"time"

	"github.com/easylab-platform/easyvcs/object"
)

func now() time.Time { return time.Now().UTC() }

// TestBackendPostgres runs a full round-trip against Postgres if evaluated,
// otherwise it is skipped. It is the integration probe for the pgx dialector.
func TestBackendPostgres(t *testing.T) {
	dsn := os.Getenv("EASYVCS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("EASYVCS_TEST_PG_DSN not set; skipping postgres integration")
	}
	cs, err := OpenDriver(DriverConfig{Kind: KindPostgres, DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	roundTrip(t, cs)
}

// TestBackendMySQL runs a full round-trip against MySQL/MariaDB.
func TestBackendMySQL(t *testing.T) {
	dsn := os.Getenv("EASYVCS_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("EASYVCS_TEST_MYSQL_DSN not set; skipping mysql integration")
	}
	cs, err := OpenDriver(DriverConfig{Kind: KindMySQL, DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	roundTrip(t, cs)
}

func roundTrip(t *testing.T, cs *CentralStore) {
	t.Helper()
	repo, err := cs.Create(RepoRef{Namespace: "n", Name: "r"})
	if err != nil {
		t.Fatal(err)
	}
	s := &Snapshot{RevisionHash: object.BlobID([]byte("h")), RevisionID: "id1", TreeID: object.BlobID([]byte("t")), Description: "d", Author: Author{Name: "a"}, CommitTime: now()}
	if err := repo.PutSnapshot(s); err != nil {
		t.Fatal(err)
	}
	if err := repo.PutRevision(&Revision{ID: "id1", Hash: s.RevisionHash, Created: now(), ChangedPaths: []string{"f"}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.PutRef(&Ref{Name: "main", Kind: RefBranch, Target: "id1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.GetRevision("id1"); err != nil {
		t.Fatal(err)
	}
}
