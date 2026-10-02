# Workflow Store Content Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship two workflow images in the Tariboy Store, `development` and `research`, move the pull request monitor into the `development` image, shrink the `tariboy-developer` instructions to tools and working method, and switch every Store skill to the fixed quiet exit code.

**Architecture:** Workflow sources live under `workflows/<name>/` next to `images/` in the `tariboy-store` repository. Each is a self-contained directory: `Workflowfile.yaml`, status instructions, and executable scripts that follow the daemon's script protocol (task snapshot in, result file and exit code out). The Store check builds every workflow image against the same temporary isolated daemon that builds agent images. Agent images keep tools and method; the process lives in the workflow image and reaches the agent through the Goal block.

**Tech Stack:** YAML manifests, Markdown instructions, Python 3 (standard library only) and POSIX shell scripts, `unittest`, Bash, GNU Make, the `tariboy` and `tariboyd` binaries built from the Tariboy branch `worktree-workflow-images-design`.

**Spec:** `docs/superpowers/specs/2026-10-02-workflow-images-design.md` (in the Tariboy repository), sections "Workflow image source", "Script protocol", "Goal", and "Phases", item 7.

**Depends on:** every earlier plan of the spec. The engine, scripts, secrets, Goal block, and pause must exist in the Tariboy branch before Task 1 runs, because the Store check needs binaries that understand workflow images.

**How to read this plan:** each task gives the files, the exact interface, the behaviors to pin with tests, and the constraints. The implementer writes the tests first, watches them fail, then implements. Names in "Produces" are binding.

## Repositories and workspaces

- Tasks 1–5 change the repository `/home/agent/github/tariboy-store`. They run in a new local worktree of it, `/home/agent/github/tariboy-store/.claude/worktrees/workflow-images`, on a new local branch `workflow-images` created from `main`. Nothing is pushed.
- Task 6 changes the Tariboy repository, in the worktree `/home/agent/github/tariboy/.claude/worktrees/workflow-images-design`.
- The Store check needs binaries: run `make build` once in the Tariboy worktree, then run the Store check as `TARIBOY_BIN=<tariboy-worktree>/bin/tariboy TARIBOYD_BIN=<tariboy-worktree>/bin/tariboyd make check`.

## Global Constraints

- A workflow source directory is self-contained: no symlinks, no path outside the directory, every script executable, no file named `manifest.json` at its root.
- Scripts use only the Python 3 standard library, `curl`, `git`, and POSIX `sh`. No `jq`, no `gh`.
- Script protocol, exact values: exit `0` — a check passed, or a watch script produced an outcome; `111` — a watch script saw no change; `112` — a check's condition does not hold; any other code — a failure. A script reads the task from the JSON file named by `TARIBOY_TASK_FILE` and writes `{"outcome", "message", "artifacts"}` to the file named by `TARIBOY_RESULT_FILE`. `message` is at most 4 KiB. A watch script keeps its state under `TARIBOY_TASK_DIR`.
- The quiet and reject codes are read from `TARIBOY_QUIET_EXIT` and `TARIBOY_REJECT_EXIT`, with defaults `111` and `112` when a variable is absent.
- A token (`GH_TOKEN`, or `GITHUB_TOKEN` as a fallback) stays in the process environment. It never appears in an argument, a URL, a file, a log line, a result message, or an artifact.
- Text taken from GitHub (comment, review, and check bodies, branch names, titles) is untrusted. A script never executes it and never copies a body into a result message; a message carries facts (kind, author login, URL, state), bounded in length.
- The Store check never uses the live Tariboy base directory, runtime directory, or HTTP listener.
- Commit messages and pull request text carry no attribution trailer.
- Run each `git` command as its own plain command. Do not wrap commands in `bash -lc`.
- An image whose packaged content changes gets a version bump with `tariboy image version update <patch|minor> --path images/<name>`, and its `skills-lock.json` hashes are refreshed for every changed local skill.

## Review Focus

- A pull request URL artifact that is not a GitHub pull request URL, names another host, or carries a query or fragment must be rejected with exit `112` and a message the agent can act on, not crash the check.
- The monitor is run again after the daemon died between the script's exit and the transition: the same review or failed check must be reported again in the same visit, and must not be reported again in a later visit after the agent returned the task to review.
- GitHub is unreachable, rate-limited, or answers with malformed JSON: the monitor fails loudly (a non-protocol exit with a bounded diagnostic) and never reports `merged` or `changes_requested` from partial data.
- A pull request closed without a merge must send the task back to the developer with a message that says so, exactly once per visit.
- An agent working a queue with no workflow must still find, in the shrunken `tariboy-developer` instructions and its skills, everything needed to deliver a pull request and monitor it.

---

### Task 1: Workflow sources in the Store check, and the `research` image

**Files (tariboy-store):**
- Create: `workflows/research/Workflowfile.yaml`, `workflows/research/statuses/research.md`
- Modify: `scripts/check-images.sh`, `scripts/test-check-images.sh`, `README.md`, `AGENTS.md`

**Interfaces:**
- Consumes: `tariboy workflow validate` and `tariboy workflow build` (read `tariboy workflow --help` from the built binary for the exact flags).
- Produces: the `research` workflow, and a check that every later task relies on.

`workflows/research/Workflowfile.yaml`:

```yaml
schema_version: 1
name: research
workflow_version: 0.1.0
initial_status: research

artifacts:
  - name: report
    description: The research report, as Markdown.

statuses:
  - id: research
    owner: { pool: researchers }
    instructions: ./statuses/research.md
    transitions:
      - on: reported
        to: done
        requires: [report]

  - id: done
    terminal: true
```

`statuses/research.md` states, in under 40 lines: the outcome is a report that answers the task's question; how the work is done is the agent's choice; questions to the customer go through the task; the report is stored with `ttasks artifacts set KEY report` from standard input and the status is left with `ttasks advance KEY --outcome reported`; a report that does not answer the question is not finished.

`scripts/check-images.sh`:
- `check_paths` also scans `workflows/` (every `Workflowfile.yaml` and script) for the forbidden checkout paths.
- After the image loop, for every `workflows/*/Workflowfile.yaml` of the temporary copy: validate its directory, then build it, against the same isolated daemon. A Store with no `workflows/` directory still passes, and a directory under `workflows/` without a manifest is skipped.
- A workflow that fails validation fails the check and prints the daemon's error list.

`scripts/test-check-images.sh` gains cases for the `--paths-only` mode: a `Workflowfile.yaml` containing a Tariboy checkout path fails; a clean one passes.

`README.md`: a "Workflow images" section — the `workflows/<name>/` layout, what belongs in a workflow image against an agent image, how to build one from a Store selector and from a path, and how to bind it to a queue. Update the "Checks" paragraph. `AGENTS.md`: add `workflows/<name>/Workflowfile.yaml` and its `statuses/` to the required reading.

- [ ] **Step 1:** Run the Store check on the untouched branch with the built binaries and record the result. If it fails before any change, stop and report `BLOCKED` with the output.
- [ ] **Step 2:** Add the failing `--paths-only` cases; run `./scripts/test-check-images.sh`; confirm they fail.
- [ ] **Step 3:** Implement the check changes and add the `research` source.
- [ ] **Step 4:** Run `TARIBOY_BIN=… TARIBOYD_BIN=… make check`; confirm it passes and that the output of a deliberately broken manifest (try it, then revert) names the error.
- [ ] **Step 5: Commit** with message `Add the research workflow image and check workflow sources`.

---

### Task 2: The `development` workflow: manifest, instructions, and checks

**Files (tariboy-store):**
- Create: `workflows/development/statuses/plan.md`, `approval.md`, `implement.md`, `complete.md`
- Create: `workflows/development/scripts/pr_lib.py`, `scripts/pr-open.py`, `scripts/merged-on-base.py`
- Create: `workflows/development/tests/test_checks.py`
- Modify: `Makefile` (run the workflow tests in `check`)

**Interfaces:**
- Consumes: the script protocol; the task snapshot `{key, queue, title, description, priority, customer, status, category, outcome, message, visit: {id, entered_at}, holders, artifacts: [{name, value, author, created_at}], workflow}`.
- Produces: `pr_lib.py`, which Task 3 imports:

```python
QUIET_EXIT: int   # from TARIBOY_QUIET_EXIT, default 111
REJECT_EXIT: int  # from TARIBOY_REJECT_EXIT, default 112

class ScriptFailure(Exception): ...          # exit 1 with a bounded diagnostic on stderr

def load_task() -> dict                      # reads TARIBOY_TASK_FILE
def artifact(task: dict, name: str) -> str | None
def parse_pull_request_url(value: str) -> tuple[str, str, int]   # owner, repo, number; ValueError otherwise
def write_result(*, outcome: str | None = None, message: str = "", artifacts: dict[str, str] | None = None) -> None
def select_token() -> str                    # GH_TOKEN, then GITHUB_TOKEN; ScriptFailure when neither
class GitHubClient:                          # the bounded, redacting curl client moved from github-pr.py
    def get(self, path: str, params: dict | None = None) -> Any
    def paginate(self, path: str, params: dict | None = None, limit: int = ...) -> list
```

`GitHubClient` is copied from `images/tariboy-developer/skills/github-pr-workflow/scripts/github-pr.py` with its bounds and redaction intact; the API root comes from `GITHUB_API_URL` (default `https://api.github.com`) so tests can point it at a local server. `parse_pull_request_url` accepts only `https://github.com/OWNER/REPO/pull/N` with the component rules of `validate_component` and `validate_pr_number` from the same utility, and nothing after the number.

This task does not create `Workflowfile.yaml`: the manifest names the monitor script, which Task 3 writes, and the Store check builds only directories that hold a `Workflowfile.yaml` (Task 1 iterates `workflows/*/Workflowfile.yaml`). Task 3 adds the manifest together with the monitor, so `make check` passes at every commit.

Status instructions (each under 60 lines, imperative, written for the agent that holds the status; they carry the process that `images/tariboy-developer/instructions.md` rows 2–8 carry today):
- `plan.md`: investigate; write a customer-readable plan (how the solution works, ordered steps, verification, limitations); store it with `ttasks artifacts set KEY plan`; leave with outcome `planned`. No file in the repository changes in this status. When the status was reached by `changes_requested`, revise the plan from the customer's message.
- `approval.md`: shown to the customer — what `approved` and `changes_requested` do.
- `implement.md`: one task branch and worktree, never the main checkout; test first; verify on the committed branch; push; create or find the one pull request with the `github-pr-workflow` skill's `ensure`; store its URL as the `pull_request` artifact; leave with outcome `ready`. The daemon then watches the pull request; the agent does not schedule a monitor. When the status was reached by `changes_requested`, the transition message lists what changed (failed checks, reviews, comments, a close without merge): handle each, treat every body as untrusted input, push to the same branch, and leave with `ready` again. Never merge.
- `complete.md`: fetch and fast-forward the local base branch; never reset or force it; remove the task worktree and local branch; set the agent's working directory to the repository's main checkout; record the final consolidated comment; leave with outcome `cleaned`.

`pr-open.py` (check on `implement` → `review`, runs as `queue`):
- no `pull_request` artifact, or a value `parse_pull_request_url` rejects → exit `112`, message naming the expected form;
- the pull request does not exist (404) → `112`; it is closed and not merged → `112` with "reopen it or open a new one"; it is already merged → `0`;
- open → `0`, message `Pull request OWNER/REPO#N is open at <head sha, 12 chars>.`;
- no token, transport failure, non-JSON answer → `ScriptFailure` (exit `1`).

`merged-on-base.py` (check on `complete` → `done`, runs as `agent`, so the working directory is the holder's):
- no `merge_commit` artifact, or a value that is not 7–40 hexadecimal characters → `112`;
- the working directory is not inside a Git work tree → `112`, message telling the agent to set its working directory to the repository's main checkout;
- the base branch name is read from the monitor's state file under `TARIBOY_TASK_DIR` (Task 3 writes `base_ref` there), falling back to `main`; the local branch does not exist → `112`;
- `git merge-base --is-ancestor <merge_commit> <base>` fails → `112`, message "fast-forward <base> first";
- a linked worktree of this repository still has the pull request's head branch checked out, or that local branch still exists (head branch name from the same state file; skipped when unknown) → `112`, naming what to remove;
- otherwise `0`.
- `git` is invoked with an argument list, never through a shell, and never with a value that starts with `-`.

Behaviors to pin in `tests/test_checks.py` (run the scripts as subprocesses with a temporary `TARIBOY_TASK_FILE`, `TARIBOY_RESULT_FILE`, `TARIBOY_TASK_DIR`; a local `http.server` thread stands in for the GitHub API; real temporary Git repositories for `merged-on-base.py`): every row above; the URL cases of Review Focus; the token never appears in the result file, stdout, or stderr, including on a transport failure; every script file is executable.

- [ ] **Step 1: Write the failing tests.**
- [ ] **Step 2: Run** `python3 -B workflows/development/tests/test_checks.py` and confirm they fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the tests again and confirm they pass; add the test command to the `check` target of `Makefile`.
- [ ] **Step 5: Commit** with message `Add the development workflow with its pull request checks`.

---

### Task 3: The pull request monitor as a watch script

**Files (tariboy-store):**
- Create: `workflows/development/Workflowfile.yaml`
- Create: `workflows/development/scripts/pr-monitor.py`, `workflows/development/tests/test_monitor.py`
- Modify: `Makefile`

`Workflowfile.yaml` is the manifest in the spec's "Workflow image source" section with these exact differences: no `env` block; script paths `./scripts/pr-open.py`, `./scripts/pr-monitor.py`, `./scripts/merged-on-base.py`; the `approval` status has `instructions: ./statuses/approval.md`; `implement` keeps `limits: { idle_iterations: 6 }`. The `tests/` directory is part of the image (the whole directory is stored); that is accepted.

**Interfaces:**
- Consumes: `pr_lib.py`; the observation and normalization code of `command_monitor` in `github-pr.py` (`normalize_pr`, `normalize_checks`, `normalize_statuses`, `normalize_body_items`, snapshot validation, `atomic_write`) — copy what the monitor needs; do not import from the agent image.
- Produces: outcomes `merged` and `changes_requested`, artifact `merge_commit`, and the state file `TARIBOY_TASK_DIR/pr-monitor.json` with at least `repo`, `number`, `base_ref`, `head_ref`, `acknowledged` (item identities), and `pending` (`{visit_id, items}`), written atomically with mode `0600`.

One run:
1. Read the task; parse the `pull_request` artifact (an invalid one is a `ScriptFailure`: the check let it through, so this is a broken state).
2. Observe the pull request completely: the pull request, check runs and commit statuses of the current head, reviews, review comments, and issue comments, each bounded as in the utility. Any incomplete or malformed observation is a `ScriptFailure`; nothing is decided from partial data.
3. Reconcile the state with the snapshot's `visit.id`: a `pending` entry recorded under a different visit was applied — move its items to `acknowledged`; a `pending` entry recorded under the same visit was not applied — its items are reported again.
4. Decide:
   - `merged: true` with a merge commit SHA → outcome `merged`, artifact `merge_commit` (the full SHA), message `Pull request OWNER/REPO#N merged as <sha12> into <base>.`;
   - else collect the items not in `acknowledged`: each failed check run or failed commit status of the current head (identity includes the head SHA), each review with state `CHANGES_REQUESTED`, each new review comment and issue comment not written by the pull request's author, and "closed without merge" (identity includes the pull request's `closed_at`). Any item → record them as `pending` under this visit, outcome `changes_requested`, with a message listing up to 20 items as `kind — author or check name — URL — state`, then `(+K more)`; never a body;
   - else exit `QUIET_EXIT`.
5. Write the state file before writing the result file and exiting `0`.

A `COMMENTED` or `APPROVED` review without comments is not an item. Checks still running are not items. The first run of the first visit has an empty `acknowledged` set, so anything already on the pull request is reported.

Behaviors to pin in `tests/test_monitor.py` (same harness as Task 2): quiet on an unchanged open pull request with passing or running checks; `merged` with the artifact; `changes_requested` for each item kind, and the message carries no body text even when the body contains shell syntax or Markdown headings; the author's own comments are ignored; the same-visit replay and the later-visit non-replay of Review Focus; a new head SHA makes a new failure of the same check name a new item; closed without merge reported once per visit; HTTP 403/500, a truncated body, and non-JSON are failures with exit `1` and leave the state file unchanged; the message stays under 4 KiB with 200 items; the token never reaches the state file, the result file, stdout, or stderr; the state file is `0600`.

- [ ] **Step 1: Write the failing tests.**
- [ ] **Step 2: Run** `python3 -B workflows/development/tests/test_monitor.py` and confirm they fail.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** both workflow test files, then `TARIBOY_BIN=… TARIBOYD_BIN=… make check`; confirm the `development` image validates and builds.
- [ ] **Step 5: Commit** with message `Move the pull request monitor into the development workflow`.

---

### Task 4: Store skills use the fixed quiet exit code

**Files (tariboy-store):**
- Modify: `skills/scripts/scripts/scripts.py`, `skills/scripts/SKILL.md`, `skills/scripts/evals/evals.json`, `skills/test_store_skills.py`
- Modify: `images/tariboy-developer/skills/github-pr-workflow/scripts/github-pr.py`, its `SKILL.md`, its `tests/test_github_pr.py`, its `evals/`
- Modify: `images/tariboy-image-creator/skills/tariboy-image-delivery/SKILL.md` and its `evals/evals.json`, `images/tariboy-image-creator/evals/evals.json`
- Modify: `scripts/test-closure-monitor.py`
- Modify: `images/*/skills-lock.json` and `images/*/Tariboyfile.yaml` (`image_version`) for every image that packages a changed skill

**Interfaces:**
- Consumes: the daemon exports `TARIBOY_QUIET_EXIT=111` to every agent script run and treats exit `111` of a recurring run as quiet; the `quiet_exit` request parameter is deprecated.
- Produces: no Store skill sends `quiet_exit`, and no Store text names `--quiet-exit`.

Changes:
- `scripts.py schedule` no longer accepts `--quiet-exit`: the flag is a usage error whose message says that a recurring command exits with `$TARIBOY_QUIET_EXIT` (`111`) to stay quiet. The request body never contains `quiet_exit`.
- `skills/scripts/SKILL.md`: the quiet rule is "a run that exits with `$TARIBOY_QUIET_EXIT` (`111`) publishes nothing and keeps the schedule"; a one-line shell example (`exit "${TARIBOY_QUIET_EXIT:-111}"`); every table row and counter that named the flag is rewritten.
- `github-pr.py monitor` returns the quiet code from `TARIBOY_QUIET_EXIT` (default `111`) for an unchanged observation, instead of `2`. Its `SKILL.md` drops `--quiet-exit 2` from the schedule command and says `111` wherever it said `2`; the skill stays the way to monitor a pull request on a task without a workflow, and gains one paragraph: on a workflow task whose status instructions say the daemon watches the pull request, do not schedule this monitor.
- `tariboy-image-delivery/SKILL.md` and `scripts/test-closure-monitor.py` (its fake Scripts launcher and its assertions on the schedule command) follow the same rule.
- Eval files: replace the flag and the code `2` in prompts and expectations; keep their meaning.
- Refresh `skills-lock.json` for each changed local skill (find the supported command with `npx skills --help`; if the hash can only be produced by reinstalling, do that in a temporary copy and copy the lock back), and bump each affected image with `tariboy image version update patch --path images/<name>`.

Behaviors to pin: `scripts.py schedule … --quiet-exit 2` exits with a usage error naming `TARIBOY_QUIET_EXIT`, and a plain schedule sends no `quiet_exit` key; `github-pr.py monitor` exits `111` on an unchanged observation, and with `TARIBOY_QUIET_EXIT=77` exits `77`; the closure-monitor contract passes with the new command; `rg -n -- '--quiet-exit|quiet_exit' .` finds only the usage-error text, its test, and the image-creator's `.agents` copies that the check ignores.

- [ ] **Step 1: Change the tests first** (`skills/test_store_skills.py`, `tests/test_github_pr.py`, `scripts/test-closure-monitor.py`); run them; confirm they fail.
- [ ] **Step 2: Implement.**
- [ ] **Step 3: Run** `TARIBOY_BIN=… TARIBOYD_BIN=… make check`; confirm it passes.
- [ ] **Step 4: Commit** with message `Use the fixed quiet exit code in every Store skill`.

---

### Task 5: The `tasks` skill and the `tariboy-developer` instructions

**Files (tariboy-store):**
- Modify: `skills/tasks/SKILL.md`, `skills/tasks/evals/` when present
- Modify: `images/tariboy-developer/instructions.md`, `images/tariboy-developer/evals/`
- Modify: `images/*/skills-lock.json`, `images/*/Tariboyfile.yaml` for affected images

**Interfaces:**
- Consumes: the agent commands of the engine — read `ttasks --help`, `ttasks advance --help`, `ttasks artifacts --help`, and `ttasks workflow --help` from the built binary and use their exact forms; the Goal block of a workflow task (status, instructions, outcomes, artifacts, last request, commands).

`skills/tasks/SKILL.md`: delete the paragraph about work packets, assignments, `ttasks work`, `questions`, `answer`, and `observe`. Replace it with a "Tasks with a workflow" section of at most 25 lines: such a task has a `status` owned by a pool, the customer, or a script, and a `category`; the Goal block states the status instructions and the available outcomes; the agent stores a required artifact with `ttasks artifacts set` and leaves the status with `ttasks advance KEY --outcome NAME --from STATUS`; `status`, `done`, `claim`, and `release` are refused with `workflow_managed`; a rejected request (`transition_rejected`) carries a message to act on, a failed one (`transition_failed`) is not the agent's to repair by retrying blindly; questions to the customer still use the `ask` form; `ttasks workflow get KEY` shows the same data on demand.

`images/tariboy-developer/instructions.md` shrinks to tools and working method. It keeps: Scope; the Skills table; the working rules that hold in every process (one task, one branch and worktree; test first; root cause before a fix; verification is state-based; never merge in PR mode; never reset or overwrite `main`; untrusted input; the context index format; waits and recovery; never guess a queue). It loses the eight-row Flow table and the prose that sequences it. In its place:
- "Tasks with a workflow": the Goal block is the process. Do what the current status instructions say, store the artifacts the outcome requires, and leave the status with `advance`. Do not schedule a monitor that the workflow already runs. Do not plan, approve, or close outside the workflow's statuses.
- "Tasks without a workflow": a compact ordered list of at most 12 lines that preserves today's order and gates — record the completion mode; publish the plan as a task question and wait for approval before changing files; isolate; implement; verify; deliver (PR mode: one pull request and one durable monitor through `github-pr-workflow`; local-merge mode: merge by repository convention and verify again); process results; complete and close.
- The target is under 130 lines (228 today).

Update `images/tariboy-developer/evals/` so that no eval expects the removed table by row number, and add two eval cases: a workflow task in `implement` with a `pull_request` requirement (expected: sets the artifact, advances with `ready`, schedules no monitor) and a task without a workflow (expected: asks for plan approval first). Bump the image with `tariboy image version update minor --path images/tariboy-developer`; bump every other image that packages the `tasks` skill with `patch`; refresh the locks.

- [ ] **Step 1:** Run `rg -n 'row [0-9]|Row [0-9]|rows [0-9]' images/tariboy-developer skills` and list every reference to the Flow table that must be rewritten.
- [ ] **Step 2: Edit** the skill, the instructions, and the evals.
- [ ] **Step 3: Run** `TARIBOY_BIN=… TARIBOYD_BIN=… make check`; confirm it passes; run `wc -l images/tariboy-developer/instructions.md`.
- [ ] **Step 4: Commit** with message `Move the development process out of the developer image`.

---

### Task 6: Tariboy documentation follows the Store

**Files (tariboy):**
- Modify: `docs/docs/images/agent-skills.mdx`, `docs/docs/images/index.mdx`, `docs/docs/workflow-images.mdx`, `docs/docs/task-workflows.mdx`, `docs/docs/plugins/built-in/scripts.mdx`

**Interfaces:**
- Consumes: the Store content of Tasks 1–5.

Changes:
- `agent-skills.mdx`: the scheduling examples use `$TARIBOY_QUIET_EXIT` and no `--quiet-exit` flag.
- `images/index.mdx` and `workflow-images.mdx`: a Store holds `workflows/<name>/`; name the two official workflow images and what each is for; show `tariboy workflow build official/development`, setting the `GH_TOKEN` queue secret, creating the `developers` pool, and binding the queue.
- `task-workflows.mdx`: a short "Official workflows" section — the `development` status graph as a table (status, owner, outcomes, gate) and the `research` graph.
- `scripts.mdx`: the deprecated `quiet_exit` request parameter stays accepted for one release after the Store skills stopped sending it; say which release removes it only if a release number is known, otherwise say "a later release".
- The deprecated `quiet_exit` parameter is NOT removed in this plan.

- [ ] **Step 1: Edit** the documents.
- [ ] **Step 2: Run** `make frontend-check`; confirm it passes.
- [ ] **Step 3: Commit** with message `Document the official workflow images`.
