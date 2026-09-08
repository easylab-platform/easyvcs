package transfer

import (
	"fmt"

	"github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/store"
)

// NonFastForwardError describes a push that would move a branch backwards or
// rewrite history, and is therefore rejected. It carries the conflicting refs
// so the client can pull/merge before retrying.
type NonFastForwardError struct {
	// ConflictingRefs are the branch refs that would be non-fast-forwarded.
	ConflictingRefs []*store.Ref
	// LocalRefs are the refs the client was pushing (for the message).
	LocalRefs []*store.Ref
}

func (e *NonFastForwardError) Error() string {
	names := ""
	for i, r := range e.ConflictingRefs {
		if i > 0 {
			names += ", "
		}
		names += r.Name
	}
	return fmt.Sprintf("non-fast-forward update rejected for branch(es): %s", names)
}

// IsAncestor reports whether revision `a` is an ancestor of (or equal to)
// revision `b` by walking the first-parent chain in the given repo. It is the
// store-level building block for fast-forward checks and does not depend on a
// workspace. This lets both the client (pre-check) and a server (authoritative
// check) use the same logic with no network.
func IsAncestor(repo *store.Repo, a string, b string) (bool, error) {
	if a == "" || b == "" {
		return false, fmt.Errorf("IsAncestor: empty revision")
	}
	// Equal ids are trivially a fast-forward.
	if a == b {
		return true, nil
	}
	bRev, err := repo.GetRevision(b)
	if err != nil {
		return false, err
	}
	bSnap, err := repo.GetSnapshot(bRev.Hash)
	if err != nil {
		return false, err
	}
	// BFS from b's snapshot across first-parent edges, checking membership of a.
	seen := map[string]bool{bRev.Hash.String(): true}
	queue := []object.ID{bSnap.RevisionHash}
	aHash, err := hashForRevision(repo, a)
	if err != nil {
		return false, err
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		curSnap, err := repo.GetSnapshot(cur)
		if err != nil {
			continue
		}
		if curSnap.RevisionHash == aHash {
			return true, nil
		}
		for _, p := range curSnap.Parents {
			if !seen[p.String()] {
				seen[p.String()] = true
				queue = append(queue, p)
			}
		}
	}
	return false, nil
}

// hashForRevision resolves a revision id (or prefix) to its current snapshot
// hash within a repo.
func hashForRevision(repo *store.Repo, idOrPrefix string) (object.ID, error) {
	rev, err := repo.GetRevision(idOrPrefix)
	if err == nil {
		return rev.Hash, nil
	}
	// Fall back to prefix match.
	revs, lerr := repo.ListRevisions()
	if lerr != nil {
		return object.ID{}, lerr
	}
	for _, r := range revs {
		if len(idOrPrefix) <= len(r.ID) && r.ID[:len(idOrPrefix)] == idOrPrefix {
			return r.Hash, nil
		}
	}
	return object.ID{}, fmt.Errorf("unknown revision %q", idOrPrefix)
}

// CheckNonFastForward compares the refs the client expects on the server
// (expected) against the refs the client is about to push (incoming). For each
// incoming branch that would make an existing server branch move non-fast
// forward (i.e. the incoming target is not a descendant of the expected target),
// it is recorded in the returned conflict list. A nil/empty conflict list means
// the update is a pure fast-forward (or the branch is new/unchanged).
//
// The incoming target revision may not be stored in repo yet (it is coming in
// the bundle), so we additionally accept the incoming bundle's snapshots to
// resolve the incoming tip's ancestry. reachable uses repo's snapshot graph for
// revisions it already knows and the bundle's snapshots for new ones.
func CheckNonFastForward(repo *store.Repo, expected []*store.Ref, incoming []*store.Ref, bundle *Bundle) []*store.Ref {
	byName := map[string]*store.Ref{}
	for _, r := range expected {
		byName[r.Name] = r
	}
	var conflicts []*store.Ref
	for _, inc := range incoming {
		if inc.Kind != store.RefBranch {
			continue
		}
		exp, ok := byName[inc.Name]
		if !ok {
			continue // brand-new branch: no conflict
		}
		if exp.Target == inc.Target {
			continue // unchanged
		}
		if repo != nil {
			ff, err := isAncestorUnion(repo, bundle, exp.Target, inc.Target)
			if err == nil && ff {
				continue // fast-forward: allowed
			}
		}
		conflicts = append(conflicts, inc)
	}
	return conflicts
}

// isAncestorUnion reports whether revision `a` is an ancestor of (or equal to)
// `b`, walking a graph that combines the server repo's snapshots with any
// snapshots carried in the incoming bundle. This handles a fast-forward where
// the incoming tip object is not yet present in the repo.
func isAncestorUnion(repo *store.Repo, b *Bundle, a, c string) (bool, error) {
	if a == "" || c == "" {
		return false, nil
	}
	if a == c {
		return true, nil
	}
	// childHashByParentName: for each known revision id, its snapshot hash.
	// We can resolve an id to a snapshot hash either from the repo (if present)
	// or from the bundle's revisions+snapshots.
	resolveHash := func(id string) (object.ID, bool) {
		if rev, err := repo.GetRevision(id); err == nil {
			return rev.Hash, true
		}
		if b != nil {
			for _, rv := range b.Revisions {
				if rv.ID == id {
					return rv.Hash, true
				}
			}
		}
		return object.ID{}, false
	}
	snapForHash := func(h object.ID) (*store.Snapshot, bool) {
		if s, err := repo.GetSnapshot(h); err == nil {
			return s, true
		}
		if b != nil {
			for _, s := range b.Snapshots {
				if s.RevisionHash == h {
					return s, true
				}
			}
		}
		return nil, false
	}
	cHash, ok := resolveHash(c)
	if !ok {
		return false, nil
	}
	aHash, ok := resolveHash(a)
	if !ok {
		return false, nil
	}
	// BFS from c snapshot across parents using the union graph.
	seen := map[object.ID]bool{cHash: true}
	queue := []object.ID{cHash}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == aHash {
			return true, nil
		}
		snap, ok := snapForHash(cur)
		if !ok {
			continue
		}
		for _, p := range snap.Parents {
			if !seen[p] {
				seen[p] = true
				queue = append(queue, p)
			}
		}
	}
	return false, nil
}
