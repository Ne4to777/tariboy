---
title: Building, testing & development
description: Canonical contributor guide for the Go control plane, React UI, Rust desktop host, documentation, and isolated end-to-end tests.
sidebar:
  label: Development
  icon: hammer
---

This is the canonical development guide for the repository. `README.md` is the
short entry point; this page owns the detailed workflow, commands, safety rules,
and documentation routing.

## Before changing code

1. Read the user request or current task as the source of truth for scope.
2. Inspect `git status` and preserve unrelated work already in the tree.
3. Read this guide and the topical documents from
   [What to read for each task](#what-to-read-for-each-task).
4. Locate the existing tests and the nearest working implementation pattern.
5. Identify whether the change affects generated output, persisted state,
   security boundaries, remote hosts, or a running daemon.
6. For behavior changes and bug fixes, reproduce the behavior and add a
   regression test before changing production code.

The root `AGENTS.md` turns this workflow into repository instructions for coding
agents. `CLAUDE.md` contains additional rationale for the live-daemon and
generated-artifact safety rules.

## Prerequisites

The control plane requires **Go 1.26**. Use the repository lockfiles for Node
and npm dependencies.

Install only the toolchains needed for the area you are changing:

- **Go control plane:** Go 1.26, Make, a C toolchain where required by the host.
- **Fast checks:** ripgrep (`rg`) for the packaging secret-scan contract in
  `make backend-check` and `make check`. Install with `apt-get install ripgrep`
  on Ubuntu or `brew install ripgrep` on macOS; the PR workflow installs it.
- **React UI:** Node.js and npm.
- **Desktop native host:** Rust (minimum package version 1.77), Cargo, the Tauri
  prerequisites, and platform build tools.
- **macOS packaging:** Apple Silicon macOS 12+, Xcode command-line tools,
  `codesign`, `hdiutil`, and the other checks reported by `make desktop-mac`.
- **Agent smoke suites:** `tmux` is checked by both `make smoke` and
  `make full-smoke`; the latter adds the interactive terminal cases.
- **Documentation:** Node.js and npm.

Coding agents must run all shell commands through a Bash login shell
(`bash -lc 'COMMAND'`) so the account's login profile and exported environment
are loaded. Keep credentials in the environment; never print their values.

Load the Rust environment before Cargo or desktop commands:

```bash
. "$HOME/.cargo/env"
```

Do not assume a GUI-launched macOS application inherits the interactive shell's
`PATH`; native runtime dependencies must be handled at the Desktop process
boundary.

## Runtime PATH contract

At daemon startup, Tariboy defines the account baseline by invoking the
configured shell as an interactive login shell with a fixed internal probe:

```text
$SHELL -ilc <fixed Tariboy PATH probe>
```

The probe is not constructed from user input. It has a five-second timeout,
bounded combined output, disconnected stdin, and a machine-readable delimiter
so startup messages cannot be mistaken for the result. The daemon accepts only
an absolute executable shell and a non-empty result without NUL bytes, then
sets the resolved `PATH` before initializing agent, plugin, or HTTP services.

Desktop reads the effective macOS account's configured shell and supplies it
only to the daemon child. Direct terminal and SSH launches use their inherited
`SHELL`. Desktop's bootstrap path can locate the daemon and common host tools,
but it is not the final runtime path. If shell selection or the bounded probe
fails, daemon startup remains available with its inherited `PATH` and emits a
stable warning; neither the inherited nor resolved path nor shell startup
output may be logged, audited, or included in support bundles.

After resolving the account baseline, a packaged daemon prepends its shared
runtime `bin` directory, which contains a `ttasks` symlink to the sibling
`tariboy-tasks` payload. This makes the bundled client available to fresh
Desktop agents without the optional CLI installation. The alias is refreshed
atomically and retained across restart handoff.

The final iteration environment preserves this precedence:

1. daemon environment, including the resolved account baseline;
2. harness-required environment;
3. explicit agent environment and resolved secrets;
4. for non-bare images, the agent bin directory prepended to the resulting
   `PATH`.

An explicit agent `PATH` therefore intentionally replaces the daemon baseline
before non-bare agent-bin prefixing. Bare images receive no agent-bin prefix
but still inherit the daemon baseline unless the agent overrides it. Harness
shell wrappers use `/bin/sh -c`, not a second login shell, so they preserve the
already-resolved environment. The runner validates the selected harness
executable against this final environment before it starts the shim.
Schema-v2 image skills add only adapter-owned launch arguments or environment
entries. They may not override `HOME`, `CODEX_HOME`, or `XDG_CONFIG_HOME`.

## Repository map

| Path | Responsibility |
| --- | --- |
| `cmd/` | Go binary entry points: daemon, CLI, shim, and store service. |
| `internal/` | Go domain logic, APIs, persistence, loop, bus, proxy, plugins, and embedded store UI. |
| `ui/` | Shared React/TypeScript source and the Desktop and store UI build targets. |
| `desktop/src-tauri/` | Rust/Tauri native host, daemon lifecycle, SSH, Keychain, tunnels, support export, and packaging resources. |
| `~/github/tariboy-store/` | Separate repository containing canonical image sources and Agent Skills. |
| `scripts/` | Isolated smoke, end-to-end, packaging, and release verification. |
| `docs/docs/` | Current product, operator, architecture, and development documentation. |
| `docs/releases/` | Release notes and acceptance evidence. |
| `docs/superpowers/specs/` | Historical and active feature design decisions. |
| `docs/superpowers/plans/` | Historical and active implementation plans. |

Current product and architecture docs take precedence over historical plans.
Read a historical spec or plan when extending the feature it describes or when
the current docs link to that decision.

## What to read for each task

Always read this page first, then read every row that matches the task. You do
not need to read unrelated subsystems.

| Task area | Required reading |
| --- | --- |
| Daemon, API, persistence, lifecycle | [Architecture overview](/docs/architecture) and [State model](/docs/architecture/state-model) |
| Iteration loop, harness execution, terminal, tmux | [Iteration loop](/docs/architecture/iteration-loop) and [Shim](/docs/architecture/shim) |
| Channels, messages, delivery, events | [Messaging](/docs/architecture/messaging) and [Channels reference](/docs/reference/channels) |
| AI proxy, usage, budgets, pricing, audit | [AI proxy and audit](/docs/architecture/ai-proxy) and [Security and controls](/docs/security-controls) |
| React UI and frontend behavior | [Web UI architecture](/docs/architecture/web-ui) |
| Desktop native host, SSH, Keychain, tunnels, packaging | [Remote hosts](/docs/remote-hosts), [Security and controls](/docs/security-controls), and this guide |
| Images and groups | [Images](/docs/images) and [Images and groups](/docs/images-and-groups) |
| Plugins and agent tools | [Plugins](/docs/plugins) and [Binaries](/docs/binaries) |
| Release, support, diagnostics | [Support](/docs/support), [Security and controls](/docs/security-controls), and `docs/internal-alpha-release-runbook.md` |

If behavior crosses rows, follow all matching routes. For example, a terminal
change in the Desktop app requires the loop/shim, Web UI, and Desktop/SSH rows.

## Build and verification matrix

Use the smallest fast entry point that covers the change; the aggregate and
heavy targets compose them:

```bash
make backend-check   # Go and smoke contracts
make frontend-check  # UI and documentation
make check           # both fast checks
make full-check      # heavy, tens of minutes, includes check
```

Run `make backend-check` for backend-only changes and `make frontend-check` for
frontend- or documentation-only changes. Run `make check` for mixed changes or
when ownership is unclear. The backend target runs `fmt-check`, `vet`, `test`,
Go unit tests and smoke contracts. The frontend target runs UI
typecheck, lint, unit tests, branding, and the documentation checks. Before
checking, `backend-check` runs `go mod download`, and `frontend-check` runs
locked `npm ci` installs under both `ui/` and `docs/`. They do not write into
`bin/` or dirty `git status`. Each step reports its result and duration as it
finishes.
Successful check-step output is suppressed; failed steps print their command
and complete diagnostics, and every run ends with a compact summary.

`make full-check` is the full set of checks that run unattended on a developer
machine, not every check in the repository. It runs `check`, then `build`, the
four core E2E scripts, `full-smoke`, the two Playwright browser suites, and one
desktop step for the host: `desktop-e2e` on Linux x86_64, `desktop` plus
`desktop-smoke` on macOS arm64, and the version and lock gates alone elsewhere.

After dependency preparation succeeds, the check steps do not stop at the
first failure. Each step is timed and recorded, and the run ends with a summary
table naming every step that failed, so one aggregate pass surfaces gofmt drift
and a UI type error together.

`frontend-check`, `check`, and `full-check` refresh `ui/node_modules` and
`docs/node_modules` from the committed lockfiles before running checks.

Three scripts stay outside `full-check` deliberately and are run by hand:
`scripts/product-alpha-e2e.sh` and `scripts/remote-provision-smoke.sh` require a
disposable SSH host (the first also requires explicit consent to destroying its
data), and `scripts/check-alpha-artifacts.sh` is a release gate that takes a
release directory as its argument. The Rust host tests are outside both targets
as well; see [Rust desktop host](#rust-desktop-host).

The individual targets remain callable on their own, so running `make vet` or
`make e2e` while iterating is still legitimate. Use the smallest relevant
check, `make check` for mixed changes, and `make full-check` when the diff
reaches e2e, packaging, or desktop behavior.

## Cleaning the working tree

`make clean` runs `git clean -dfx`. It permanently removes every ignored and
non-ignored untracked file and directory in the repository, including build
output, caches, dependency directories, and other local-only files. It leaves
tracked files and tracked edits unchanged. Git's single-force protection also
leaves nested repositories and worktrees intact.

Preview the exact paths before deleting them when the working tree may contain
local files you still need:

```bash
git clean -ndx
```

### Go control plane and plugins

`make backend-check` covers this subsystem with `fmt-check`, `vet`, and `test`.
To run one of them from the repository root while iterating:

```bash
make vet
make test
make fmt-check
```

Those targets enumerate the module's packages and exclude `node_modules`; a Go
fixture embedded in an npm dependency is not Tariboy source and must not extend
the Go quality gate.

`make fmt` is not a check: it rewrites files. Only `fmt-check` is part of
`backend-check`; run `make fmt` yourself when it reports drift.

`make build` produces the core binaries. It is not part of `check`, which
writes nothing into `bin/`, and it is the first build step of `full-check`.
Optional external plugins have their own build under `plugins/`. Canonical
image sources, Agent Skills, and their static prompts are maintained in the
separate `tariboy-store` repository rather than embedded in the binaries.
`desktop-binaries` compiles the local and remote daemon payloads without
building an agent image.
`make install` installs the resulting commands under `$HOME/.local/bin`; it is
not required for normal unit-test work.

The Tasks mode E2E inside `backend-check` builds its five required binaries in
temporary directories, so the check never regenerates workspace `bin/`.

`make server-install` builds all six real server binaries, stages and verifies
their checksums, publishes a versioned release under
`$HOME/.local/lib/tariboy/<version>`, and transactionally switches the matching
`$HOME/.local/bin` links. `ttasks` is an additional alias for
`tariboy-tasks`. It does not start or restart the daemon.

Configurable Native Tasks workflow changes normally need focused tests under
`internal/tasks`, the operator route/OpenAPI registry tests under
`internal/commands`, and the agent command contracts in the separate
`tariboy-store` repository's `make check`.
Changes to queue-trigger or observation delivery also exercise bus/daemon
recovery; Compose changes exercise `internal/compose`. The isolated product
path is `scripts/workflow-e2e.sh` and is included by `make full-check`. Keep the
current behavior documented in [Configurable task workflows](/docs/task-workflows).

### Image skill bridge development

Image skill changes span `internal/agentskills`, `internal/imagefile`,
`internal/image`, `internal/imageportable`, `internal/agentdir`,
`internal/harness`, and `internal/loop`. Keep unit fixtures self-contained and
use fake harness executables for deterministic version and capability probes.
The adapter contract version in `internal/harness` is part of the bridge cache
key; bump it whenever generated layout or launch semantics become incompatible.
Keep the pinned Claude Code minimum covered by fixtures, and use
effective-config capability probing for OpenCode instead of guessing a version.
Codex uses no image-skill version probe; cover its escaped, deterministic,
8,000-Unicode-character prompt catalog and both batch/TUI delivery paths.

Every daemon or agent smoke must use isolated `TARIBOY_BASE_DIR` and
`TARIBOY_RUNTIME_DIR` values and an isolated or disabled listener. Focused
real-harness checks are opt-in because they execute locally installed harness
binaries; the Codex check also makes a model request. Cached bridges are
rebuildable agent runtime data and are not
part of runnable image exports or the support-bundle allowlist: exports carry
the canonical image skill tree, and support bundles keep their existing narrow
diagnostic scope.

Run the deterministic fake-harness contract with:

```bash
make smoke-image-skills-contract
```

To add a real-harness check, select one harness or all installed harnesses:

```bash
TARIBOY_IMAGE_SKILLS_REAL_HARNESS=opencode make smoke-image-skills-contract
TARIBOY_IMAGE_SKILLS_REAL_HARNESS=all make smoke-image-skills-contract
```

Claude Code's plugin details and OpenCode's effective config are checked
without a model request. OpenCode also prints a manual `/skills` check. Codex
runs an isolated model-backed iteration, requires it to read the fixture's
absolute `SKILL.md` path, and verifies a proof phrase from the skill body. That
result is reported as model-backed usage, never as native inventory. The smoke
requires `TARIBOY_IMAGE_SKILLS_CODEX_AUTH_JSON` to name an explicit test
credential, copies only that file into the smoke root, and never reads, copies,
or mutates the operator's Codex profile. Set the optional
`TARIBOY_IMAGE_SKILLS_CODEX_CONFIG_TOML` only when that test credential needs a
dedicated config.

### React UI

`frontend-check` and its aggregate entry points install the locked dependencies
before checking. To install them directly while iterating:

```bash
cd ui
npm ci
```

`make check` then covers `npx tsc -b`, `npm run lint`, `npm test`, and
`npm run branding:check`. `make full-check` adds `npm run test:tasks-browser`
and `npm run test:images-browser`, and builds the desktop bundle through its
desktop step. Run any of them directly under `ui/` while iterating:

```bash
cd ui
npm test
npm run lint
npm run build:desktop
npm run test:tasks-browser
npm run test:images-browser
npm run build:store
```

For interactive visual checks of React-only changes, start the production UI
and its real API together:

```bash
make ui-dev
```

The target prints the browser URL, builds and starts a daemon with temporary
isolated data and runtime directories on `127.0.0.1:4176`, and serves the UI on
`127.0.0.1:4175`. Press Ctrl-C to stop both processes and remove the temporary
daemon state. Use the native Desktop gates only when the change depends on the
Tauri host, WebView behavior, packaging, or generated Desktop assets.

Use the build target affected by the change during iteration. Run both when
shared UI code is consumed by both applications. `npm run build:store` is a
build, not a check: it regenerates the committed `internal/storeui/dist` bundle
and is not part of either entry point. Tasks changes also run
`test:tasks-browser`: it builds the current working-tree daemon, starts it with
temporary isolated state, and exercises real Tasks API, WebSocket updates,
`PATCH` saves, and pointer drag-and-drop through a test-only Vite fixture.
Images workspace layout changes also run `test:images-browser`: it starts a
test-only Vite fixture with fake image data and never connects to or starts a
daemon.

### Rust desktop host

Neither entry point runs the Rust tests or clippy — `full-check` compiles this
crate through its desktop step, but does not test it. Run them yourself when
changing `desktop/src-tauri`:

```bash
. "$HOME/.cargo/env"
cd desktop/src-tauri
cargo test
cargo clippy --all-targets -- -D warnings
```

On Linux, install the native Tauri prerequisites plus user-local toolchains:

```bash
# Go 1.26 must precede an older distribution Go in PATH.
export PATH="$HOME/.local/toolchains/go1.26.5/bin:$PATH"

. "$HOME/.cargo/env"
rustup default stable
cargo install tauri-cli --version "^2" --locked
cargo install tauri-driver --locked
```

The OS must provide `libwebkit2gtk-4.1-dev`, `WebKitWebDriver` (Ubuntu package
`webkit2gtk-driver`), and a usable display. Headless machines also need Xvfb
(Ubuntu packages `xvfb` and `x11-xkb-utils`). Override discovery with
`TAURI_DRIVER_BIN`, `WEBKIT_WEBDRIVER_BIN`, or `TARIBOY_XVFB_BIN` when those
executables are installed in user-local paths; include their library directory
in `LD_LIBRARY_PATH` when they were extracted from packages rather than
installed system-wide.

`make desktop-e2e-build` checks all three up front through
`make desktop-e2e-tools-check`, so a missing one fails once — naming the tool,
how to install it and which variable overrides it — before the Go, SPA and
Tauri builds start, instead of repeating itself once per spec after them. The
check honours the same override variables as the fixture, and only demands
Xvfb when `DISPLAY` is empty. `make desktop PLATFORM=linux` does not depend on
the WebDriver toolchain.

Build the production SPA and bundled payloads into a WebDriver-ready,
non-packaged Linux debug executable, then run every real Desktop E2E spec:

```bash
make desktop-e2e-build
make desktop-e2e
```

The Desktop suite runs independent specs in two workers by default. Each worker
still starts a real Tauri/WebKit stack and receives separate owner-only daemon,
runtime, app-data, display, and driver-port state. Set
`DESKTOP_E2E_WORKERS=1` for a constrained host; do not raise the default without
measuring the resulting WebKit and daemon resource use.

The executable is
`desktop/src-tauri/target/debug/tariboy-desktop`. No `.deb` or `.AppImage`
is produced. Run one spec while iterating with:

```bash
make desktop-e2e DESKTOP_E2E_ARGS="tests/desktop/cold-start.pw.ts"
```

Specs live under `ui/tests/desktop/` and import the shared fixture from
`fixture.ts`. Playwright owns suite organization, assertions, timeouts, and
failure artifacts; its small typed W3C client sends commands through
`tauri-driver` to the real Tauri WebKit WebView. Add future product scenarios as
new `*.pw.ts` specs and reuse the fixture instead of launching another driver
inside a test.

Each fixture run creates fresh owner-only base, runtime, and Desktop app-data
directories, rejects the live `127.0.0.1:9990` listener, chooses free driver
ports, and stops its isolated daemon before removing temporary state. It never
stops or reconnects the user's live daemon. If `DISPLAY` is unset, the fixture
starts its own Xvfb and terminates it during cleanup. Failed setup includes
bounded Xvfb, driver, and daemon diagnostics in the Playwright report.

From the repository root, `make desktop` builds the Desktop UI, both platform
binary payloads, and native packages for the selected platform. `PLATFORM`
defaults to the supported native host and accepts `darwin` or `linux`; it does
not enable cross-compilation. A standalone invocation runs `npm ci` when
`ui/node_modules` is absent, using the committed lockfile before it builds the
SPA. Verification entry points already install Node packages, so their Desktop
step skips that duplicate work.

On Darwin arm64:

```bash
make desktop PLATFORM=darwin  # .app and .dmg
make desktop-smoke
make desktop-mac
```

The Darwin recipe sets `CI=true` only for `cargo tauri build`. This
selects Tauri's supported headless-safe DMG path: the disk image and
`Applications` link are still created, but Finder AppleScript is skipped.
Packaging therefore does not depend on an interactive macOS Automation grant.

On Linux x86_64:

```bash
make desktop PLATFORM=linux  # .deb and .AppImage
```

The preflight rejects unknown platform values, unsupported host architectures,
and a platform that does not match the current host before compiling payloads.

Ordinary `make desktop` packages keep Tauri updater artifacts disabled, so local
macOS and Linux builds do not need the release owner's private key. The macOS
path still creates the ad-hoc-signed app and DMG.

`make desktop-mac` is the signed release path. It requires
`TAURI_SIGNING_PRIVATE_KEY` and `TAURI_SIGNING_PRIVATE_KEY_PASSWORD`, enables the
Tauri updater archive and signature explicitly, and fails before packaging when
either variable is missing. The release owner permanently owns and backs up that
private key; its matching public key is pinned in the Desktop bundle. Never print
or commit the signing Secrets. Key rotation requires a separately planned
release because installed applications trust the embedded public key.

Tauri runs the macOS disk image through its own `bundle_dmg.sh` and reports a
failure there as a bare `failed to run bundle_dmg.sh`, without that script's
output. When bundling fails, `make desktop-mac` therefore prints
`scripts/desktop-bundle-diagnostics.sh` — host, free disk space, attached disk
images, mounted volumes, running disk image processes, and the partial bundle
output — and then reproduces the failure once with `DESKTOP_BUNDLE_VERBOSE=1`,
which passes `--verbose` to `cargo tauri build` so the bundler streams
`bundle_dmg.sh`'s own error. A build that fails twice ends with both reports in
the log; a build that succeeds only on that retry says so instead of passing
silently. The reporter never fails and names any tool it cannot run, so it can
be called on its own:

```bash
scripts/desktop-bundle-diagnostics.sh
```

Pushing an exact `vX.Y.Z` tag whose version matches both canonical declarations
runs the same target on GitHub's `macos-15` runner. The workflow requires both
signing Secrets, finds the single generated `.app.tar.gz` and `.sig` pair, and
stages them with the checked DMG, `SHA256SUMS`, `release.json`, and `latest.json`.
Before staging, the manifest gate rejects malformed packets or a signature key
identifier that differs from the public key in `tauri.conf.json`. This identifies
an accidental wrong signing Secret; cryptographic archive verification remains
the installed updater's responsibility. The workflow creates a draft GitHub
Release, uploads the complete set, and only then makes the release public, so the
latest catalog cannot reference missing assets. The first updater-enabled version
must still be installed from the DMG; only later, higher signed versions can be
installed by the updater.

The tray action **Install/Update CLI** owns the local five-file payload and
six-command-path install. It preflights and atomically switches `tariboyd`,
`tariboy`, `tariboy-tasks`, `ttasks`, `tariboy-shim`, and
`tariboy-plugin-telegram` links under `~/.local/bin`; `ttasks` points to the
real `tariboy-tasks` payload. It then restarts the bundled daemon even when the
links already target the same app path. Its filesystem and lifecycle work must
stay off the UI thread. Changes to
this flow require Rust installer tests plus the daemon restart-handoff tests;
never exercise it against the user's live daemon.

### Documentation

The documentation homepage uses `docs/pages/index.astro` for Blume's full-width
layout and page styles. Its content stays in `docs/docs/index.mdx`, rendered
through `BlumePage`, so search and agent-facing Markdown share the same source.
Other documentation pages keep the standard sidebar layout. Use site-root paths
such as `/quickstart` in Markdown and Card links; Blume adds the deployment
base to produce `/tariboy/quickstart`. Raw HTML anchors are not rewritten.

`frontend-check` and its aggregate entry points install the locked dependencies
before checking. To install them directly while iterating:

```bash
cd docs
npm ci
```

The `docs` step of `make check` then runs `npm run doctor`, `npm run build`, and
the GitHub Pages URL contract on every pass, so documentation is validated
whenever navigation, MDX, links, or contributor instructions change. Run the
build commands and contract directly when iterating on a docs-only change:

```bash
cd docs
npm run doctor
npm run build
../scripts/docs-build-contract-test.sh
```

Publish the production build to GitHub Pages with:

```bash
make docs
```

The target runs `doctor` and `build`, replaces the generated site in the root
of the orphan `docs` branch, commits it when the output changed, and pushes the
branch to `origin`. It uses a temporary Git worktree, so the source branch and
working tree stay selected and untouched. On the first publication, configure
the repository under **Settings → Pages** to use **Deploy from a branch**, then
select the `docs` branch and `/ (root)` folder. GitHub Pages serves this project
site at `https://alekzonder.github.io/tariboy/`.

After a successful Desktop release, a separate workflow job runs `make docs`
from the tagged commit and pushes the `docs` branch. Site deployment follows
separately. If documentation publication fails, rerun the failed docs job; the
already-published release remains intact.

### End-to-end suites

`make full-check` runs all four core E2E scripts and `full-smoke`. The individual
targets remain available for iterating on one suite:

```bash
make e2e
make workflow-e2e
make iteration-timeout-e2e
make group-request-deadline-e2e
make smoke
make full-smoke
```

`make smoke` covers non-interactive CLI behavior. `make full-smoke` adds
interactive `tmux` terminal cases; `full-check` runs `full-smoke` and therefore
covers both. The real harness is selected explicitly with
`TARIBOY_SMOKE_REAL_HARNESS` (`codex`, the default, or `none`). Ordinary
`make smoke` skips the optional Codex cases when its capability probe is not
available. `make full-smoke` fixes the selection to Codex and requires that
probe to pass; it neither discovers nor requires Claude. The optional
`TARIBOY_SMOKE_CODEX_BIN` setting selects a non-default Codex executable.
Inside `full-check` the scripts are invoked directly rather than
through these targets, because each target depends on `build` and would relink
the binaries once per suite.

## Test isolation

Any test daemon or agent must use its own data directory, runtime directory,
and listener configuration. Never stop, restart, reuse, or point test tooling at
the user's live daemon, `~/.tariboy`, `~/.tariboyd`, or
`127.0.0.1:9990`.

For an ad-hoc daemon:

```bash
test_base="$(mktemp -d)"
test_runtime="$(mktemp -d)"
TARIBOY_BASE_DIR="$test_base" \
TARIBOY_RUNTIME_DIR="$test_runtime" \
./bin/tariboyd --http-addr ""
```

Remove the temporary directories only after the isolated process is confirmed
stopped. The repository smoke and end-to-end scripts already enforce their own
isolation.

:::danger
A second daemon using the live base directory shares the SQLite database and
loop managers. It can double-execute iterations, reconcile the same agents, and
reap live sessions. Isolation is a data-safety requirement.
:::

## Generated artifacts

The two UI targets have different Git contracts:

- `desktop/branding/white-pixel-t-source.svg` is the repository-owned icon
  source. `ui/scripts/generate-desktop-branding.mjs` validates its native
  transparency and generates the Tauri PNG icons and `icon.icns`. Run
  `npm run branding:check` from `ui/`; do not hand-edit generated branding.
- `desktop/dist` and `desktop/src-tauri/resources/bin/` are ignored build
  output. `make desktop` and `make desktop-e2e-build` rebuild the Desktop UI and
  platform payloads; do not stage these directories. Rust target output and
  `ui/test-results-desktop/` are also local generated artifacts.
- `internal/storeui/dist` is embedded into `tariboy-store` and committed.
  After a store UI or relevant shared UI change, run `make store-ui` in an
  isolated worktree at the target commit and commit only the intended generated
  store bundle with its source change.
`tariboyd` is API/WebSocket only. It does not embed a Desktop UI, and there is
no `internal/webui` tree.

Do not hand-edit generated output. Rebuild it through the owning target and
inspect the resulting diff for unrelated source or secret material.

## Change workflow

1. **Understand:** read the task, this guide, and topical docs; inspect current
   code, tests, and recent changes.
2. **Design or diagnose:** agree on behavior before feature work; establish root
   cause before bug fixes.
3. **Protect behavior:** add a focused failing test for code changes, or define
   an executable validation for documentation and scripts.
4. **Implement narrowly:** preserve unrelated work and avoid opportunistic
   refactors.
5. **Synchronize docs:** update operator, architecture, reference, and
   contributor docs whenever behavior, commands, prerequisites, or boundaries
   change.
6. **Verify:** run every relevant row from the verification matrix plus
   `git diff --check`.
7. **Review:** inspect the complete diff for correctness, security, generated
   files, and scope before committing.
8. **Commit:** use a focused message and report what was verified.

## Versioning

Ordinary tasks, including code-changing tasks, do not bump the version. Move
the canonical version only when cutting a release, by running
`scripts/set-version.sh NEW_VERSION`, the single supported mechanism.
Hand-editing a pinned version file is not supported. This release-time policy
prevents version-pin merge conflicts that previously forced implementation
chains to run one at a time.

Choose the release version from everything landed since the previous release:

- use a patch bump for bug fixes, refactoring, performance work, test-only code
  changes, build tooling, and other changes that add no user-facing capability;
- use a minor bump for new commands, APIs, UI capabilities, or other
  user-facing functionality;
- while the product remains on `0.x`, also use a minor bump for an intentional
  incompatible behavior change unless the user specifies another release
  decision;
- use the highest-impact category when a release includes several changes.

`internal/version.Version` is the canonical full version.
`scripts/set-version.sh` writes `scripts/release-version.txt`, a second
declaration of that value, at the same time and no other mechanism writes it.
A disagreement proves the supported mechanism was bypassed and fails the
release gate. After `make build`, verify that `./bin/tariboy version` and
`./bin/tariboy --version` print the exact canonical value; do not use
`make install` merely to put a build on `PATH`.

## Definition of done

A change is complete when:

- the requested behavior and scope are implemented;
- focused regression tests and the affected subsystem suites pass;
- formatting, lint, vet, or Clippy passes for each changed language;
- daemon and agent tests used isolated state;
- generated artifacts match their documented Git contract;
- current documentation describes the resulting behavior and commands;
- release cuts use exactly one appropriate patch or minor version bump through
  `scripts/set-version.sh`;
- every new documentation link resolves and the docs site builds;
- `git diff --check` passes;
- the final diff contains no unrelated edits, credentials, or temporary files;
- Important and Critical review findings are resolved.
