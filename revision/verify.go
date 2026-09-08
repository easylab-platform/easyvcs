package revision

import (
	"github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/store"
)

// VerifyResult reports the outcome of a store consistency check.
type VerifyResult struct {
	Repos           int
	Revisions       int
	Snapshots       int
	Objects         int
	MissingObjects  int
	BrokenSnapshots int
}

// Verify checks store consistency: every snapshot's tree must be readable and
// every revision's current snapshot must resolve. It does not mutate anything.
func Verify(cs *store.CentralStore) (VerifyResult, error) {
	var res VerifyResult
	repos, err := cs.List()
	if err != nil {
		return res, err
	}
	res.Repos = len(repos)
	for _, rref := range repos {
		repo, err := cs.OpenRepo(rref)
		if err != nil {
			continue
		}
		revs, err := repo.ListRevisions()
		if err != nil {
			continue
		}
		res.Revisions += len(revs)
		for _, rev := range revs {
			snap, err := repo.GetSnapshot(rev.Hash)
			if err != nil {
				res.BrokenSnapshots++
				continue
			}
			res.Snapshots++
			// Walk the snapshot's tree, counting reachable objects and checking
			// any object we touch is actually present.
			missing, err := verifyTree(repo, snap.TreeID)
			if err != nil {
				res.BrokenSnapshots++
			}
			res.MissingObjects += missing
		}
	}
	// Objects present in the global store.
	if repo := firstRepo(cs); repo != nil {
		ids, err := repo.ObjectIDs()
		if err == nil {
			res.Objects = len(ids)
		}
	}
	return res, nil
}

// verifyTree walks a tree, returning the number of referenced objects that are
// missing from the store (content-addressed objects should always be present if
// they were written transitively).
func verifyTree(repo *store.Repo, treeID object.ID) (int, error) {
	var missing int
	obj, err := repo.ReadObject(treeID)
	if err != nil {
		return 1, err
	}
	if obj.Kind != object.KindTree || obj.Tree == nil {
		return 0, nil
	}
	for _, e := range obj.Tree.SortedEntries() {
		if e.Kind == object.KindTree {
			m, err := verifyTree(repo, e.ID)
			if err != nil {
				missing += m
				continue
			}
			missing += m
		} else {
			ok, err := repo.ObjectExists(e.ID)
			if err != nil || !ok {
				missing++
			}
		}
	}
	return missing, nil
}
