package stores

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/alekzonder/tariboy/internal/workflowfile"
)

// StoreWorkflow is a workflow source found under a Store's workflows/
// directory. A source that cannot be read carries Error and no Version.
type StoreWorkflow struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Error   string `json:"error,omitempty"`
}

// PrepareWorkflowBuild resolves "store/workflow" to its source directory and
// holds the catalog lock until release is called, like PrepareBuild.
func (c *Catalog) PrepareWorkflowBuild(selector string) (prepared PreparedBuild, release func(), err error) {
	catalogMu.Lock()
	release = catalogMu.Unlock
	fail := func(cause error) (PreparedBuild, func(), error) {
		release()
		return PreparedBuild{}, nil, cause
	}
	parts := strings.Split(selector, "/")
	if len(parts) != 2 || validName(parts[0]) != nil || validName(parts[1]) != nil {
		return fail(fmt.Errorf("%w selector %q (want store/workflow)", ErrInvalid, selector))
	}
	store, err := c.get(parts[0])
	if err != nil {
		return fail(err)
	}
	if err := realDirectory(store.Path); err != nil {
		return fail(err)
	}
	workflowsDir := filepath.Join(store.Path, "workflows")
	if err := realDirectory(workflowsDir); err != nil {
		return fail(err)
	}
	workflowDir := filepath.Join(workflowsDir, parts[1])
	if err := realDirectory(workflowDir); err != nil {
		return fail(err)
	}
	if err := regularFile(filepath.Join(workflowDir, workflowfile.DefaultFilename)); err != nil {
		return fail(err)
	}
	return PreparedBuild{Name: parts[1], Path: workflowDir}, release, nil
}

// workflowInventory lists the workflow sources under root/workflows. A missing
// directory is an empty list; a broken source fills Error and leaves its
// siblings intact.
func workflowInventory(root string) ([]StoreWorkflow, error) {
	if err := realDirectory(root); err != nil {
		return nil, err
	}
	workflowsDir := filepath.Join(root, "workflows")
	if _, err := os.Lstat(workflowsDir); errors.Is(err, os.ErrNotExist) {
		return []StoreWorkflow{}, nil
	} else if err != nil {
		return nil, err
	}
	if err := realDirectory(workflowsDir); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(workflowsDir)
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	result := make([]StoreWorkflow, 0, len(entries))
	for _, entry := range entries {
		item := StoreWorkflow{Name: entry.Name()}
		dir := filepath.Join(workflowsDir, entry.Name())
		if err := realDirectory(dir); err != nil {
			item.Error = err.Error()
		} else if err := regularFile(filepath.Join(dir, workflowfile.DefaultFilename)); err != nil {
			item.Error = err.Error()
		} else if parsed, err := workflowfile.Parse(dir); err != nil {
			item.Error = err.Error()
		} else {
			item.Version = parsed.WorkflowVersion
		}
		result = append(result, item)
	}
	return result, nil
}
