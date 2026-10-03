package workflowimage

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alekzonder/tariboy/internal/portablearchive"
	"github.com/alekzonder/tariboy/internal/workflowfile"
)

const archiveKind = "workflow-image"

// archiveMetadata travels in the portable archive manifest. The archive
// format does not keep permission bits, so the executable paths are listed
// here; the digest lets the importer prove it rebuilt the same content.
type archiveMetadata struct {
	Name       string   `json:"name"`
	Version    string   `json:"version"`
	Digest     string   `json:"digest"`
	Executable []string `json:"executable"`
}

// Export writes the stored files of the image a tag or digest names as a
// portable archive of kind "workflow-image".
func (s *Store) Export(w io.Writer, name, tagOrDigest string) (Manifest, error) {
	m, err := s.Inspect(name, tagOrDigest)
	if err != nil {
		return Manifest{}, err
	}
	meta := archiveMetadata{Name: m.Name, Version: m.Version, Digest: m.Digest, Executable: []string{}}
	for _, f := range m.Files {
		if f.Executable {
			meta.Executable = append(meta.Executable, f.Path)
		}
	}
	return m, writeArchive(w, s.ContentDir(m.Name, m.Digest), m.Files, meta)
}

func writeArchive(w io.Writer, root string, files []FileEntry, meta archiveMetadata) error {
	raw, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return writePortable(w, root, files, archiveKind, raw)
}

func writePortable(w io.Writer, root string, files []FileEntry, kind string, meta json.RawMessage) error {
	list := make([]portablearchive.File, len(files))
	paths := make([]string, len(files))
	for i, f := range files {
		list[i] = portablearchive.File{Path: f.Path, SHA256: f.SHA256, Size: f.Size}
		paths[i] = f.Path
	}
	return portablearchive.Write(w, root, portablearchive.Manifest{
		Format: "tariboy-portable", Version: 1, Kind: kind, Files: list, Metadata: meta,
	}, paths)
}

// Import stages an archive written by Export under stagingRoot and publishes
// it like a build: the version tag and "latest" point at it. Content whose
// digest differs from the one the archive names is refused before anything is
// written. The staged copy is always removed.
func (r *Registry) Import(archive io.Reader, size int64, stagingRoot string, now time.Time) (Manifest, bool, error) {
	if err := os.MkdirAll(stagingRoot, 0o700); err != nil {
		return Manifest{}, false, err
	}
	tmp, err := os.MkdirTemp(stagingRoot, "import-")
	if err != nil {
		return Manifest{}, false, err
	}
	defer os.RemoveAll(tmp)
	dir := filepath.Join(tmp, "source")
	pm, err := portablearchive.Stage(archive, size, dir, portablearchive.DefaultLimits())
	if err != nil {
		return Manifest{}, false, fmt.Errorf("%w: %v", ErrBadArchive, err)
	}
	if pm.Kind != archiveKind {
		return Manifest{}, false, fmt.Errorf("%w: archive kind is %q", ErrBadArchive, pm.Kind)
	}
	var meta archiveMetadata
	dec := json.NewDecoder(strings.NewReader(string(pm.Metadata)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&meta); err != nil || !digestPattern.MatchString(meta.Digest) {
		return Manifest{}, false, fmt.Errorf("%w: invalid metadata", ErrBadArchive)
	}
	listed := make(map[string]bool, len(pm.Files))
	for _, f := range pm.Files {
		listed[f.Path] = true
	}
	for _, p := range meta.Executable {
		// Stage checked every listed path, so a listed one is safe to join.
		if !listed[p] {
			return Manifest{}, false, fmt.Errorf("%w: executable %q is not in the archive", ErrBadArchive, p)
		}
		if err := os.Chmod(filepath.Join(dir, filepath.FromSlash(p)), 0o700); err != nil {
			return Manifest{}, false, err
		}
	}
	src, err := workflowfile.Parse(dir)
	if err != nil {
		return Manifest{}, false, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return r.publish(src, meta.Digest, now)
}
