package workflowfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/alekzonder/tariboy/internal/imagefile"
)

var (
	identifierPattern   = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	workflowNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	envNamePattern      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// ValidationError is one problem found in a manifest or the files it names.
// Path uses the YAML field names with indexes.
type ValidationError struct {
	Code    string `json:"code"`
	Path    string `json:"path"` // for example "statuses[2].transitions[0].to"
	Message string `json:"message"`
}

type validator struct {
	errs []ValidationError
}

func (v *validator) add(code, path, format string, args ...any) {
	v.errs = append(v.errs, ValidationError{Code: code, Path: path, Message: fmt.Sprintf(format, args...)})
}

// fileRef is a manifest field that names a source file.
type fileRef struct {
	field  string
	value  string
	script bool
}

// ValidName reports whether name is a legal workflow name.
func ValidName(name string) bool { return workflowNamePattern.MatchString(name) }

// Validate returns every independently discoverable error, ordered by path
// and then by code. A nil or empty result means the manifest and its files
// are valid. Files are read relative to f.Dir.
func Validate(f *File) []ValidationError {
	v := &validator{}
	v.header(f)
	v.secretsAndEnv(f)
	v.limits("limits", f.Limits)
	artifacts := v.artifacts(f)
	v.sources(f)
	v.statuses(f, artifacts)
	v.files(f)
	sort.SliceStable(v.errs, func(i, j int) bool {
		if v.errs[i].Path != v.errs[j].Path {
			return v.errs[i].Path < v.errs[j].Path
		}
		return v.errs[i].Code < v.errs[j].Code
	})
	return v.errs
}

func (v *validator) header(f *File) {
	if f.SchemaVersion != 1 {
		v.add("schema_version_unsupported", "schema_version", "schema_version must be 1, got %d", f.SchemaVersion)
	}
	if !workflowNamePattern.MatchString(f.Name) {
		v.add("name_invalid", "name", "name %q must match %s", f.Name, workflowNamePattern)
	}
	if f.WorkflowVersion == "" {
		v.add("version_invalid", "workflow_version", "workflow_version is required")
	} else if err := imagefile.ValidateImageVersion(f.WorkflowVersion); err != nil {
		v.add("version_invalid", "workflow_version", "%v", err)
	}
}

func (v *validator) secretsAndEnv(f *File) {
	seen := map[string]bool{}
	for i, name := range f.RequiresSecrets {
		p := fmt.Sprintf("requires_secrets[%d]", i)
		switch {
		case !envNamePattern.MatchString(name):
			v.add("secret_name_invalid", p, "secret name %q must match %s", name, envNamePattern)
		case strings.HasPrefix(name, "TARIBOY_"):
			v.add("secret_name_invalid", p, "secret name %q must not start with TARIBOY_", name)
		case seen[name]:
			v.add("secret_name_invalid", p, "secret %q is repeated", name)
		}
		seen[name] = true
	}
	for name := range f.Env {
		p := "env." + name
		switch {
		case !envNamePattern.MatchString(name):
			v.add("env_name_invalid", p, "env name %q must match %s", name, envNamePattern)
		case strings.HasPrefix(name, "TARIBOY_"):
			v.add("env_name_invalid", p, "env name %q must not start with TARIBOY_", name)
		}
	}
}

func (v *validator) limits(prefix string, l *Limits) {
	if l == nil {
		return
	}
	for _, c := range []struct {
		name string
		val  *int
	}{
		{"idle_iterations", l.IdleIterations},
		{"rejected_requests", l.RejectedRequests},
		{"script_failures", l.ScriptFailures},
	} {
		if c.val != nil && *c.val <= 0 {
			v.add("limit_invalid", prefix+"."+c.name, "%s must be positive, got %d", c.name, *c.val)
		}
	}
	if l.UnavailableGrace != "" {
		if d, err := time.ParseDuration(l.UnavailableGrace); err != nil || d < 0 {
			v.add("duration_invalid", prefix+".unavailable_grace", "unavailable_grace %q must be a non-negative duration", l.UnavailableGrace)
		}
	}
}

func (v *validator) artifacts(f *File) map[string]bool {
	known := map[string]bool{}
	for i, a := range f.Artifacts {
		p := fmt.Sprintf("artifacts[%d].name", i)
		if !identifierPattern.MatchString(a.Name) {
			v.add("artifact_name_invalid", p, "artifact name %q must match %s", a.Name, identifierPattern)
		} else if known[a.Name] {
			v.add("artifact_duplicate", p, "artifact %q is declared twice", a.Name)
		}
		known[a.Name] = true
	}
	return known
}

func (v *validator) sources(f *File) {
	seen := map[string]bool{}
	for i, src := range f.Sources {
		p := fmt.Sprintf("sources[%d]", i)
		if !identifierPattern.MatchString(src.Name) {
			v.add("source_name_invalid", p+".name", "source name %q must match %s", src.Name, identifierPattern)
		} else if seen[src.Name] {
			v.add("source_duplicate", p+".name", "source %q is declared twice", src.Name)
		}
		seen[src.Name] = true
		if d, err := time.ParseDuration(src.Every); err != nil || d < MinSourceEvery {
			v.add("duration_invalid", p+".every", "every %q must be a duration of at least %s", src.Every, MinSourceEvery)
		}
		v.timeout(p+".timeout", src.Timeout, MaxWatchTimeout)
	}
}

func (v *validator) statuses(f *File, artifacts map[string]bool) {
	if len(f.Statuses) == 0 {
		v.add("status_missing", "statuses", "at least one status is required")
		return
	}
	ids := map[string]int{}
	resolvable := true
	for i, s := range f.Statuses {
		p := fmt.Sprintf("statuses[%d].id", i)
		if !identifierPattern.MatchString(s.ID) {
			v.add("status_id_invalid", p, "status id %q must match %s", s.ID, identifierPattern)
			resolvable = false
		} else if _, dup := ids[s.ID]; dup {
			v.add("status_duplicate", p, "status id %q is used twice", s.ID)
			resolvable = false
		}
		if _, ok := ids[s.ID]; !ok {
			ids[s.ID] = i
		}
	}
	initial, ok := ids[f.InitialStatus]
	switch {
	case !ok:
		v.add("initial_status_unknown", "initial_status", "initial_status %q names no status", f.InitialStatus)
		resolvable = false
	case f.Statuses[initial].Terminal:
		v.add("initial_status_terminal", "initial_status", "initial_status %q is terminal", f.InitialStatus)
		resolvable = false
	}
	for i := range f.Statuses {
		if !v.status(f, i, ids, artifacts) {
			resolvable = false
		}
	}
	if resolvable {
		v.reachability(f, ids, initial)
	}
}

// status validates one status. It reports false when a transition target is
// unknown, which makes reachability meaningless.
func (v *validator) status(f *File, i int, ids map[string]int, artifacts map[string]bool) bool {
	s := &f.Statuses[i]
	p := fmt.Sprintf("statuses[%d]", i)
	targetsKnown := true

	if s.Terminal {
		if s.Owner.Kind != "" {
			v.add("terminal_has_work", p+".owner", "terminal status %q must not have an owner", s.ID)
		}
		if len(s.Transitions) > 0 {
			v.add("terminal_has_work", p+".transitions", "terminal status %q must not have transitions", s.ID)
		}
		if s.Watch != nil {
			v.add("terminal_has_work", p+".watch", "terminal status %q must not have a watch", s.ID)
		}
		if s.Instructions != "" {
			v.add("terminal_has_work", p+".instructions", "terminal status %q must not have instructions", s.ID)
		}
		if s.Limits != nil {
			v.add("terminal_has_work", p+".limits", "terminal status %q must not have limits", s.ID)
		}
		return true
	}

	if s.Cancelled {
		v.add("cancelled_not_terminal", p+".cancelled", "cancelled is allowed only on a terminal status")
	}
	switch s.Owner.Kind {
	case "":
		v.add("owner_missing", p+".owner", "status %q needs an owner", s.ID)
	case OwnerPool:
		if !identifierPattern.MatchString(s.Owner.Pool) {
			v.add("owner_invalid", p+".owner.pool", "pool name %q must match %s", s.Owner.Pool, identifierPattern)
		}
	}
	v.limits(p+".limits", s.Limits)

	isScript := s.Owner.Kind == OwnerScript
	if isScript {
		if s.Watch == nil {
			v.add("watch_missing", p+".watch", "script status %q needs a watch", s.ID)
		}
		if s.Instructions != "" {
			v.add("instructions_not_allowed", p+".instructions", "script status %q must not have instructions", s.ID)
		}
	} else if s.Watch != nil {
		v.add("watch_not_allowed", p+".watch", "only a script status may have a watch")
	}
	if s.Watch != nil {
		v.watch(p+".watch", s.Watch)
	}

	if len(s.Transitions) == 0 {
		v.add("transitions_missing", p+".transitions", "status %q needs at least one transition", s.ID)
	}
	outcomes := map[string]bool{}
	for j := range s.Transitions {
		t := &s.Transitions[j]
		tp := fmt.Sprintf("%s.transitions[%d]", p, j)
		if !identifierPattern.MatchString(t.On) {
			v.add("outcome_invalid", tp+".on", "outcome %q must match %s", t.On, identifierPattern)
		} else if outcomes[t.On] {
			v.add("outcome_duplicate", tp+".on", "outcome %q is used twice in status %q", t.On, s.ID)
		}
		outcomes[t.On] = true
		if _, ok := ids[t.To]; !ok {
			v.add("transition_target_unknown", tp+".to", "transition target %q names no status", t.To)
			targetsKnown = false
		}
		if s.Owner.Kind == OwnerCustomer || isScript {
			if len(t.Requires) > 0 {
				v.add("requires_not_allowed", tp+".requires", "requires is allowed only on transitions out of a pool status")
			}
			if len(t.Checks) > 0 {
				v.add("checks_not_allowed", tp+".checks", "checks are allowed only on transitions out of a pool status")
			}
		}
		for k, name := range t.Requires {
			if !artifacts[name] {
				v.add("artifact_unknown", fmt.Sprintf("%s.requires[%d]", tp, k), "artifact %q is not declared", name)
			}
		}
		for k := range t.Checks {
			v.check(fmt.Sprintf("%s.checks[%d]", tp, k), &t.Checks[k])
		}
	}
	return targetsKnown
}

func (v *validator) watch(p string, w *Watch) {
	if d, err := time.ParseDuration(w.Every); err != nil || d < MinWatchEvery {
		v.add("duration_invalid", p+".every", "every %q must be a duration of at least %s", w.Every, MinWatchEvery)
	}
	v.timeout(p+".timeout", w.Timeout, MaxWatchTimeout)
}

// timeout validates an optional script timeout at p against limit.
func (v *validator) timeout(p, value string, limit time.Duration) {
	if value == "" {
		return
	}
	d, err := time.ParseDuration(value)
	switch {
	case err != nil || d <= 0:
		v.add("duration_invalid", p, "timeout %q must be a positive duration", value)
	case d > limit:
		v.add("timeout_too_long", p, "timeout %q exceeds %s", value, limit)
	}
}

func (v *validator) check(p string, c *Check) {
	if c.RunAs != "" && c.RunAs != RunAsQueue && c.RunAs != RunAsAgent {
		v.add("run_as_invalid", p+".run_as", "run_as %q must be %s or %s", c.RunAs, RunAsQueue, RunAsAgent)
	}
	v.timeout(p+".timeout", c.Timeout, MaxCheckTimeout)
}

// reachability reports statuses no path from the initial status reaches, and
// a workflow in which no terminal status is reachable. Edges out of terminal
// statuses are ignored.
func (v *validator) reachability(f *File, ids map[string]int, initial int) {
	seen := map[int]bool{initial: true}
	queue := []int{initial}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if f.Statuses[cur].Terminal {
			continue
		}
		for _, t := range f.Statuses[cur].Transitions {
			if next, ok := ids[t.To]; ok && !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	terminal := false
	for i, s := range f.Statuses {
		if !seen[i] {
			v.add("status_unreachable", fmt.Sprintf("statuses[%d].id", i), "status %q cannot be reached from %q", s.ID, f.InitialStatus)
		} else if s.Terminal {
			terminal = true
		}
	}
	if !terminal {
		v.add("terminal_unreachable", "statuses", "no terminal status is reachable from %q", f.InitialStatus)
	}
}

// refs lists every field that names a file, in manifest order.
func (f *File) refs() []fileRef {
	var out []fileRef
	for i, src := range f.Sources {
		out = append(out, fileRef{fmt.Sprintf("sources[%d].script", i), src.Script, true})
	}
	for i, s := range f.Statuses {
		p := fmt.Sprintf("statuses[%d]", i)
		if s.Instructions != "" {
			out = append(out, fileRef{p + ".instructions", s.Instructions, false})
		}
		if s.Watch != nil {
			out = append(out, fileRef{p + ".watch.script", s.Watch.Script, true})
		}
		for j, t := range s.Transitions {
			for k, c := range t.Checks {
				out = append(out, fileRef{fmt.Sprintf("%s.transitions[%d].checks[%d].script", p, j, k), c.Script, true})
			}
		}
	}
	return out
}

// cleanSourcePath applies the path rule: the path starts with "./", is not
// absolute, stays inside the source directory, and is not under the top-level
// .git directory, which the build does not store. It returns the cleaned path
// without the leading "./".
func cleanSourcePath(p string) (string, bool) {
	if !strings.HasPrefix(p, "./") || path.IsAbs(p) {
		return "", false
	}
	clean := path.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	if clean == ".git" || strings.HasPrefix(clean, ".git/") {
		return "", false
	}
	return clean, true
}

// files checks each named path once: the path rule first, then the file
// system, so an invalid path is never statted.
func (v *validator) files(f *File) {
	type target struct {
		field       string // first field naming the path
		scriptField string // first script field naming the path, if any
	}
	byPath := map[string]*target{}
	var order []string
	for _, r := range f.refs() {
		clean, ok := cleanSourcePath(r.value)
		if !ok {
			v.add("path_invalid", r.field, "path %q must start with ./, stay inside the source directory, and not be under .git", r.value)
			continue
		}
		t := byPath[clean]
		if t == nil {
			t = &target{field: r.field}
			byPath[clean] = t
			order = append(order, clean)
		}
		if r.script && t.scriptField == "" {
			t.scriptField = r.field
		}
	}
	for _, clean := range order {
		t := byPath[clean]
		mode, code, msg := inspect(f.Dir, clean)
		if code != "" {
			v.add(code, t.field, "%s", msg)
		} else if t.scriptField != "" && mode&0o111 == 0 {
			v.add("script_not_executable", t.scriptField, "script %q has no executable bit", "./"+clean)
		}
	}
}

// inspect walks the components of rel under dir with Lstat. Any symlink,
// including a directory component, is not regular; a missing component is a
// missing file.
func inspect(dir, rel string) (fs.FileMode, string, string) {
	parts := strings.Split(rel, "/")
	cur := dir
	var info fs.FileInfo
	for i, part := range parts {
		cur = filepath.Join(cur, part)
		var err error
		info, err = os.Lstat(cur)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return 0, "file_missing", fmt.Sprintf("file %q does not exist", "./"+rel)
			}
			return 0, "file_missing", fmt.Sprintf("file %q cannot be read: %v", "./"+rel, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return 0, "file_not_regular", fmt.Sprintf("%q is a symlink", "./"+strings.Join(parts[:i+1], "/"))
		}
	}
	if !info.Mode().IsRegular() {
		return 0, "file_not_regular", fmt.Sprintf("%q is not a regular file", "./"+rel)
	}
	return info.Mode(), "", ""
}

// Pools returns the distinct pool names the statuses use, sorted.
func (f *File) Pools() []string {
	set := map[string]bool{}
	for _, s := range f.Statuses {
		if s.Owner.Kind == OwnerPool && s.Owner.Pool != "" {
			set[s.Owner.Pool] = true
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Files returns every source-relative script and instruction path the
// manifest names, cleaned, without the leading "./", sorted and distinct.
// Paths that fail the path rule are skipped; Validate reports them.
func (f *File) Files() []string {
	set := map[string]bool{}
	for _, r := range f.refs() {
		if clean, ok := cleanSourcePath(r.value); ok {
			set[clean] = true
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
