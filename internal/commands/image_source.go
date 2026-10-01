package commands

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"time"

	"github.com/alekzonder/tariboy/internal/api"
	"github.com/alekzonder/tariboy/internal/image"
	"github.com/alekzonder/tariboy/internal/imagefile"
	"github.com/alekzonder/tariboy/internal/imagesnapshot"
	"github.com/alekzonder/tariboy/internal/imagesource"
	"github.com/alekzonder/tariboy/internal/paths"
	"github.com/alekzonder/tariboy/internal/registry"
)

func imageSourceStore(c *registry.Ctx) *imagesource.Store {
	return &imagesource.Store{
		Root:  paths.Paths{Base: c.BaseDir}.ImageSourcesDir(),
		Clock: time.Now,
	}
}

func imageSnapshotStore(c *registry.Ctx) *imagesnapshot.Store {
	return &imagesnapshot.Store{
		DB: c.Store.DB, Root: filepath.Join(c.BaseDir, "image-source-snapshots"), Clock: time.Now,
	}
}

func imageSourceUserError(err error) (api.UserError, bool) {
	switch {
	case errors.Is(err, imagesource.ErrInvalidName):
		return api.UserError{Code: "bad_source", Msg: err.Error(), Status: http.StatusBadRequest}, true
	case errors.Is(err, imagesource.ErrExists):
		return api.UserError{Code: "source_exists", Msg: err.Error(), Status: http.StatusConflict}, true
	case errors.Is(err, imagesource.ErrNotFound):
		return api.UserError{Code: "source_not_found", Msg: err.Error(), Status: http.StatusNotFound}, true
	case errors.Is(err, imagesource.ErrInvalidPath),
		errors.Is(err, imagesource.ErrInvalidUTF8),
		errors.Is(err, imagesource.ErrFileTooLarge),
		errors.Is(err, imagesource.ErrUnsafeFile):
		return api.UserError{Code: "bad_source_path", Msg: err.Error(), Status: http.StatusForbidden}, true
	default:
		return api.UserError{}, false
	}
}

func imageSourceError(err error) error {
	if userErr, ok := imageSourceUserError(err); ok {
		return userErr
	}
	return err
}

func imageSourceLs() registry.Command {
	return registry.Command{
		Path:    "image.source.ls",
		Summary: "List editable image sources",
		HTTP:    &registry.HTTPRoute{Method: http.MethodGet, Path: "/api/image-sources"},
		Handler: func(c *registry.Ctx, _ registry.Params) (any, error) {
			sources, err := imageSourceStore(c).List()
			if err != nil {
				return nil, imageSourceError(err)
			}
			return map[string]any{"sources": sources, "count": len(sources)}, nil
		},
	}
}

func imageSourceCreate() registry.Command {
	return registry.Command{
		Path:    "image.source.create",
		Summary: "Create an editable image source",
		Args: []registry.Arg{
			{Name: "name", Type: registry.String, Required: true, Help: "source name"},
			{Name: "prompt", Flag: "prompt", Type: registry.String, Help: "initial prompt"},
		},
		HTTP: &registry.HTTPRoute{Method: http.MethodPost, Path: "/api/image-sources"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			source, err := imageSourceStore(c).Create(imagesource.CreateRequest{
				Name:   str(p, "name"),
				Prompt: str(p, "prompt"),
			})
			if err != nil {
				if userErr, ok := imageSourceUserError(err); ok {
					return nil, userErr
				}
				return nil, api.UserError{Code: "bad_source", Msg: err.Error(), Status: http.StatusBadRequest}
			}
			return source, nil
		},
	}
}

func imageSourceInspect() registry.Command {
	return registry.Command{
		Path:    "image.source.inspect",
		Summary: "Show editable image source metadata",
		Args:    []registry.Arg{{Name: "name", Type: registry.String, Required: true, Help: "source name"}},
		HTTP:    &registry.HTTPRoute{Method: http.MethodGet, Path: "/api/image-sources/{name}"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			source, err := imageSourceStore(c).Get(str(p, "name"))
			if err != nil {
				return nil, imageSourceError(err)
			}
			return source, nil
		},
	}
}

func imageSourceRm() registry.Command {
	return registry.Command{
		Path:    "image.source.rm",
		Summary: "Remove an editable image source without removing built images",
		Args:    []registry.Arg{{Name: "name", Type: registry.String, Required: true, Help: "source name"}},
		HTTP:    &registry.HTTPRoute{Method: http.MethodDelete, Path: "/api/image-sources/{name}"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			name := str(p, "name")
			if err := imageSourceStore(c).Delete(name); err != nil {
				return nil, imageSourceError(err)
			}
			return map[string]any{"removed": name}, nil
		},
	}
}

func imageSourceFiles() registry.Command {
	return registry.Command{
		Path:    "image.source.files",
		Summary: "List editable files in an image source",
		Args:    []registry.Arg{{Name: "name", Type: registry.String, Required: true, Help: "source name"}},
		HTTP:    &registry.HTTPRoute{Method: http.MethodGet, Path: "/api/image-sources/{name}/files"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			files, err := imageSourceStore(c).ListFiles(str(p, "name"))
			if err != nil {
				return nil, imageSourceError(err)
			}
			return map[string]any{"files": files, "count": len(files)}, nil
		},
	}
}

func imageSourceFileGet() registry.Command {
	return registry.Command{
		Path:    "image.source.file.get",
		Summary: "Read an editable image source file",
		Args: []registry.Arg{
			{Name: "name", Type: registry.String, Required: true, Help: "source name"},
			{Name: "path", Type: registry.String, Required: true, Help: "source-relative file path"},
		},
		HTTP: &registry.HTTPRoute{Method: http.MethodGet, Path: "/api/image-sources/{name}/files/{path...}"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			path := str(p, "path")
			content, err := imageSourceStore(c).ReadFile(str(p, "name"), path)
			if err != nil {
				return nil, imageSourceError(err)
			}
			return map[string]any{"path": path, "content": string(content)}, nil
		},
	}
}

func imageSourceFilePut() registry.Command {
	return registry.Command{
		Path:      "image.source.file.put",
		Summary:   "Write an editable image source file",
		CLIHidden: true,
		Args: []registry.Arg{
			{Name: "name", Type: registry.String, Required: true, Help: "source name"},
			{Name: "path", Type: registry.String, Required: true, Help: "source-relative file path"},
			{Name: "content", Type: registry.String, Required: true, Help: "UTF-8 file content"},
		},
		HTTP: &registry.HTTPRoute{Method: http.MethodPut, Path: "/api/image-sources/{name}/files/{path...}"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			path := str(p, "path")
			if err := imageSourceStore(c).WriteFile(str(p, "name"), path, []byte(str(p, "content"))); err != nil {
				return nil, imageSourceError(err)
			}
			return map[string]any{"path": path, "saved": true}, nil
		},
	}
}

func imageSourceValidate() registry.Command {
	return registry.Command{
		Path:    "image.source.validate",
		Summary: "Validate an editable image source",
		Args:    []registry.Arg{{Name: "name", Type: registry.String, Required: true, Help: "source name"}},
		HTTP:    &registry.HTTPRoute{Method: http.MethodPost, Path: "/api/image-sources/{name}/validate"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			name := str(p, "name")
			store := imageSourceStore(c)
			if _, err := store.Get(name); err != nil {
				return nil, imageSourceError(err)
			}
			dir := filepath.Join(paths.Paths{Base: c.BaseDir}.ImageSourcesDir(), name)
			parsed, err := imagefile.ParseV2(dir)
			if err != nil {
				return map[string]any{
					"valid": false,
					"diagnostics": []map[string]string{{
						"path": "Tariboyfile.yaml", "message": err.Error(),
					}},
				}, nil
			}
			if len(parsed.Extends) > 0 {
				return map[string]any{"valid": false, "diagnostics": []map[string]string{{"path": "Tariboyfile.yaml", "message": imagefile.ExtendsUnassembledMessage}}}, nil
			}
			return map[string]any{"valid": true, "diagnostics": []any{}}, nil
		},
	}
}

func imageSourceBuild() registry.Command {
	return registry.Command{
		Path:    "image.source.build",
		Summary: "Build an image from an editable image source",
		Args: []registry.Arg{
			{Name: "name", Type: registry.String, Required: true, Help: "source name"},
			{Name: "tag", Flag: "tag", Type: registry.String, Default: "latest", Help: "target image tag"},
		},
		HTTP: &registry.HTTPRoute{Method: http.MethodPost, Path: "/api/image-sources/{name}/build"},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			name := str(p, "name")
			tag := str(p, "tag")
			if tag == "" {
				tag = "latest"
			}
			ref, err := image.ParseRef(name + ":" + tag)
			if err != nil {
				return nil, api.UserError{Code: "bad_ref", Msg: err.Error(), Status: http.StatusBadRequest}
			}
			if image.IsReserved(ref) {
				return nil, api.UserError{Code: "reserved_image", Msg: "image " + ref.String() + " is managed by tariboyd", Status: http.StatusConflict}
			}

			sources := imageSourceStore(c)
			source, err := sources.Get(name)
			if err != nil {
				return nil, imageSourceError(err)
			}
			var manifest image.Manifest
			var parseErr error
			store := imageStore(c)
			var record imagesource.BuildRecord
			err = image.WithPublicationGate(func() error {
				if err := store.Migrate(); err != nil {
					return err
				}
				publication, err := store.BeginPublication([]image.Ref{ref})
				if err != nil {
					return err
				}
				record, err = sources.RecordBuild(name, func(dir string) (imagesource.BuildRecord, error) {
					parsed, err := imagefile.ParseV2(dir)
					if err != nil {
						parseErr = err
						return imagesource.BuildRecord{}, err
					}
					layout := paths.Paths{Base: c.BaseDir}
					pluginsDir := layout.PluginsDir()
					manifest, err = image.BuildV2(parsed, imagefile.ResolveRoots{Plugins: pluginsDir}, ref, store, time.Now, imagePluginResolver(c, pluginsDir))
					if err != nil {
						return imagesource.BuildRecord{}, err
					}
					if _, err := imageSnapshotStore(c).CaptureWithProvenance(
						context.Background(), ref.String(), manifest.Digest, name, dir, source.Provenance,
					); err != nil {
						return imagesource.BuildRecord{}, err
					}
					return imagesource.BuildRecord{
						Ref: ref.String(), Digest: manifest.Digest, BuiltAt: manifest.BuiltAt,
					}, nil
				}, publication.Restore)
				return err
			})
			if err != nil {
				if parseErr != nil {
					return nil, api.UserError{
						Code: "bad_imagefile", Msg: parseErr.Error(), Status: http.StatusBadRequest,
					}
				}
				if userErr, ok := imageSourceUserError(err); ok {
					return nil, userErr
				}
				return nil, api.UserError{Code: "build_failed", Msg: err.Error(), Status: http.StatusBadRequest}
			}
			return map[string]any{
				"ref":      record.Ref,
				"digest":   record.Digest,
				"built_at": record.BuiltAt,
				"layers":   len(manifest.Layers),
			}, nil
		},
	}
}
