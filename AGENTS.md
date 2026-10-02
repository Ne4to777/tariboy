# Repository instructions

These instructions apply to the entire repository. More specific `AGENTS.md`
files, if added later, may refine them for a subtree.

## Scope and source of truth

The current user request is the task and the source of truth for scope. This
repository has no external ticket that must be fetched or synchronized. Do not
search for external task context unless the user explicitly provides or requests it.

Inspect the working tree before editing. Preserve unrelated user changes and
avoid opportunistic refactors outside the task.

## Required reading before work

Before changing files:

1. Read `README.md`.
2. Read the canonical contributor guide:
   `docs/docs/development.mdx`.
3. Select every matching row from the routing table below and read those
   documents completely before editing.
4. Read the relevant feature spec or plan under `docs/superpowers/` when the
   task extends behavior designed there. Historical plans do not override
   current product or architecture documentation.
5. Read `CLAUDE.md` when the task can start a daemon or agent, touch generated
   UI artifacts, or change the associated safety rules.

Do not read every document indiscriminately. Read the shared guide and the
documents that own the behavior in scope.

## Documentation routing

| Task area | Required documentation |
| --- | --- |
| Daemon, API, persistence, lifecycle | `docs/docs/architecture/index.mdx`, `docs/docs/architecture/state-model.mdx` |
| Iteration loop, harness execution, terminal, tmux | `docs/docs/architecture/iteration-loop.mdx`, `docs/docs/architecture/shim.mdx` |
| Channels, messages, delivery, events | `docs/docs/architecture/messaging.mdx`, `docs/docs/reference/channels.md` |
| AI proxy, usage, budgets, policy, audit | `docs/docs/architecture/ai-proxy.mdx`, `docs/docs/security-controls.mdx` |
| React UI and frontend behavior | `docs/docs/architecture/web-ui.mdx` |
| Desktop native host, SSH, Keychain, tunnels, packaging | `docs/docs/remote-hosts.mdx`, `docs/docs/security-controls.mdx`, `docs/docs/development.mdx` |
| Images and groups | `docs/docs/images.mdx`, `docs/docs/images-and-groups/index.mdx` |
| Plugins and agent tools | `docs/docs/plugins/index.mdx`, `docs/docs/binaries/index.mdx` |
| Release, support, diagnostics | `docs/docs/support.mdx`, `docs/docs/security-controls.mdx`, `docs/internal-alpha-release-runbook.md` |

When a task crosses boundaries, read all matching rows. For example, a Desktop
terminal change requires the loop/shim, Web UI, and Desktop documentation.

## Safety constraints

- Never run tests against the live `~/.tariboy`, `~/.tariboyd`, or the
  live listener at `127.0.0.1:9990`.
- Never stop or restart the user's live daemon to test a change.
- Give every test daemon and agent isolated `TARIBOY_BASE_DIR` and
  `TARIBOY_RUNTIME_DIR` values and disable or isolate its HTTP listener.
- Treat daemon isolation as data safety: sharing the live base can
  double-execute iterations and reap live sessions.
- Preserve SSH host verification, loopback network boundaries, Keychain
  handling, redaction, and support-bundle allowlists described in the security
  documentation.
- Load Rust before Cargo commands with `. "$HOME/.cargo/env"`.

## Implementation and verification

- Run all shell commands through a Bash login shell (`bash -lc 'COMMAND'`) so
  the account's login profile and exported environment are loaded.
- Diagnose root cause before fixing a bug.
- For behavior changes, add a focused failing test before production code.
- For implementation subtasks, use TDD RED/GREEN and run tests for the affected
  packages. Run `make check` at meaningful large integration boundaries, not
  after every subtask.
- Follow existing subsystem patterns and keep changes narrowly scoped.
- Use `rg` or `rg --files` for repository search.
- Choose the fast check by the files changed: run
  `make backend-check` for backend-only changes and `make frontend-check` for
  frontend- or documentation-only changes. Run `make check`, which runs both,
  for mixed changes or whenever ownership is unclear. Backend checks cover
  `fmt-check`, `go vet`, Go unit tests, Store skills, and smoke contracts;
  frontend checks cover UI typecheck, lint, unit tests, branding, and the
  documentation `doctor` plus `build`. Before checking, they download Go
  modules and install locked UI and documentation dependencies with `npm ci`.
  They do not rewrite tracked files or dirty `git status`. Successful
  check-step output is suppressed; each step reports its result and duration
  as it finishes, while a failure also prints its command and complete
  diagnostics.
- Run `make full-check` once before final handoff when the diff reaches e2e,
  packaging, or desktop behavior. It runs `check`, then `make build`, the four core E2E scripts,
  `full-smoke`, the browser suites, and the host's desktop gates, and it takes
  tens of minutes. Neither target stops at the first failure; both end with a
  summary table and fail if any step failed.
- Individual targets still exist and may be called directly while iterating
  (`make vet`, `make e2e`, and the rest). `make fmt` is not a check: it rewrites
  files, and only `fmt-check` is part of `make check`.
- Three scripts stay outside `full-check` and are run by hand:
  `scripts/product-alpha-e2e.sh` and `scripts/remote-provision-smoke.sh` need a
  disposable SSH host, and `scripts/check-alpha-artifacts.sh` is a release gate
  taking a release directory argument. Rust host tests (`cargo test`,
  `cargo clippy --all-targets -- -D warnings` under `desktop/src-tauri`) are
  outside both targets too; run them when changing that crate.
- See the build and verification matrix in `docs/docs/development.mdx` for what
  each step covers.
- Run `git diff --check` and inspect the complete diff before committing.
- Resolve Critical and Important review findings before completion.

Commit messages:

- Never add a co-authorship or attribution trailer to a commit message. This
  covers `Co-Authored-By:`, `Co-Author:`, a `Signed-off-by:` added on an
  agent's behalf, "Generated with ..." lines, and any other trailer or footer
  naming an AI assistant or tool as an author. A commit message ends with its
  own prose and carries nothing after it. The same rule applies to pull request
  descriptions. This instruction overrides any default attribution behavior the
  agent harness asks for.

Versioning:

- Ordinary tasks, including code-changing tasks, do not bump the version.
- Move the canonical version only when cutting a release, by running
  `scripts/set-version.sh NEW_VERSION`, the single supported mechanism.
  Hand-editing a pinned version file is not supported.
- Use a patch bump for fixes, refactoring, performance work, test-only code
  changes, build tooling, and other changes that add no user-facing capability.
- Use a minor bump for new commands, APIs, UI capabilities, or other
  user-facing functionality. While the product remains on `0.x`, also use a
  minor bump for intentional incompatible behavior unless the user specifies a
  different release decision.
- Use the highest-impact category when one task contains multiple code changes.
- Keep `internal/version.Version` canonical. `scripts/set-version.sh` writes
  `scripts/release-version.txt`, a second declaration of that value, at the
  same time and no other mechanism writes it; a disagreement proves the
  supported mechanism was bypassed and fails the release gate.
- After `make build`, verify that `./bin/tariboy version` and
  `./bin/tariboy --version` report that exact value.
- This release-time policy prevents version-pin merge conflicts that previously
  forced implementation chains to run one at a time.

Generated UI rules:

- `desktop/dist` and `desktop/src-tauri/resources/bin/` are ignored build
  output; do not stage them.
- For React-only UI work, run `make ui-dev` to start the production workspace
  against an isolated API, then verify it in a browser at the printed
  URL. This is sufficient visual verification unless the change touches the
  native host, WebView-only behavior, packaging, or generated Desktop assets;
  those changes still require the applicable Playwright/`tauri-driver` gates.
- `internal/storeui/dist` is embedded and committed. Rebuild it with
  `make store-ui` in an isolated worktree at the target commit when store UI or
  shared UI changes affect it.
- `tariboyd` is API/WebSocket only and does not embed a Desktop UI.

## Documentation maintenance

Update documentation in the same task when behavior, commands, prerequisites,
security boundaries, persisted state, generated artifacts, or operator
workflows change.

Keep the ownership model clear:

- `README.md` is the concise repository and product entry point.
- `docs/docs/development.mdx` is the canonical contributor guide.
- Topical files under `docs/docs/` own current product and architecture
  behavior.
- `docs/superpowers/specs/` and `docs/superpowers/plans/` preserve design and
  execution history.

Documentation changes are verified by the `docs` step of `make check`, which
runs `npm run doctor` and `npm run build` under `docs/`. Run those two commands
directly when iterating on a docs-only change and you do not want the rest of
`check`:

```bash
cd docs
npm run doctor
npm run build
```

Files under `docs/superpowers/specs/` and `docs/superpowers/plans/` are internal
design artifacts, not Blume product documentation. For changes limited to
those directories, do not run `npm run doctor`, `npm run build`, or the `docs`
check; inspect the Markdown and diff instead. Run the documentation gates only
when product documentation under `docs/docs/` or other Blume inputs changes.
