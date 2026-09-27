---
title: Security and controls
description: Trust boundaries, credential handling, budgets, audit, and operator stop controls.
sidebar:
  label: Security
  icon: shield
---

The alpha is designed for named internal partners and trusted hosts. It has no
built-in product analytics. OTLP export is disabled by default and activates
only when an operator explicitly sets `OTEL_EXPORTER_OTLP_ENDPOINT` or the
persisted `otlp_endpoint` daemon setting.

## Network boundaries

- Local and remote daemon HTTP listeners bind loopback.
- Remote access uses an SSH local-forwarding tunnel.
- System OpenSSH owns host-key verification, agent use, ProxyJump, and 2FA.
- Tariboy never disables strict host-key checking.
- Non-loopback daemon TCP requires a bearer token file.

## Credentials

SSH private keys remain with OpenSSH and are never copied. Interactive prompt
replies live in memory only and are redacted from events. Manual HTTPS host
tokens live in macOS Keychain under service
`app.tariboy.desktop.host`, keyed by host ID. Host registry JSON contains no
tokens.

The AI proxy gives scoped short-lived tokens to harnesses/plugins and scrubs
upstream provider keys from plugin environments. Audit transcripts contain
model traffic and are sensitive operator data even though support bundles
exclude them.

Active iteration tokens and the stable loopback proxy address are carried across
a daemon restart in `~/.tariboy/aiproxy-handoff.json`. The base directory is
owner-only and the handoff file plus atomic temporary files use mode `0600`.
Tokens are never logged, audited, stored in SQLite, or included in support
bundles. Startup prunes leases that no longer map to a `running` iteration;
adoption revokes the remaining lease when its shim finishes.

## Runtime controls

- Interactive enablement and Autopilot enablement are independent.
- Disabling Autopilot prevents new iterations.
- Kill terminates current work without stopping unrelated agents.
- Soft and hard timeouts bound an iteration.
- Budgets constrain spend.
- Agent USD budgets have independent calendar hour/day/ISO-week/month limits;
  zero is unlimited, and an exhausted request is rejected before upstream access.
- Usage and per-iteration audit make outcomes reviewable.

Removing a host is local cleanup, not remote deletion. Quitting the app leaves
daemons running. Treat both behaviors as continuity features, not stop controls.

## Local and remote files

Local data defaults to `~/.tariboy`; runtime files default to
`~/.tariboyd`. Remote binaries live in versioned directories under
`~/.local/lib/tariboy`; managed symlinks live in `~/.local/bin`. The local
Desktop installer preflights all six command paths before changing any of them
and refuses regular files, directories, or symlinks not owned by the verified
Tariboy app bundle. The bundle has five real payload files; `ttasks` is a
managed alias whose source is `tariboy-tasks`.

Image builds resolve only the literal `$PLUGINS` root, source-relative paths,
or explicit absolute paths. Prompt paths and plugin references reject traversal
outside their roots.
Source-relative skill directories may use `../` to reference a sibling tree;
ordinary builds freeze only those explicitly declared skills alongside the
source snapshot. A source directory may itself be selected through a symlink;
symlinks inside packaged content, missing or non-regular files, and oversized
prompt files are rejected. The optional `image-creator` capability can build
from any source, skill, or prompt path readable by the daemon account; relative
source paths start at the agent's managed workdir. Grant it only to agents
trusted with that host-level read and packaging access.
Native host metadata and exported support archives use owner-only permissions.
The global `<base-dir>/global-agent-shell.sh` and per-agent
`<base-dir>/agents/<name>/agent-shell.sh` scripts are owner-only, atomically
written only after `bash -n` validation; a rejected value cannot replace the
last valid script.

User Stores clone Git sources on the selected daemon host into its own
`<base-dir>/stores/<name>/`, or use an explicitly registered local directory.
Store names and image selectors are validated; image selection rejects path
traversal and symlink escapes. Git uses the server account's existing SSH and
credential helpers without weakening host verification. Credential-bearing
URLs are rejected and subprocess output is not returned as error text.
Refresh uses fast-forward-only pull and never resets or stashes user changes.
Removing a Store preserves local source directories and built images.
Building a Store with `skills-lock.json` invokes npm's skills installer on
that host before freezing sources. For an image with `extends`, the installer
also runs once per layer with a lock, inside an owner-only temporary assembly
directory under the base directory that is removed after the build. Register and build sources you trust to
install their declared skills. Store source contents remain outside the
support-bundle allowlist.

Database backups written by nightly maintenance live in the owner-only
`<base-dir>/backups/db/` directory, and each backup file is mode `0600`. A
backup is a complete, unmasked copy of `tariboyd.db`: task text, messages,
Usage rows, and **agent secret values in plaintext**. Unlike `tariboy backup`,
nothing is masked, and a deleted or rotated secret remains in each kept backup
until rotation removes it. Treat backups like the database itself and never
copy them off the host casually. Backups are outside the support-bundle
allowlist.

## Pricing catalog boundary

The daemon downloads model prices only from LiteLLM's fixed production HTTPS
URL; proxy input, model names, and operator configuration cannot redirect that
request. The client has a ten-second timeout, does not follow redirects, reads
at most eight MiB, and validates the complete catalog before publication.

The validated source document is cached as
`$TARIBOY_BASE_DIR/model-prices-litellm.json` (normally
`~/.tariboy/model-prices-litellm.json`). It is written through an owner-only
`0600` temporary file and atomically renamed. Catalog payloads and provider
credentials never enter catalog diagnostics; the corresponding logs/events
contain only a bounded source, generation time, accepted model count, and
stable error class.

The pricing cache is not part of the support-bundle allowlist and is never
collected, even when sensitive agent data is enabled. This feature does not
change loopback listener restrictions, remote bearer tokens, SSH host
verification, Keychain handling, prompt redaction, AI transcript handling, or
any other support-bundle exclusion. See [AI proxy and
audit](/docs/architecture/ai-proxy#pricing-catalog-and-request-costs) for its
runtime lifecycle.

## Alpha signing and Gatekeeper

`0.71.0` is ad-hoc signed, not Developer ID signed or notarized. Verify
`SHA256SUMS` before opening it. If Gatekeeper blocks it, Control-click only the
named `/Applications/Tariboy.app`, choose **Open**, and confirm.
If Control-click Open is unavailable, use **System Settings → Privacy &
Security → Open Anyway** for that exact app. This is a temporary scoped
exception for named alpha partners.

Do not disable Gatekeeper globally, change system-wide `spctl` policy, or remove
quarantine recursively from Applications. Developer ID signing and notarization
remain later release work.

Desktop accepts an update only when the official Tauri updater verifies the
download against the permanent public key pinned in the installed application.
The release owner's matching private key stays in GitHub Actions Secrets and
must be securely owned and backed up before an updater-enabled release. The
fixed catalog may select only a higher stable version for the supported Apple
Silicon macOS target. The WebView cannot provide a URL, signature, path, or
archive bytes.

The verified archive remains in Rust process memory and is never sent through
IPC, written to Web Storage, or added to support bundles. Closing the window to
the tray retains it; quitting the application discards it. A failed install
retains the verified archive for retry, while an interrupted download starts
again from zero. Archive memory use is proportional to the update size.

An exact `vX.Y.Z` tag triggers the repository's GitHub Actions workflow on a
macOS runner. The workflow rejects a tag that differs from either canonical
version declaration, builds and verifies the release through `make
desktop-mac`, and publishes the DMG, signed updater archive and catalog,
`SHA256SUMS`, and `release.json` with the job-scoped `contents: write`
permission. Before staging, the workflow rejects malformed signing packets or a
signature key identifier that differs from the pinned updater public key. This
catches an accidental wrong signing Secret; cryptographic archive verification
remains the official updater's responsibility. GitHub publication does not change
the ad-hoc-signing or Gatekeeper constraints above.

The alpha deliberately enables Tauri WebView developer tools in its
release build for local diagnosis. DevTools exposes the current WebView state
to the person operating the local Desktop session; it does not relax daemon
loopback binding, bearer-token requirements, CORS, or SSH trust boundaries.
The main Desktop WebView has the narrow Tauri window-drag permission needed by
the overlay titlebar; it does not receive resize-dragging or unrestricted
window mutation permissions.

## Support-bundle boundary

Every export selects exactly one host. The default bundle includes its raw ID
and label plus bounded platform/version/health/prerequisite state and
allowlisted lifecycle/error lines. Other host records are not exported.

An explicit, unchecked sensitive-data option adds the complete redacted allowed
files for the newest 10 iterations of each agent on that host. It reads only
`result.json`, `logs/shim.log`, `logs/harness.stdout.log`, and
`logs/harness.stderr.log`; it never recursively walks an agent directory.

The collector never includes credentials or environment values (including
`PATH`), SSH aliases/configuration, `PROMPT.md`, context, transcripts, audit,
workdirs, configured cwd contents, global or agent shell scripts, image/plugin
state, provisioning replies, or user files. It does not open agent environment
configuration. Harness output can
nevertheless contain arbitrary private natural language, so inspect the ZIP
before sending it under your organization's support data policy. Export does
not upload anything automatically.

Activity audit export is a separate, explicit operator action and is not a
support bundle. It can export one iteration or all retained iterations and may
contain prompts, reasoning, commands, tool arguments and results, model
responses, and user data. The Desktop warns before downloading the ZIP; the
export remains a local download and is never uploaded automatically. This does
not change the support-bundle exclusions above.

Portable image and team archives use a versioned manifest and bounded streaming
validation. Import rejects absolute or parent paths, NULs, duplicate/colliding
paths, symlinks, hardlinks, devices, FIFOs, excessive compressed/expanded size,
file count and path length, unmanifested content, and digest mismatches before
adopting staged data. Runnable image archives contain only the runnable image
artifact and its ref identity: never original source files or source CWD. Team
archives are compose-only and contain no image build contexts, source trees, or
runnable image bytes. They also never contain agent workspaces/data,
credentials, environment values outside the explicit compose contract,
external cwd contents, or plugin state. Raw archive routes use the same
loopback, CORS, and remote bearer-token boundary as JSON API routes.
