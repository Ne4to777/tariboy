# Tariboy

**The control plane for autonomous coding agents.**

Define reusable images, run coding agents interactively, and let Autopilot
carry the work forward. Bring agents together when the job needs a team —
locally or on remote hosts, from one desktop app.

**Image → Agent → Interactive → Autopilot → Team**

[Get started](https://alekzonder.github.io/tariboy/quickstart) ·
[Documentation](https://alekzonder.github.io/tariboy/) ·
[Development](https://alekzonder.github.io/tariboy/development)

> **Alpha:** Tariboy is under heavy development. APIs, workflows,
> and configuration may change without notice. The alpha onboarding path is
> macOS 12+ on Apple Silicon, with remote Linux x86_64 hosts over SSH.

## From one agent to a team

### 1. Image — define how an agent works

Package plugins, Agent Skills, and an ordered prompt template into a reusable
image. Build from a directory containing `Tariboyfile.yaml`, inspect exactly
what the agent will receive, and export runnable images to another host.

Register the official Store and build `official/basic`, or build a
role-specific image. Harness, model, and effort are configured on the agent.

[Explore images →](https://alekzonder.github.io/tariboy/images)

### 2. Agent — give the image a place to work

Create a named agent from an image. Choose its host, working directory,
harness, model, and runtime settings. Use Claude Code, Codex, or OpenCode
installed on that host, and keep each agent independently configured.

[Create your first agent →](https://alekzonder.github.io/tariboy/quickstart#4-create-an-agent)

### 3. Interactive — work with the agent directly

Open Console to guide the agent in a live session. Provide instructions and
follow the work as it runs. Interactive and Autopilot are independent controls.

For an ordinary terminal session without image instructions, choose
`bare:latest`. It does not support Autopilot.

[Start an interactive session →](https://alekzonder.github.io/tariboy/quickstart#5-use-console)

### 4. Autopilot — let work continue

Run bounded iterations on a timer or in response to durable messages. Inspect
triggers, outcomes, usage, and cost; set budgets and deadlines; pause new
iterations or kill current work when needed.

Autopilot is managed by the host daemon. Closing the desktop app does not stop
it or delete the agent's data.

[Configure Autopilot →](https://alekzonder.github.io/tariboy/autopilot)

### 5. Team — coordinate agents around shared work

Bring independently configured agents together with a lead and shared
channels. Use Native Tasks to decompose work, delegate it, track progress, and
keep questions and answers attached to the task.

Copy team configuration as compose YAML or export a portable team archive.
Transfer runnable images separately; keep their original sources for rebuilds.

[Build a team →](https://alekzonder.github.io/tariboy/images-and-groups) ·
[Organize tasks →](https://alekzonder.github.io/tariboy/tasks)

## Get started

You need an Apple Silicon Mac running macOS 12+, `Tariboy_0.75.0_aarch64.dmg` and
its checksums from the matching GitHub Release, and a supported harness
installed where your agent will run. Remote use also needs a Linux x86_64 host
reachable through your existing SSH configuration.

1. **Install Tariboy.** Verify the checksum, move the app to Applications, and
   open it. Follow the [quickstart](https://alekzonder.github.io/tariboy/quickstart) for the alpha
   signing instructions.
2. **Choose a host and image.** Keep the local host or add an SSH host, then
   register `git@github.com:alekzonder/tariboy-store.git`, refresh it, and
   build `official/basic` (or another Store image).
3. **Create an agent.** Pick its harness and model, enable Interactive, and open
   Console. Add Autopilot when you are ready, then grow into a team.

[Follow the ten-minute quickstart →](https://alekzonder.github.io/tariboy/quickstart)

### Build from source

The control plane requires Go 1.26; agent skill scripts require Python 3.
Desktop builds also need Node.js/npm, Rust, and native Tauri prerequisites.
See the [development guide](https://alekzonder.github.io/tariboy/development#prerequisites) for setup.

```bash
make build
./bin/tariboy version
./bin/tariboy --help
```

For native Desktop packages, follow the
[Desktop build instructions](https://alekzonder.github.io/tariboy/development#rust-desktop-host).
The [CLI reference](https://alekzonder.github.io/tariboy/reference/commands) covers automation and
advanced operations, including the `ttasks` Native Tasks client.

## Documentation

| I want to… | Read |
| --- | --- |
| Run my first agent | [Quickstart](https://alekzonder.github.io/tariboy/quickstart) |
| Package prompts, skills, and plugins | [Images](https://alekzonder.github.io/tariboy/images) |
| Run work autonomously | [Autopilot](https://alekzonder.github.io/tariboy/autopilot) |
| Coordinate a team | [Images & groups](https://alekzonder.github.io/tariboy/images-and-groups) |
| Delegate and track work | [Tasks](https://alekzonder.github.io/tariboy/tasks) and [Task workflows](https://alekzonder.github.io/tariboy/task-workflows) |
| Connect a remote host | [Remote hosts](https://alekzonder.github.io/tariboy/remote-hosts) |
| Extend agent capabilities | [Plugins](https://alekzonder.github.io/tariboy/plugins) |
| Understand the control plane | [Architecture](https://alekzonder.github.io/tariboy/architecture) and [Binaries](https://alekzonder.github.io/tariboy/binaries) |
| Set limits or troubleshoot | [Security & controls](https://alekzonder.github.io/tariboy/security-controls) and [Support](https://alekzonder.github.io/tariboy/support) |

## Development

Start with the canonical [contributor guide](https://alekzonder.github.io/tariboy/development) for
the repository map, prerequisites, verification matrix, and generated-artifact
rules. Coding agents must also follow [AGENTS.md](AGENTS.md).

After installing the dependencies described in the guide, run the fast checks:

```bash
make check
```

Never test against the live `~/.tariboy`, `~/.tariboyd`, or
`127.0.0.1:9990`. Use the guide's isolated test environments.

## Safety and control

Each host runs its own `tariboyd`, which owns durable state and agent execution.
HTTP listeners bind loopback; remote access uses system OpenSSH forwarding and
the user's host verification. Pause prevents new autonomous iterations;
Kill stops current work. Quitting the app leaves daemons running.

Upgrading a daemon older than `0.10.1` requires it to be idle; see the
[quickstart](https://alekzonder.github.io/tariboy/quickstart#1-install) for the upgrade path.

Read [Security & controls](https://alekzonder.github.io/tariboy/security-controls) before onboarding
others, and use the [Support guide](https://alekzonder.github.io/tariboy/support) for diagnostics.

## The name

Tariboy takes its name from the Indonesian *tukang tari*: the boy who sets the
rhythm for rowers in *Pacu Jalur* boat races. Tariboy sets the workflow rhythm
for agents and agent teams.
