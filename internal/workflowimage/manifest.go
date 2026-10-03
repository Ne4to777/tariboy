// Package workflowimage stores built workflow images: the Workflowfile.yaml
// source of a workflow plus its scripts and instructions, addressed by the
// SHA-256 digest of their content.
package workflowimage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/alekzonder/tariboy/internal/workflowfile"
)

var (
	ErrNotFound         = errors.New("workflow image not found")
	ErrVersionPublished = errors.New("workflow version is already published with different content")
	ErrInvalid          = errors.New("workflow source is invalid")
	// ErrInUse means a queue is bound to the image or a task, open or closed,
	// is pinned to it.
	ErrInUse = errors.New("workflow image is in use")
	// ErrDigestMismatch means an imported archive does not hold the content
	// its metadata names.
	ErrDigestMismatch = errors.New("workflow archive content does not match its digest")
	// ErrBadArchive means an import is not a readable workflow image archive.
	ErrBadArchive = errors.New("not a workflow image archive")
)

// InvalidError is the error Publish returns when the manifest fails
// validation. It matches ErrInvalid and carries every validation error.
type InvalidError struct {
	Errors []workflowfile.ValidationError
}

func (e *InvalidError) Error() string {
	parts := make([]string, 0, len(e.Errors))
	for _, v := range e.Errors {
		parts = append(parts, fmt.Sprintf("%s: %s", v.Path, v.Message))
	}
	return ErrInvalid.Error() + ": " + strings.Join(parts, "; ")
}

func (e *InvalidError) Unwrap() error { return ErrInvalid }

type FileEntry struct {
	Path       string `json:"path"` // source-relative, slash-separated
	SHA256     string `json:"sha256"`
	Executable bool   `json:"executable"`
	Size       int64  `json:"size"`
}

type Manifest struct {
	SchemaVersion int               `json:"schema_version"`
	Name          string            `json:"name"`
	Version       string            `json:"version"`
	Digest        string            `json:"digest"`   // lowercase hex SHA-256
	BuiltAt       string            `json:"built_at"` // RFC 3339, set at first publication
	Definition    workflowfile.File `json:"definition"`
	Files         []FileEntry       `json:"files"`
}

// computeDigest hashes one line per file, sorted by path:
// "<path>\x00<executable 0|1>\x00<sha256 of content>\n". Workflowfile.yaml is
// one of the files, so the definition is covered through its raw bytes and
// the digest does not depend on Go types. Timestamps, ownership, and other
// permission bits do not take part.
func computeDigest(files []FileEntry) string {
	h := sha256.New()
	sorted := append([]FileEntry(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	for _, f := range sorted {
		exec := "0"
		if f.Executable {
			exec = "1"
		}
		fmt.Fprintf(h, "%s\x00%s\x00%s\n", f.Path, exec, f.SHA256)
	}
	return hex.EncodeToString(h.Sum(nil))
}
