package api

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/alekzonder/tariboy/internal/paths"
	"github.com/alekzonder/tariboy/internal/workflowimage"
)

func (s *Server) workflowImageRegistry() *workflowimage.Registry {
	return &workflowimage.Registry{
		Store: &workflowimage.Store{Dir: paths.Paths{Base: s.cctx.BaseDir}.WorkflowsDir()},
		DB:    s.cctx.Store.DB,
	}
}

// writeWorkflowArchiveErr maps the workflow image errors of export and import
// to the same codes the workflow commands use.
func writeWorkflowArchiveErr(w http.ResponseWriter, err error) {
	var invalid *workflowimage.InvalidError
	switch {
	case errors.As(err, &invalid):
		WriteErrData(w, http.StatusBadRequest, "workflow_invalid", err.Error(), map[string]any{"errors": invalid.Errors})
	case errors.Is(err, workflowimage.ErrBadArchive):
		WriteErr(w, http.StatusBadRequest, "bad_archive", err.Error())
	case errors.Is(err, workflowimage.ErrDigestMismatch):
		WriteErr(w, http.StatusBadRequest, "workflow_digest_mismatch", err.Error())
	case errors.Is(err, workflowimage.ErrInvalid):
		WriteErr(w, http.StatusBadRequest, "workflow_invalid", err.Error())
	case errors.Is(err, workflowimage.ErrVersionPublished):
		WriteErr(w, http.StatusConflict, "workflow_version_published", err.Error())
	case errors.Is(err, workflowimage.ErrNotFound):
		WriteErr(w, http.StatusNotFound, "not_found", err.Error())
	default:
		WriteErr(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

func (s *Server) serveWorkflowImageExport(w http.ResponseWriter, r *http.Request) {
	temp, err := os.CreateTemp(s.cctx.BaseDir, ".workflow-export-*.tar.gz")
	if err != nil {
		WriteErr(w, http.StatusInternalServerError, "internal", "cannot stage export")
		return
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	store := s.workflowImageRegistry().Store
	if _, err := store.Export(temp, r.PathValue("name"), r.PathValue("tag")); err != nil {
		writeWorkflowArchiveErr(w, err)
		return
	}
	if _, err := temp.Seek(0, io.SeekStart); err != nil {
		WriteErr(w, http.StatusInternalServerError, "internal", "cannot read export")
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="tariboy-workflow-image.tar.gz"`)
	_, _ = io.Copy(w, temp)
}

func (s *Server) serveWorkflowImageImport(w http.ResponseWriter, r *http.Request) {
	const maxUpload = 64 << 20
	if r.ContentLength < 0 || r.ContentLength > maxUpload {
		WriteErr(w, http.StatusRequestEntityTooLarge, "archive_too_large", "workflow archive size is missing or exceeds 64 MiB")
		return
	}
	defer r.Body.Close()
	reg := s.workflowImageRegistry()
	m, created, err := reg.Import(http.MaxBytesReader(w, r.Body, maxUpload), r.ContentLength,
		filepath.Join(s.cctx.BaseDir, "workflow-image-imports"), time.Now())
	if err != nil {
		writeWorkflowArchiveErr(w, err)
		return
	}
	tags, err := reg.Store.Tags(m.Name, m.Digest)
	if err != nil {
		writeWorkflowArchiveErr(w, err)
		return
	}
	if tags == nil {
		tags = []string{}
	}
	WriteOK(w, map[string]any{"name": m.Name, "version": m.Version, "digest": m.Digest, "tags": tags, "created": created})
}
