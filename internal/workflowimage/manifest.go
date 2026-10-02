// Package workflowimage stores built workflow images: the Workflowfile.yaml
// source of a workflow plus its scripts and instructions, addressed by the
// SHA-256 digest of their content.
package workflowimage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// computeDigest hashes the normalized definition and then one line per file,
// sorted by path: path, executable flag, and content hash. Timestamps,
// ownership, and other permission bits do not take part. files must be sorted.
func computeDigest(def *workflowfile.File, files []FileEntry) (string, error) {
	definition, err := json.Marshal(def)
	if err != nil {
		return "", fmt.Errorf("encode definition: %w", err)
	}
	h := sha256.New()
	h.Write(definition)
	sorted := append([]FileEntry(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	for _, f := range sorted {
		exec := "0"
		if f.Executable {
			exec = "1"
		}
		fmt.Fprintf(h, "%s\x00%s\x00%s\n", f.Path, exec, f.SHA256)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
