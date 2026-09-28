# Tariboy internal alpha release runbook

Release: `0.71.0`

## Ownership

- Release owner and incident lead: repository owner
  [`alekzonder`](https://github.com/alekzonder).
- Updater signing owner: `alekzonder`, who retains and securely backs up the
  permanent Tauri private key. Installed applications pin its public key, so
  rotation requires a separately planned release.
- Backup reviewer: a repository collaborator other than `alekzonder`, recorded
  as the second approver on the release issue before publication.
- Support intake: the repository issue system, restricted to non-sensitive
  summaries. Support ZIPs travel only through an approved internal channel.

The Git remote establishes the repository owner. It does not establish an
organization-specific artifact host or upload protocol.

## Release prerequisites

- clean `main` worktree at the reviewed commit;
- Apple Silicon Mac with macOS/Xcode tools, Go 1.26, Rust, Tauri CLI, Node, and
  `rg`;
- system `codesign`, `hdiutil`, `lipo`, `shasum`;
- Linux x86_64 payloads are checked for presence, ELF format, embedded version,
  and `VERSION` metadata; the macOS release gate does not execute them;
- no production credentials in build environment or repository.
- confirmed secure ownership and backup of the permanent updater private key;
- GitHub Actions Secrets `TAURI_SIGNING_PRIVATE_KEY` and
  `TAURI_SIGNING_PRIVATE_KEY_PASSWORD`, configured by the release owner.

## Set the release version

1. Decide the new version: use patch or minor based on everything landed since
   the previous release.
2. Run `scripts/set-version.sh NEW_VERSION`. It rewrites the allowlisted version
   files, writes `scripts/release-version.txt`, and regenerates
   `desktop/src-tauri/Cargo.lock` through Cargo.
3. Run the release gates:

   ```bash
   . "$HOME/.cargo/env"
   make desktop-version-check
   make desktop-lock-check
   go test ./scripts/...
   ```

4. Complete the build, package, and verification steps below.

After step 2, this runbook and other documentation may name artifacts, such as
the versioned DMG, that do not exist yet. That is expected pre-existing
behavior: the packaging step creates them.

## Build

```bash
git status --short
git rev-parse HEAD
. "$HOME/.cargo/env"
make desktop-mac
```

The target requires both updater signing variables, builds both binary platforms
and the SPA, creates ad-hoc-signed app and DMG bundles plus the Tauri updater
archive and signature, verifies signatures, and stages the DMG metadata. It fails
before packaging when either signing variable is missing. Ordinary `make desktop`
keeps updater artifacts disabled and does not require the release key.

```text
dist/releases/0.71.0/
  Tariboy_0.71.0_aarch64.dmg
  tariboy_0.71.0_linux-x86_64.tar.gz
  SHA256SUMS
  release.json
```

`tariboy_<version>_linux-x86_64.tar.gz` is the server payload a Linux host
installs with `tariboy update`. It is the same install layout the desktop app
uploads over SSH: the five server binaries, `VERSION`, their `SHA256SUMS`, and
the fixed `remote-install.sh`. The artifact gate extracts it, checks those
entries, its `VERSION`, its internal checksums, and that its installer matches
the repository copy.

DMG creation intentionally uses Tauri's headless-safe mode. It skips Finder
AppleScript layout automation, so release packaging does not require an
Automation permission grant; the application, `Applications` link, signatures,
and artifact checks are unchanged.

When bundling fails, the target prints the disk image diagnostics report and
retries once with `DESKTOP_BUNDLE_VERBOSE=1`, which makes Tauri stream
`bundle_dmg.sh`'s own output. Read both reports before rerunning the tag
workflow: a leftover attached image, an exhausted disk, or a stuck
`diskimages-helper` is a runner fault, while a reproducible `bundle_dmg.sh`
error is a packaging defect that a rerun will not fix.

```bash
scripts/desktop-bundle-diagnostics.sh
```

Run the isolated desktop smoke separately before publishing; it is not part of
DMG packaging:

```bash
make desktop-smoke
```

Re-run the independent gate:

```bash
scripts/check-alpha-artifacts.sh dist/releases/0.71.0
```

The tag workflow then derives the generated `.app.tar.gz` filename, requires
its matching `.sig`, and runs:

```bash
release_version=$(cat scripts/release-version.txt)
python3 scripts/desktop-updater-manifest.py \
  "$release_version" PATH/TO/Tariboy.app.tar.gz PATH/TO/Tariboy.app.tar.gz.sig \
  "dist/releases/$release_version" desktop/src-tauri/tauri.conf.json
```

That command rejects missing, empty, malformed, mismatched, or unsafe inputs
before staging the updater pair and `latest.json`. It also rejects a signature
packet whose key identifier differs from the public key pinned in the supplied
Tauri configuration. This key-identity gate catches an accidental wrong signing
Secret; it does not cryptographically verify the archive. It appends both updater
assets to `SHA256SUMS` without dropping the DMG entries.

## Two-person review

The release owner and backup reviewer each verify:

- [ ] commit SHA in `release.json` is the reviewed `main` commit;
- [ ] artifact gate exits zero on Apple Silicon;
- [ ] `shasum -a 256 -c SHA256SUMS` exits zero;
- [ ] the release lists `tariboy_<version>_linux-x86_64.tar.gz` for `tariboy update`;
- [ ] release notes and known constraints match the artifact;
- [ ] support/rollback contacts and process are understood;
- [ ] a disposable-host product acceptance run is attached;
- [ ] no live user host or base directory was used for acceptance.

Both approvers record their GitHub usernames and timestamp on the release issue
before the release tag is pushed.

## Publication gate

After both approvals, push an exact tag whose version matches the release:

```bash
git tag -a v0.71.0 -m 'Tariboy 0.71.0'
git push origin v0.71.0
```

`.github/workflows/desktop-release.yml` validates the tag against
`internal/version/version.go` and `scripts/release-version.txt`, runs `make
desktop-mac` on `macos-15`, and creates the matching GitHub Release as a draft
with generated notes. A fixed macOS note precedes the generated notes: the build
is not notarized by Apple, and a blocked DMG is allowed through **System
Settings → Privacy & Security → Open Anyway**. Remove that note from the
workflow once releases are notarized. Repository checks are completed before the release tag is
pushed rather than repeated by the publication workflow. The workflow uploads
the versioned DMG, generated updater archive and signature, `SHA256SUMS`,
`release.json`, and `latest.json` using its job-scoped `contents: write` token,
then makes the draft public. A failed build, validation, or upload therefore
never becomes the latest public release. The signing Secrets are available only
to the build step.

After publication, a reviewer downloads all six files into a new directory,
verifies `SHA256SUMS`, confirms that `latest.json` names the versioned archive
and `darwin-aarch64`, and confirms that the `release.json` commit SHA is the
tagged, reviewed commit before invitations are sent. New users install this
first updater-enabled version from the DMG; updater installation begins only
with a later, higher signed version.

## Design-partner rollout

Invite five to eight internal partners who regularly operate coding agents and
can use a disposable Linux x86_64 host. Send:

> Tariboy is one desktop workspace for coding-agent sessions across hosts.
> This alpha starts with familiar interactive terminals, then adds reusable
> images, bounded Autopilot, usage, and audit. We are looking for a ten-minute
> observed onboarding and one weekly 20-minute interview. The app has no
> built-in product analytics; optional OTLP is off unless you configure it.
> Please report issues directly and do not use production secrets or
> irreplaceable hosts during the alpha.

Use the [demo script](internal-alpha-demo-script.md) for kickoff and the
[feedback template](internal-alpha-feedback-template.md) after each session.

## Incident and rollback

1. Release owner marks the affected GitHub Release unavailable and records the
   reason on the release issue.
2. Ask affected users to disable Autopilot; use Kill only for unsafe active work.
3. Preserve non-sensitive logs and export support bundles with user review.
4. Reinstall the previous verified DMG.
5. Update hosts so their remote daemon matches the rolled-back app.
6. Record affected version, commit, scope, and recovery in the repository issue.

Removing a host does not stop its remote daemon. If a remote daemon must stop,
run `tariboy daemon stop` on that host explicitly.

## Uninstall

Follow the ownership checks in the support guide before removing
`/Applications/Tariboy.app` or the managed `~/.local/bin/tariboy` link.
Stop daemons separately. Preserve `~/.tariboy` unless durable agent data is
intentionally being deleted.
