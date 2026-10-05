package workflowfile

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// validFixture writes the files of a valid manifest into a temp dir and
// returns the manifest. Every negative case starts from it.
func validFixture(t *testing.T) *File {
	t.Helper()
	dir := t.TempDir()
	write := func(rel string, mode os.FileMode) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("statuses/plan.md", 0o644)
	write("scripts/check.sh", 0o755)
	write("scripts/watch.sh", 0o755)
	write("scripts/source.sh", 0o755)
	return &File{
		SchemaVersion:   1,
		Name:            "development",
		WorkflowVersion: "0.1.0",
		InitialStatus:   "plan",
		RequiresSecrets: []string{"GH_TOKEN"},
		Env:             map[string]string{"PR_POLL_SECONDS": "60"},
		Limits:          &Limits{UnavailableGrace: "5m"},
		Artifacts:       []Artifact{{Name: "plan"}, {Name: "pull_request"}},
		Sources:         []Source{{Name: "pull-requests", Script: "./scripts/source.sh", Every: "2m", Timeout: "30s"}},
		Statuses: []Status{
			{ID: "plan", Owner: Owner{Kind: OwnerPool, Pool: "developers"}, Instructions: "./statuses/plan.md",
				Transitions: []Transition{{On: "planned", To: "approval", Requires: []string{"plan"},
					Checks: []Check{{Script: "./scripts/check.sh", RunAs: RunAsQueue, Timeout: "30s"}}}}},
			{ID: "approval", Owner: Owner{Kind: OwnerCustomer},
				Transitions: []Transition{{On: "approved", To: "ci"}, {On: "rejected", To: "cancelled"}}},
			{ID: "ci", Owner: Owner{Kind: OwnerScript},
				Watch:       &Watch{Script: "./scripts/watch.sh", Every: "1m", Timeout: "30s"},
				Transitions: []Transition{{On: "passed", To: "done"}}},
			{ID: "done", Terminal: true},
			{ID: "cancelled", Terminal: true, Cancelled: true},
		},
		Dir: dir,
	}
}

func hasError(errs []ValidationError, code, path string) bool {
	for _, e := range errs {
		if e.Code == code && e.Path == path {
			return true
		}
	}
	return false
}

func TestValidateAcceptsValidFixture(t *testing.T) {
	if errs := Validate(validFixture(t)); len(errs) != 0 {
		t.Fatalf("errors = %+v", errs)
	}
}

func TestValidateCodes(t *testing.T) {
	watchAny := &Watch{Script: "./scripts/watch.sh", Every: "1m"}
	cases := []struct {
		name   string
		mutate func(t *testing.T, f *File)
		code   string
		path   string
	}{
		{"schema_version_unsupported", func(t *testing.T, f *File) { f.SchemaVersion = 2 }, "schema_version_unsupported", "schema_version"},
		{"name_invalid", func(t *testing.T, f *File) { f.Name = "Bad Name" }, "name_invalid", "name"},
		{"version_invalid missing", func(t *testing.T, f *File) { f.WorkflowVersion = "" }, "version_invalid", "workflow_version"},
		{"version_invalid not semver", func(t *testing.T, f *File) { f.WorkflowVersion = "v1" }, "version_invalid", "workflow_version"},
		{"status_missing", func(t *testing.T, f *File) { f.Statuses = nil }, "status_missing", "statuses"},
		{"status_id_invalid", func(t *testing.T, f *File) { f.Statuses[3].ID = "Done!" }, "status_id_invalid", "statuses[3].id"},
		{"status_duplicate", func(t *testing.T, f *File) { f.Statuses[4].ID = "done" }, "status_duplicate", "statuses[4].id"},
		{"initial_status_unknown", func(t *testing.T, f *File) { f.InitialStatus = "nope" }, "initial_status_unknown", "initial_status"},
		{"initial_status_terminal", func(t *testing.T, f *File) { f.InitialStatus = "done" }, "initial_status_terminal", "initial_status"},
		{"status_unreachable", func(t *testing.T, f *File) {
			f.Statuses = append(f.Statuses, Status{ID: "island", Owner: Owner{Kind: OwnerCustomer},
				Transitions: []Transition{{On: "x", To: "done"}}})
		}, "status_unreachable", "statuses[5].id"},
		{"terminal_unreachable", func(t *testing.T, f *File) {
			f.Statuses[1].Transitions = []Transition{{On: "approved", To: "plan"}}
			f.Statuses = f.Statuses[:2]
		}, "terminal_unreachable", "statuses"},
		{"owner_missing", func(t *testing.T, f *File) { f.Statuses[0].Owner = Owner{} }, "owner_missing", "statuses[0].owner"},
		{"owner_invalid", func(t *testing.T, f *File) { f.Statuses[0].Owner.Pool = "Dev Team" }, "owner_invalid", "statuses[0].owner.pool"},
		{"terminal_has_work owner", func(t *testing.T, f *File) { f.Statuses[3].Owner = Owner{Kind: OwnerCustomer} }, "terminal_has_work", "statuses[3].owner"},
		{"terminal_has_work transitions", func(t *testing.T, f *File) { f.Statuses[3].Transitions = []Transition{{On: "x", To: "plan"}} }, "terminal_has_work", "statuses[3].transitions"},
		{"terminal_has_work watch", func(t *testing.T, f *File) { f.Statuses[3].Watch = watchAny }, "terminal_has_work", "statuses[3].watch"},
		{"terminal_has_work instructions", func(t *testing.T, f *File) { f.Statuses[3].Instructions = "./statuses/plan.md" }, "terminal_has_work", "statuses[3].instructions"},
		{"terminal_has_work limits", func(t *testing.T, f *File) { f.Statuses[3].Limits = &Limits{} }, "terminal_has_work", "statuses[3].limits"},
		{"cancelled_not_terminal", func(t *testing.T, f *File) { f.Statuses[0].Cancelled = true }, "cancelled_not_terminal", "statuses[0].cancelled"},
		{"transitions_missing", func(t *testing.T, f *File) { f.Statuses[1].Transitions = nil }, "transitions_missing", "statuses[1].transitions"},
		{"outcome_invalid", func(t *testing.T, f *File) { f.Statuses[1].Transitions[0].On = "Not OK" }, "outcome_invalid", "statuses[1].transitions[0].on"},
		{"outcome_duplicate", func(t *testing.T, f *File) { f.Statuses[1].Transitions[1].On = "approved" }, "outcome_duplicate", "statuses[1].transitions[1].on"},
		{"transition_target_unknown", func(t *testing.T, f *File) { f.Statuses[1].Transitions[0].To = "nope" }, "transition_target_unknown", "statuses[1].transitions[0].to"},
		{"requires_not_allowed customer", func(t *testing.T, f *File) { f.Statuses[1].Transitions[0].Requires = []string{"plan"} }, "requires_not_allowed", "statuses[1].transitions[0].requires"},
		{"requires_not_allowed script", func(t *testing.T, f *File) { f.Statuses[2].Transitions[0].Requires = []string{"plan"} }, "requires_not_allowed", "statuses[2].transitions[0].requires"},
		{"checks_not_allowed", func(t *testing.T, f *File) {
			f.Statuses[1].Transitions[0].Checks = []Check{{Script: "./scripts/check.sh"}}
		}, "checks_not_allowed", "statuses[1].transitions[0].checks"},
		{"watch_missing", func(t *testing.T, f *File) { f.Statuses[2].Watch = nil }, "watch_missing", "statuses[2].watch"},
		{"watch_not_allowed pool", func(t *testing.T, f *File) { f.Statuses[0].Watch = watchAny }, "watch_not_allowed", "statuses[0].watch"},
		{"watch_not_allowed customer", func(t *testing.T, f *File) { f.Statuses[1].Watch = watchAny }, "watch_not_allowed", "statuses[1].watch"},
		{"instructions_not_allowed", func(t *testing.T, f *File) { f.Statuses[2].Instructions = "./statuses/plan.md" }, "instructions_not_allowed", "statuses[2].instructions"},
		{"artifact_name_invalid", func(t *testing.T, f *File) { f.Artifacts[1].Name = "Bad Name" }, "artifact_name_invalid", "artifacts[1].name"},
		{"artifact_duplicate", func(t *testing.T, f *File) { f.Artifacts[1].Name = "plan" }, "artifact_duplicate", "artifacts[1].name"},
		{"artifact_unknown", func(t *testing.T, f *File) { f.Statuses[0].Transitions[0].Requires = []string{"nope"} }, "artifact_unknown", "statuses[0].transitions[0].requires[0]"},
		{"path_invalid no prefix", func(t *testing.T, f *File) { f.Statuses[0].Instructions = "statuses/plan.md" }, "path_invalid", "statuses[0].instructions"},
		{"path_invalid absolute", func(t *testing.T, f *File) { f.Statuses[0].Instructions = "/etc/passwd" }, "path_invalid", "statuses[0].instructions"},
		{"path_invalid escape", func(t *testing.T, f *File) { f.Statuses[2].Watch.Script = "./scripts/../../outside.sh" }, "path_invalid", "statuses[2].watch.script"},
		{"path_invalid git", func(t *testing.T, f *File) {
			if err := os.MkdirAll(filepath.Join(f.Dir, ".git", "hooks"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.Dir, ".git", "hooks", "check.sh"), []byte("x\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			f.Statuses[0].Transitions[0].Checks[0].Script = "./.git/hooks/check.sh"
		}, "path_invalid", "statuses[0].transitions[0].checks[0].script"},
		{"path_invalid git directory", func(t *testing.T, f *File) { f.Statuses[0].Instructions = "./.git" }, "path_invalid", "statuses[0].instructions"},
		{"path_invalid check", func(t *testing.T, f *File) { f.Statuses[0].Transitions[0].Checks[0].Script = "" }, "path_invalid", "statuses[0].transitions[0].checks[0].script"},
		{"file_missing", func(t *testing.T, f *File) { f.Statuses[0].Instructions = "./statuses/none.md" }, "file_missing", "statuses[0].instructions"},
		{"file_not_regular directory", func(t *testing.T, f *File) { f.Statuses[0].Instructions = "./statuses" }, "file_not_regular", "statuses[0].instructions"},
		{"file_not_regular symlink", func(t *testing.T, f *File) {
			if err := os.Symlink("plan.md", filepath.Join(f.Dir, "statuses", "link.md")); err != nil {
				t.Fatal(err)
			}
			f.Statuses[0].Instructions = "./statuses/link.md"
		}, "file_not_regular", "statuses[0].instructions"},
		{"file_not_regular symlinked directory", func(t *testing.T, f *File) {
			if err := os.Symlink("statuses", filepath.Join(f.Dir, "linked")); err != nil {
				t.Fatal(err)
			}
			f.Statuses[0].Instructions = "./linked/plan.md"
		}, "file_not_regular", "statuses[0].instructions"},
		{"script_not_executable", func(t *testing.T, f *File) {
			if err := os.Chmod(filepath.Join(f.Dir, "scripts", "watch.sh"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, "script_not_executable", "statuses[2].watch.script"},
		{"duration_invalid every", func(t *testing.T, f *File) { f.Statuses[2].Watch.Every = "soon" }, "duration_invalid", "statuses[2].watch.every"},
		{"duration_invalid every missing", func(t *testing.T, f *File) { f.Statuses[2].Watch.Every = "" }, "duration_invalid", "statuses[2].watch.every"},
		{"duration_invalid every zero", func(t *testing.T, f *File) { f.Statuses[2].Watch.Every = "0s" }, "duration_invalid", "statuses[2].watch.every"},
		{"duration_invalid every below minimum", func(t *testing.T, f *File) { f.Statuses[2].Watch.Every = "500ms" }, "duration_invalid", "statuses[2].watch.every"},
		{"timeout_too_long watch", func(t *testing.T, f *File) { f.Statuses[2].Watch.Timeout = "31m" }, "timeout_too_long", "statuses[2].watch.timeout"},
		{"duration_invalid watch timeout", func(t *testing.T, f *File) { f.Statuses[2].Watch.Timeout = "-1s" }, "duration_invalid", "statuses[2].watch.timeout"},
		{"duration_invalid check timeout", func(t *testing.T, f *File) { f.Statuses[0].Transitions[0].Checks[0].Timeout = "x" }, "duration_invalid", "statuses[0].transitions[0].checks[0].timeout"},
		{"duration_invalid grace", func(t *testing.T, f *File) { f.Limits.UnavailableGrace = "-5m" }, "duration_invalid", "limits.unavailable_grace"},
		{"duration_invalid status grace", func(t *testing.T, f *File) { f.Statuses[0].Limits = &Limits{UnavailableGrace: "x"} }, "duration_invalid", "statuses[0].limits.unavailable_grace"},
		{"timeout_too_long", func(t *testing.T, f *File) { f.Statuses[0].Transitions[0].Checks[0].Timeout = "31m" }, "timeout_too_long", "statuses[0].transitions[0].checks[0].timeout"},
		{"run_as_invalid", func(t *testing.T, f *File) { f.Statuses[0].Transitions[0].Checks[0].RunAs = "root" }, "run_as_invalid", "statuses[0].transitions[0].checks[0].run_as"},
		{"limit_invalid zero", func(t *testing.T, f *File) { zero := 0; f.Limits.IdleIterations = &zero }, "limit_invalid", "limits.idle_iterations"},
		{"limit_invalid negative", func(t *testing.T, f *File) { n := -1; f.Statuses[0].Limits = &Limits{RejectedRequests: &n} }, "limit_invalid", "statuses[0].limits.rejected_requests"},
		{"limit_invalid script_failures", func(t *testing.T, f *File) { n := -2; f.Limits.ScriptFailures = &n }, "limit_invalid", "limits.script_failures"},
		{"secret_name_invalid", func(t *testing.T, f *File) { f.RequiresSecrets = []string{"GH_TOKEN", "1BAD"} }, "secret_name_invalid", "requires_secrets[1]"},
		{"secret_name_invalid repeat", func(t *testing.T, f *File) { f.RequiresSecrets = []string{"GH_TOKEN", "GH_TOKEN"} }, "secret_name_invalid", "requires_secrets[1]"},
		{"secret_name_invalid prefix", func(t *testing.T, f *File) { f.RequiresSecrets = []string{"TARIBOY_RESULT_FILE"} }, "secret_name_invalid", "requires_secrets[0]"},
		{"env_name_invalid", func(t *testing.T, f *File) { f.Env = map[string]string{"bad-name": "x"} }, "env_name_invalid", "env.bad-name"},
		{"source_name_invalid", func(t *testing.T, f *File) { f.Sources[0].Name = "Pull Requests" }, "source_name_invalid", "sources[0].name"},
		{"source_duplicate", func(t *testing.T, f *File) { f.Sources = append(f.Sources, f.Sources[0]) }, "source_duplicate", "sources[1].name"},
		{"path_invalid source", func(t *testing.T, f *File) { f.Sources[0].Script = "scripts/source.sh" }, "path_invalid", "sources[0].script"},
		{"file_missing source", func(t *testing.T, f *File) { f.Sources[0].Script = "./scripts/none.sh" }, "file_missing", "sources[0].script"},
		{"script_not_executable source", func(t *testing.T, f *File) {
			if err := os.Chmod(filepath.Join(f.Dir, "scripts", "source.sh"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, "script_not_executable", "sources[0].script"},
		{"duration_invalid source every", func(t *testing.T, f *File) { f.Sources[0].Every = "" }, "duration_invalid", "sources[0].every"},
		{"duration_invalid source every below minimum", func(t *testing.T, f *File) { f.Sources[0].Every = "9s" }, "duration_invalid", "sources[0].every"},
		{"duration_invalid source timeout", func(t *testing.T, f *File) { f.Sources[0].Timeout = "0s" }, "duration_invalid", "sources[0].timeout"},
		{"timeout_too_long source", func(t *testing.T, f *File) { f.Sources[0].Timeout = "31m" }, "timeout_too_long", "sources[0].timeout"},
		{"env_name_invalid prefix", func(t *testing.T, f *File) { f.Env = map[string]string{"TARIBOY_X": "x"} }, "env_name_invalid", "env.TARIBOY_X"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := validFixture(t)
			tc.mutate(t, f)
			errs := Validate(f)
			if !hasError(errs, tc.code, tc.path) {
				t.Fatalf("want %s at %s, got %+v", tc.code, tc.path, errs)
			}
		})
	}
}

func TestValidateReturnsSeveralErrorsOrdered(t *testing.T) {
	f := validFixture(t)
	f.Name = "Bad"
	f.SchemaVersion = 3
	f.Statuses[1].Transitions[0].To = "nope"
	f.Env = map[string]string{"TARIBOY_X": "x"}
	errs := Validate(f)
	for _, w := range [][2]string{
		{"name_invalid", "name"},
		{"schema_version_unsupported", "schema_version"},
		{"transition_target_unknown", "statuses[1].transitions[0].to"},
		{"env_name_invalid", "env.TARIBOY_X"},
	} {
		if !hasError(errs, w[0], w[1]) {
			t.Errorf("missing %s at %s in %+v", w[0], w[1], errs)
		}
	}
	for i := 1; i < len(errs); i++ {
		a, b := errs[i-1], errs[i]
		if a.Path > b.Path || (a.Path == b.Path && a.Code > b.Code) {
			t.Fatalf("not ordered: %+v before %+v", a, b)
		}
	}
}

func TestValidateDoesNotCascadeReachability(t *testing.T) {
	f := validFixture(t)
	f.Statuses[1].Transitions[0].To = "nope"
	for _, e := range Validate(f) {
		if e.Code == "status_unreachable" || e.Code == "terminal_unreachable" {
			t.Fatalf("cascade error %+v", e)
		}
	}
}

func TestValidateDoesNotStatEscapingPaths(t *testing.T) {
	f := validFixture(t)
	f.Statuses[2].Watch.Script = "./scripts/../../outside.sh"
	f.Statuses[0].Instructions = "/etc/passwd"
	errs := Validate(f)
	for _, e := range errs {
		if e.Code == "file_missing" || e.Code == "file_not_regular" || e.Code == "script_not_executable" {
			t.Fatalf("unexpected file error %+v", e)
		}
	}
	if !hasError(errs, "path_invalid", "statuses[2].watch.script") || !hasError(errs, "path_invalid", "statuses[0].instructions") {
		t.Fatalf("errors = %+v", errs)
	}
}

func TestValidateChecksSharedPathOnce(t *testing.T) {
	f := validFixture(t)
	f.Statuses[0].Instructions = "./statuses/none.md"
	f.Statuses[1].Instructions = "./statuses/none.md"
	n := 0
	for _, e := range Validate(f) {
		if e.Code == "file_missing" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("file_missing count = %d", n)
	}
}

func TestPools(t *testing.T) {
	f := validFixture(t)
	f.Statuses = append(f.Statuses, Status{ID: "z", Owner: Owner{Kind: OwnerPool, Pool: "alpha"}},
		Status{ID: "y", Owner: Owner{Kind: OwnerPool, Pool: "developers"}})
	if got, want := f.Pools(), []string{"alpha", "developers"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Pools = %v, want %v", got, want)
	}
}

func TestFiles(t *testing.T) {
	f := validFixture(t)
	f.Statuses[1].Instructions = "./statuses/plan.md"
	f.Statuses[0].Transitions[0].Checks = append(f.Statuses[0].Transitions[0].Checks,
		Check{Script: "./a/../scripts/check.sh"}, Check{Script: "/etc/passwd"})
	want := []string{"scripts/check.sh", "scripts/source.sh", "scripts/watch.sh", "statuses/plan.md"}
	if got := f.Files(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Files = %v, want %v", got, want)
	}
}
