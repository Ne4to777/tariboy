package image

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Store is an on-disk image store rooted at Dir (paths.ImagesDir()).
//
// Layout:
//
//	<dir>/<name>/refs/<id>.tar.gz   the image content, addressed by ref id
//	<dir>/<name>/tags/<tag>         one line naming the id that tag points at
//
// A tag is only a pointer. Building writes the content of its ref id and moves
// the requested tags onto it, so every ref can be rebuilt at any time and by
// any caller. Agents and the operator CLI share this one mechanism.
type Store struct{ Dir string }

var refIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ponytail: global lock, use per-ref locks if image publication becomes a bottleneck.
var publicationGate sync.Mutex

// WithPublicationGate serializes publication against concurrent readers that
// resolve a tag and then open its content.
func WithPublicationGate(fn func() error) error {
	publicationGate.Lock()
	defer publicationGate.Unlock()
	return fn()
}

// RefID derives a ref's identity from its image name and the image_version
// declared in Tariboyfile.yaml. Rebuilding the same version rewrites the same
// ref; bumping the version creates another one, so a pinned agent keeps the
// generation it was assigned. An image that declares no version has no derived
// id and stays content-addressed by its archive bytes.
func RefID(name, imageVersion string) string {
	if imageVersion == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(name + "\n" + imageVersion))
	return hex.EncodeToString(sum[:])
}

func (s *Store) nameDir(name string) string { return filepath.Join(s.Dir, name) }
func (s *Store) refsDir(name string) string { return filepath.Join(s.Dir, name, "refs") }
func (s *Store) tagsDir(name string) string { return filepath.Join(s.Dir, name, "tags") }
func (s *Store) tagPath(ref Ref) string     { return filepath.Join(s.tagsDir(ref.Name), ref.Tag) }
func (s *Store) contentPath(name, id string) string {
	return filepath.Join(s.refsDir(name), id+".tar.gz")
}

// Resolve reports the ref id a tag currently points at.
func (s *Store) Resolve(ref Ref) (string, error) {
	data, err := os.ReadFile(s.tagPath(ref))
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("image %s not found", ref.String())
		}
		return "", err
	}
	id := strings.TrimSpace(string(data))
	if !refIDPattern.MatchString(id) {
		return "", fmt.Errorf("image %s has an invalid tag pointer", ref.String())
	}
	return id, nil
}

// archiveFor resolves the tag and returns the path of its content.
func (s *Store) archiveFor(ref Ref) (string, error) {
	id, err := s.Resolve(ref)
	if err != nil {
		return "", err
	}
	path := s.contentPath(ref.Name, id)
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("image %s content %s is unavailable", ref.String(), id)
	}
	return path, nil
}

// TagExists reports whether the tag pointer exists, which distinguishes a ref
// that was never built here from one whose content cannot be read. A stat
// failure other than "not exist" is returned rather than reported as absent.
func (s *Store) TagExists(ref Ref) (bool, error) {
	if _, err := os.Stat(s.tagPath(ref)); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *Store) Exists(ref Ref) bool {
	_, err := s.archiveFor(ref)
	return err == nil
}

// ArchivePath returns the on-disk file a ref currently points at.
func (s *Store) ArchivePath(ref Ref) (string, error) { return s.archiveFor(ref) }

func (s *Store) ArchiveBytes(ref Ref) ([]byte, error) {
	path, err := s.archiveFor(ref)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// SetTag points one tag at content that is already stored. This is how a build
// publishes several tags — a versioned tag and latest — without building twice.
func (s *Store) SetTag(ref Ref, id string) error {
	if !refIDPattern.MatchString(id) {
		return fmt.Errorf("invalid image ref id %q", id)
	}
	if _, err := os.Stat(s.contentPath(ref.Name, id)); err != nil {
		return fmt.Errorf("image %s content %s is unavailable", ref.String(), id)
	}
	if err := os.MkdirAll(s.tagsDir(ref.Name), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.tagsDir(ref.Name), "."+ref.Tag+"-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := io.WriteString(tmp, id+"\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.tagPath(ref)); err != nil {
		return err
	}
	return syncDirectory(s.tagsDir(ref.Name))
}

// InstallArchive publishes validated archive bytes as the content of their ref
// and moves ref onto it. Existing content of the same id is replaced.
func (s *Store) InstallArchive(ref Ref, archive []byte) (Manifest, error) {
	manifest, err := validatePortableArchive(archive, ref)
	if err != nil {
		return Manifest{}, fmt.Errorf("validate image %s: %w", ref.String(), err)
	}
	tmpName, err := s.stageBytes(ref, "install", archive)
	if err != nil {
		return Manifest{}, err
	}
	defer os.Remove(tmpName)
	if err := s.install(ref, manifest.Digest, tmpName); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// PublishArchiveFile publishes an already staged archive file, checking only
// its manifest identity, and consumes the file. Callers holding untrusted
// bytes use InstallArchive, which applies the full portable contract.
func (s *Store) PublishArchiveFile(ref Ref, path string) (string, error) {
	return s.publishArchive(ref, path, nil)
}

// RetagArchive publishes a runnable archive under another image name by
// rewriting only its manifest identity. Payload bytes and their declared order
// are preserved.
func (s *Store) RetagArchive(source, target Ref, archive []byte) (Manifest, error) {
	retagged, _, err := RetagArchiveBytes(source, target, archive)
	if err != nil {
		return Manifest{}, err
	}
	return s.InstallArchive(target, retagged)
}

func (s *Store) stageBytes(ref Ref, kind string, archive []byte) (string, error) {
	if err := os.MkdirAll(s.nameDir(ref.Name), 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(s.nameDir(ref.Name), "."+kind+"-*.tmp")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", err
	}
	if _, err := tmp.Write(archive); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	return tmpName, nil
}

// install moves staged content into place and advances ref onto it.
func (s *Store) install(ref Ref, id, tmpName string) error {
	if !refIDPattern.MatchString(id) {
		return fmt.Errorf("invalid image ref id for %s", ref.String())
	}
	if err := os.MkdirAll(s.refsDir(ref.Name), 0o700); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.contentPath(ref.Name, id)); err != nil {
		return fmt.Errorf("publish image %s: %w", ref.String(), err)
	}
	if err := syncDirectory(s.refsDir(ref.Name)); err != nil {
		return err
	}
	return s.SetTag(ref, id)
}

// RetagArchiveBytes rewrites manifest identity without touching a store.
func RetagArchiveBytes(source, target Ref, archive []byte) ([]byte, Manifest, error) {
	if _, err := validatePortableArchive(archive, source); err != nil {
		return nil, Manifest{}, fmt.Errorf("validate imported image: %w", err)
	}
	in, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, Manifest{}, err
	}
	defer in.Close()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	tr := tar.NewReader(in)
	seenManifest := false
	for {
		header, nextErr := tr.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return nil, Manifest{}, nextErr
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return nil, Manifest{}, fmt.Errorf("invalid image archive member %q", header.Name)
		}
		body, readErr := io.ReadAll(tr)
		if readErr != nil {
			return nil, Manifest{}, readErr
		}
		if header.Name == "manifest.json" {
			if seenManifest {
				return nil, Manifest{}, errors.New("duplicate image manifest")
			}
			seenManifest = true
			var fields map[string]any
			if err := json.Unmarshal(body, &fields); err != nil {
				return nil, Manifest{}, err
			}
			fields["name"] = target.Name
			delete(fields, "tag")
			delete(fields, "digest")
			imageVersion, _ := fields["image_version"].(string)
			if id := RefID(target.Name, imageVersion); id != "" {
				fields["id"] = id
			} else {
				delete(fields, "id")
			}
			body, err = json.MarshalIndent(fields, "", "  ")
			if err != nil {
				return nil, Manifest{}, err
			}
		}
		if err := writeTarFileMode(tw, header.Name, body, header.Mode&0o7777); err != nil {
			return nil, Manifest{}, err
		}
	}
	if !seenManifest {
		return nil, Manifest{}, errors.New("image manifest is missing")
	}
	if err := tw.Close(); err != nil {
		return nil, Manifest{}, err
	}
	if err := gz.Close(); err != nil {
		return nil, Manifest{}, err
	}
	retagged := out.Bytes()
	manifest, err := validatePortableArchive(retagged, target)
	if err != nil {
		return nil, Manifest{}, fmt.Errorf("validate retagged image: %w", err)
	}
	return retagged, manifest, nil
}

// Inspect reads manifest.json from the content the tag points at.
func (s *Store) Inspect(ref Ref) (Manifest, error) {
	path, err := s.archiveFor(ref)
	if err != nil {
		return Manifest{}, err
	}
	return inspectArchive(path, ref)
}

// InspectPinned resolves the exact generation assigned to an agent. Content is
// kept by id, so a moved tag never invalidates a pinned assignment.
func (s *Store) InspectPinned(ref Ref, id string) (Manifest, error) {
	manifest, _, err := s.inspectPinnedArchive(ref, id)
	return manifest, err
}

func (s *Store) inspectPinnedArchive(ref Ref, id string) (Manifest, string, error) {
	if !refIDPattern.MatchString(id) {
		return Manifest{}, "", errors.New("invalid pinned image id")
	}
	path := s.contentPath(ref.Name, id)
	manifest, err := inspectArchive(path, ref)
	if err != nil {
		return Manifest{}, "", fmt.Errorf("pinned image %s@%s is unavailable: %w", ref.String(), id, err)
	}
	if manifest.Digest != id {
		return Manifest{}, "", fmt.Errorf("image %s id mismatch for %s", ref.String(), id)
	}
	return manifest, path, nil
}

func inspectArchive(archivePath string, ref Ref) (Manifest, error) {
	data, err := readFileFromTar(archivePath, "manifest.json")
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("parse manifest of %s: %w", ref.String(), err)
	}
	if m.SchemaVersion > ManifestSchemaVersion {
		return Manifest{}, fmt.Errorf("image %s manifest schema_version %d is newer than supported %d", ref.String(), m.SchemaVersion, ManifestSchemaVersion)
	}
	if m.Name != ref.Name {
		return Manifest{}, fmt.Errorf("archive image %s does not match %s", m.Name, ref.String())
	}
	m.Tag = ref.Tag
	if m.ID != "" {
		m.Digest = m.ID
		return m, nil
	}
	digest, err := fileDigest(archivePath)
	if err != nil {
		return Manifest{}, fmt.Errorf("digest image %s: %w", ref.String(), err)
	}
	m.Digest = digest
	return m, nil
}

func fileDigest(path string) (string, error) {
	archive, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer archive.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, archive); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// ValidateArchive verifies a staged archive before callers publish it at ref.
func ValidateArchive(archivePath string, ref Ref) (Manifest, error) {
	return inspectArchive(archivePath, ref)
}

func readFileFromTar(archive, want string) ([]byte, error) {
	f, err := os.Open(archive)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if h.Name == want {
			return io.ReadAll(tr)
		}
	}
	return nil, fmt.Errorf("%s not found in %s", want, archive)
}

// publishArchive validates staged content, stores it under its ref id and
// moves ref onto it. There is no no-clobber mode: rebuilding is always allowed.
func (s *Store) publishArchive(ref Ref, tmpName string, archiveOut *[]byte) (string, error) {
	manifest, err := ValidateArchive(tmpName, ref)
	if err != nil {
		return "", fmt.Errorf("validate image %s: %w", ref.String(), err)
	}
	var archive []byte
	if archiveOut != nil {
		archive, err = os.ReadFile(tmpName)
		if err != nil {
			return "", err
		}
	}
	if err := s.install(ref, manifest.Digest, tmpName); err != nil {
		return "", err
	}
	if archiveOut != nil {
		*archiveOut = archive
	}
	return manifest.Digest, nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func writeTarFile(tw *tar.Writer, name string, data []byte) error {
	return writeTarFileMode(tw, name, data, 0o600)
}

func writeTarFileMode(tw *tar.Writer, name string, data []byte, mode int64) error {
	if err := tw.WriteHeader(&tar.Header{
		Name: name, Mode: mode, Size: int64(len(data)), Typeflag: tar.TypeReg,
	}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}
