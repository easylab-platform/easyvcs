package revision

import (
	"fmt"

	"github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/store"
)

// GCOptions configures the reachable-object collection.
type GCOptions struct {
	// DryRun only reports the number of prunable objects without deleting.
	DryRun bool
}

// GCResult summarizes a garbage-collection pass.
type GCResult struct {
	Total int // objects present
	Swept int // objects pruned (0 in dry-run)
}

// GCRun computes the set of objects reachable from every repository's snapshots
// (blob -> tree -> conflict, all content-addressed and shared globally), then
// prunes any object not reachable. It is a whole-store operation; use DryRun to
// preview. It returns counts.
func GCRun(cs *store.CentralStore, opts GCOptions) (GCResult, error) {
	var res GCResult

	repos, err := cs.List()
	if err != nil {
		return res, err
	}

	reachable := map[string]bool{}
	for _, rref := range repos {
		repo, err := cs.OpenRepo(rref)
		if err != nil {
			continue
		}
		// Gather every tree/blob/conflict reachable from each snapshot tree.
		revs, err := repo.ListRevisions()
		if err != nil {
			continue
		}
		for _, rev := range revs {
			// Collect all objects reachable from the snapshot's tree.
			if err := collectReachableFromTree(repo, rev.Hash, reachable); err != nil {
				continue
			}
		}
	}

	all, err := objectIDsGlobal(cs)
	if err != nil {
		return res, err
	}
	res.Total = len(all)
	repo := firstRepo(cs)
	for _, id := range all {
		if reachable[id.String()] {
			continue
		}
		if opts.DryRun {
			res.Swept++
			continue
		}
		// Delete from the shared object store. A failed delete must not be
		// counted as swept (the report would lie about the store's state).
		if repo == nil {
			return res, fmt.Errorf("gc: no repository handle to delete objects from")
		}
		if err := repo.DeleteObject(id); err != nil {
			return res, fmt.Errorf("gc: delete object %s: %w", id, err)
		}
		res.Swept++
	}
	return res, nil
}

// collectReachableFromTree loads a snapshot by hash and walks its tree,
// recording every object id (blob/tree/conflict) reachable, regardless of
// which repo it lives in (objects are global).
func collectReachableFromTree(repo *store.Repo, snapHash object.ID, reachable map[string]bool) error {
	cur, err := repo.GetSnapshot(snapHash)
	if err != nil {
		return err
	}
	return collectTreeReachable(repo, cur.TreeID, reachable)
}

// collectTreeReachable walks a tree and its nested trees/blobs/conflicts.
func collectTreeReachable(repo *store.Repo, treeID object.ID, reachable map[string]bool) error {
	if reachable[treeID.String()] {
		return nil
	}
	reachable[treeID.String()] = true
	obj, err := repo.ReadObject(treeID)
	if err != nil {
		return nil
	}
	if obj.Kind != object.KindTree || obj.Tree == nil {
		return nil
	}
	for _, e := range obj.Tree.SortedEntries() {
		reachable[e.ID.String()] = true
		if e.Kind == object.KindTree {
			if err := collectTreeReachable(repo, e.ID, reachable); err != nil {
				return err
			}
		}
	}
	return nil
}

// objectIDsGlobal lists every object present in the shared store.
func objectIDsGlobal(cs *store.CentralStore) ([]object.ID, error) {
	repos, err := cs.List()
	if err != nil || len(repos) == 0 {
		return nil, err
	}
	repo, err := cs.OpenRepo(repos[0])
	if err != nil {
		return nil, err
	}
	return repo.ObjectIDs()
}

// firstRepo returns the first repository (for object deletion — objects are
// global so any repo handle can delete them).
func firstRepo(cs *store.CentralStore) *store.Repo {
	repos, err := cs.List()
	if err != nil || len(repos) == 0 {
		return nil
	}
	r, err := cs.OpenRepo(repos[0])
	if err != nil {
		return nil
	}
	return r
}
