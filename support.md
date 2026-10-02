---
title: Support
description: Diagnose alpha issues, export safe diagnostics, roll back, and uninstall.
sidebar:
  label: Support
  icon: life-buoy
---

## First checks

1. Open **Settings → General** and note app/daemon version and local status.
2. For a remote problem, inspect the host phase and last structured error.
3. Confirm the SSH alias still works in Terminal.
4. Check free disk, writable `~/.local`, `flock`, `python3`, `tmux`, and the
   selected harness on the remote host.
5. For a daemon older than `0.10.1`, finish active work before updating it.

The app and bundled daemons must report `0.71.2`. Resolve a local mismatch with
the menu-bar **Install/Update CLI** action and a remote mismatch with the host
Update action; do not copy or repoint individual binaries.

The first local update from a daemon older than `0.10.1` is intentionally
fail-closed if active work exists or Desktop cannot check for it. Legacy
daemons never persisted the proxy address and token leases needed for a safe
restart. Let the reported agents or iterations finish (or stop them), confirm
the daemon is healthy, and run **Install/Update CLI** again. No links are
changed before this check passes. Once `0.10.1` or newer is running, live shims
survive subsequent updates and are adopted after the brief proxy outage.
At `0.14.1` and newer, an adopted interactive session remains attachable and
controllable throughout that handoff. A persistent `4404` for an agent that
still reports `running` is not an expected restart state; export a support
bundle rather than restarting the agent or killing its surviving tmux session.

## Desktop updates

The first updater-enabled version must be installed from its verified DMG.
Later higher stable versions can be checked, downloaded, signature-verified,
and installed from **App settings**. Downloading never installs automatically.
Only a package verified against the public key pinned in the app becomes ready;
the package stays in memory until it is installed or the app fully exits.

Installation needs a writable installed application location. It cannot update
an app running from the read-only mounted DMG and may ask for system
authorization. A download or verification failure can be retried from the
start. An installation failure keeps the verified package ready for another
install attempt and never restarts the app.

If installation or the following launch fails, quit Tariboy and reinstall the
previously verified DMG. The updater does not promise transactional rollback or
recovery when the new application cannot start.

If an interactive harness exits, its tmux PTY reaches EOF and Desktop changes
the terminal to **Session not running or not interactive** without repeatedly
reattaching. Immediately after Start, Desktop allows five seconds for the shim
socket to appear. A persistent `terminal attach` category in `logs/shim.log`
identifies attach setup rather than harness failure without exposing tmux
stderr or environment values. A `no sessions` line in xterm on an older build
is the tmux attach client racing an already-finished iteration, not a command
typed by the agent. Update the app and CLI together, then inspect the iteration
result and `logs/shim.log`.

## Export diagnostics

Open **Settings → General → Support bundle**, select exactly one local, SSH, or
HTTPS host, and read the disclosure. The save dialog proposes
`tariboy-support-<host>-<timestamp>.zip`, so the archive remains attributable
after it is shared.

By default the export contains only Desktop metadata, the selected host ID and
label, bounded health/platform/prerequisite state, and allowlisted lifecycle
diagnostics from that host's daemon. No other registered host is included. If a
host is unreachable, or still runs a daemon without the diagnostics endpoint,
Desktop still creates a partial ZIP with a stable collection error.

The **Include agent data (sensitive)** checkbox is off by default. Enabling it
adds complete redacted `result.json`, `logs/shim.log`,
`logs/harness.stdout.log`, and `logs/harness.stderr.log` files for the newest
10 iterations of every agent on the selected host. The collector never silently
truncates one of those selected files: agent data above 128 MiB is rejected,
and Desktop bounds a daemon response at 160 MiB.

The collector never includes `PROMPT.md`, model/proxy transcripts,
`audit.jsonl`, secrets, environment configuration or values (including
`PATH`), `CONTEXT.md`, workdirs, configured cwd contents, image/plugin state,
SSH aliases/configuration, provisioning replies, or user files, and it does
not open agent environment configuration. The transient tmux exit-status file
is also outside the allowlist. Harness output is still arbitrary,
sensitive text; automatic redaction cannot prove it contains no private
business data. Inspect the ZIP before sending it.

No upload is automatic. Send the inspected bundle only to the release owner
through an approved internal channel.
Repository owner and alpha incident lead: GitHub user
[`alekzonder`](https://github.com/alekzonder).

## Roll back the desktop

1. Disable Autopilot for affected agents.
2. Quit Tariboy from its menu-bar item.
3. Move the current app out of Applications without deleting its support data.
4. Reinstall the previously verified DMG.
5. Relaunch, choose **Install/Update CLI** to repoint all six local links and
   restart the local daemon, then update remote hosts to the version bundled by
   that app.

Local link switching is preflighted and transactional. Remote activation also
attempts to restore the previous release if verification/restart fails. Do not
manually repoint only one of the six local or remote managed symlinks.

## Uninstall

Verify the app identifier and managed CLI link before removing either. This
sequence stops the verified bundled daemon CLI first and refuses unrelated
paths:

```bash
set -eu
app=/Applications/Tariboy.app
identifier=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$app/Contents/Info.plist")
test "$identifier" = app.tariboy.desktop
bundle_bin=$app/Contents/Resources/bin/darwin-arm64
for managed in tariboyd:tariboyd tariboy:tariboy tariboy-tasks:tariboy-tasks ttasks:tariboy-tasks tariboy-shim:tariboy-shim tariboy-plugin-telegram:tariboy-plugin-telegram; do
  name=${managed%%:*}
  source=${managed#*:}
  link="$HOME/.local/bin/$name"
  expected="$bundle_bin/$source"
  test -x "$expected"
  if test -L "$link"; then
    target=$(readlink "$link")
    test "$target" = "$expected" || {
      echo "refusing to remove unrelated link: $link -> $target" >&2
      exit 1
    }
  elif test -e "$link"; then
    echo "refusing to remove non-link: $link" >&2
    exit 1
  fi
done
"$bundle_bin/tariboy" daemon stop || true
for name in tariboyd tariboy tariboy-tasks ttasks tariboy-shim tariboy-plugin-telegram; do
  link="$HOME/.local/bin/$name"
  test ! -L "$link" || rm -- "$link"
done
rm -rf -- "$app"
```

Agent data intentionally remains.

Deleting `~/.tariboy`, `~/.tariboyd`, or remote equivalents is destructive
and removes durable state/logs. Back up needed data and perform deletion
separately.

## Known alpha constraints

- Apple Silicon macOS desktop only.
- Automatic SSH installation supports Linux x86_64 only.
- Ad-hoc signing requires a one-app Gatekeeper confirmation.
- Desktop updates require a writable installed app; Developer ID signing and
  notarization are not yet available.
- No built-in product analytics; optional OTLP is off by default and requires
  explicit operator configuration.
- Exact `vX.Y.Z` tags publish checked artifacts to GitHub Releases; the tag is
  created only after the release checks and two-person review.
