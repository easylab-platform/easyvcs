package revision

import (
	"fmt"
	"time"

	"github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/store"
)

// Derive creates a new branch revision that shares the source revision's
// content (same tree and parents) but carries a fresh, independent revision id,
// plus a ForkFrom pointer back to the source. This is the "branch = derive"
// primitive: the derived revision can be amended on its own branch without
// mutating the revision shared by any other branch (which would otherwise leak
// an in-place amend across branches).
//
// Because snapshotID includes the revision id, the derived snapshot has a
// different hash than the source even though its content is identical, so the
// two revisions are never confused and never share a mutable snapshot.
//
// description is the commit message for the derived revision. If empty, the
// source revision's message is used; if the source has no message either, an
// error is returned (a derive must always carry a commit message).
//
// autoCommit controls what happens when the source is an uncommitted change
// (a revision with no commit message): with autoCommit true the derive proceeds
// (the caller is asserting they want to finalize the fork point now); with
// autoCommit false an error is returned, requiring the caller to commit the
// source first.
func (w *Workspace) Derive(sourceRevisionID string, description string, autoCommit bool) (*store.Snapshot, *store.Revision, error) {
	src, err := w.store.GetRevision(sourceRevisionID)
	if err != nil {
		return nil, nil, ErrRevisionNotFound
	}
	srcSnap, err := w.store.GetSnapshot(src.Hash)
	if err != nil {
		return nil, nil, err
	}

	// A revision qualifies as "committed" once it has a commit message. An
	// empty-message revision is a placeholder fork point.
	committed := srcSnap.Description != ""
	if !committed && !autoCommit {
		return nil, nil, fmt.Errorf(
			"derive: source revision %s is not committed (no message); commit it first or pass auto_commit",
			sourceRevisionID,
		)
	}

	msg := description
	if msg == "" {
		msg = srcSnap.Description
	}
	if msg == "" {
		return nil, nil, fmt.Errorf("derive: a commit message is required")
	}

	newID, err := object.RandomChangeID()
	if err != nil {
		return nil, nil, err
	}

	ns := &store.Snapshot{
		RevisionID:  newID,
		Parents:     srcSnap.Parents,
		TreeID:      srcSnap.TreeID,
		Description: msg,
		Author:      srcSnap.Author,
		CommitTime:  time.Now().UTC(),
	}
	ns.RevisionHash = snapshotID(ns)
	if err := w.store.PutSnapshot(ns); err != nil {
		return nil, nil, err
	}

	newRev := &store.Revision{
		ID:           newID,
		Hash:         ns.RevisionHash,
		Created:      time.Now().UTC(),
		ForkFrom:     sourceRevisionID,
		ChangedPaths: src.ChangedPaths,
	}
	if err := w.store.PutRevision(newRev); err != nil {
		return nil, nil, err
	}
	return ns, newRev, nil
}

// SetDescription rewrites a revision's commit message while keeping its id,
// tree, parents, and author unchanged. Since the snapshot hash derives from the
// description, the snapshot is rewritten and the revision is repointed to it.
func (w *Workspace) SetDescription(revisionID string, description string) (*store.Snapshot, *store.Revision, error) {
	rev, err := w.store.GetRevision(revisionID)
	if err != nil {
		return nil, nil, ErrRevisionNotFound
	}
	cur, err := w.store.GetSnapshot(rev.Hash)
	if err != nil {
		return nil, nil, err
	}
	ns := &store.Snapshot{
		RevisionID:  cur.RevisionID,
		Parents:     cur.Parents,
		TreeID:      cur.TreeID,
		Description: description,
		Author:      cur.Author,
		CommitTime:  cur.CommitTime,
	}
	ns.RevisionHash = snapshotID(ns)
	if err := w.store.PutSnapshot(ns); err != nil {
		return nil, nil, err
	}
	updated := &store.Revision{
		ID:           rev.ID,
		Hash:         ns.RevisionHash,
		Created:      rev.Created,
		ForkFrom:     rev.ForkFrom,
		ChangedPaths: rev.ChangedPaths,
	}
	if err := w.store.PutRevision(updated); err != nil {
		return nil, nil, err
	}
	return ns, updated, nil
}
