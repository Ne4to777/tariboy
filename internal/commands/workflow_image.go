package commands

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/alekzonder/tariboy/internal/api"
	"github.com/alekzonder/tariboy/internal/paths"
	"github.com/alekzonder/tariboy/internal/registry"
	"github.com/alekzonder/tariboy/internal/workflowfile"
	"github.com/alekzonder/tariboy/internal/workflowimage"
)

func workflowImageRegistry(c *registry.Ctx) (*workflowimage.Registry, error) {
	if c.Store == nil {
		return nil, api.UserError{Code: "build_failed", Msg: "workflow image metadata store is unavailable"}
	}
	return &workflowimage.Registry{
		Store: &workflowimage.Store{Dir: paths.Paths{Base: c.BaseDir}.WorkflowsDir()},
		DB:    c.Store.DB,
	}, nil
}

func workflowImageStore(c *registry.Ctx) *workflowimage.Store {
	return &workflowimage.Store{Dir: paths.Paths{Base: c.BaseDir}.WorkflowsDir()}
}

// workflowImageError maps the workflow image errors to their API codes.
func workflowImageError(err error) error {
	var invalid *workflowimage.InvalidError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &invalid):
		return workflowInvalid(err.Error(), invalid.Errors)
	case errors.Is(err, workflowimage.ErrVersionPublished):
		return api.UserError{Code: "workflow_version_published", Msg: err.Error(), Status: http.StatusConflict}
	case errors.Is(err, workflowimage.ErrInUse):
		return api.UserError{Code: "workflow_in_use", Msg: err.Error(), Status: http.StatusConflict}
	case errors.Is(err, workflowimage.ErrNotFound):
		return api.UserError{Code: "not_found", Msg: err.Error(), Status: http.StatusNotFound}
	case errors.Is(err, workflowimage.ErrInvalid):
		return workflowInvalid(err.Error(), nil)
	default:
		return err
	}
}

func workflowInvalid(msg string, errs []workflowfile.ValidationError) error {
	ue := api.UserError{Code: "workflow_invalid", Msg: msg, Status: http.StatusBadRequest}
	if len(errs) > 0 {
		ue.Data = map[string]any{"errors": errs}
	}
	return ue
}

// workflowSourcePath resolves the source or path argument to a source
// directory. When a Store selector is given, release must be called once the
// build no longer reads the Store tree.
func workflowSourcePath(c *registry.Ctx, p registry.Params) (path string, release func(), err error) {
	selector, path := strings.TrimSpace(str(p, "source")), strings.TrimSpace(str(p, "path"))
	switch {
	case selector != "" && path != "":
		return "", nil, api.UserError{Code: "bad_source", Msg: "source and path are mutually exclusive", Status: http.StatusBadRequest}
	case selector != "":
		prepared, release, err := storeCatalog(c).PrepareWorkflowBuild(selector)
		if err != nil {
			return "", nil, storeError(err)
		}
		return prepared.Path, release, nil
	case path == "":
		return "", nil, api.UserError{Code: "missing_path", Msg: "workflow source path is required", Status: http.StatusBadRequest}
	}
	return path, func() {}, nil
}

// parseWorkflowSource parses the manifest at path. A manifest that cannot be
// read or decoded is reported as workflow_invalid.
func parseWorkflowSource(path string) (*workflowfile.File, error) {
	f, err := workflowfile.Parse(path)
	if err != nil {
		return nil, workflowInvalid(err.Error(), nil)
	}
	return f, nil
}

func workflowValidate() registry.Command {
	return registry.Command{
		Path:    "workflow.validate",
		Summary: "Validate a workflow source directory without building it",
		Args: []registry.Arg{
			{Name: "source", Type: registry.String, Help: "Store selector (store/workflow)"},
			{Name: "path", Flag: "path", Type: registry.String, Help: "Workflowfile.yaml or its directory"},
		},
		HTTP: &registry.HTTPRoute{Method: http.MethodPost, Path: "/api/workflow-images/validate"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			path, release, err := workflowSourcePath(c, p)
			if err != nil {
				return nil, err
			}
			defer release()
			f, err := parseWorkflowSource(path)
			if err != nil {
				return nil, err
			}
			errs := workflowfile.Validate(f)
			if errs == nil {
				errs = []workflowfile.ValidationError{}
			}
			return map[string]any{
				"valid":   len(errs) == 0,
				"name":    f.Name,
				"version": f.WorkflowVersion,
				"pools":   nonNil(f.Pools()),
				"files":   nonNil(f.Files()),
				"errors":  errs,
			}, nil
		},
	}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func workflowBuild() registry.Command {
	return registry.Command{
		Path:    "workflow.build",
		Summary: "Build a workflow image from a Workflowfile.yaml",
		Args: []registry.Arg{
			{Name: "source", Type: registry.String, Help: "Store selector (store/workflow)"},
			{Name: "path", Flag: "path", Type: registry.String, Help: "Workflowfile.yaml or its directory"},
		},
		HTTP: &registry.HTTPRoute{Method: http.MethodPost, Path: "/api/workflow-images/build"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			path, release, err := workflowSourcePath(c, p)
			if err != nil {
				return nil, err
			}
			defer release()
			reg, err := workflowImageRegistry(c)
			if err != nil {
				return nil, err
			}
			f, err := parseWorkflowSource(path)
			if err != nil {
				return nil, err
			}
			m, created, err := reg.Publish(f, time.Now())
			if err != nil {
				return nil, workflowImageError(err)
			}
			tags, err := reg.Store.Tags(m.Name, m.Digest)
			if err != nil {
				return nil, err
			}
			return map[string]any{"name": m.Name, "version": m.Version, "digest": m.Digest, "tags": nonNil(tags), "created": created}, nil
		},
	}
}

func workflowLs() registry.Command {
	return registry.Command{
		Path:    "workflow.ls",
		Summary: "List built workflow images",
		HTTP:    &registry.HTTPRoute{Method: http.MethodGet, Path: "/api/workflow-images"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			listed, err := workflowImageStore(c).List()
			if err != nil {
				return nil, workflowImageError(err)
			}
			rows := make([]map[string]any, 0, len(listed))
			for _, l := range listed {
				rows = append(rows, map[string]any{"name": l.Name, "tag": l.Tag, "version": l.Version, "digest": l.Digest, "built_at": l.BuiltAt})
			}
			return map[string]any{"workflows": rows, "count": len(rows)}, nil
		},
	}
}

func workflowInspect() registry.Command {
	return registry.Command{
		Path:    "workflow.inspect",
		Summary: "Show a workflow image manifest",
		Args: []registry.Arg{
			{Name: "name", Type: registry.String, Required: true, Help: "workflow name"},
			{Name: "tag", Type: registry.String, Default: "latest", Help: "tag or full digest"},
		},
		HTTP: &registry.HTTPRoute{Method: http.MethodGet, Path: "/api/workflow-images/{name}/{tag}"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			tag := str(p, "tag")
			if tag == "" {
				tag = "latest"
			}
			m, err := workflowImageStore(c).Inspect(str(p, "name"), tag)
			if err != nil {
				return nil, workflowImageError(err)
			}
			return m, nil
		},
	}
}

func workflowRm() registry.Command {
	return registry.Command{
		Path:    "workflow.rm",
		Summary: "Remove a workflow image tag, and its content when no tag remains",
		Args: []registry.Arg{
			{Name: "name", Type: registry.String, Required: true, Help: "workflow name"},
			{Name: "tag", Type: registry.String, Required: true, Help: "tag to remove"},
		},
		HTTP: &registry.HTTPRoute{Method: http.MethodDelete, Path: "/api/workflow-images/{name}/{tag}"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			reg, err := workflowImageRegistry(c)
			if err != nil {
				return nil, err
			}
			name, tag := str(p, "name"), str(p, "tag")
			// Registry.Remove deletes the content and its row with the last tag.
			_, contentRemoved, err := reg.Remove(name, tag)
			if err != nil {
				return nil, workflowImageError(err)
			}
			return map[string]any{"name": name, "tag": tag, "removed": true, "content_removed": contentRemoved}, nil
		},
	}
}
