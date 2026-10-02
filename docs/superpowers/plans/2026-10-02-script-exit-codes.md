# Script Exit Codes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the quiet exit code of durable scripts a fixed protocol constant, `111`, instead of a per-definition setting, without breaking schedules and agent skills that still use the old setting.

**Architecture:** `internal/script` owns two protocol constants and one pure function that wraps a legacy command so its old quiet code becomes `111`. The store stops reading a per-definition quiet code: a recurring run is quiet exactly when it exits `111`. The deprecated `quiet_exit` request parameter and a one-time migration both route through the same wrapping, so old definitions and old skills keep working. The supervisor exports the constant to every run.

**Tech Stack:** Go 1.26 (`internal/script`, `internal/store`, `internal/loop`, `internal/commands`, `internal/agentapi`), SQLite migrations, React/TypeScript with Vitest (`ui/`), MDX documentation (`docs/docs/`).

**Spec:** `docs/superpowers/specs/2026-10-02-workflow-images-design.md`, section "Phases", item 1, and section "Script protocol", "Exit codes". Source task: IMPROVE-3khm.

## Global Constraints

- The quiet exit code is `111`. The reserved reject exit code is `112`. Neither is configurable.
- The environment variable name is `TARIBOY_QUIET_EXIT`.
- Quiet applies to recurring (`every`) scripts only. A one-shot run that exits `111` publishes its result.
- The `quiet_exit` request parameter stays accepted for one release as a deprecated alias. It is never stored and never returned.
- The `scripts.quiet_exit` column stays in the schema and is always `NULL` after the migration. Do not rebuild the `scripts` table.
- Run commands directly from the worktree root, without the `bash -lc` wrapper that `AGENTS.md` asks for: the customer waived it for this work on 2026-10-02, and the worktree session rejects `git` inside it.
- Work only in the worktree `.claude/worktrees/workflow-images-design`.
- Never run tests against the live `~/.tariboy`, `~/.tariboyd`, or `127.0.0.1:9990`. The tests in this plan use temporary databases and need no daemon.
- Do not bump the version and do not edit `CHANGELOG.md`; both happen at release time.
- Commit messages carry no co-authorship or attribution trailer.
- Do not edit `docs/docs/images/agent-skills.mdx`: it describes the Store skill, which still passes `--quiet-exit 2` until phase 7 of the spec.

## Review Focus

- A legacy command containing single quotes, several lines, a heredoc, or a trailing backslash must behave exactly as before once wrapped. Pinned in Task 1.
- A legacy definition whose stored quiet code is `0` must turn success into quiet and pass every other code through unchanged. Pinned in Task 1.
- An old skill passing `quiet_exit: 2` to a script that already exits `111` must stay quiet. Pinned in Task 1 (code `111` passes through the wrapper) and Task 2.
- A one-shot script that exits `111` must still publish its result, or a failing one-shot command would vanish silently. Pinned in Task 2.
- An agent whose own environment defines `TARIBOY_QUIET_EXIT` must not change the protocol value its scripts see. Pinned in Task 4.

## File Structure

| File | Responsibility |
| --- | --- |
| `internal/script/protocol.go` (new) | Protocol constants, the run environment entry, and the legacy command wrapper. |
| `internal/script/protocol_test.go` (new) | Wrapper text and real-shell behavior. |
| `internal/script/model.go` | Definition without a quiet code; deprecated input field. |
| `internal/script/store.go` | Fixed quiet rule; wrapping of deprecated input. |
| `internal/store/migrations/0052_script_quiet_exit_constant.sql` (new) | Rewrite of existing definitions. |
| `internal/loop/manager.go` | Export of the constant to each run. |
| `internal/commands/scripts.go`, `internal/agentapi/agentapi.go` | Views without `quiet_exit`; deprecated parameter help. |
| `ui/src/pages/AgentScripts.tsx`, `ui/src/lib/api.ts` | Scripts tab without the quiet exit field. |
| `docs/docs/plugins/built-in/scripts.mdx`, `docs/docs/binaries/agent-tools.mdx`, `docs/docs/reference/commands.md`, `docs/docs/reference/channels.md` | Current behavior. |

---

### Task 1: Protocol constants and the legacy command wrapper

**Files:**
- Create: `internal/script/protocol.go`
- Test: `internal/script/protocol_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `const QuietExit = 111`
  - `const RejectExit = 112`
  - `const QuietExitEnv = "TARIBOY_QUIET_EXIT"`
  - `func ProtocolEnv() []string` — returns `[]string{"TARIBOY_QUIET_EXIT=111"}`
  - `func LegacyQuietCommand(command string, code int) string`

- [ ] **Step 1: Write the failing test**

Create `internal/script/protocol_test.go`:

```go
package script

import (
	"errors"
	"os/exec"
	"reflect"
	"testing"
)

func TestProtocolConstants(t *testing.T) {
	if QuietExit != 111 || RejectExit != 112 {
		t.Fatalf("QuietExit=%d RejectExit=%d, want 111 and 112", QuietExit, RejectExit)
	}
	if got, want := ProtocolEnv(), []string{"TARIBOY_QUIET_EXIT=111"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ProtocolEnv()=%#v, want %#v", got, want)
	}
}

func TestLegacyQuietCommandText(t *testing.T) {
	got := LegacyQuietCommand("poll --state 'a b'", 2)
	want := "sh -c 'poll --state '\\''a b'\\'''\n" +
		"__tariboy_rc=$?\n" +
		"[ \"$__tariboy_rc\" -eq 2 ] && exit 111\n" +
		"exit \"$__tariboy_rc\""
	if got != want {
		t.Fatalf("wrapped command:\n%s\nwant:\n%s", got, want)
	}
}

func shellExit(t *testing.T, command string) int {
	t.Helper()
	err := exec.Command("sh", "-c", command).Run()
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("run %q: %v", command, err)
	}
	return exit.ExitCode()
}

func TestLegacyQuietCommandMapsOnlyTheLegacyCode(t *testing.T) {
	cases := []struct {
		name    string
		command string
		code    int
		want    int
	}{
		{"legacy quiet code becomes 111", "exit 2", 2, 111},
		{"success passes through", "true", 2, 0},
		{"other failure passes through", "exit 3", 2, 3},
		{"already 111 stays 111", "exit 111", 2, 111},
		{"legacy code zero turns success into 111", "true", 0, 111},
		{"legacy code zero leaves failure alone", "exit 4", 0, 4},
		{"single quotes", "test 'a b' = 'a b' && exit 2", 2, 111},
		{"several lines", "x=1\ny=1\ntest \"$x\" = \"$y\" && exit 2", 2, 111},
		{"heredoc", "cat <<'EOF' >/dev/null\nit's a line\nEOF\nexit 2", 2, 111},
		{"trailing backslash", "true \\", 2, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shellExit(t, LegacyQuietCommand(tc.command, tc.code)); got != tc.want {
				t.Fatalf("wrapped %q exits %d, want %d", tc.command, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/script/ -run "TestProtocolConstants|TestLegacyQuietCommand" -count=1`

Expected: FAIL to compile with `undefined: QuietExit`, `undefined: ProtocolEnv`, and `undefined: LegacyQuietCommand`.

- [ ] **Step 3: Write the implementation**

Create `internal/script/protocol.go`:

```go
package script

import (
	"strconv"
	"strings"
)

// Exit codes with a fixed meaning in the script protocol. Both lie in 100–113,
// a range that neither the shell, sysexits, nor Go's flag package uses, so a
// broken command cannot report either of them by accident.
const (
	// QuietExit means "nothing changed": a recurring run records its result,
	// publishes nothing, and keeps its schedule.
	QuietExit = 111
	// RejectExit is reserved for workflow checks: the script ran and its
	// condition does not hold. An agent script that exits with it has failed
	// like any other nonzero exit.
	RejectExit = 112
)

// QuietExitEnv names the variable that carries QuietExit into every run, so a
// script writes `exit "$TARIBOY_QUIET_EXIT"` instead of a magic number.
const QuietExitEnv = "TARIBOY_QUIET_EXIT"

// ProtocolEnv returns the environment entries every script run receives.
// Append it after the agent's own environment so the protocol value wins.
func ProtocolEnv() []string {
	return []string{QuietExitEnv + "=" + strconv.Itoa(QuietExit)}
}

// LegacyQuietCommand wraps a command written for a configurable quiet exit
// code so that code is reported as QuietExit. The command runs in a nested
// shell with its text single-quoted, which keeps quotes, newlines, heredocs,
// and a trailing backslash inside it intact. Every other exit code, including
// QuietExit itself, passes through unchanged.
//
// Migration 0052_script_quiet_exit_constant.sql builds the same text in SQL;
// change both together.
func LegacyQuietCommand(command string, code int) string {
	quoted := "'" + strings.ReplaceAll(command, "'", `'\''`) + "'"
	return "sh -c " + quoted + "\n" +
		"__tariboy_rc=$?\n" +
		`[ "$__tariboy_rc" -eq ` + strconv.Itoa(code) + " ] && exit " + strconv.Itoa(QuietExit) + "\n" +
		`exit "$__tariboy_rc"`
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/script/ -run "TestProtocolConstants|TestLegacyQuietCommand" -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/script/protocol.go internal/script/protocol_test.go
git commit -m "Add the script protocol exit codes and the legacy quiet wrapper"
```

---

### Task 2: Fixed quiet rule in the store

**Files:**
- Modify: `internal/script/model.go:29-42` and `:64-70`
- Modify: `internal/script/store.go:34-52`, `:61-90`, `:126-133`, `:300-306`
- Modify: `internal/commands/scripts.go:82-95`, `:196-207`
- Modify: `internal/agentapi/agentapi.go:1096-1107`
- Modify: `internal/commands/scripts_test.go:33`
- Modify: `internal/loop/manager_test.go:204`
- Test: `internal/script/store_contract_test.go`

**Interfaces:**
- Consumes: `script.QuietExit`, `script.LegacyQuietCommand(command string, code int) string` from Task 1.
- Produces:
  - `script.Definition` without the `QuietExit` field.
  - `script.CreateSchedule.QuietExit *int` remains as deprecated input, JSON name `quiet_exit`.
  - `Store.CreateSchedule` stores `LegacyQuietCommand(command, code)` when the deprecated code is set and is not `111`.
  - `Store.CompleteRun` treats a run as quiet only when the definition is recurring and the exit code is `111`.

- [ ] **Step 1: Write the failing tests**

In `internal/script/store_contract_test.go`, replace the whole function `TestCreateScheduleValidatesIntervalAndQuietExit` with:

```go
func TestCreateScheduleValidatesIntervalAndQuietExit(t *testing.T) {
	st := newContractStore(t, time.Now())
	for _, input := range []CreateSchedule{
		{Name: "watch", Description: "watch", Command: "true", IntervalSeconds: 0},
		{Name: "watch", Description: "watch", Command: "true", IntervalSeconds: 1, QuietExit: intPtr(-1)},
		{Name: "watch", Description: "watch", Command: "true", IntervalSeconds: 1, QuietExit: intPtr(256)},
	} {
		if _, _, err := st.CreateSchedule("alice", input); err == nil {
			t.Fatalf("accepted invalid schedule: %#v", input)
		}
	}
}

func TestCreateScheduleWrapsOnlyADeprecatedQuietExit(t *testing.T) {
	st := newContractStore(t, time.Now())
	cases := []struct {
		name  string
		quiet *int
		want  string
	}{
		{"no quiet exit", nil, "poll"},
		{"the protocol code", intPtr(QuietExit), "poll"},
		{"a legacy code", intPtr(2), LegacyQuietCommand("poll", 2)},
		{"legacy code zero", intPtr(0), LegacyQuietCommand("poll", 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			definition, run, err := st.CreateSchedule("alice", CreateSchedule{Name: "watch", Description: "watch", Command: "poll", IntervalSeconds: 20, QuietExit: tc.quiet})
			if err != nil || definition.Mode != ModeEvery || run.Status != RunPending {
				t.Fatalf("definition/run=%#v/%#v err=%v", definition, run, err)
			}
			if definition.Command != tc.want {
				t.Fatalf("returned command %q, want %q", definition.Command, tc.want)
			}
			stored, err := st.GetDefinition("alice", definition.ID)
			if err != nil || stored.Command != tc.want {
				t.Fatalf("stored command %q err=%v, want %q", stored.Command, err, tc.want)
			}
			var column *int
			if err := st.db.QueryRow(`SELECT quiet_exit FROM scripts WHERE id=?`, definition.ID).Scan(&column); err != nil || column != nil {
				t.Fatalf("quiet_exit column=%v err=%v, want NULL", column, err)
			}
		})
	}
}
```

Replace the whole function `TestCompleteRunSuppressesOnlyExplicitQuietExit` with these three functions:

```go
func TestCompleteRunSuppressesOnlyTheQuietExitCode(t *testing.T) {
	now := time.Date(2026, 8, 20, 7, 0, 0, 0, time.UTC)
	st := newContractStore(t, now)
	definition, run, err := st.CreateSchedule("alice", CreateSchedule{Name: "watch", Description: "watch", Command: "false", IntervalSeconds: 30})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := st.ClaimRun("alice", run.ID, now.Format(time.RFC3339), "/tmp/watch.log"); err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	finished := now.Add(10 * time.Second)
	if _, err := st.CompleteRun("alice", run.ID, Completion{Status: RunFailed, ExitCode: intPtr(QuietExit), FinishedAt: finished.Format(time.RFC3339), LogPath: "/tmp/watch.log"}); err != nil {
		t.Fatal(err)
	}
	var outboxCount int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM script_result_outbox WHERE run_id=?`, run.ID).Scan(&outboxCount); err != nil || outboxCount != 0 {
		t.Fatalf("quiet outbox count=%d err=%v", outboxCount, err)
	}
	got, err := st.GetDefinition("alice", definition.ID)
	if err != nil || got.State != StateActive || got.NextRunAt != finished.Add(30*time.Second).Format(time.RFC3339) {
		t.Fatalf("recurring definition=%#v err=%v", got, err)
	}
}

func TestRecurringExitTwoPublishesAndStops(t *testing.T) {
	now := time.Date(2026, 8, 20, 7, 0, 0, 0, time.UTC)
	st := newContractStore(t, now)
	definition, run, err := st.CreateSchedule("alice", CreateSchedule{Name: "watch", Description: "watch", Command: "false", IntervalSeconds: 30})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := st.ClaimRun("alice", run.ID, now.Format(time.RFC3339), "/tmp/watch.log"); err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	if _, err := st.CompleteRun("alice", run.ID, Completion{Status: RunFailed, ExitCode: intPtr(2), FinishedAt: now.Add(time.Second).Format(time.RFC3339), LogPath: "/tmp/watch.log"}); err != nil {
		t.Fatal(err)
	}
	var outboxCount int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM script_result_outbox WHERE run_id=?`, run.ID).Scan(&outboxCount); err != nil || outboxCount != 1 {
		t.Fatalf("exit 2 outbox count=%d err=%v, want 1", outboxCount, err)
	}
	got, err := st.GetDefinition("alice", definition.ID)
	if err != nil || got.State != StateCompleted || got.NextRunAt != "" {
		t.Fatalf("exit 2 left the schedule running: %#v err=%v", got, err)
	}
}

func TestOneShotQuietExitCodeStillPublishes(t *testing.T) {
	now := time.Date(2026, 8, 20, 7, 0, 0, 0, time.UTC)
	st := newContractStore(t, now)
	definition, run, err := st.CreateOnce("alice", CreateOnce{Name: "check", Description: "checks", Command: "make check"})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := st.ClaimRun("alice", run.ID, now.Format(time.RFC3339), "/tmp/check.log"); err != nil || !claimed {
		t.Fatalf("claim=%v err=%v", claimed, err)
	}
	if _, err := st.CompleteRun("alice", run.ID, Completion{Status: RunFailed, ExitCode: intPtr(QuietExit), FinishedAt: now.Add(time.Second).Format(time.RFC3339), LogPath: "/tmp/check.log"}); err != nil {
		t.Fatal(err)
	}
	var outboxCount int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM script_result_outbox WHERE run_id=?`, run.ID).Scan(&outboxCount); err != nil || outboxCount != 1 {
		t.Fatalf("one-shot exit 111 outbox count=%d err=%v, want 1", outboxCount, err)
	}
	got, err := st.GetDefinition("alice", definition.ID)
	if err != nil || got.State != StateCompleted {
		t.Fatalf("definition=%#v err=%v", got, err)
	}
}
```

In `TestQuietRecurringRunKeepsItsSchedule`, replace the `CreateSchedule` line with:

```go
	definition, first, err := st.CreateSchedule("alice", CreateSchedule{Name: "watch", Description: "watch", Command: "true", IntervalSeconds: 30})
```

and the first `CompleteRun` line with:

```go
	if _, err := st.CompleteRun("alice", first.ID, Completion{Status: RunFailed, ExitCode: intPtr(QuietExit), FinishedAt: finished.Format(time.RFC3339)}); err != nil {
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/script/ -count=1`

Expected: FAIL. `TestCreateScheduleWrapsOnlyADeprecatedQuietExit/a_legacy_code` reports the unwrapped command `"poll"`, `TestCompleteRunSuppressesOnlyTheQuietExitCode` reports `quiet outbox count=1`, and `TestQuietRecurringRunKeepsItsSchedule` reports that the quiet run stopped its schedule.

- [ ] **Step 3: Change the model**

In `internal/script/model.go`, remove the `QuietExit *int` line from `Definition`, so the struct reads:

```go
type Definition struct {
	ID              string
	Agent           string
	Name            string
	Description     string
	Command         string
	Mode            string
	IntervalSeconds int
	State           string
	CreatedAt       string
	NextRunAt       string
	LatestRun       *Run
}
```

Replace `CreateSchedule` with:

```go
type CreateSchedule struct {
	Name            string `json:"name"`
	Description     string `json:"description"`
	Command         string `json:"command"`
	IntervalSeconds int    `json:"interval_seconds"`
	// QuietExit is deprecated input kept for one release. The quiet exit code
	// is the constant QuietExit; a different value here wraps the command with
	// LegacyQuietCommand so that code is reported as QuietExit. It is never
	// stored and never returned.
	QuietExit *int `json:"quiet_exit,omitempty"`
}
```

- [ ] **Step 4: Change the store**

In `internal/script/store.go`, replace `CreateOnce`, `CreateSchedule`, and the signature and body of `create` up to the `INSERT INTO scripts` statement:

```go
func (s *Store) CreateOnce(agent string, in CreateOnce) (Definition, Run, error) {
	if err := validateCommon(in.Name, in.Description, in.Command); err != nil {
		return Definition{}, Run{}, err
	}
	return s.create(agent, in.Name, in.Description, in.Command, ModeOnce, 0)
}

func (s *Store) CreateSchedule(agent string, in CreateSchedule) (Definition, Run, error) {
	if err := validateCommon(in.Name, in.Description, in.Command); err != nil {
		return Definition{}, Run{}, err
	}
	if in.IntervalSeconds <= 0 {
		return Definition{}, Run{}, errors.New("recurring interval must be positive")
	}
	command := in.Command
	if in.QuietExit != nil {
		if *in.QuietExit < 0 || *in.QuietExit > 255 {
			return Definition{}, Run{}, errors.New("quiet exit must be between 0 and 255")
		}
		if *in.QuietExit != QuietExit {
			command = LegacyQuietCommand(command, *in.QuietExit)
		}
	}
	return s.create(agent, in.Name, in.Description, command, ModeEvery, in.IntervalSeconds)
}
```

```go
func (s *Store) create(agent, name, description, command, mode string, interval int) (Definition, Run, error) {
	now := s.clock().UTC().Format(time.RFC3339)
	tx, err := s.db.Begin()
	if err != nil {
		return Definition{}, Run{}, err
	}
	defer tx.Rollback()
	scriptID, err := nextID(tx, "scripts", "scr-"+agent+"-", agent, now)
	if err != nil {
		return Definition{}, Run{}, err
	}
	runID, err := nextID(tx, "script_runs", "srun-"+agent+"-", agent, now)
	if err != nil {
		return Definition{}, Run{}, err
	}
	definition := Definition{ID: scriptID, Agent: agent, Name: name, Description: description, Command: command, Mode: mode, IntervalSeconds: interval, State: StateActive, CreatedAt: now}
	run := Run{ID: runID, ScriptID: scriptID, Agent: agent, Status: RunPending, CreatedAt: now}
	if _, err := tx.Exec(`INSERT INTO scripts(id,agent,name,description,command,mode,interval_seconds,state,created_at,next_run_at) VALUES(?,?,?,?,?,?,?,?,?,NULL)`,
		definition.ID, definition.Agent, definition.Name, definition.Description, definition.Command, definition.Mode, nullInterval(interval), definition.State, definition.CreatedAt); err != nil {
		return Definition{}, Run{}, err
	}
```

The rest of `create` (inserting the run, committing, returning) is unchanged.

Replace `selectDefinition` and `scanDefinition`:

```go
const selectDefinition = `SELECT id,agent,name,description,command,mode,COALESCE(interval_seconds,0),state,created_at,COALESCE(next_run_at,'') FROM scripts`
```

```go
func scanDefinition(row interface{ Scan(...any) error }) (Definition, error) {
	var definition Definition
	err := row.Scan(&definition.ID, &definition.Agent, &definition.Name, &definition.Description, &definition.Command, &definition.Mode, &definition.IntervalSeconds, &definition.State, &definition.CreatedAt, &definition.NextRunAt)
	return definition, err
}
```

In `CompleteRun`, replace the line that computes `quiet` and the comment under it with:

```go
	// QuietExit is the only quiet result, and only for a recurring script: a
	// one-shot run always reports. A recurring script keeps its schedule only
	// while it stays quiet. Any published result stops it, so the agent is
	// notified once and resumes it deliberately with Rerun.
	quiet := definition.Mode == ModeEvery && completion.ExitCode != nil && *completion.ExitCode == QuietExit
```

The following `state := StateCompleted` block is unchanged.

- [ ] **Step 5: Update the callers that read the removed field**

In `internal/commands/scripts.go`, in `scriptDefinitionView`, delete:

```go
	if definition.QuietExit != nil {
		row["quiet_exit"] = *definition.QuietExit
	}
```

In the same file, in `scriptCreateArgs`, replace the `quiet_exit` argument with:

```go
			registry.Arg{Name: "quiet_exit", Flag: "quiet-exit", Type: registry.Int, Help: "deprecated: the quiet exit code is always 111; another value wraps the command so that code is reported as 111"})
```

In `internal/agentapi/agentapi.go`, in `scriptView`, delete:

```go
	if definition.QuietExit != nil {
		row["quiet_exit"] = *definition.QuietExit
	}
```

In `internal/commands/scripts_test.go`, in `fakeScriptControl.ScheduleScript`, remove `QuietExit: in.QuietExit, ` from the `script.Definition` literal, so the line reads:

```go
	definition := script.Definition{ID: "scr-2", Agent: owner, Name: in.Name, Description: in.Description, Command: in.Command, Mode: script.ModeEvery, IntervalSeconds: in.IntervalSeconds, State: script.StateActive}
```

In `internal/loop/manager_test.go`, rename `TestScriptSupervisorExplicitQuietExitTwoSchedulesWithoutResult` to `TestScriptSupervisorDeprecatedQuietExitTwoStaysQuiet`. Directly after its `awaitScriptRun` line, insert a check of the mapped exit code, so the two lines read:

```go
	r = awaitScriptRun(t, st, "worker", r.ID, func(r script.Run) bool { return r.Status == script.RunFailed })
	if r.ExitCode == nil || *r.ExitCode != script.QuietExit {
		t.Fatalf("run=%#v, want the legacy exit 2 reported as %d", r, script.QuietExit)
	}
```

Every other line of that test stays as it is: the `ScheduleScript` call with `QuietExit: &quietExit` and the command `exit 2`, the next-run check, the empty outbox, and the empty service chat. The test now proves that a schedule created through the deprecated parameter stays quiet end to end.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go build ./... && go test ./internal/script/ ./internal/commands/ ./internal/agentapi/ -count=1 && go test ./internal/loop/ -run "TestScriptSupervisor" -count=1`

Expected: PASS for all four packages.

- [ ] **Step 7: Commit**

```bash
git add internal/script/model.go internal/script/store.go internal/script/store_contract_test.go internal/commands/scripts.go internal/commands/scripts_test.go internal/agentapi/agentapi.go internal/loop/manager_test.go
git commit -m "Fix the quiet exit code of recurring scripts at 111"
```

---

### Task 3: Migrate existing definitions

**Files:**
- Create: `internal/store/migrations/0052_script_quiet_exit_constant.sql`
- Test: `internal/store/store_test.go`

**Interfaces:**
- Consumes: the wrapper text defined by `script.LegacyQuietCommand` in Task 1. The store package cannot import `internal/script` (it would be an import cycle), so the test pins the same literal text that `TestLegacyQuietCommandText` pins.
- Produces: after `store.Open`, no row of `scripts` has a non-`NULL` `quiet_exit`, and every definition that had a code other than `111` has a wrapped command.

- [ ] **Step 1: Write the failing test**

Append to `internal/store/store_test.go`:

```go
func TestScriptQuietExitMigrationWrapsLegacyDefinitions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quiet-exit.db")
	s := openBeforeMigration(t, path, "0052_script_quiet_exit_constant.sql")
	if _, err := s.DB.Exec(`INSERT INTO scripts(id, agent, name, description, command, mode, interval_seconds, quiet_exit, state, created_at, next_run_at) VALUES
		('scr-legacy', 'worker', 'legacy', 'd', 'poll --state ''a b''', 'every', 60, 2, 'active', '2026-09-01T00:00:00Z', '2026-09-01T00:01:00Z'),
		('scr-zero', 'worker', 'zero', 'd', 'poll', 'every', 60, 0, 'active', '2026-09-01T00:00:00Z', NULL),
		('scr-current', 'worker', 'current', 'd', 'poll', 'every', 60, 111, 'active', '2026-09-01T00:00:00Z', NULL),
		('scr-plain', 'worker', 'plain', 'd', 'poll', 'every', 60, NULL, 'completed', '2026-09-01T00:00:00Z', NULL),
		('scr-once', 'worker', 'once', 'd', 'make check', 'once', NULL, NULL, 'completed', '2026-09-01T00:00:00Z', NULL)`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	wrapped := func(command string, code string) string {
		return "sh -c '" + command + "'\n" +
			"__tariboy_rc=$?\n" +
			"[ \"$__tariboy_rc\" -eq " + code + " ] && exit 111\n" +
			"exit \"$__tariboy_rc\""
	}
	want := map[string]string{
		"scr-legacy":  wrapped(`poll --state '\''a b'\''`, "2"),
		"scr-zero":    wrapped("poll", "0"),
		"scr-current": "poll",
		"scr-plain":   "poll",
		"scr-once":    "make check",
	}
	for id, command := range want {
		var got string
		var quiet *int
		if err := s.DB.QueryRow(`SELECT command, quiet_exit FROM scripts WHERE id = ?`, id).Scan(&got, &quiet); err != nil {
			t.Fatal(err)
		}
		if got != command {
			t.Errorf("%s command:\n%s\nwant:\n%s", id, got, command)
		}
		if quiet != nil {
			t.Errorf("%s quiet_exit = %d, want NULL", id, *quiet)
		}
	}
	var state, next string
	if err := s.DB.QueryRow(`SELECT state, COALESCE(next_run_at, '') FROM scripts WHERE id = 'scr-legacy'`).Scan(&state, &next); err != nil {
		t.Fatal(err)
	}
	if state != "active" || next != "2026-09-01T00:01:00Z" {
		t.Fatalf("legacy schedule state=%q next_run_at=%q, want it untouched", state, next)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/store/ -run TestScriptQuietExitMigrationWrapsLegacyDefinitions -count=1`

Expected: FAIL. The `scr-legacy` command is still `poll --state 'a b'` and its `quiet_exit` is `2`.

- [ ] **Step 3: Write the migration**

Create `internal/store/migrations/0052_script_quiet_exit_constant.sql`:

```sql
-- The quiet exit code of a recurring script is now the protocol constant 111,
-- not a per-definition setting. A definition stored with another code keeps
-- working: its command is wrapped in a nested shell that reports that code as
-- 111 and passes every other exit code through. The text matches
-- script.LegacyQuietCommand; change both together.
--
-- The column stays, always NULL. Dropping it needs a rebuild of scripts, and
-- a rebuild under enforced foreign keys would cascade into script_runs.

UPDATE scripts
SET command = 'sh -c ''' || replace(command, '''', '''\''''') || '''' || char(10)
           || '__tariboy_rc=$?' || char(10)
           || '[ "$__tariboy_rc" -eq ' || quiet_exit || ' ] && exit 111' || char(10)
           || 'exit "$__tariboy_rc"'
WHERE quiet_exit IS NOT NULL AND quiet_exit <> 111;

UPDATE scripts SET quiet_exit = NULL WHERE quiet_exit IS NOT NULL;
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/store/ -count=1`

Expected: PASS, including `TestOpenMigrates` and `TestOpenIsIdempotent`.

- [ ] **Step 5: Commit**

```bash
git add internal/store/migrations/0052_script_quiet_exit_constant.sql internal/store/store_test.go
git commit -m "Wrap stored scripts that used a configurable quiet exit"
```

---

### Task 4: Export the quiet exit code to every run

**Files:**
- Modify: `internal/loop/manager.go:1873-1874`
- Test: `internal/loop/manager_test.go`

**Interfaces:**
- Consumes: `script.ProtocolEnv() []string`, `script.QuietExit` from Task 1; the fixed quiet rule from Task 2.
- Produces: every script run sees `TARIBOY_QUIET_EXIT=111`, regardless of the agent's own environment.

- [ ] **Step 1: Write the failing test**

Add to `internal/loop/manager_test.go`, after `TestScriptSupervisorDeprecatedQuietExitTwoStaysQuiet`:

```go
func TestScriptSupervisorExportsTheQuietExitCode(t *testing.T) {
	m, as, _, raw := newManager(t, &fakeRunner{})
	st := script.NewStore(raw, time.Now)
	m.cfg.Scripts, m.cfg.Bus = st, bus.New(raw, time.Now)
	// The agent's own value must not replace the protocol constant.
	if err := as.Create(agent.Agent{Name: "worker", ImageRef: "basic:latest", Cwd: t.TempDir(), Env: map[string]string{"TARIBOY_QUIET_EXIT": "5"}}); err != nil {
		t.Fatal(err)
	}
	startScriptTestSupervisor(t, m)
	definition, r, err := m.ScheduleScript("worker", script.CreateSchedule{Name: "quiet", Description: "test", Command: `exit "$TARIBOY_QUIET_EXIT"`, IntervalSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	r = awaitScriptRun(t, st, "worker", r.ID, func(r script.Run) bool { return r.Status == script.RunFailed })
	if r.ExitCode == nil || *r.ExitCode != script.QuietExit {
		t.Fatalf("run=%#v, want exit %d from $TARIBOY_QUIET_EXIT", r, script.QuietExit)
	}
	definition, err = st.GetDefinition("worker", definition.ID)
	if err != nil {
		t.Fatal(err)
	}
	if definition.State != script.StateActive || definition.NextRunAt == "" {
		t.Fatalf("quiet run stopped its schedule: %#v", definition)
	}
	var outboxCount int
	if err := raw.DB.QueryRow(`SELECT COUNT(*) FROM script_result_outbox WHERE run_id=?`, r.ID).Scan(&outboxCount); err != nil || outboxCount != 0 {
		t.Fatalf("quiet outbox count=%d err=%v", outboxCount, err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/loop/ -run TestScriptSupervisorExportsTheQuietExitCode -count=1`

Expected: FAIL with `want exit 111 from $TARIBOY_QUIET_EXIT`; the run exits `5`, the agent's own value.

- [ ] **Step 3: Write the implementation**

In `internal/loop/manager.go`, in the script run path, replace:

```go
	secrets, _ := m.cfg.Store.SecretMap(ag.Name)
	env := BuildEnv(os.Environ(), l.BinDir(), ag.Name, "", l.Sock(), false, "", "", ag.Env, secrets)
```

with:

```go
	secrets, _ := m.cfg.Store.SecretMap(ag.Name)
	env := BuildEnv(os.Environ(), l.BinDir(), ag.Name, "", l.Sock(), false, "", "", ag.Env, secrets)
	// Appended last: os/exec keeps the final value of a duplicated key, so an
	// agent's own environment cannot redefine the protocol.
	env = append(env, script.ProtocolEnv()...)
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/loop/ -run "TestScriptSupervisor" -count=1`

Expected: PASS.

- [ ] **Step 5: Run the backend checks**

Run: `make backend-check`

Expected: every step reports success in the summary table.

- [ ] **Step 6: Commit**

```bash
git add internal/loop/manager.go internal/loop/manager_test.go
git commit -m "Export TARIBOY_QUIET_EXIT to every script run"
```

---

### Task 5: Remove the quiet exit field from the Scripts tab

**Files:**
- Modify: `ui/src/pages/AgentScripts.tsx:20`, `:99-110`, `:188`, `:197`
- Modify: `ui/src/lib/api.ts:598`, `:611`
- Test: `ui/src/pages/AgentScripts.test.tsx:47-58`

**Interfaces:**
- Consumes: the daemon no longer returns `quiet_exit` on a script definition (Task 2).
- Produces: `ScriptDefinition` and `ScheduleScriptSpec` without `quiet_exit`; a schedule request body with exactly `script_name`, `description`, `command`, and `interval_seconds`.

- [ ] **Step 1: Write the failing test**

In `ui/src/pages/AgentScripts.test.tsx`, replace the test `"starts an immediate fixed-interval script with explicit quiet exit"` with:

```tsx
it("starts a fixed-interval script without a quiet exit setting", async () => {
  const calls: Array<{ path: string; method: string; body?: unknown }> = [];
  stubFetch(calls); renderPage(); await screen.findByRole("button", { name: /nightly/ });
  fireEvent.click(screen.getAllByRole("button", { name: "Schedule" })[0]);
  expect(screen.queryByLabelText("Quiet exit (optional)")).not.toBeInTheDocument();
  expect(screen.getByText("A scheduled run that exits 111 publishes nothing and keeps the schedule running.")).toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Name"), { target: { value: "watch" } });
  fireEvent.change(screen.getByLabelText("Description"), { target: { value: "watch build" } });
  fireEvent.change(screen.getByLabelText("Command"), { target: { value: "./check-build" } });
  fireEvent.change(screen.getByLabelText("Every (seconds)"), { target: { value: "30" } });
  fireEvent.click(screen.getAllByRole("button", { name: "Schedule" }).at(-1)!);
  await waitFor(() => expect(calls.some((call) => call.path === "/api/agents/alpha/scripts/schedule")).toBe(true));
  const body = calls.find((call) => call.path === "/api/agents/alpha/scripts/schedule")!.body;
  expect(body).toEqual({ script_name: "watch", description: "watch build", command: "./check-build", interval_seconds: 30 });
});
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd ui && npx vitest run src/pages/AgentScripts.test.tsx`

Expected: FAIL in the new test: the "Quiet exit (optional)" field is still in the document.

- [ ] **Step 3: Write the implementation**

In `ui/src/lib/api.ts`, delete the line `  quiet_exit?: number;` from `ScriptDefinition` and from `ScheduleScriptSpec`. `ScheduleScriptSpec` becomes:

```ts
export interface ScheduleScriptSpec extends RunOnceScriptSpec {
  interval_seconds: number;
}
```

In `ui/src/pages/AgentScripts.tsx`:

Replace the `emptyForm` line with:

```tsx
const emptyForm = { script_name: "", description: "", command: "", mode: "once" as ScriptMode, interval_seconds: "" };
```

In `submit`, delete these four lines:

```tsx
    const quiet = form.quiet_exit === "" ? undefined : Number(form.quiet_exit);
```

```tsx
    if (quiet !== undefined && (!Number.isInteger(quiet) || quiet < 0 || quiet > 255)) {
      toast.error("Quiet exit must be between 0 and 255"); return;
    }
```

and replace the `scheduleAgentScript` call with:

```tsx
      else await scheduleAgentScript(name, { ...common, interval_seconds: interval });
```

In the form, replace the fragment that renders both schedule fields:

```tsx
        {form.mode === "every" && <><Field label="Every (seconds)"><Input required min="1" type="number" value={form.interval_seconds} onChange={(event) => setForm({ ...form, interval_seconds: event.target.value })} /></Field><Field label="Quiet exit (optional)"><Input min="0" max="255" type="number" value={form.quiet_exit} onChange={(event) => setForm({ ...form, quiet_exit: event.target.value })} /></Field></>}
```

with:

```tsx
        {form.mode === "every" && <><Field label="Every (seconds)"><Input required min="1" type="number" value={form.interval_seconds} onChange={(event) => setForm({ ...form, interval_seconds: event.target.value })} /></Field><p className="self-end text-xs text-muted-foreground">A scheduled run that exits 111 publishes nothing and keeps the schedule running.</p></>}
```

In the scripts table row, delete this expression from the Mode cell:

```tsx
{definition.quiet_exit !== undefined && <div className="text-xs text-muted-foreground">quiet exit {definition.quiet_exit}</div>}
```

so the cell reads:

```tsx
<td className="p-2">{definition.mode === "every" ? `Every ${definition.interval_seconds}s` : "Once"}</td>
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd ui && npx vitest run src/pages/AgentScripts.test.tsx && npx tsc -b && npm run lint`

Expected: all tests in the file pass; the type check and lint report no errors.

- [ ] **Step 5: Commit**

```bash
git add ui/src/pages/AgentScripts.tsx ui/src/pages/AgentScripts.test.tsx ui/src/lib/api.ts
git commit -m "Remove the quiet exit field from the Scripts tab"
```

---

### Task 6: Documentation

**Files:**
- Modify: `docs/docs/plugins/built-in/scripts.mdx:31-36`, `:61-64`, `:95-113`
- Modify: `docs/docs/binaries/agent-tools.mdx:92`, `:102-111`
- Modify: `docs/docs/reference/commands.md:183`
- Modify: `docs/docs/reference/channels.md:362`

**Interfaces:**
- Consumes: the behavior delivered by Tasks 1–5.
- Produces: product documentation that describes the fixed code, the variable, and the deprecated flag.

- [ ] **Step 1: Update `docs/docs/plugins/built-in/scripts.mdx`**

Replace the paragraph that starts "A recurring script keeps running only while it stays quiet." with:

```mdx
A recurring script keeps running only while it stays quiet. As soon as a run
publishes a `script.result` message, the definition stops: its state becomes
`completed`, no next run is scheduled, and the agent is notified once instead of
repeatedly. Resume it deliberately with `rerun` after handling that message. A
run stays quiet by exiting with the quiet exit code `111` (see
[Results and exit codes](#results-and-exit-codes)).
```

In the Desktop steps, replace step 1 with:

```mdx
1. Choose **Run once** for one queued command, or **Schedule** for a recurring
   command. Enter a name, description, and command. A scheduled script also
   needs a positive interval in seconds.
```

Replace everything from the line "By default:" through the paragraph that ends "quiet by default." with:

````mdx
Exit codes:

- exit `0` records `succeeded` and publishes a result;
- exit `111` from a recurring script is the quiet exit code: the run is stored
  with its exit code and log, but creates no message, does not wake the agent,
  and leaves the schedule running;
- every other nonzero exit, including `2`, records `failed` and publishes a
  result;
- timeout, cancellation, and daemon-restart interruption have distinct
  statuses without invented exit codes.

Every published result stops a recurring definition until it is resumed. A
one-shot run that exits `111` is an ordinary failure and publishes its result.

The quiet exit code is fixed. Each run receives it as `TARIBOY_QUIET_EXIT`, so a
script reports "nothing changed" without a magic number:

```bash
scripts/scripts.sh schedule poll --every 60 -- ./bin/poll-queue
```

where `bin/poll-queue` is:

```sh
#!/bin/sh
new_items=$(queue-client count) || exit 1
[ "$new_items" -eq 0 ] && exit "$TARIBOY_QUIET_EXIT"
echo "$new_items new items"
```

`111` was chosen because nothing else uses it. Code `2`, the earlier
convention, is also what a shell reports for a usage error and what `grep`,
`diff`, and Go's `flag` package report for their own failures, so a broken
command would have stayed quiet forever. Exit `112` is reserved by the same
protocol and is an ordinary failure for agent scripts.

The former `--quiet-exit CODE` flag is deprecated and accepted for one more
release. A code other than `111` wraps the stored command in a nested `sh -c`
that reports that code as `111`; the definition then shows the wrapped command.
Schedules created before this change were rewritten the same way when the
daemon was upgraded.
````

- [ ] **Step 2: Update `docs/docs/binaries/agent-tools.mdx`**

Replace the line:

```text
scripts/scripts.sh schedule poll --every 60 --quiet-exit 2 -- ./bin/poll-queue
```

with:

```text
scripts/scripts.sh schedule poll --every 60 -- ./bin/poll-queue
```

Replace the two paragraphs that start "Each run's combined stdout and stderr" and "Only an explicit recurring" with:

```mdx
Each run's combined stdout and stderr is kept in its own agent scripts log. Exit
`0` records success; every other exit, including `2`, records failure. Both
deliver `script.result` so the next iteration can act. The message carries
script/run IDs, name, mode, status, optional exit code, and the absolute
`log_path`, without embedding stdout or stderr. Read that file when the result
details are needed.

The one exception is the quiet exit code `111`, available to the script as
`TARIBOY_QUIET_EXIT`: a recurring run that exits with it publishes nothing and
keeps its schedule. The run and log remain visible. Recurring runs start once
immediately, wait the configured delay after completion, and never overlap. The
`--quiet-exit CODE` flag is deprecated; see
[scripts](/docs/plugins/built-in/scripts#results-and-exit-codes).
```

- [ ] **Step 3: Update the reference pages**

In `docs/docs/reference/commands.md`, replace the row:

```md
| `scripts/scripts.sh schedule NAME --every SECONDS [--quiet-exit CODE] -- COMMAND` | Run now and repeat after each completion |
```

with:

```md
| `scripts/scripts.sh schedule NAME --every SECONDS -- COMMAND` | Run now and repeat after each completion; a run that exits `111` (`TARIBOY_QUIET_EXIT`) stays quiet |
```

In `docs/docs/reference/channels.md`, replace the sentence:

```md
the message. A quiet run publishes nothing and keeps the schedule running. An
```

with:

```md
the message. A quiet run — one that exits `111` — publishes nothing and keeps
the schedule running. An
```

- [ ] **Step 4: Verify the documentation and the UI**

Run: `make frontend-check`

Expected: every step, including `docs`, reports success in the summary table.

- [ ] **Step 5: Verify the whole change**

Run: `make check`

Expected: every step reports success.

Run: `git diff --check main...HEAD`

Expected: no output.

Run: `rg -n "quiet-exit|quiet_exit" docs/docs ui/src internal --glob "!**/dist/**"`

Expected: matches only in `internal/script/model.go` and `internal/script/store.go` (deprecated input), `internal/script/*_test.go`, `internal/commands/scripts.go` (deprecated argument), `internal/store/migrations/0034_script_runs_and_outbox.sql`, `internal/store/migrations/0052_script_quiet_exit_constant.sql`, `internal/store/store_test.go`, `docs/docs/plugins/built-in/scripts.mdx` and `docs/docs/binaries/agent-tools.mdx` (deprecation notes), and `docs/docs/images/agent-skills.mdx` (left for phase 7).

- [ ] **Step 6: Commit**

```bash
git add docs/docs/plugins/built-in/scripts.mdx docs/docs/binaries/agent-tools.mdx docs/docs/reference/commands.md docs/docs/reference/channels.md
git commit -m "Document the fixed quiet exit code for scripts"
```

---

## Follow-up outside this plan

- `tariboy-store`: switch every skill script and every `--quiet-exit 2` call to `TARIBOY_QUIET_EXIT`, then update `docs/docs/images/agent-skills.mdx`. This is phase 7 of the spec and the separate task noted in IMPROVE-3khm.
- After the Store skills are updated and released: remove the deprecated `quiet_exit` request parameter and the `--quiet-exit` argument.
