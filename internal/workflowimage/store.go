package workflowimage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alekzonder/tariboy/internal/workflowfile"
)

const (
	maxFiles     = 256
	maxFileSize  = 4 << 20
	maxTotalSize = 32 << 20

	latestTag    = "latest"
	manifestName = "manifest.json"
	tmpPrefix    = ".tmp-"
)

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// mu serializes publication and removal across every Store in the process.
var mu sync.Mutex

// beforeTagWrite runs before moveTags writes each tag. Tests replace it to
// inject failures between tag writes.
var beforeTagWrite = func(tag string) {}

// Store is the on-disk store of workflow images. Dir is <base-dir>/workflows.
//
//	<Dir>/<name>/refs/<digest>/  copied source tree plus manifest.json
//	<Dir>/<name>/tags/<tag>      file holding the digest
type Store struct{ Dir string }

// Listing is a manifest together with the tag that names it.
type Listing struct {
	Manifest
	Tag string
}

// sourceFile is a file read from the source, held in memory until it is
// copied so the digest and the stored bytes cannot differ.
type sourceFile struct {
	FileEntry
	content []byte
}

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// checkName rejects values that could escape the store when joined into a
// path.
func checkName(kind, v string) error {
	if v == "" || v == "." || v == ".." || strings.ContainsAny(v, "/\\\x00") {
		return invalidf("%s %q is not a valid name", kind, v)
	}
	return nil
}

func (s *Store) nameDir(name string) string    { return filepath.Join(s.Dir, name) }
func (s *Store) refsDir(name string) string    { return filepath.Join(s.Dir, name, "refs") }
func (s *Store) tagsDir(name string) string    { return filepath.Join(s.Dir, name, "tags") }
func (s *Store) tagPath(name, t string) string { return filepath.Join(s.tagsDir(name), t) }

// ContentDir returns the absolute path of the unpacked tree, or "" when name
// or digest is not a valid path component.
func (s *Store) ContentDir(name, digest string) string {
	if checkName("name", name) != nil || !digestPattern.MatchString(digest) {
		return ""
	}
	p := filepath.Join(s.refsDir(name), digest)
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// FilePath returns the absolute path of a file inside the unpacked tree. It
// refuses a path that is absolute or leaves the tree, and a file that is not
// there.
func (s *Store) FilePath(name, digest, rel string) (string, error) {
	root := s.ContentDir(name, digest)
	if root == "" {
		return "", invalidf("invalid image reference %q %q", name, digest)
	}
	if rel == "" || !filepath.IsLocal(rel) || strings.Contains(rel, "\\") {
		return "", invalidf("path %q is outside the image", rel)
	}
	p := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Lstat(p)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: file %q", ErrNotFound, rel)
	}
	return p, nil
}

// readRegular reads a regular file without following a symlink at path, so a
// file swapped after the directory walk cannot pull in outside bytes. The
// type, size, and mode come from the opened descriptor, and the read stops
// one byte past limit.
func readRegular(path string, limit int64) ([]byte, fs.FileMode, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("cannot be opened as a regular file: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !fi.Mode().IsRegular() {
		return nil, 0, errors.New("is not a regular file")
	}
	if fi.Size() > limit {
		return nil, 0, fmt.Errorf("is larger than %d bytes", limit)
	}
	content, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, 0, err
	}
	if int64(len(content)) > limit {
		return nil, 0, fmt.Errorf("is larger than %d bytes", limit)
	}
	return content, fi.Mode(), nil
}

// scan reads the whole source directory. It rejects anything but regular
// files and directories, and enforces the source limits.
func scan(root string) ([]sourceFile, error) {
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, invalidf("source directory: %v", err)
	}
	var files []sourceFile
	var total int64
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return invalidf("%q is not a regular file (symlinks and special files are not allowed)", rel)
		}
		if rel == manifestName {
			return invalidf("%q is reserved for the stored manifest", rel)
		}
		if len(files) >= maxFiles {
			return invalidf("source has more than %d files", maxFiles)
		}
		if info.Size() > maxFileSize {
			return invalidf("file %q is larger than %d bytes", rel, maxFileSize)
		}
		content, mode, err := readRegular(p, maxFileSize)
		if err != nil {
			return invalidf("%q: %v", rel, err)
		}
		sum := sha256.Sum256(content)
		total += int64(len(content))
		if total > maxTotalSize {
			return invalidf("source is larger than %d bytes", maxTotalSize)
		}
		files = append(files, sourceFile{
			FileEntry: FileEntry{
				Path:       rel,
				SHA256:     hex.EncodeToString(sum[:]),
				Executable: mode.Perm()&0o111 != 0,
				Size:       int64(len(content)),
			},
			content: content,
		})
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrInvalid) {
			return nil, err
		}
		return nil, fmt.Errorf("scan source: %w", err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// checkScanned verifies against the scanned entries, not the file system,
// that the manifest file and every file the manifest names were captured, and
// that every script is executable. Validate looked at the file system earlier;
// this closes the gap between the two looks.
func checkScanned(src *workflowfile.File, entries []FileEntry) error {
	byPath := make(map[string]FileEntry, len(entries))
	for _, e := range entries {
		byPath[e.Path] = e
	}
	if _, ok := byPath[workflowfile.DefaultFilename]; !ok {
		return invalidf("%q was not captured from the source", workflowfile.DefaultFilename)
	}
	for _, p := range src.Files() {
		if _, ok := byPath[p]; !ok {
			return invalidf("file %q named by the manifest was not captured from the source", "./"+p)
		}
	}
	var scripts []string
	for _, s := range src.Statuses {
		if s.Watch != nil {
			scripts = append(scripts, s.Watch.Script)
		}
		for _, t := range s.Transitions {
			for _, c := range t.Checks {
				scripts = append(scripts, c.Script)
			}
		}
	}
	for _, script := range scripts {
		p := path.Clean(script)
		if e, ok := byPath[p]; !ok || !e.Executable {
			return invalidf("script %q is not an executable file in the captured source", "./"+p)
		}
	}
	return nil
}

// Publish validates src, copies the whole source directory, and points the
// version tag and "latest" at it. The digest covers every file's path,
// executable bit, and content, Workflowfile.yaml included, and nothing else.
// A version whose stored content differs is refused, whether or not its tag
// still exists. The bool reports whether the content was new.
func (s *Store) Publish(src *workflowfile.File, now time.Time) (Manifest, bool, error) {
	mu.Lock()
	defer mu.Unlock()
	return s.publishLocked(src, "", now)
}

// Validate runs every check Publish makes before it writes anything: the
// manifest validation, then the source scan. A source the scan refuses is
// reported as one source_invalid error. It reads src.Dir and writes nothing.
func Validate(src *workflowfile.File) []workflowfile.ValidationError {
	_, _, err := prepareSource(src)
	var invalid *InvalidError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &invalid):
		return invalid.Errors
	default:
		return []workflowfile.ValidationError{{Code: "source_invalid", Path: "source", Message: err.Error()}}
	}
}

// storeNameErrors reports a name or version that could escape the store when
// joined into a path, under the manifest field that holds it. The manifest
// rules already refuse such values; this is the store's own guard.
func storeNameErrors(src *workflowfile.File) []workflowfile.ValidationError {
	var errs []workflowfile.ValidationError
	if err := checkName("workflow name", src.Name); err != nil {
		errs = append(errs, workflowfile.ValidationError{Code: "name_invalid", Path: "name", Message: err.Error()})
	}
	if err := checkName("workflow version", src.WorkflowVersion); err != nil {
		errs = append(errs, workflowfile.ValidationError{Code: "version_invalid", Path: "workflow_version", Message: err.Error()})
	}
	return errs
}

// prepareSource validates src and reads its whole directory.
func prepareSource(src *workflowfile.File) ([]sourceFile, []FileEntry, error) {
	if errs := workflowfile.Validate(src); len(errs) > 0 {
		return nil, nil, &InvalidError{Errors: errs}
	}
	if errs := storeNameErrors(src); len(errs) > 0 {
		return nil, nil, &InvalidError{Errors: errs}
	}
	files, err := scan(src.Dir)
	if err != nil {
		return nil, nil, err
	}
	entries := make([]FileEntry, len(files))
	for i, f := range files {
		entries[i] = f.FileEntry
	}
	if err := checkScanned(src, entries); err != nil {
		return nil, nil, err
	}
	return files, entries, nil
}

// publishLocked is Publish for a caller that already holds mu. A non-empty
// want refuses a source whose digest differs, before anything is written.
func (s *Store) publishLocked(src *workflowfile.File, want string, now time.Time) (Manifest, bool, error) {
	files, entries, err := prepareSource(src)
	if err != nil {
		return Manifest{}, false, err
	}
	digest := computeDigest(entries)
	if want != "" && digest != want {
		return Manifest{}, false, fmt.Errorf("%w: archive names %s, content is %s", ErrDigestMismatch, want, digest)
	}

	name, version := src.Name, src.WorkflowVersion
	prev, err := s.readTag(name, version)
	switch {
	case err == nil && prev == digest:
		m, err := s.readManifest(name, digest)
		return m, false, err
	case err == nil:
		return Manifest{}, false, fmt.Errorf("%w: %s %s is %s", ErrVersionPublished, name, version, prev)
	case !errors.Is(err, ErrNotFound):
		return Manifest{}, false, err
	}
	// The version tag can be gone while another tag still holds content of
	// that version; the version stays taken until that content is deleted.
	if other, err := s.storedVersionDigest(name, version, digest); err != nil {
		return Manifest{}, false, err
	} else if other != "" {
		return Manifest{}, false, fmt.Errorf("%w: %s %s is %s", ErrVersionPublished, name, version, other)
	}

	var m Manifest
	created := false
	if _, statErr := os.Stat(s.ContentDir(name, digest)); statErr == nil {
		if m, err = s.readManifest(name, digest); err != nil {
			return Manifest{}, false, err
		}
	} else if errors.Is(statErr, fs.ErrNotExist) {
		def := *src
		def.Dir = "" // the stored definition does not carry the source path
		m = Manifest{
			SchemaVersion: src.SchemaVersion,
			Name:          name,
			Version:       version,
			Digest:        digest,
			BuiltAt:       now.UTC().Format(time.RFC3339),
			Definition:    def,
			Files:         entries,
		}
		if err := s.install(m, files); err != nil {
			return Manifest{}, false, err
		}
		created = true
	} else {
		return Manifest{}, false, statErr
	}

	if err := s.moveTags(name, digest, version, latestTag); err != nil {
		// Delete new content only when the tags are back as they were; a tag
		// that could not be restored still names it.
		if created && !errors.Is(err, errRestoreTags) {
			if rmErr := removeAll(s.ContentDir(name, digest)); rmErr != nil {
				err = errors.Join(err, fmt.Errorf("remove new content: %w", rmErr))
			}
		}
		return Manifest{}, false, err
	}
	return m, created, nil
}

// storedVersionDigest returns the digest of stored content of name that has
// version but is not digest, or "" when there is none. It reads every
// refs/<digest>/manifest.json, whether or not a tag names it.
func (s *Store) storedVersionDigest(name, version, digest string) (string, error) {
	refs, err := os.ReadDir(s.refsDir(name))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	for _, ref := range refs {
		if !ref.IsDir() || !digestPattern.MatchString(ref.Name()) || ref.Name() == digest {
			continue
		}
		m, err := s.readManifest(name, ref.Name())
		if err != nil {
			return "", err
		}
		if m.Version == version {
			return ref.Name(), nil
		}
	}
	return "", nil
}

// install writes the tree into a temporary sibling directory, makes it
// read-only, and renames it into place.
func (s *Store) install(m Manifest, files []sourceFile) (err error) {
	refs := s.refsDir(m.Name)
	if err := os.MkdirAll(refs, 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(refs, tmpPrefix)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			removeAll(tmp)
		}
	}()
	dirs := map[string]bool{tmp: true}
	modes := map[string]fs.FileMode{}
	write := func(rel string, content []byte, mode fs.FileMode) error {
		p := filepath.Join(tmp, filepath.FromSlash(rel))
		for d := filepath.Dir(p); !dirs[d]; d = filepath.Dir(d) {
			dirs[d] = true
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(p, content, 0o600); err != nil {
			return err
		}
		modes[p] = mode
		return nil
	}
	for _, f := range files {
		mode := fs.FileMode(0o400)
		if f.Executable {
			mode = 0o500
		}
		if err := write(f.Path, f.content, mode); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := write(manifestName, append(data, '\n'), 0o400); err != nil {
		return err
	}
	for p, mode := range modes {
		if err := os.Chmod(p, mode); err != nil {
			return err
		}
	}
	// Deepest directories first, so a parent is still writable while its
	// children change.
	list := make([]string, 0, len(dirs))
	for d := range dirs {
		list = append(list, d)
	}
	sort.Slice(list, func(i, j int) bool { return len(list[i]) > len(list[j]) })
	for _, d := range list {
		if err := os.Chmod(d, 0o500); err != nil {
			return err
		}
	}
	return os.Rename(tmp, s.ContentDir(m.Name, m.Digest))
}

// errRestoreTags marks a moveTags failure whose rollback also failed, so some
// tags may still name the new digest.
var errRestoreTags = errors.New("restore tags")

// moveTags points every tag at digest. If one fails it puts the earlier tags
// back as they were; when that also fails the error matches errRestoreTags.
func (s *Store) moveTags(name, digest string, tags ...string) error {
	if err := os.MkdirAll(s.tagsDir(name), 0o700); err != nil {
		return err
	}
	var done []tagState
	for _, tag := range tags {
		beforeTagWrite(tag)
		old, err := s.readTag(name, tag)
		had := err == nil
		if err == nil || errors.Is(err, ErrNotFound) {
			err = s.writeTag(name, tag, digest)
		}
		if err != nil {
			if rerr := s.restoreTags(name, done); rerr != nil {
				err = errors.Join(err, fmt.Errorf("%w: %w", errRestoreTags, rerr))
			}
			return err
		}
		done = append(done, tagState{tag, old, had})
	}
	return nil
}

type tagState struct {
	tag    string
	digest string
	had    bool
}

func (s *Store) restoreTags(name string, states []tagState) error {
	var errs []error
	for _, st := range states {
		if st.had {
			errs = append(errs, s.writeTag(name, st.tag, st.digest))
		} else if err := os.Remove(s.tagPath(name, st.tag)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Store) writeTag(name, tag, digest string) error {
	f, err := os.CreateTemp(s.tagsDir(name), tmpPrefix)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(digest + "\n")
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(f.Name())
		return err
	}
	if err := os.Rename(f.Name(), s.tagPath(name, tag)); err != nil {
		_ = os.Remove(f.Name())
		return err
	}
	return nil
}

// readTag returns the digest a tag names, or ErrNotFound.
func (s *Store) readTag(name, tag string) (string, error) {
	data, err := os.ReadFile(s.tagPath(name, tag))
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%w: %s %s", ErrNotFound, name, tag)
	}
	if err != nil {
		return "", err
	}
	digest := strings.TrimSpace(string(data))
	if !digestPattern.MatchString(digest) {
		return "", fmt.Errorf("tag %s of %s holds %q, not a digest", tag, name, digest)
	}
	return digest, nil
}

func (s *Store) readManifest(name, digest string) (Manifest, error) {
	data, err := os.ReadFile(filepath.Join(s.ContentDir(name, digest), manifestName))
	if errors.Is(err, fs.ErrNotExist) {
		return Manifest{}, fmt.Errorf("%w: %s %s", ErrNotFound, name, digest)
	}
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("read manifest of %s %s: %w", name, digest, err)
	}
	return m, nil
}

// Resolve maps a tag, or a full digest that exists for name, to a digest. An
// empty tag means "latest".
func (s *Store) Resolve(name, tag string) (string, error) {
	if err := checkName("name", name); err != nil {
		return "", err
	}
	if tag == "" {
		tag = latestTag
	}
	if err := checkName("tag", tag); err != nil {
		return "", err
	}
	digest, err := s.readTag(name, tag)
	if err == nil {
		return digest, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return "", err
	}
	if digestPattern.MatchString(tag) {
		if info, statErr := os.Stat(s.ContentDir(name, tag)); statErr == nil && info.IsDir() {
			return tag, nil
		}
	}
	return "", err
}

// Inspect returns the manifest a tag or digest names.
func (s *Store) Inspect(name, tagOrDigest string) (Manifest, error) {
	digest, err := s.Resolve(name, tagOrDigest)
	if err != nil {
		return Manifest{}, err
	}
	return s.readManifest(name, digest)
}

// List returns one entry per name and tag, sorted by name and then tag.
func (s *Store) List() ([]Listing, error) {
	names, err := os.ReadDir(s.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Listing
	for _, n := range names {
		if !n.IsDir() || checkName("name", n.Name()) != nil {
			continue
		}
		tags, err := s.tagNames(n.Name())
		if err != nil {
			return nil, err
		}
		for _, tag := range tags {
			digest, err := s.readTag(n.Name(), tag)
			if err != nil {
				return nil, err
			}
			m, err := s.readManifest(n.Name(), digest)
			if err != nil {
				return nil, err
			}
			out = append(out, Listing{Manifest: m, Tag: tag})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Tag < out[j].Tag
	})
	return out, nil
}

// tagNames returns the tag file names of name, sorted.
func (s *Store) tagNames(name string) ([]string, error) {
	entries, err := os.ReadDir(s.tagsDir(name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.Type().IsRegular() && !strings.HasPrefix(e.Name(), tmpPrefix) {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// Tags returns the tags of name that point at digest, sorted.
func (s *Store) Tags(name, digest string) ([]string, error) {
	if err := checkName("name", name); err != nil {
		return nil, err
	}
	names, err := s.tagNames(name)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, tag := range names {
		d, err := s.readTag(name, tag)
		if err != nil {
			return nil, err
		}
		if d == digest {
			out = append(out, tag)
		}
	}
	return out, nil
}

// RemoveTag deletes a tag. When no other tag names the same digest, the
// content is deleted too.
func (s *Store) RemoveTag(name, tag string) (string, bool, error) {
	if err := checkName("name", name); err != nil {
		return "", false, err
	}
	if err := checkName("tag", tag); err != nil {
		return "", false, err
	}
	mu.Lock()
	defer mu.Unlock()
	return s.removeTagLocked(name, tag)
}

// removeTagLocked is RemoveTag for a caller that already holds mu and has
// checked the names.
func (s *Store) removeTagLocked(name, tag string) (string, bool, error) {
	digest, err := s.readTag(name, tag)
	if err != nil {
		return "", false, err
	}
	if err := os.Remove(s.tagPath(name, tag)); err != nil {
		return "", false, err
	}
	rest, err := s.Tags(name, digest)
	if err != nil {
		return digest, false, err
	}
	if len(rest) > 0 {
		return digest, false, nil
	}
	if err := removeAll(s.ContentDir(name, digest)); err != nil {
		return digest, false, err
	}
	return digest, true, nil
}

// makeWritable restores owner write permission on a stored tree so it can be
// deleted.
func makeWritable(root string) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if d != nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
}

// removeAll deletes a stored tree, restoring write permission first.
func removeAll(path string) error {
	makeWritable(path)
	return os.RemoveAll(path)
}
