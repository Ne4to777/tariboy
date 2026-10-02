# Workflow Image Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build, store, and inspect workflow images: a versioned, immutable artifact made from a Store source directory that holds a `Workflowfile.yaml`, status instructions, and scripts.

**Architecture:** `internal/workflowfile` parses and validates the manifest and knows nothing about storage. `internal/workflowimage` publishes a validated source as a content-addressed, read-only tree under `<base-dir>/workflows/` with tag pointer files, and records each published manifest in SQLite. `internal/stores` discovers `workflows/<name>/` sources. `internal/commands` exposes validate, build, list, inspect, and remove. Nothing in this phase executes a workflow or touches the existing task workflow engine.

**Tech Stack:** Go 1.26, `gopkg.in/yaml.v3`, SQLite migrations, the command registry in `internal/registry`, MDX documentation.

**Spec:** `docs/superpowers/specs/2026-10-02-workflow-images-design.md`, sections "Workflow image source", "Build, storage, and versions", and "Phases", item 2.

**How to read this plan:** each task gives the files, the exact exported interface, the behaviors to pin with tests, and the constraints. The implementer writes the test code first, watches it fail, then writes the implementation. Names and signatures in "Produces" are binding; internal helpers are the implementer's choice.

## Global Constraints

- The manifest file name is `Workflowfile.yaml`. The loader is strict: an unknown field is an error, and a second YAML document is an error.
- `schema_version` must be `1`.
- `workflow_version` is SemVer 2.0.0 without a `v` prefix; reuse `imagefile.ValidateImageVersion`.
- Identifier rule for status IDs, outcome names, artifact names, and pool names: `^[a-z][a-z0-9_-]{0,63}$`.
- Workflow name rule: `^[a-z0-9][a-z0-9._-]{0,63}$`.
- Secret and environment names: `^[A-Za-z_][A-Za-z0-9_]*$`; an `env` name must not start with `TARIBOY_`.
- Owner forms in YAML: the scalar `customer`, the scalar `script`, or the mapping `{pool: NAME}`.
- Limit defaults: `idle_iterations: 3`, `rejected_requests: 5`, `script_failures: 3`, `unavailable_grace: 5m`. A check `timeout` defaults to `60s` and may not exceed `30m`. A watch `timeout` defaults to `60s`; `every` is required and positive.
- `run_as` is `queue` (default) or `agent`.
- Paths in the manifest start with `./`, stay inside the source directory, and name regular files. Symlinks anywhere in the source are rejected. Scripts must have an executable bit.
- A published `workflow_version` is immutable: identical content is a no-op, different content fails with `workflow_version_published`.
- Stored trees are owner-only and read-only: directories `0500`, regular files `0400`, executable files `0500`.
- Source limits: at most 256 files, at most 4 MiB per file, at most 32 MiB in total.
- Do not modify `internal/tasks/workflow_*.go`, the `task_workflow_versions` table, or the `ttasks workflows` commands. Phase 3 replaces them.
- Run commands directly from the worktree root, without `bash -lc`. Run each `git` command as its own plain command.
- Never run tests against the live `~/.tariboy`, `~/.tariboyd`, or `127.0.0.1:9990`.
- Do not bump the version and do not edit `CHANGELOG.md`. Commit messages carry no attribution trailer.

## Review Focus

- A source path such as `./scripts/../../outside.sh` or an absolute path must be rejected before any file is read.
- A symlink inside the source, including one that points inside the source, must fail the build; a stored tree must never contain one.
- Rebuilding an unchanged source must be a no-op that reports the same digest, even when file modification times differ.
- Two builds of the same version with one changed script byte must fail and leave the first build's content and tags untouched.
- A build that fails after writing content must not leave a tag pointing at missing content, and must not leave a SQLite row without content.

## File Structure

| File | Responsibility |
| --- | --- |
| `internal/workflowfile/workflowfile.go` | Manifest types, strict parsing, owner decoding. |
| `internal/workflowfile/validate.go` | Validation rules and `ValidationError`. |
| `internal/workflowfile/limits.go` | Limit defaults and per-status resolution. |
| `internal/workflowfile/version.go` | `workflow_version` read and bump. |
| `internal/workflowimage/manifest.go` | Stored manifest type and digest computation. |
| `internal/workflowimage/store.go` | On-disk content, tags, publish, resolve, list, remove. |
| `internal/workflowimage/registry.go` | SQLite record of published manifests. |
| `internal/store/migrations/0053_task_workflow_images.sql` | The `task_workflow_images` table. |
| `internal/stores/workflows.go` | Store discovery of `workflows/` sources. |
| `internal/commands/workflow_image.go` | Operator commands and routes. |
| `docs/docs/workflow-images.mdx` | Product documentation. |

---

### Task 1: Manifest types and strict parsing

**Files:**
- Create: `internal/workflowfile/workflowfile.go`
- Test: `internal/workflowfile/workflowfile_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:

```go
package workflowfile

const DefaultFilename = "Workflowfile.yaml"

const (
	OwnerPool     = "pool"
	OwnerCustomer = "customer"
	OwnerScript   = "script"

	RunAsQueue = "queue"
	RunAsAgent = "agent"
)

type File struct {
	SchemaVersion   int               `yaml:"schema_version" json:"schema_version"`
	Name            string            `yaml:"name" json:"name"`
	WorkflowVersion string            `yaml:"workflow_version" json:"workflow_version"`
	InitialStatus   string            `yaml:"initial_status" json:"initial_status"`
	RequiresSecrets []string          `yaml:"requires_secrets,omitempty" json:"requires_secrets,omitempty"`
	Env             map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	Limits          *Limits           `yaml:"limits,omitempty" json:"limits,omitempty"`
	Artifacts       []Artifact        `yaml:"artifacts,omitempty" json:"artifacts,omitempty"`
	Statuses        []Status          `yaml:"statuses" json:"statuses"`
	Dir             string            `yaml:"-" json:"-"` // directory holding the manifest
}

type Limits struct {
	IdleIterations   *int   `yaml:"idle_iterations,omitempty" json:"idle_iterations,omitempty"`
	RejectedRequests *int   `yaml:"rejected_requests,omitempty" json:"rejected_requests,omitempty"`
	ScriptFailures   *int   `yaml:"script_failures,omitempty" json:"script_failures,omitempty"`
	UnavailableGrace string `yaml:"unavailable_grace,omitempty" json:"unavailable_grace,omitempty"`
}

type Artifact struct {
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
}

type Owner struct {
	Kind string `json:"kind"`           // OwnerPool, OwnerCustomer, OwnerScript, or "" when absent
	Pool string `json:"pool,omitempty"` // set only for OwnerPool
}

type Status struct {
	ID           string       `yaml:"id" json:"id"`
	Owner        Owner        `yaml:"owner,omitempty" json:"owner"`
	Instructions string       `yaml:"instructions,omitempty" json:"instructions,omitempty"`
	Watch        *Watch       `yaml:"watch,omitempty" json:"watch,omitempty"`
	Transitions  []Transition `yaml:"transitions,omitempty" json:"transitions,omitempty"`
	Limits       *Limits      `yaml:"limits,omitempty" json:"limits,omitempty"`
	Terminal     bool         `yaml:"terminal,omitempty" json:"terminal,omitempty"`
	Cancelled    bool         `yaml:"cancelled,omitempty" json:"cancelled,omitempty"`
}

type Watch struct {
	Script  string `yaml:"script" json:"script"`
	Every   string `yaml:"every" json:"every"`
	Timeout string `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

type Transition struct {
	On       string   `yaml:"on" json:"on"`
	To       string   `yaml:"to" json:"to"`
	Requires []string `yaml:"requires,omitempty" json:"requires,omitempty"`
	Checks   []Check  `yaml:"checks,omitempty" json:"checks,omitempty"`
}

type Check struct {
	Script  string `yaml:"script" json:"script"`
	RunAs   string `yaml:"run_as,omitempty" json:"run_as,omitempty"`
	Timeout string `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// Parse reads a Workflowfile.yaml, or the one inside a directory. It decodes
// strictly and sets Dir. It does not validate the graph; call Validate.
func Parse(path string) (*File, error)

func (o *Owner) UnmarshalYAML(node *yaml.Node) error
func (o Owner) MarshalYAML() (any, error)
```

- [ ] **Step 1: Write the failing tests** for these behaviors:
  - the `development` example from the spec parses, and every field lands in the struct, including `owner: { pool: developers }`, `owner: customer`, `owner: script`, the `watch` block, `requires`, `checks`, and per-status `limits`;
  - `Parse` accepts a directory and a file path, and sets `Dir` to the directory;
  - an unknown root field, an unknown status field, and an unknown transition field each fail with the field name in the error;
  - a second YAML document fails;
  - `owner: nobody`, `owner: { pool: a, extra: b }`, `owner: { team: a }`, and `owner: [a]` each fail;
  - `Owner` round-trips through `yaml.Marshal` in all three forms;
  - YAML `on:` as a transition key is read as the string field `On` (yaml.v3 does not treat `on` as a boolean key; the test pins this).
- [ ] **Step 2: Run** `go test ./internal/workflowfile/ -count=1` and confirm it fails to compile.
- [ ] **Step 3: Implement** `workflowfile.go`. Follow the strict-decoder pattern of `imagefile.ParseV2` (`internal/imagefile/v2.go`): `yaml.NewDecoder`, `KnownFields(true)`, and a second `Decode` that must return `io.EOF`.
- [ ] **Step 4: Run** `go test ./internal/workflowfile/ -count=1` and confirm it passes.
- [ ] **Step 5: Commit** with message `Add the Workflowfile manifest types and strict parser`.

---

### Task 2: Manifest validation and limits

**Files:**
- Create: `internal/workflowfile/validate.go`
- Create: `internal/workflowfile/limits.go`
- Test: `internal/workflowfile/validate_test.go`
- Test: `internal/workflowfile/limits_test.go`

**Interfaces:**
- Consumes: the types from Task 1.
- Produces:

```go
type ValidationError struct {
	Code    string `json:"code"`
	Path    string `json:"path"`    // for example "statuses[2].transitions[0].to"
	Message string `json:"message"`
}

// Validate returns every independently discoverable error, ordered by path.
// A nil or empty result means the manifest and its files are valid.
func Validate(f *File) []ValidationError

type EffectiveLimits struct {
	IdleIterations   int
	RejectedRequests int
	ScriptFailures   int
	UnavailableGrace time.Duration
}

// StatusLimits resolves workflow defaults, workflow overrides, and the
// status's own overrides, in that order.
func (f *File) StatusLimits(statusID string) EffectiveLimits

// Pools returns the distinct pool names the statuses use, sorted.
func (f *File) Pools() []string

// Files returns every source-relative script and instruction path the
// manifest names, cleaned, without the leading "./", sorted and distinct.
func (f *File) Files() []string

const (
	DefaultCheckTimeout = 60 * time.Second
	MaxCheckTimeout     = 30 * time.Minute
	DefaultWatchTimeout = 60 * time.Second
)
```

Validation codes, one test case each, plus one test proving several errors are returned together:

| Code | Rule |
| --- | --- |
| `schema_version_unsupported` | `schema_version` is not `1` |
| `name_invalid` | name fails the workflow name rule |
| `version_invalid` | `workflow_version` is missing or not SemVer |
| `status_missing` | `statuses` is empty |
| `status_id_invalid` | a status ID fails the identifier rule |
| `status_duplicate` | two statuses share an ID |
| `initial_status_unknown` | `initial_status` names no status |
| `initial_status_terminal` | `initial_status` is terminal |
| `status_unreachable` | a status cannot be reached from `initial_status` |
| `terminal_unreachable` | no terminal status is reachable |
| `owner_missing` | a non-terminal status has no owner |
| `owner_invalid` | a pool owner's pool name fails the identifier rule |
| `terminal_has_work` | a terminal status has an owner, transitions, watch, instructions, or limits |
| `cancelled_not_terminal` | `cancelled: true` on a non-terminal status |
| `transitions_missing` | a non-terminal status has no transition |
| `outcome_invalid` | `on` fails the identifier rule |
| `outcome_duplicate` | two transitions of one status share `on` |
| `transition_target_unknown` | `to` names no status |
| `requires_not_allowed` | `requires` on a transition out of a customer or script status |
| `checks_not_allowed` | `checks` on a transition out of a customer or script status |
| `watch_missing` | a script status has no `watch` |
| `watch_not_allowed` | `watch` on a pool or customer status |
| `instructions_not_allowed` | `instructions` on a script status |
| `artifact_name_invalid` | an artifact name fails the identifier rule |
| `artifact_duplicate` | two artifacts share a name |
| `artifact_unknown` | `requires` names an undeclared artifact |
| `path_invalid` | a path does not start with `./`, is absolute, or leaves the source directory |
| `file_missing` | a named file does not exist |
| `file_not_regular` | a named file is a symlink, directory, or special file |
| `script_not_executable` | a script has no executable bit |
| `duration_invalid` | `every`, a `timeout`, or `unavailable_grace` does not parse as a non-negative Go duration; `every` and timeouts must be positive |
| `timeout_too_long` | a check timeout exceeds `30m` |
| `run_as_invalid` | `run_as` is neither `queue` nor `agent` |
| `limit_invalid` | a count limit is negative or zero |
| `secret_name_invalid` | a `requires_secrets` entry fails the name rule or repeats |
| `env_name_invalid` | an `env` name fails the name rule or starts with `TARIBOY_` |

- [ ] **Step 1: Write the failing tests.** Build fixtures in `t.TempDir()` with a helper that writes a manifest and its files; start every negative case from one valid fixture and change one thing. Add tests for `StatusLimits` (defaults, workflow override, status override, `unavailable_grace: 0`), `Pools`, and `Files`. Include the Review Focus case: `./scripts/../../outside.sh` and `/etc/passwd` are `path_invalid`, and validation does not stat them.
- [ ] **Step 2: Run** `go test ./internal/workflowfile/ -count=1` and confirm the new tests fail.
- [ ] **Step 3: Implement** `validate.go` and `limits.go`. Check paths before touching the filesystem; use `os.Lstat` so a symlink is reported as `file_not_regular`.
- [ ] **Step 4: Run** `go test ./internal/workflowfile/ -count=1` and confirm it passes.
- [ ] **Step 5: Commit** with message `Validate workflow manifests and resolve their limits`.

---

### Task 3: Content-addressed workflow image store

**Files:**
- Create: `internal/workflowimage/manifest.go`
- Create: `internal/workflowimage/store.go`
- Test: `internal/workflowimage/store_test.go`

**Interfaces:**
- Consumes: `workflowfile.File`, `workflowfile.Validate`, `(*File).Files()`.
- Produces:

```go
package workflowimage

var (
	ErrNotFound         = errors.New("workflow image not found")
	ErrVersionPublished = errors.New("workflow version is already published with different content")
	ErrInvalid          = errors.New("workflow source is invalid")
)

type FileEntry struct {
	Path       string `json:"path"`   // source-relative, slash-separated
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

type Store struct{ Dir string } // <base-dir>/workflows

// Publish validates src, copies the whole source directory, and points the
// version tag and "latest" at it. The digest covers the normalized definition
// and every file's path, executable bit, and content, and nothing else.
func (s *Store) Publish(src *workflowfile.File, now time.Time) (Manifest, bool, error) // bool: content was new

func (s *Store) Resolve(name, tag string) (string, error)          // tag or full digest -> digest
func (s *Store) Inspect(name, tagOrDigest string) (Manifest, error)
func (s *Store) List() ([]Manifest, error)                         // one entry per name and tag
func (s *Store) Tags(name, digest string) ([]string, error)
func (s *Store) RemoveTag(name, tag string) (digest string, contentRemoved bool, err error)
func (s *Store) ContentDir(name, digest string) string            // absolute path of the unpacked tree
func (s *Store) FilePath(name, digest, rel string) (string, error) // refuses paths outside the tree
```

Layout: `<Dir>/<name>/refs/<digest>/` holds the copied source tree plus `manifest.json`; `<Dir>/<name>/tags/<tag>` is a file whose content is the digest.

- [ ] **Step 1: Write the failing tests** for these behaviors:
  - a valid source publishes; `Inspect` by version tag, by `latest`, and by digest return the same manifest; `List` shows both tags;
  - the stored tree has the documented modes and contains every source file plus `manifest.json`;
  - publishing the same source again returns the same digest, `false`, and the original `BuiltAt`, even after `os.Chtimes` on a source file;
  - changing one script byte under the same version returns `ErrVersionPublished` and leaves the first digest's content and both tags intact;
  - changing the version publishes a second digest; `latest` moves, the old version tag stays;
  - toggling a script's executable bit changes the digest;
  - an invalid manifest returns an error wrapping `ErrInvalid` that carries the validation errors;
  - a symlink in the source (even to a file inside it), a file over 4 MiB, more than 256 files, and a total over 32 MiB each fail without leaving a `refs/` entry;
  - `RemoveTag` of one of two tags keeps the content; removing the last tag removes it; an unknown tag is `ErrNotFound`;
  - `FilePath` rejects `../x` and an absolute path;
  - a publish that fails while moving tags restores the previous tags (inject the failure with a read-only `tags/` directory).
- [ ] **Step 2: Run** `go test ./internal/workflowimage/ -count=1` and confirm it fails to compile.
- [ ] **Step 3: Implement** `manifest.go` and `store.go`. Copy into a temporary sibling directory and rename it into place, so a crash never leaves a partial `refs/<digest>/`. Write tag files with a temporary file and rename. A package-level mutex serializes publication and removal.
- [ ] **Step 4: Run** `go test ./internal/workflowimage/ -count=1` and confirm it passes.
- [ ] **Step 5: Commit** with message `Store workflow images by content digest`.

---

### Task 4: SQLite record of published manifests

**Files:**
- Create: `internal/store/migrations/0053_task_workflow_images.sql`
- Create: `internal/workflowimage/registry.go`
- Test: `internal/workflowimage/registry_test.go`
- Test: `internal/store/store_test.go` (one migration test)

**Interfaces:**
- Consumes: `workflowimage.Store`, `workflowimage.Manifest`, `store.Store` (`internal/store`).
- Produces:

```sql
CREATE TABLE task_workflow_images (
    digest   TEXT PRIMARY KEY,
    name     TEXT NOT NULL,
    version  TEXT NOT NULL,
    manifest TEXT NOT NULL,
    built_at TEXT NOT NULL
);
CREATE INDEX idx_task_workflow_images_name ON task_workflow_images(name, version);
```

```go
type Registry struct {
	Store *Store
	DB    *sql.DB
}

// Publish publishes to disk and records the manifest. On a database failure
// it removes content and tags this call introduced.
func (r *Registry) Publish(src *workflowfile.File, now time.Time) (Manifest, bool, error)

// Get returns the recorded manifest for a digest.
func (r *Registry) Get(digest string) (Manifest, error)

// Remove removes a tag; when the content goes, it removes the row too.
func (r *Registry) Remove(name, tag string) error

// Reconcile inserts rows for stored content that has none. Called at daemon
// start so a crash between the two writes heals.
func (r *Registry) Reconcile() error
```

- [ ] **Step 1: Write the failing tests:** publish records a row whose manifest JSON round-trips; republishing does not duplicate it; a database failure during publish (closed `*sql.DB`) leaves no new content and no new tag; `Remove` deletes the row only when the content is gone; `Reconcile` restores a deleted row; the migration test confirms the table and index exist after `store.Open`.
- [ ] **Step 2: Run** `go test ./internal/workflowimage/ ./internal/store/ -count=1` and confirm the new tests fail.
- [ ] **Step 3: Implement** the migration and `registry.go`. Call `Reconcile` from daemon startup next to the image store migration (find the call to `imageStore(...).Migrate()` or its daemon-start equivalent in `internal/daemon/daemon.go` and follow that pattern); a `Reconcile` error is logged, not fatal.
- [ ] **Step 4: Run** `go test ./internal/workflowimage/ ./internal/store/ ./internal/daemon/ -count=1` and confirm it passes.
- [ ] **Step 5: Commit** with message `Record published workflow manifests in SQLite`.

---

### Task 5: Store discovery and operator commands

**Files:**
- Create: `internal/stores/workflows.go`
- Modify: `internal/stores/stores.go` (the `Detail` type and `detail`)
- Create: `internal/commands/workflow_image.go`
- Modify: `internal/commands/daemon.go` (registration)
- Test: `internal/stores/workflows_test.go`
- Test: `internal/commands/workflow_image_test.go`

**Interfaces:**
- Consumes: `workflowfile.Parse`, `workflowfile.Validate`, `workflowimage.Registry`.
- Produces:

```go
// internal/stores
type StoreWorkflow struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Error   string `json:"error,omitempty"`
}
// Detail gains: Workflows []StoreWorkflow `json:"workflows"`

// PrepareWorkflowBuild resolves "store/workflow" to its source directory and
// holds the catalog lock until release is called, like PrepareBuild.
func (c *Catalog) PrepareWorkflowBuild(selector string) (PreparedBuild, func(), error)
```

Commands, each registered with `mustRegister` in `internal/commands/daemon.go`:

| Path | Route | Arguments | Result |
| --- | --- | --- | --- |
| `workflow.validate` | `POST /api/workflow-images/validate` | `source` (store selector) or `path` | `{valid, name, version, pools, files, errors}` |
| `workflow.build` | `POST /api/workflow-images/build` | `source` or `path` | `{name, version, digest, tags, created}` |
| `workflow.ls` | `GET /api/workflow-images` | none | `{workflows: [{name, tag, version, digest, built_at}], count}` |
| `workflow.inspect` | `GET /api/workflow-images/{name}/{tag}` | `name`, `tag` | the manifest |
| `workflow.rm` | `DELETE /api/workflow-images/{name}/{tag}` | `name`, `tag` | `{name, tag, removed, content_removed}` |

Error mapping: validation failure → `workflow_invalid` (HTTP 400) with `details.errors`; `ErrVersionPublished` → `workflow_version_published` (409); `ErrNotFound` → `not_found` (404); `source` together with `path` → `bad_source` (400).

- [ ] **Step 1: Write the failing tests.** Stores: a Store with `workflows/a/Workflowfile.yaml`, a broken `workflows/b`, and no `workflows/` directory each produce the documented `Detail.Workflows`; `PrepareWorkflowBuild` resolves a valid selector and rejects `x/../y`. Commands: follow the handler-test pattern in `internal/commands/stores_test.go`; cover each command's success result and each mapped error.
- [ ] **Step 2: Run** `go test ./internal/stores/ ./internal/commands/ -count=1` and confirm the new tests fail.
- [ ] **Step 3: Implement.** A relative `path` is resolved by the CLI against the caller's working directory exactly as `image.build` does. If the registry path prefix `workflow.` collides with an existing command group, stop and report it instead of renaming.
- [ ] **Step 4: Run** `go test ./internal/stores/ ./internal/commands/ -count=1` and `go build ./...`; confirm both pass.
- [ ] **Step 5: Commit** with message `Add workflow image commands and Store discovery`.

---

### Task 6: Version commands

**Files:**
- Create: `internal/workflowfile/version.go`
- Test: `internal/workflowfile/version_test.go`
- Modify: the file that implements the local `tariboy image version get|update` commands (locate it with `rg -n "version update" internal cmd`)

**Interfaces:**
- Consumes: the YAML-node version logic in `internal/imagefile/version.go`.
- Produces:

```go
// GetVersion reads workflow_version from a Workflowfile.yaml or its directory.
func GetVersion(path string) (string, error)

// UpdateVersion bumps major, minor, or patch, resets lower parts, drops
// suffixes, and rewrites the file atomically, preserving permissions.
func UpdateVersion(path, part string) (string, error)
```

and the local commands `tariboy workflow version get [--path FILE_OR_DIR]` and `tariboy workflow version update <major|minor|patch> [--path FILE_OR_DIR]`, which need no daemon and default to `./Workflowfile.yaml`.

- [ ] **Step 1: Write the failing tests:** get and update on a file and on a directory; `1.2.3-rc.1+build.7` becomes `1.3.0` after `update minor`; a missing or invalid version is an error and leaves the file unchanged; file permissions survive; comments in the YAML survive.
- [ ] **Step 2: Run** `go test ./internal/workflowfile/ -count=1` and confirm the new tests fail.
- [ ] **Step 3: Implement.** Do not copy `imagefile/version.go`: extract its field-independent part into an exported helper in `internal/imagefile` that takes the YAML key and the default filename, keep `imagefile.GetVersion` and `imagefile.UpdateVersion` as thin callers so their tests still pass, and call the helper from `workflowfile`. Wire the two local commands beside the image ones.
- [ ] **Step 4: Run** `go test ./internal/workflowfile/ ./internal/imagefile/ -count=1` plus the test package of the CLI file you changed; confirm they pass.
- [ ] **Step 5: Commit** with message `Add workflow version commands`.

---

### Task 7: Documentation and checks

**Files:**
- Create: `docs/docs/workflow-images.mdx`
- Modify: `docs/docs/meta.ts` (navigation entry after Task workflows)
- Modify: `docs/docs/images/index.mdx` (one paragraph under "Stores on a server" that a Store may also hold `workflows/<name>/Workflowfile.yaml`, linking to the new page)
- Modify: `docs/docs/reference/commands.md` (rows for the `tariboy workflow` commands)
- Modify: `docs/docs/architecture/state-model.mdx` (one paragraph: workflow images on disk and the `task_workflow_images` table)

**Interfaces:**
- Consumes: the behavior of Tasks 1–6.
- Produces: `docs/docs/workflow-images.mdx` covering the source layout, the full `Workflowfile.yaml` reference (root fields, status fields, limits, validation codes), build and immutability, storage layout, and the commands. State plainly that this release builds and stores workflow images and that queues do not execute them yet.

- [ ] **Step 1: Write** the page and the edits. Use the manifest example from the spec. Follow the frontmatter and heading style of `docs/docs/task-workflows.mdx`.
- [ ] **Step 2: Run** `make check` and confirm every step reports success.
- [ ] **Step 3: Run** `git diff --check main...HEAD` and confirm no output.
- [ ] **Step 4: Commit** with message `Document workflow images`.
