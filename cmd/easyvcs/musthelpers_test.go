package main

import (
	"testing"

	"github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/revision"
)

// mustWriteBlob writes a blob, panicking (failing the test) on a persistence
// error so tests can ignore the returned error for brevity.
func mustWriteBlob(t *testing.T, ws *revision.Workspace, data []byte) object.ID {
	t.Helper()
	id, err := ws.WriteBlob(data)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// mustWriteTree writes a tree, failing the test on error.
func mustWriteTree(t *testing.T, ws *revision.Workspace, tree *object.Tree) object.ID {
	t.Helper()
	id, err := ws.WriteTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// mustWriteConflict writes a conflict object, failing the test on error.
func mustWriteConflict(t *testing.T, ws *revision.Workspace, c *object.Conflict) object.ID {
	t.Helper()
	id, err := ws.WriteConflict(c)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
