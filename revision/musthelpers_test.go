package revision

import "github.com/easylab-platform/easyvcs/object"

// mustWriteBlob writes a blob, panicking (failing the test) on a persistence
// error so tests can ignore the returned error for brevity.
func mustWriteBlob(ws *Workspace, data []byte) object.ID {
	id, err := ws.WriteBlob(data)
	if err != nil {
		panic(err)
	}
	return id
}

// mustWriteTree writes a tree, panicking on error.
func mustWriteTree(ws *Workspace, t *object.Tree) object.ID {
	id, err := ws.WriteTree(t)
	if err != nil {
		panic(err)
	}
	return id
}

// mustWriteConflict writes a conflict object, panicking on error.
func mustWriteConflict(ws *Workspace, c *object.Conflict) object.ID {
	id, err := ws.WriteConflict(c)
	if err != nil {
		panic(err)
	}
	return id
}
