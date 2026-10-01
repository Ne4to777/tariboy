package commands

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/alekzonder/tariboy/internal/agent"
	"github.com/alekzonder/tariboy/internal/api"
	"github.com/alekzonder/tariboy/internal/compose"
	"github.com/alekzonder/tariboy/internal/image"
	"github.com/alekzonder/tariboy/internal/imagefile"
	"github.com/alekzonder/tariboy/internal/imagesource"
	"github.com/alekzonder/tariboy/internal/paths"
	"github.com/alekzonder/tariboy/internal/registry"
	"github.com/alekzonder/tariboy/internal/teamportable"
	"gopkg.in/yaml.v3"
)

func nestedMap(value any) map[string]any { result, _ := value.(map[string]any); return result }

func planTeamSource(imported map[string]string, planned teamportable.Image) (bool, error) {
	if digest, ok := imported[planned.SourceName]; ok {
		if digest != planned.SourceDigest {
			return false, fmt.Errorf("source %s has conflicting digests", planned.SourceName)
		}
		return true, nil
	}
	imported[planned.SourceName] = planned.SourceDigest
	return false, nil
}

func rewriteTeamComposeRefs(input []byte, refs map[string]string) ([]byte, error) {
	file, err := compose.Parse(input)
	if err != nil {
		return nil, err
	}
	for name, spec := range file.Agents {
		if replacement := refs[spec.Image]; replacement != "" {
			spec.Image = replacement
			file.Agents[name] = spec
		}
	}
	return yaml.Marshal(file)
}

func registryError(code string, err error) error { return api.UserError{Code: code, Msg: err.Error()} }

func teamCompose() registry.Command {
	return registry.Command{Path: "team.compose", Summary: "Render a group as tariboy-compose.yaml", Args: []registry.Arg{{Name: "name", Type: registry.String, Required: true}}, HTTP: &registry.HTTPRoute{Method: http.MethodGet, Path: "/api/groups/{name}/compose"}, Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
		name := str(p, "name")
		if err := checkGroupName(name); err != nil {
			return nil, err
		}
		gc, err := requireGroups(c)
		if err != nil {
			return nil, err
		}
		view, err := gc.Inspect(name)
		if err != nil {
			return nil, registryError("not_found", err)
		}
		lead, _ := view["lead"].(string)
		agents, err := agent.NewStore(c.Store).ListByGroup(name)
		if err != nil {
			return nil, err
		}
		file := compose.File{Version: 1, Images: map[string]compose.ImageSpec{}, Groups: map[string]compose.GroupSpec{name: {Lead: lead}}, Agents: map[string]compose.AgentSpec{}}
		for _, member := range agents {
			ref, err := image.ParseRef(member.ImageRef)
			if err != nil {
				return nil, err
			}
			file.Images[ref.Name] = compose.ImageSpec{Context: "./images/" + ref.Name}
			enabled := member.LoopEnabled
			timeout := ""
			if member.TimeoutS > 0 {
				timeout = (time.Duration(member.TimeoutS) * time.Second).String()
			}
			file.Agents[member.Name] = compose.AgentSpec{Image: member.ImageRef, Group: name, Cwd: member.Cwd, Harness: &compose.HarnessSpec{Type: member.HarnessType, Model: member.Model, Effort: member.Effort, Interactive: member.Interactive}, Plugins: append([]string(nil), member.Plugins...), Loop: &compose.LoopSpec{Enabled: &enabled}, Timeout: timeout}
		}
		raw, err := yaml.Marshal(file)
		if err != nil {
			return nil, err
		}
		return map[string]any{"name": name, "yaml": string(raw)}, nil
	}}
}

func teamApplyYAML() registry.Command {
	return registry.Command{Path: "team.import.yaml.apply-internal", Summary: "Apply validated team compose YAML", Args: []registry.Arg{{Name: "yaml", Type: registry.String, Required: true}}, Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
		file, err := compose.Parse([]byte(str(p, "yaml")))
		if err != nil {
			return nil, registryError("bad_compose", err)
		}
		if err := file.Validate(); err != nil {
			return nil, registryError("bad_compose", err)
		}
		for _, spec := range file.Agents {
			ref, err := image.ParseRef(spec.Image)
			if err != nil || !imageStore(c).Exists(ref) {
				return nil, api.UserError{Code: "image_missing", Msg: "import or build image " + spec.Image + " first"}
			}
		}
		gc, err := requireGroups(c)
		if err != nil {
			return nil, err
		}
		for name, group := range file.Groups {
			if _, err := gc.Create(name, group.Lead); err != nil {
				return nil, registryError("create_failed", err)
			}
		}
		results := make([]map[string]any, 0, len(file.Agents))
		for name, spec := range file.Agents {
			if existing, getErr := agent.NewStore(c.Store).Get(name); getErr == nil {
				if existing.Group != spec.Group || existing.ImageRef != spec.Image {
					results = append(results, map[string]any{"name": name, "status": "failed", "error": "existing agent has different group or image"})
				} else {
					results = append(results, map[string]any{"name": name, "status": "reused"})
				}
				continue
			}
			harness, model, effort, interactive := "", "", "", false
			if spec.Harness != nil {
				harness, model, effort, interactive = spec.Harness.Type, spec.Harness.Model, spec.Harness.Effort, spec.Harness.Interactive
			}
			loop := true
			if spec.Loop != nil && spec.Loop.Enabled != nil {
				loop = *spec.Loop.Enabled
			}
			timeoutS, timeoutErr := spec.TimeoutSeconds()
			created, runErr := "", timeoutErr
			if runErr == nil {
				created, runErr = c.Control.Run(registry.RunSpec{ImageRef: spec.Image, Name: name, Cwd: spec.Cwd, Harness: harness, Model: model, Effort: effort, Interactive: interactive, Plugins: spec.Plugins, Loop: loop, TimeoutS: timeoutS, Group: spec.Group})
			}
			item := map[string]any{"name": created, "status": "created"}
			if runErr != nil {
				item["name"] = name
				item["status"] = "failed"
				item["error"] = runErr.Error()
			}
			results = append(results, item)
		}
		complete := true
		for _, item := range results {
			if item["status"] == "failed" {
				complete = false
			}
		}
		return map[string]any{"groups": len(file.Groups), "agents": results, "complete": complete}, nil
	}}
}

func teamImportYAML() registry.Command {
	return registry.Command{Path: "team.import.yaml", Summary: "Preview pasted tariboy-compose.yaml", Args: []registry.Arg{{Name: "yaml", Type: registry.String, Required: true}}, HTTP: &registry.HTTPRoute{Method: http.MethodPost, Path: "/api/team-imports/preview-yaml"}, Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
		raw := []byte(str(p, "yaml"))
		file, err := compose.Parse(raw)
		if err == nil {
			err = file.Validate()
		}
		if err != nil {
			return nil, registryError("bad_compose", err)
		}
		if len(file.Groups) != 1 {
			return nil, api.UserError{Code: "bad_compose", Msg: "team import requires exactly one group"}
		}
		team := ""
		for name := range file.Groups {
			team = name
		}
		service := teamportable.Service{StagingRoot: filepath.Join(c.BaseDir, "team-imports")}
		preview, err := service.PreviewYAML(team, raw)
		if err != nil {
			return nil, registryError("preview_failed", err)
		}
		return planTeamImport(c, preview)
	}}
}

func applyTeamImage(c *registry.Ctx, preview teamportable.Preview, planned teamportable.Image, reuseSource bool, onSourceReady func()) error {
	return image.WithPublicationGate(func() error {
		return applyTeamImageLocked(c, preview, planned, reuseSource, onSourceReady)
	})
}

func applyTeamImageLocked(c *registry.Ctx, preview teamportable.Preview, planned teamportable.Image, reuseSource bool, onSourceReady func()) error {
	ref, err := image.ParseRef(planned.Ref)
	if err != nil || image.IsReserved(ref) {
		return api.UserError{Code: "bad_ref", Msg: "invalid image ref " + planned.Ref}
	}
	if imageStore(c).Exists(ref) {
		current, err := imageStore(c).Inspect(ref)
		if err != nil {
			return err
		}
		snapshot, ok, err := imageSnapshotStore(c).Lookup(context.Background(), ref.String())
		if err != nil {
			return err
		}
		if !ok || snapshot.ImageDigest != current.Digest || snapshot.SourceDigest != planned.SourceDigest {
			return api.UserError{Code: "image_conflict", Msg: "image " + ref.String() + " has different source; choose a new tag"}
		}
		return nil
	}
	sources := imageSourceStore(c)
	_, sourceErr := sources.Get(planned.SourceName)
	if !reuseSource && sourceErr == nil {
		return api.UserError{Code: "source_conflict", Msg: "image source " + planned.SourceName + " already exists"}
	}
	if sourceErr != nil {
		sourceDir := filepath.Join(preview.StagedDir, "images", planned.SourceName)
		if _, err := sources.ImportTree(planned.SourceName, sourceDir); err != nil {
			return registryError("source_import_failed", err)
		}
		onSourceReady()
	}
	managedSourceDir := filepath.Join(paths.Paths{Base: c.BaseDir}.ImageSourcesDir(), planned.SourceName)
	parsed, err := imagefile.ParseV2(managedSourceDir)
	if err != nil {
		return err
	}
	layout := paths.Paths{Base: c.BaseDir}
	manifest, err := image.BuildV2(parsed, imagefile.ResolveRoots{Plugins: layout.PluginsDir()}, ref, imageStore(c), time.Now, imagePluginResolver(c, layout.PluginsDir()))
	if err != nil {
		return registryError("image_build_failed", err)
	}
	snapshot, err := imageSnapshotStore(c).Capture(context.Background(), ref.String(), manifest.Digest, planned.SourceName, managedSourceDir)
	if err != nil {
		_ = imageStore(c).Remove(ref)
		return err
	}
	if snapshot.SourceDigest != planned.SourceDigest {
		_ = imageStore(c).Remove(ref)
		_, _ = c.Store.DB.Exec(`DELETE FROM image_source_snapshots WHERE image_ref=?`, ref.String())
		return api.UserError{Code: "source_digest_mismatch", Msg: "image source digest does not match archive metadata"}
	}
	_, _ = sources.RecordBuild(planned.SourceName, func(string) (imagesource.BuildRecord, error) {
		return imagesource.BuildRecord{Ref: ref.String(), Digest: manifest.Digest, BuiltAt: manifest.BuiltAt}, nil
	}, nil)
	return nil
}

func planTeamImport(c *registry.Ctx, preview teamportable.Preview) (any, error) {
	file, err := compose.Parse(preview.ComposeYAML)
	if err == nil {
		err = file.Validate()
	}
	if err != nil {
		return nil, registryError("bad_compose", err)
	}
	imagePlans := make([]map[string]any, 0, len(preview.Images))
	for _, planned := range preview.Images {
		action, conflict, message := "build", false, ""
		ref, parseErr := image.ParseRef(planned.Ref)
		if parseErr != nil || image.IsReserved(ref) {
			action, conflict, message = "invalid", true, "invalid or reserved image ref"
		} else if imageStore(c).Exists(ref) {
			current, inspectErr := imageStore(c).Inspect(ref)
			snapshot, ok, lookupErr := imageSnapshotStore(c).Lookup(context.Background(), ref.String())
			if inspectErr != nil || lookupErr != nil || !ok || snapshot.ImageDigest != current.Digest || snapshot.SourceDigest != planned.SourceDigest {
				action, conflict, message = "retag", true, "destination ref has different source"
			} else {
				action = "reuse"
			}
		}
		imagePlans = append(imagePlans, map[string]any{"ref": planned.Ref, "source_name": planned.SourceName, "source_digest": planned.SourceDigest, "action": action, "conflict": conflict, "message": message})
	}
	agentPlans := make([]map[string]any, 0, len(file.Agents))
	for name, spec := range file.Agents {
		action, conflict, message := "create", false, ""
		refPlanned := false
		for _, planned := range preview.Images {
			if planned.Ref == spec.Image {
				refPlanned = true
				break
			}
		}
		parsedRef, refErr := image.ParseRef(spec.Image)
		if !refPlanned && (refErr != nil || !imageStore(c).Exists(parsedRef)) {
			action, conflict, message = "blocked", true, "required image is unavailable; import/build it or edit YAML"
		} else if existing, getErr := agent.NewStore(c.Store).Get(name); getErr == nil {
			if existing.Group == spec.Group && existing.ImageRef == spec.Image {
				action = "reuse"
			} else {
				action, conflict, message = "rename", true, "destination agent has different group or image"
			}
		}
		agentPlans = append(agentPlans, map[string]any{"name": name, "action": action, "conflict": conflict, "message": message})
	}
	groupPlans := make([]map[string]any, 0, len(file.Groups))
	gc, groupErr := requireGroups(c)
	if groupErr != nil {
		return nil, groupErr
	}
	for name := range file.Groups {
		action, conflict, message := "create", false, ""
		if _, inspectErr := gc.Inspect(name); inspectErr == nil {
			action, conflict, message = "choose", true, "destination team exists; rename it in YAML or explicitly update it"
		}
		groupPlans = append(groupPlans, map[string]any{"name": name, "action": action, "conflict": conflict, "message": message})
	}
	return map[string]any{"import_id": preview.ImportID, "team": preview.Team, "yaml": string(preview.ComposeYAML), "images": imagePlans, "agents": agentPlans, "groups": groupPlans}, nil
}

func teamImportArchivePlan() registry.Command {
	return registry.Command{Path: "team.import.archive.plan", Summary: "Validate and plan a staged portable team import", Args: []registry.Arg{{Name: "id", Type: registry.String, Required: true}}, Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
		service := teamportable.Service{Snapshots: imageSnapshotStore(c), StagingRoot: filepath.Join(c.BaseDir, "team-imports")}
		preview, err := service.Load(str(p, "id"))
		if err != nil {
			return nil, registryError("import_not_found", err)
		}
		return planTeamImport(c, preview)
	}}
}

func teamImportArchiveApply() registry.Command {
	return registry.Command{Path: "team.import.archive.apply", Summary: "Build images and create a staged portable team", Args: []registry.Arg{{Name: "id", Type: registry.String, Required: true}}, HTTP: &registry.HTTPRoute{Method: http.MethodPost, Path: "/api/team-imports/{id}/apply"}, Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
		service := teamportable.Service{Snapshots: imageSnapshotStore(c), StagingRoot: filepath.Join(c.BaseDir, "team-imports")}
		preview, err := service.Load(str(p, "id"))
		if err != nil {
			return nil, registryError("import_not_found", err)
		}
		operation, err := service.Operation(preview.ImportID)
		if err != nil {
			return nil, registryError("import_not_found", err)
		}
		editedYAML := str(p, "yaml")
		if editedYAML == "" {
			editedYAML = operation.ComposeYAML
		}
		if editedYAML != "" {
			file, parseErr := compose.Parse([]byte(editedYAML))
			if parseErr == nil {
				parseErr = file.Validate()
			}
			if parseErr != nil {
				return nil, registryError("bad_compose", parseErr)
			}
			preview.ComposeYAML = []byte(editedYAML)
		}
		resolvedRefs := map[string]string{}
		for original, replacement := range operation.ResolvedRefs {
			resolvedRefs[original] = replacement
		}
		for original, value := range nestedMap(p["refs"]) {
			if replacement, ok := value.(string); ok && replacement != "" {
				resolvedRefs[original] = replacement
			}
		}
		if len(resolvedRefs) > 0 {
			for index := range preview.Images {
				if replacement := resolvedRefs[preview.Images[index].Ref]; replacement != "" {
					preview.Images[index].Ref = replacement
				}
			}
			preview.ComposeYAML, err = rewriteTeamComposeRefs(preview.ComposeYAML, resolvedRefs)
			if err != nil {
				return nil, registryError("bad_compose", err)
			}
		}
		updateExisting := operation.UpdateExisting
		if value, ok := p["update_existing"].(bool); ok {
			updateExisting = value
		}
		plannedResult, planErr := planTeamImport(c, preview)
		if planErr != nil {
			return nil, planErr
		}
		if planned, ok := plannedResult.(map[string]any); ok {
			if groups, ok := planned["groups"].([]map[string]any); ok {
				for _, group := range groups {
					if conflict, _ := group["conflict"].(bool); conflict && !updateExisting {
						return nil, api.UserError{Code: "team_conflict", Msg: "destination team exists; rename it in YAML or choose update existing"}
					}
				}
			}
			if agents, ok := planned["agents"].([]map[string]any); ok {
				for _, candidate := range agents {
					if conflict, _ := candidate["conflict"].(bool); conflict {
						return nil, api.UserError{Code: "agent_conflict", Msg: fmt.Sprint(candidate["name"]) + ": " + fmt.Sprint(candidate["message"])}
					}
				}
			}
		}
		operation.ComposeYAML = string(preview.ComposeYAML)
		operation.ResolvedRefs = resolvedRefs
		operation.UpdateExisting = updateExisting
		operation.Status, operation.Error = "running", ""
		for index := range preview.Images {
			if index < len(operation.Steps) {
				operation.Steps[index].Name = preview.Images[index].Ref
			}
		}
		_ = service.SaveOperation(operation)
		if err := service.TouchLease(preview.ImportID); err != nil {
			return nil, err
		}
		defer service.RemoveLease(preview.ImportID)
		heartbeatDone := make(chan struct{})
		defer close(heartbeatDone)
		go func() {
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					_ = service.TouchLease(preview.ImportID)
				case <-heartbeatDone:
					return
				}
			}
		}()
		importedSources := map[string]string{}
		for index, planned := range preview.Images {
			reuseSource, planErr := planTeamSource(importedSources, planned)
			if planErr != nil {
				return nil, registryError("source_conflict", planErr)
			}
			if index < len(operation.Steps) && operation.Steps[index].Status == "complete" {
				continue
			}
			operation.Steps[index].Status, operation.Steps[index].Error = "running", ""
			_ = service.SaveOperation(operation)
			reuseSource = reuseSource || operation.Steps[index].SourceReady
			if applyErr := applyTeamImage(c, preview, planned, reuseSource, func() {
				operation.Steps[index].SourceReady = true
				_ = service.SaveOperation(operation)
			}); applyErr != nil {
				operation.Steps[index].Status, operation.Steps[index].Error = "failed", applyErr.Error()
				operation.Status, operation.Error = "failed", applyErr.Error()
				_ = service.SaveOperation(operation)
				return nil, applyErr
			}
			operation.Steps[index].Status = "complete"
			_ = service.SaveOperation(operation)
		}
		result, err := teamApplyYAML().Handler(c, registry.Params{"yaml": string(preview.ComposeYAML)})
		if err != nil {
			operation.Status, operation.Error = "failed", err.Error()
			_ = service.SaveOperation(operation)
			return nil, err
		}
		if mapped, ok := result.(map[string]any); ok {
			mapped["import_id"] = preview.ImportID
			if agents, ok := mapped["agents"].([]map[string]any); ok {
				for _, item := range agents {
					step := teamportable.OperationStep{Kind: "agent", Name: fmt.Sprint(item["name"]), Status: fmt.Sprint(item["status"])}
					if itemError, ok := item["error"].(string); ok {
						step.Error = itemError
					}
					operation.Steps = append(operation.Steps, step)
				}
			}
			if complete, _ := mapped["complete"].(bool); complete {
				operation.Status = "complete"
			} else {
				operation.Status = "failed"
			}
			_ = service.SaveOperation(operation)
			mapped["operation"] = operation
		}
		return result, nil
	}}
}

func teamImportArchiveStatus() registry.Command {
	return registry.Command{Path: "team.import.archive.status", Summary: "Inspect portable team import progress", Args: []registry.Arg{{Name: "id", Type: registry.String, Required: true}}, HTTP: &registry.HTTPRoute{Method: http.MethodGet, Path: "/api/team-imports/{id}"}, Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
		service := teamportable.Service{StagingRoot: filepath.Join(c.BaseDir, "team-imports")}
		operation, err := service.RecoverOperation(str(p, "id"), time.Now(), 5*time.Minute)
		if err != nil {
			return nil, registryError("import_not_found", err)
		}
		return operation, nil
	}}
}
