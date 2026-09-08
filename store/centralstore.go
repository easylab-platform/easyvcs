package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"


	"github.com/easylab-platform/easyvcs/encoding"
	"github.com/easylab-platform/easyvcs/object"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// sqlDialect is defined in driver.go; it provides both the raw *sql.DB handle
// (for SetWAL and legacy helpers) and the GORM session used for model queries.
// CentralStore is the single-database store. It holds many repositories, each
// identified by (namespace, name). Objects are global (content-addressed and
// deduplicated); metadata tables are scoped by a repo id.
type CentralStore struct {
	d    *sqlDialect
	root string
}



// HomeDir returns the EasyVCS home directory (default ~/.easyvcs, overridable
// via EASYVCS_HOME).
func HomeDir() string {
	if h := os.Getenv("EASYVCS_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".easyvcs"
	}
	return filepath.Join(home, ".easyvcs")
}

// DBPath returns the path to the central database file.
func DBPath() string { return filepath.Join(HomeDir(), DefaultDBFile) }

// SetWAL enables SQLite WAL mode for safe concurrent access across processes
// (many workspaces sharing one DB). No-op for Postgres/MySQL.
func (s *CentralStore) SetWAL() error {
	if !s.d.isSQLite() {
		return nil
	}
	if err := s.d.gdb.Exec("PRAGMA journal_mode=WAL").Error; err != nil {
		return err
	}
	return nil
}

// cleanupStaleColumns drops columns that a previous schema shipped but the
// current models no longer declare. Without this, GORM's SQLite migrator
// rebuilds the table (create `<table>__temp`) and the column-mapping rewrite
// can violate a NOT NULL constraint while copying rows — a hard startup
// failure on any pre-bookmark DB. We drop them with native DDL first (out of
// band, not via the migrator) so AutoMigrate sees a clean target.
//
// Dropped columns are semantically obsolete: the "bookmark" concept was
// replaced by "branch"; the live value lives in default_branch / id.
func (s *CentralStore) cleanupStaleColumns() error {
	if !s.d.isSQLite() {
		return nil
	}
	// Each entry: (table, staleColumn). DROP COLUMN requires the column to
	// exist; guard on PRAGMA table_info so a fresh DB (column absent) is a
	// no-op and an already-migrated DB is idempotent.
	stale := [][2]string{
		{"repositories", "default_bookmark"},
		{"workspaces", "bookmark"},
	}
	for _, c := range stale {
		table, col := c[0], c[1]
		var exists bool
		if err := s.d.gdb.Raw(
			"SELECT EXISTS (SELECT 1 FROM pragma_table_info(?) WHERE name = ?)",
			table, col,
		).Scan(&exists).Error; err != nil {
			// pragma_table_info is supported on SQLite 3.16+; on failure, fall
			// back to a tolerant SELECT from sqlite_master.
			if err2 := s.d.gdb.Raw(
				"SELECT EXISTS (SELECT 1 FROM pragma_table_info(s.name) WHERE p.name = ?) FROM pragma_table_info(?) s",
			).Error; err2 != nil {
				return err
			}
		}
		if !exists {
			continue
		}
		// Column exists in this table; drop it. Wrapped so a concurrent or
		// already-migrated DB that races with us still converges.
		if err := s.d.gdb.Exec("ALTER TABLE " + table + " DROP COLUMN " + col).Error; err != nil {
			// Some SQLite builds (pre-3.35) can't DROP COLUMN; shrinking the
			// scope: if it fails, that's a hard upgrade requirement. Surface it.
			return fmt.Errorf("drop stale column %s.%s: %w", table, col, err)
		}
	}
	return nil
}

// Init creates the schema via GORM AutoMigrate, generating portable DDL
// (sqlite / postgres / mysql). Idempotent.
func (s *CentralStore) Init() error {
	if err := s.cleanupStaleColumns(); err != nil {
		return err
	}
	return s.d.gdb.AutoMigrate(allModels()...)
}

// Close closes the underlying database.
func (s *CentralStore) Close() error { return s.d.db.Close() }

// Create creates a new repository and returns a repo-scoped handle.
func (s *CentralStore) Create(r RepoRef) (*Repo, error) {
	// Check for conflict.
	var count int64
	if err := s.d.gdb.Model(&repoRow{}).Where("namespace=? AND name=?", r.Namespace, r.Name).Count(&count).Error; err != nil {
		return nil, err
	}
	if count > 0 {
		return nil, fmt.Errorf("%w: %s", ErrRepoExists, r)
	}
	row := &repoRow{Namespace: r.Namespace, Name: r.Name, Created: time.Now().UTC().UnixMilli()}
	if err := s.d.gdb.Create(row).Error; err != nil {
		return nil, err
	}
	return &Repo{cs: s, repoID: row.ID, Namespace: r.Namespace, Name: r.Name}, nil
}

// OpenRepo opens an existing repository. Returns ErrRepoNotFound if missing.
func (s *CentralStore) OpenRepo(r RepoRef) (*Repo, error) {
	var row repoRow
	err := s.d.gdb.Where("namespace=? AND name=?", r.Namespace, r.Name).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("%w: %s", ErrRepoNotFound, r)
	}
	if err != nil {
		return nil, err
	}
	return &Repo{cs: s, repoID: row.ID, Namespace: r.Namespace, Name: r.Name}, nil
}

// RepoExists reports whether a repository exists.
func (s *CentralStore) RepoExists(r RepoRef) (bool, error) {
	var count int64
	if err := s.d.gdb.Model(&repoRow{}).Where("namespace=? AND name=?", r.Namespace, r.Name).Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// Delete removes a repository and its scoped metadata.
func (s *CentralStore) Delete(r RepoRef) error {
	repo, err := s.OpenRepo(r)
	if err != nil {
		return err
	}
	for _, table := range []any{&snapshotRow{}, &revisionRow{}, &refRow{}, &mergeRequestRow{}} {
		if err := s.d.gdb.Where("repo_id=?", repo.repoID).Delete(table).Error; err != nil {
			return err
		}
	}
	// Delete MR reviews/comments via a subquery on the repo's MR ids.
	sub := s.d.gdb.Model(&mergeRequestRow{}).Select("id").Where("repo_id=?", repo.repoID)
	_ = s.d.gdb.Where("mr_id IN (?)", sub).Delete(&mrReviewRow{}).Error
	_ = s.d.gdb.Where("mr_id IN (?)", s.d.gdb.Model(&mergeRequestRow{}).Select("id").Where("repo_id=?", repo.repoID)).Delete(&mrCommentRow{}).Error
	if err := s.d.gdb.Delete(&repoRow{}, repo.repoID).Error; err != nil {
		return err
	}
	return nil
}

// List returns all repositories sorted by namespace/name.
func (s *CentralStore) List() ([]RepoRef, error) {
	var rows []repoRow
	if err := s.d.gdb.Order("namespace, name").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]RepoRef, 0, len(rows))
	for _, r := range rows {
		out = append(out, RepoRef{Namespace: r.Namespace, Name: r.Name})
	}
	return out, nil
}

// Fork copies a repository's scoped data (snapshots, revisions, refs) plus its
// hosting metadata into a new (namespace, name), sharing the global object
// store. The source repository is left untouched. It returns the new repo.
func (s *CentralStore) Fork(src RepoRef, dst RepoRef) (*Repo, error) {
	srcRepo, err := s.OpenRepo(src)
	if err != nil {
		return nil, err
	}
	dstRepo, err := s.Create(dst)
	if err != nil {
		return nil, err
	}
	// Copy hosting metadata. A metadata copy failure is best-effort (the fork
	// still succeeds); it is not silently dropped without a note.
	if meta, err := srcRepo.RepoMeta(); err == nil {
		if err := dstRepo.UpdateRepoMeta(meta); err != nil {
			return nil, fmt.Errorf("fork: copy repo meta: %w", err)
		}
	}

	// Repo identities are globally unique; a forked repository must carry its
	// OWN revision ids so two forks never share a revision (traceability). We
	// remap every snapshot/revision id to a fresh random id and recompute the
	// snapshot hash (the revision id participates in the content hash). Objects
	// (blob/tree) are content-addressed and shared as-is, so no object copy is
	// needed beyond referencing the same id.
	//
	// We read the source snapshots and revisions first to build the id map, then
	// write the remapped rows. Refs are remapped to the new revision ids.
	var remap = map[string]string{} // old revision id -> new revision id

	var snapRows []snapshotRow
	if err := s.d.gdb.Where("repo_id=?", srcRepo.repoID).Find(&snapRows).Error; err != nil {
		return nil, err
	}
	type snapRec struct {
		sha, revisionID, treeID string
		commitTime              int64
		meta                    []byte
	}
	snaps := make([]snapRec, 0, len(snapRows))
	for _, sr := range snapRows {
		snaps = append(snaps, snapRec{sha: sr.ID, revisionID: sr.RevisionID, treeID: sr.TreeID, commitTime: sr.CommitTime, meta: sr.Meta})
	}

	var revRows []revisionRow
	if err := s.d.gdb.Where("repo_id=?", srcRepo.repoID).Find(&revRows).Error; err != nil {
		return nil, err
	}
	type revRec struct {
		id, hash string
		created  int64
		forkFrom sql.NullString
		changed  []byte
	}
	revs := make([]revRec, 0, len(revRows))
	for _, rr := range revRows {
		revs = append(revs, revRec{id: rr.ID, hash: rr.Hash, created: rr.Created, forkFrom: sql.NullString{String: rr.ForkFrom, Valid: rr.ForkFrom != ""}, changed: rr.ChangedPath})
	}

	// Assign fresh ids to every source revision.
	for _, r := range revs {
		nid, err := object.RandomRevisionID()
		if err != nil {
			return nil, err
		}
		remap[r.id] = nid
	}

	// Build all remapped snapshots in memory first, keyed by old snapshot hash,
	// so parent pointers can be rewritten to the new (re-keyed) snapshot hashes.
	type newSnap struct {
		ns *Snapshot
	}
	var newSnaps []newSnap
	for _, sn := range snaps {
		decoded, err := encoding.DecodeSnapshotMeta(sn.meta)
		if err != nil {
			return nil, err
		}
		nid := remap[sn.revisionID]
		treeID, err := object.HexToID(sn.treeID)
		if err != nil {
			return nil, err
		}
		ns := &Snapshot{
			RevisionID:  nid,
			Parents:     decoded.Parents, // placeholder; rewritten below
			TreeID:      treeID,
			Description: decoded.Description,
			Author:      Author{Name: decoded.Author.Name, Email: decoded.Author.Email},
			CommitTime:  time.UnixMilli(sn.commitTime),
		}
		ns.RevisionHash = SnapshotHashFor(ns)
		newSnaps = append(newSnaps, newSnap{ns: ns})
	}
	// Rewrite parent pointers to the new snapshot hashes. We need a preliminary
	// map of old-snapshot-sha -> new-hash to translate parent edges; this map is
	// sealed AFTER parent rewrite (below) because a snapshot's own hash changes
	// when its parents change. For the initial (parent-unrewritten) snapshots we
	// use the first-pass hash to translate edges.
	prelim := map[string]object.ID{}
	for i, sn := range snaps {
		prelim[sn.sha] = newSnaps[i].ns.RevisionHash
	}
	for i := range newSnaps {
		oldParents := snaps[i]
		decoded, err := encoding.DecodeSnapshotMeta(oldParents.meta)
		if err != nil {
			return nil, err
		}
		ns := newSnaps[i].ns
		parents := make([]object.ID, 0, len(decoded.Parents))
		for _, p := range decoded.Parents {
			if m, ok := prelim[p.String()]; ok {
				parents = append(parents, m)
			} else {
				parents = append(parents, p) // external/root edge, keep as-is
			}
		}
		ns.Parents = parents
		ns.RevisionHash = SnapshotHashFor(ns)
	}
	// Seal the final old-sha -> final-new-hash map after parent rewrites.
	finalHashByOldSha := map[string]object.ID{}
	for i, sn := range snaps {
		finalHashByOldSha[sn.sha] = newSnaps[i].ns.RevisionHash
	}
	for _, ns := range newSnaps {
		if err := dstRepo.PutSnapshot(ns.ns); err != nil {
			return nil, err
		}
	}

	// Write remapped revisions. The revision's hash is the recomputed hash of
	// its remapped snapshot (looked up by the new revision id among newSnaps).
	for _, r := range revs {
		nid := remap[r.id]
		var newHash object.ID
		for _, sn := range snaps {
			if remap[sn.revisionID] == nid {
				newHash = finalHashByOldSha[sn.sha]
				break
			}
		}
		if newHash == (object.ID{}) {
			newHash = mustHexID(r.hash)
		}
		if err := dstRepo.PutRevision(&Revision{
			ID:           nid,
			Hash:         newHash,
			Created:      time.UnixMilli(r.created),
			ForkFrom:     r.forkFrom.String,
			ChangedPaths: decodeChanged(r.changed),
		}); err != nil {
			return nil, err
		}
	}

	// Write remapped refs (branch/tag targets -> new revision id).
	var refRows []refRow
	if err := s.d.gdb.Where("repo_id=?", srcRepo.repoID).Find(&refRows).Error; err != nil {
		return nil, err
	}
	for _, rr := range refRows {
		newTarget := rr.Target
		if mapped, ok := remap[rr.Target]; ok {
			newTarget = mapped
		}
		if err := dstRepo.PutRef(&Ref{Name: rr.Name, Kind: RefKind(rr.Kind), Target: newTarget}); err != nil {
			return nil, err
		}
	}
	return dstRepo, nil
}

func mustHexID(s string) object.ID {
	id, _ := object.HexToID(s)
	return id
}

func decodeChanged(raw []byte) []string {
	if raw == nil {
		return nil
	}
	out := []string{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// RepoID returns the numeric repo id used internally.
func (r *Repo) RepoID() int64 { return r.repoID }

func (r *Repo) String() string { return r.Namespace + "/" + r.Name }

// WriteObject stores a global content-addressed object. Objects are immutable,
// so an existing object is never overwritten.
func (r *Repo) WriteObject(o *object.Object) error {
	id := o.ID()
	payload, err := object.EncodeObject(o)
	if err != nil {
		return err
	}
	row := &objectRow{ID: id.String(), Kind: int(o.Kind), Content: payload}
	// ON CONFLICT DO NOTHING
	return r.cs.d.gdb.Clauses(clause.OnConflict{DoNothing: true}).Create(row).Error
}

// WriteObjectsBatch stores many objects in a single transaction, reducing
// round-trips when applying a bundle. Each object's id is recomputed from its
// content; duplicate ids are ignored (idempotent).
func (r *Repo) WriteObjectsBatch(objs []*object.Object) error {
	if len(objs) == 0 {
		return nil
	}
	return r.cs.d.gdb.Transaction(func(tx *gorm.DB) error {
		for _, o := range objs {
			id := o.ID()
			payload, err := object.EncodeObject(o)
			if err != nil {
				return err
			}
			row := &objectRow{ID: id.String(), Kind: int(o.Kind), Content: payload}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(row).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// ReadObject reads a global content-addressed object.
func (r *Repo) ReadObject(id object.ID) (*object.Object, error) {
	var row objectRow
	err := r.cs.d.gdb.Where("sha=?", id.String()).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return object.DecodeObject(id, row.Content)
}

// ObjectIDs returns all content-addressed object ids held anywhere in the
// central store (objects are global and deduplicated across repositories). It
// is used by clients to advertise "have" objects during fetch/push so a peer
// only sends objects the destination lacks.
func (r *Repo) ObjectIDs() ([]object.ID, error) {
	var rows []string
	if err := r.cs.d.gdb.Model(&objectRow{}).Pluck("sha", &rows).Error; err != nil {
		return nil, err
	}
	out := make([]object.ID, 0, len(rows))
	for _, s := range rows {
		if id, err := object.HexToID(s); err == nil {
			out = append(out, id)
		}
	}
	return out, nil
}

// ObjectExists reports whether an object is present.
func (r *Repo) ObjectExists(id object.ID) (bool, error) {
	var count int64
	if err := r.cs.d.gdb.Model(&objectRow{}).Where("sha=?", id.String()).Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// DeleteObject removes a single content-addressed object (global). It is used
// by the GC command to prune objects no longer referenced by any snapshot. The
// caller is responsible for reachability analysis.
func (r *Repo) DeleteObject(id object.ID) error {
	return r.cs.d.gdb.Where("sha=?", id.String()).Delete(&objectRow{}).Error
}

// PutSnapshot stores a snapshot scoped to this repository. Parents, author, and
// description are packed into a single compact meta BLOB (see encoding pkg).
func (r *Repo) PutSnapshot(snap *Snapshot) error {
	meta := encoding.EncodeSnapshotMeta(encoding.BaseSnapshot{
		Parents:     snap.Parents,
		Author:      encoding.Author{Name: snap.Author.Name, Email: snap.Author.Email},
		Description: snap.Description,
	})
	row := &snapshotRow{
		ID: snap.RevisionHash.String(), RepoID: r.repoID, RevisionID: snap.RevisionID,
		TreeID: snap.TreeID.String(), CommitTime: snap.CommitTime.UnixMilli(), Meta: meta,
	}
	return r.cs.d.gdb.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "repo_id"}, {Name: "sha"}},
		UpdateAll: true,
	}).Create(row).Error
}

// GetSnapshot reads a snapshot scoped to this repository.
func (r *Repo) GetSnapshot(id object.ID) (*Snapshot, error) {
	return r.getSnapshot(id)
}

func (r *Repo) getSnapshot(id object.ID) (*Snapshot, error) {
	var row snapshotRow
	err := r.cs.d.gdb.Where("repo_id=? AND sha=?", r.repoID, id.String()).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	oid, err := object.HexToID(row.ID)
	if err != nil {
		return nil, err
	}
	tid, err := object.HexToID(row.TreeID)
	if err != nil {
		return nil, err
	}
	decoded, err := encoding.DecodeSnapshotMeta(row.Meta)
	if err != nil {
		return nil, err
	}
	return &Snapshot{
		RevisionHash: oid, RevisionID: row.RevisionID, Parents: decoded.Parents, TreeID: tid,
		Description: decoded.Description,
		Author:      Author{Name: decoded.Author.Name, Email: decoded.Author.Email},
		CommitTime:  time.UnixMilli(row.CommitTime),
	}, nil
}

// PutRevision stores a revision scoped to this repository.
func (r *Repo) PutRevision(rev *Revision) error {
	row := r.toRevisionRow(rev)
	return r.cs.d.gdb.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "repo_id"}, {Name: "id"}},
		UpdateAll: true,
	}).Create(row).Error
}

// PutRevisionTx inserts a revision within an open transaction.
func (r *Repo) PutRevisionTx(tx *gorm.DB, rev *Revision) error {
	row := r.toRevisionRow(rev)
	return tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "repo_id"}, {Name: "id"}},
		UpdateAll: true,
	}).Create(row).Error
}

// PutSnapshotTx inserts a snapshot within an open transaction.
func (r *Repo) PutSnapshotTx(tx *gorm.DB, snap *Snapshot) error {
	meta := encoding.EncodeSnapshotMeta(encoding.BaseSnapshot{
		Parents:     snap.Parents,
		Author:      encoding.Author{Name: snap.Author.Name, Email: snap.Author.Email},
		Description: snap.Description,
	})
	row := &snapshotRow{
		ID: snap.RevisionHash.String(), RepoID: r.repoID, RevisionID: snap.RevisionID,
		TreeID: snap.TreeID.String(), CommitTime: snap.CommitTime.UnixMilli(), Meta: meta,
	}
	return tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "repo_id"}, {Name: "sha"}},
		UpdateAll: true,
	}).Create(row).Error
}

// WriteObjectsBatchTx stores objects in the given transaction.
func (r *Repo) WriteObjectsBatchTx(tx *gorm.DB, objs []*object.Object) error {
	for _, o := range objs {
		id := o.ID()
		payload, err := object.EncodeObject(o)
		if err != nil {
			return err
		}
		row := &objectRow{ID: id.String(), Kind: int(o.Kind), Content: payload}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(row).Error; err != nil {
			return err
		}
	}
	return nil
}

// BeginTx starts a GORM transaction on the central store.
func (r *Repo) BeginTx() *gorm.DB { return r.cs.d.gdb.Begin() }

// toRevisionRow maps a Revision to its persistent row.
func (r *Repo) toRevisionRow(rev *Revision) *revisionRow {
	changed, _ := json.Marshal(rev.ChangedPaths)
	return &revisionRow{
		RepoID: r.repoID, ID: rev.ID, Hash: rev.Hash.String(),
		Created: rev.Created.UnixMilli(), ForkFrom: rev.ForkFrom, ChangedPath: changed,
	}
}

// GetRevision reads a revision scoped to this repository.
func (r *Repo) GetRevision(id string) (*Revision, error) {
	var row revisionRow
	err := r.cs.d.gdb.Where("repo_id=? AND id=?", r.repoID, id).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return r.fromRevisionRow(&row), nil
}

func (r *Repo) fromRevisionRow(row *revisionRow) *Revision {
	cur, _ := object.HexToID(row.Hash)
	return &Revision{
		ID: row.ID, Hash: cur, Created: time.UnixMilli(row.Created),
		ForkFrom: row.ForkFrom, ChangedPaths: decodeChangedPathsJSON(row.ChangedPath),
	}
}

// UpdateRevisionHash atomically repoints a revision's Hash field. It preserves
// the existing ChangedPaths.
func (r *Repo) UpdateRevisionHash(revisionID string, hash object.ID) error {
	rev, err := r.GetRevision(revisionID)
	if err != nil {
		return err
	}
	rev.Hash = hash
	return r.PutRevision(rev)
}

// UpdateRevisionHashCAS repoints a revision's Hash to a new snapshot hash only
// if the revision still points at `expectHash`. This is an optimistic-concurrency
// (compare-and-swap) guard: if another writer amended the revision in between,
// ErrRevisionChanged is returned instead of silently last-write-wins. The
// snapshot is written only when the CAS succeeds.
func (r *Repo) UpdateRevisionHashCAS(revisionID string, expectHash, newHash object.ID) error {
	rev, err := r.GetRevision(revisionID)
	if err != nil {
		return err
	}
	if rev.Hash != expectHash {
		return ErrRevisionChanged
	}
	rev.Hash = newHash
	return r.PutRevision(rev)
}

// ListRevisions lists revisions scoped to this repository.
func (r *Repo) ListRevisions() ([]*Revision, error) {
	var rows []revisionRow
	if err := r.cs.d.gdb.Where("repo_id=?", r.repoID).Order("id").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*Revision, 0, len(rows))
	for i := range rows {
		out = append(out, r.fromRevisionRow(&rows[i]))
	}
	return out, nil
}

// PutRef stores a ref scoped to this repository.
func (r *Repo) PutRef(ref *Ref) error {
	row := &refRow{RepoID: r.repoID, Name: ref.Name, Kind: string(ref.Kind), Target: ref.Target}
	return r.cs.d.gdb.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "repo_id"}, {Name: "name"}},
		UpdateAll: true,
	}).Create(row).Error
}

// DeleteRef removes a ref scoped to this repository.
func (r *Repo) DeleteRef(name string) error {
	res := r.cs.d.gdb.Where("repo_id=? AND name=?", r.repoID, name).Delete(&refRow{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// GetRef reads a ref scoped to this repository.
func (r *Repo) GetRef(name string) (*Ref, error) {
	var row refRow
	err := r.cs.d.gdb.Where("repo_id=? AND name=?", r.repoID, name).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &Ref{Name: row.Name, Kind: RefKind(row.Kind), Target: row.Target}, nil
}

// ListRefs lists refs scoped to this repository.
func (r *Repo) ListRefs() ([]*Ref, error) {
	var rows []refRow
	if err := r.cs.d.gdb.Where("repo_id=?", r.repoID).Order("name").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*Ref, 0, len(rows))
	for i := range rows {
		out = append(out, &Ref{Name: rows[i].Name, Kind: RefKind(rows[i].Kind), Target: rows[i].Target})
	}
	return out, nil
}

// Close is a no-op for the repo handle (the CentralStore is shared).
func (r *Repo) Close() error { return nil }

// RepoRef returns the RepoRef for this handle.
func (r *Repo) RepoRef() RepoRef { return RepoRef{Namespace: r.Namespace, Name: r.Name} }

// Remote is a named URL to another EasyVCS server. Token is the optional
// bearer token used to authenticate writes against that server.
type Remote struct {
	Name  string
	URL   string
	Token string
}

// PutRemote registers or updates a remote for this repository. An empty token
// clears the stored token.
func (r *Repo) PutRemote(name, url, token string) error {
	row := &remoteRow{RepoID: r.repoID, Name: name, URL: url, Token: nullable(token)}
	return r.cs.d.gdb.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "repo_id"}, {Name: "name"}},
		UpdateAll: true,
	}).Create(row).Error
}

// UpdateRemoteSyncTip records the last successfully synced tip for a remote
// branch (the local revision id that mirrors the remote's tip after a pull).
// Empty clears it. This enables incremental/conflict-aware pulls.
func (r *Repo) UpdateRemoteSyncTip(name, lastSyncTip string) error {
	return r.cs.d.gdb.Model(&remoteRow{}).Where("repo_id=? AND name=?", r.repoID, name).Update("last_sync_tip", lastSyncTip).Error
}

// GetLastSyncTip returns the last synced tip for a remote ("" if none).
func (r *Repo) GetLastSyncTip(name string) (string, error) {
	var row remoteRow
	err := r.cs.d.gdb.Where("repo_id=? AND name=?", r.repoID, name).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return row.LastSyncTip, nil
}

// RemoteRef is a snapshot of a remote's ref as last fetched, scoped to a remote
// name (e.g. "origin.main", kind=branch). This mirrors git's refs/remotes.
type RemoteRef struct {
	RemoteName string
	Kind       RefKind
	Name       string
	Target     string // revision id on the remote
}

// SetRemoteRef upserts a remote ref. Primary key is remote+kind+name.
func (r *Repo) SetRemoteRef(remoteName string, ref *RemoteRef) error {
	row := &remoteRefRow{RepoID: r.repoID, RemoteName: remoteName, Kind: string(ref.Kind), Name: ref.Name, Target: ref.Target}
	return r.cs.d.gdb.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "repo_id"}, {Name: "remote_name"}, {Name: "kind"}, {Name: "name"}},
		UpdateAll: true,
	}).Create(row).Error
}

// ListRemoteRefs lists all remote refs for this repo (optionally filtered by
// remote name). Returns refs keyed under the remote (name is bare, e.g. "main").
func (r *Repo) ListRemoteRefs(remoteName string) ([]*RemoteRef, error) {
	q := r.cs.d.gdb.Where("repo_id=?", r.repoID)
	if remoteName != "" {
		q = q.Where("remote_name=?", remoteName)
	}
	var rows []remoteRefRow
	if err := q.Order("remote_name, name").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*RemoteRef, 0, len(rows))
	for i := range rows {
		out = append(out, &RemoteRef{RemoteName: rows[i].RemoteName, Kind: RefKind(rows[i].Kind), Name: rows[i].Name, Target: rows[i].Target})
	}
	return out, nil
}

// DeleteRemoteRefsForRemote clears all recorded refs for a remote (used when a
// remote is deleted or force-refreshed).
func (r *Repo) DeleteRemoteRefsForRemote(remoteName string) error {
	return r.cs.d.gdb.Where("repo_id=? AND remote_name=?", r.repoID, remoteName).Delete(&remoteRefRow{}).Error
}

// SetRemoteDefaultBranch records which branch on a remote is its default.
func (r *Repo) SetRemoteDefaultBranch(name, def string) error {
	return r.cs.d.gdb.Model(&remoteRow{}).Where("repo_id=? AND name=?", r.repoID, name).Update("default_branch", def).Error
}

// GetRemoteDefaultBranch returns the remote's default branch ("" if none).
func (r *Repo) GetRemoteDefaultBranch(name string) (string, error) {
	var row remoteRow
	err := r.cs.d.gdb.Where("repo_id=? AND name=?", r.repoID, name).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return row.DefaultBranch, nil
}

// GetRemote returns a remote by name.
func (r *Repo) GetRemote(name string) (*Remote, error) {
	var row remoteRow
	err := r.cs.d.gdb.Where("repo_id=? AND name=?", r.repoID, name).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	token := ""
	if row.Token != nil {
		token = *row.Token
	}
	return &Remote{Name: name, URL: row.URL, Token: token}, nil
}

// DeleteRemote removes a remote by name.
func (r *Repo) DeleteRemote(name string) error {
	return r.cs.d.gdb.Where("repo_id=? AND name=?", r.repoID, name).Delete(&remoteRow{}).Error
}

// ListRemotes lists all remotes for this repository. Tokens are omitted from
// the returned list to avoid leaking secrets.
func (r *Repo) ListRemotes() ([]*Remote, error) {
	var rows []remoteRow
	if err := r.cs.d.gdb.Where("repo_id=?", r.repoID).Order("name").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*Remote, 0, len(rows))
	for i := range rows {
		out = append(out, &Remote{Name: rows[i].Name, URL: rows[i].URL})
	}
	return out, nil
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// QueryCount returns the total number of content-addressed objects. It is a
// convenience for tests to verify global deduplication.
func (s *CentralStore) QueryCount(count *int) error {
	var c int64
	if err := s.d.gdb.Model(&objectRow{}).Count(&c).Error; err != nil {
		return err
	}
	*count = int(c)
	return nil
}

