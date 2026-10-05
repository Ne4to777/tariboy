---
title: Ten-minute quickstart
description: Install Tariboy, connect a host, build an image, and run one agent interactively and on Autopilot.
sidebar:
  label: Quickstart
  icon: rocket
---

This path is for the `0.73.0` alpha on an Apple Silicon Mac.
Allow ten minutes after the DMG is available.

## Before you begin

- macOS 12 or newer on Apple Silicon;
- a verified `Tariboy_0.73.0_aarch64.dmg`;
- for remote use, an SSH config alias resolving to Linux x86_64;
- a writable `~/.local`, `flock`, and `python3` on that remote host;
- at least one supported harness installed where the agent will run.

## 1. Install

Verify the release checksum:

```bash
shasum -a 256 -c SHA256SUMS
```

Open the DMG and drag `Tariboy.app` to Applications. The alpha is ad-hoc
signed. If Gatekeeper says the developer cannot be verified, close that dialog,
Control-click the exact `/Applications/Tariboy.app`, choose **Open**, and
confirm once. If that action is unavailable on the installed macOS version,
open **System Settings → Privacy & Security**, find the blocked Tariboy
notice, choose **Open Anyway**, and confirm the exact named app. This temporary
alpha exception applies only to `/Applications/Tariboy.app`.

Never disable Gatekeeper globally and never run `spctl --master-disable`.

Desktop opens the workspace directly. Daemon startup and saved-host reconnect
continue in the background, with their state visible in the workspace.

This alpha includes WebView developer tools in release builds. Open
them with **Option-Command-I** on macOS or choose **Inspect Element** from the
WebView context menu.

To expose the bundled local commands on your shell `PATH`, choose
**Install/Update CLI** from the Tariboy menu-bar item. It updates six Tariboy
command links under `~/.local/bin` and restarts the local daemon. The bundle
has five real payload files; `ttasks` is the alias for `tariboy-tasks`. Running
agent shims continue; their harnesses retry through the brief proxy outage.
When upgrading a running daemon older than `0.10.1`, Desktop first verifies
that it is idle and refuses to change links or restart it while work is active.
Finish or stop that legacy work and choose **Install/Update CLI** again. Future
updates use the persisted handoff state and do not require an idle daemon.

## 2. Add a host

Open **Settings → Hosts → Add SSH host**. Enter a label and an existing SSH
config alias such as `build-box`. Tariboy runs system `ssh`, so ProxyJump,
ssh-agent, `known_hosts`, and interactive authentication work as they do in a
terminal.

Run preflight. Review the detected platform, architecture, disk, `flock`,
`python3`, `tmux`, and harnesses. Choose **Install** when the host reports Linux
x86_64 and the required install prerequisites are green. Provisioning ends by
opening a loopback-forwarding tunnel and checking daemon health.

For local-only use, keep the default Local host and continue.

Later, **Edit host → Update Tariboy** performs upload, activation, daemon
restart, and reconnect as one operation. The update click is the restart
confirmation. Running agents on daemon `0.10.1` or newer survive through shim
handoff; legacy daemons fail closed when active work cannot be preserved.

## 3. Create or select an image

Open **Images**:

- register `git@github.com:alekzonder/tariboy-store.git` as a Store named
  `official`, refresh it, then build `official/basic`; or
- choose an existing built image; or
- enter the path to an original directory containing `Tariboyfile.yaml`, set a
  required name such as `reviewer`, validate the exact plugin/prompt template,
  and build `reviewer:latest`.

An image is a reusable template. It is not an agent and does not start work.

## 4. Create an agent

Open **Agents → New agent**, select the host and image, and name the workspace.
Harness, model, and effort are shown directly in the Runtime section. Model and
effort suggestions come from Desktop presets and remain editable. They are
runtime settings, not schema-v2 image content. A successful create remembers a
custom model or effort locally for that harness; the list is not a live check
of models installed on the host.
Interactive and Autopilot are independent:

- enable **Interactive** to use Console;
- enable **Autopilot** to allow timer/message-triggered iterations.

For the first walkthrough, enable both on a non-bare image.

## 5. Use Console

Open the agent's **Console** tab and start the session. This is a real terminal
to the selected harness. Startup can take a few seconds while the shim socket
appears; Console retries during that bounded window. Leaving the tab does not
hide or stop the session, and returning attaches a new terminal client to the
same tmux session.

To try an ordinary terminal with no image instructions, create a separate agent
from `bare:latest`. Bare requires Interactive and keeps Autopilot off.

## 6. Turn on Autopilot

Open **Autopilot**. Set a conservative interval and timeout, then enable it.
The status card shows whether new autonomous iterations may start.

Autopilot reacts only when enabled. A matching message creates durable pending
delivery and nudges the loop; a timer can also trigger it.

## 7. Inspect and stop safely

Open **Activity** to see trigger, current/previous iterations, outcome, usage,
cost, messages, and audit. Then:

- disable Autopilot to pause new iterations;
- use **Kill** to terminate only current work;
- stop the interactive session independently from Autopilot.

Quitting Tariboy leaves daemons and remote data running. Removing a host
stops its local tunnel and removes local metadata, but does not delete the
remote installation or agent data.

## Next

Read [Images](/docs/images), [Remote hosts](/docs/remote-hosts),
[Autopilot](/docs/autopilot), and [Security and controls](/docs/security-controls).
