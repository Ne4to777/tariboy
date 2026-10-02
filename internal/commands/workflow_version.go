package commands

import (
	"github.com/alekzonder/tariboy/internal/registry"
	"github.com/alekzonder/tariboy/internal/workflowfile"
)

func workflowVersionGet() registry.Command {
	return registry.Command{
		Path: "workflow.version.get", Summary: "Print workflow_version from a local Workflowfile.yaml",
		Args:    []registry.Arg{{Name: "path", Flag: "path", Type: registry.String, Default: ".", Help: "Workflowfile.yaml or its directory"}},
		Handler: func(_ *registry.Ctx, p registry.Params) (any, error) { return workflowfile.GetVersion(str(p, "path")) },
	}
}

func workflowVersionUpdate() registry.Command {
	return registry.Command{
		Path: "workflow.version.update", Summary: "Bump workflow_version in a local Workflowfile.yaml",
		Args: []registry.Arg{
			{Name: "part", Type: registry.String, Required: true, Help: "SemVer component to increment: major, minor, or patch"},
			{Name: "path", Flag: "path", Type: registry.String, Default: ".", Help: "Workflowfile.yaml or its directory"},
		},
		Handler: func(_ *registry.Ctx, p registry.Params) (any, error) {
			return workflowfile.UpdateVersion(str(p, "path"), str(p, "part"))
		},
	}
}
