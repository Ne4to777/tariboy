## [0.71.1] - 2026-09-28

### Changed

- All tasks hides the agent sidebar and its titlebar toggle, and the agent
  in the route no longer narrows the table or preselects the New task agent.
  The saved sidebar state is kept for the Agents view.
- Every GitHub Release starts with a note that the macOS build is not
  notarized and how to allow a blocked DMG.

### Fixed

- Closed tasks with an unread agent question stay in the Active task list
  until the question is read, so the Tasks badge always has a row to open.
- Tag releases restore a Desktop Rust cache warmed on `main`, instead of
  compiling every Rust dependency from scratch.

[0.71.1]: https://github.com/alekzonder/tariboy/compare/v0.71.0...v0.71.1

## [0.71.0] - 2026-09-27

### Added

- Simple and Expert interface modes in Application settings. Simple (the
  default) shows an agent's Chat, Tasks, Console and Configuration tabs and
  opens agents on Chat; Expert keeps the full workspace.
- An Agents | All tasks switch in the titlebar. All tasks lists every
  server's tasks in one view and moving a task to another agent moves it to
  that agent's server.
- Host workspaces: a titlebar switcher narrows the sidebar, Servers tab and
  footer counts to a workspace's hosts, and a manager creates, renames and
  deletes workspaces and moves hosts between them. Default holds every
  unassigned host, so no migration is needed.
- Schema-v2 Tariboyfiles can inherit parent images through `extends`.
- The Tasks tab shows the number of tasks with an unread agent question.
- The audit log records the cause of `harness_error` iterations (launch
  error, or exit code and stderr tail).
- tariboyd creates a default `TASK` queue on start when none exists.

### Changed

- Task Duration is measured from the first move to `in_progress`
  (`started_at`, backfilled by migration 0049) instead of creation.
- New agents default to a 30-minute AI inactivity timeout (was 5 minutes);
  existing agents keep their value.
- Start and Stop live only in the agent header.
- The chat task drawer resizes and shares its width with the Tasks sheet.
- Chat history and chat list reads use a stored, indexed message sender
  (migration 0048) and agent budget windows are summed in one indexed
  query, removing multi-second API stalls on large databases. A chat with
  no messages reports an empty last sender instead of `system`.

### Removed

- The terminal Workspace canvas at `/workspace`. Its saved sidebar width and
  hidden state carry over.
- The Store Build name and tag inputs; use `tariboy image build --name` and
  `--tag` for overrides.

### Fixed

- AI proxy usage is recorded for gzip-compressed upstream responses
  (previously counted without model, tokens or cost).
- New agent resets Interactive to the selected image's default.
- Reordering servers inside a workspace keeps hidden hosts' order.

[0.71.0]: https://github.com/alekzonder/tariboy/compare/v0.70.0...v0.71.0

## [0.70.0] - 2026-09-24

### Added

- Nightly database maintenance. The daemon backs up `tariboyd.db` with
  `VACUUM INTO`, keeps the newest backups, then deletes data older than the
  retention period (finished Native Task trees, AI proxy Usage rows outside the
  longest budget period, fully delivered and consumed messages, daemon events,
  task idempotency records) and compacts the file when enough pages are free.
  Defaults: enabled, 03:00 local, 7 backups, 90 days, compaction at 10% free
  pages. Manage it with `tariboy maintenance get|set|run`, `/api/maintenance`
  or the Backup & data retention card in Settings → General.

### Changed

- Exec moved from the Console tab to the agent header, before Start/Stop, and
  opens a dialog for the optional one-shot prompt. It shows only while the
  agent is enabled and respects an unavailable host.
- The task drawer opens immediately with a loading state instead of waiting
  for the task and its history.
- Refreshing a managed Git Store clone now fetches, hard-resets to the upstream
  branch and removes untracked files, so local modifications no longer block
  it. Local Store checkouts keep fast-forward-only pulls.

### Security

- Database backups are written to the owner-only `<base-dir>/backups/db/`
  (files mode `0600`) and are complete, unmasked copies of the database,
  including agent secret values in plaintext. They are outside the support
  bundle allowlist.

[0.70.0]: https://github.com/alekzonder/tariboy/compare/v0.69.1...v0.70.0

## [0.69.1] - 2026-09-24

### Fixed

- Deliver `script.result` and channel-less self schedules to the agent again.
  Since 0.69.0 they were published to `chat:service:<agent>` with the agent as
  author, and the bus dropped an author's own delivery there, so the message
  was stored but never woke the agent. The service chat now delivers like the
  inbox; DM, tasks and group chats still suppress an agent's own replies.
- A failed Store refresh now returns `store_refresh_failed` (HTTP 409) with the
  tail of Git's error output instead of a generic internal error, so the UI and
  CLI name the cause, such as a local change blocking the fast-forward.
- Store builds restore each tracked `skills-lock.json` after
  `npx skills experimental_install` rewrites it, including on failure, so a
  build no longer leaves the checkout dirty and blocks a later Refresh.

[0.69.1]: https://github.com/alekzonder/tariboy/compare/v0.69.0...v0.69.1

## [0.69.0] - 2026-09-23

### Added

- Chats over the existing channel bus. Every agent has three chats:
  `dm:<agent>` for the conversation, `tasks:<agent>` for task notifications
  and `service:<agent>` for its own wakes. The customer can create a chat that
  belongs to no single agent and holds several:
  `tariboy chat create ID --title TITLE --participants agent:a,agent:b`,
  `tariboy chat join ID PRINCIPAL` and `tariboy chat leave ID PRINCIPAL`
  (`POST /api/chats`, `POST|DELETE /api/chats/{chat}/participants`). Joining
  a chat subscribes an agent to it. Leaving ends membership but keeps
  deliveries the agent already holds.
- The chat list and feed come from the new `chats` and `chat_participants`
  tables (`GET /api/chats`, `GET /api/chats/{chat}`,
  `GET /api/chats/{chat}/participants`), and
  `GET /api/chats/{chat}/unanswered` lists the messages a principal has not
  answered.
- The agent Chat tab opens the agent's tasks and service chats as tabs beside
  the conversation. The sidebar row sums unread messages across all three
  chats and previews whichever chat moved last.

### Changed

- Task `task.*` notifications now publish to `chat:tasks:<agent>`, and
  `task.goal`, `script.result` and channel-less schedules to
  `chat:service:<agent>`. Delivery, wake-up and idempotency behavior is
  unchanged. Schedules created earlier keep their stored channel.
- A reply without `--reply-to` lands in the chat that owns the original
  message, and websocket message hints carry that chat's id.
- An image ref can be rebuilt through every build path: a build publishes one
  ref derived from the image name and `image_version` and moves every
  requested tag, including `latest`, onto it. The mutable/immutable
  publication split is gone. The daemon converts an existing image store once
  at startup and keeps each archive's content digest as its ref id, so pinned
  digests keep resolving.

### Fixed

- The sidebar `Update all` button now asks for confirmation and updates every
  outdated server in turn, instead of only opening one edit dialog.
- Subscription ids no longer collide when two subscriptions are created
  within the same clock tick.

[0.69.0]: https://github.com/alekzonder/tariboy/compare/v0.68.0...v0.69.0

## [0.68.0] - 2026-09-21

### Added

- Show why an agent loop halted: a pre-launch failure (an image manifest that
  does not verify, a missing image skill bridge, an unusable harness) now halts
  the loop with its cause, records one `iteration_failed` audit event, and shows
  the halt reason in the agent workspace header next to the message-queue and
  budget lines, so it is visible on every tab instead of only on Autopilot.
  Start and Restart clear it.
- Report how long a task took: the agent task table gains a `Duration` column,
  measured from creation to completion, or to now while the task is still open.

### Changed

- Rebuild the Tasks workspace around one toolbar. `All tasks`, `My tasks` and
  `Waiting for me` become toolbar chips, the queue rail becomes a
  `Queue: <value>` control listing every queue with its task count, and queue
  administration opens as a 340px right sheet whose cards scroll on their own
  with queue creation in a footer. The list takes the full width of the island.
  The queue filter is applied in the client and now survives switching between
  an Agent tab and All tasks; it is per session on purpose.
- Give the task table one set of column widths shared by its header and rows.
  `Key` is a fixed 168px holding the tree indents, the chevron and the key, with
  deep indents capped and the key truncating rather than wrapping; `Task` takes
  the remaining width; the `Queue` column is gone, since the key prefix already
  names the queue; the drag grip moves into the row padding and appears on hover.

### Fixed

- Name the queue settings sheet for assistive technology and drop the
  `aria-label` that hid the task count and the check mark from a screen reader
  on each queue menu item.

[0.68.0]: https://github.com/alekzonder/tariboy/compare/v0.67.0...v0.68.0

## [0.67.0] - 2026-09-21

### Added

- A Store can refresh and rebuild itself on a schedule: `tariboy store auto NAME --interval MINUTES --image IMAGE` (`POST /api/stores/{name}/auto`) stores a whole-minute interval and the images selected for automatic builds, and the Store detail edits the same policy. Every minute the daemon refreshes each Store whose interval has elapsed and rebuilds the selected images that report an update, publishing both `image_version` and `latest`. Automatic refresh is disabled by default (`--interval 0`), a failed refresh skips that Store builds for the cycle, a failed build does not stop the remaining selections, and `tariboy store show` reports the policy.

[0.67.0]: https://github.com/alekzonder/tariboy/compare/v0.66.2...v0.67.0

## [0.66.2] - 2026-09-18

### Fixed

- Stop a recurring script after it publishes a result, so the agent handles one actionable result instead of receiving repeated wake-ups; a quiet configured exit remains scheduled, and a completed schedule can be explicitly resumed with Exec.

[0.66.2]: https://github.com/alekzonder/tariboy/compare/v0.66.1...v0.66.2

## [0.66.1] - 2026-09-18

### Fixed

- Diagnose a failed macOS release bundling run instead of reporting a bare
  `failed to run bundle_dmg.sh`: the packager prints the disk image state — free
  space, attached images, mounted volumes, running disk image processes and the
  partial bundle output — and reproduces the failure once with verbose bundler
  output, so `bundle_dmg.sh`'s own error reaches the build log.

[0.66.1]: https://github.com/alekzonder/tariboy/compare/v0.66.0...v0.66.1

## [0.66.0] - 2026-09-18

### Added

- Turn the agent Chat tab into a real thread: a single toolbar, author-grouped messages with avatars and hover actions, Markdown with code blocks and task links, day separators and a New messages rule, read receipts, and a composer with Markdown marks, attach, preview and auto-grow. A dot on the Chat tab shows unread.

### Changed

- Read the per-agent unread mark the daemon already keeps instead of tracking unread in the tab, and anchor the New messages rule to the mark the chat was opened at, so it holds its place while the customer reads and is dismissed only by Mark read, by sending a message, or by leaving the chat.
- Accept `--exact` on `tariboy chat read` (`exact` on `POST /api/chats/{agent}/read`), the one write allowed to move the read mark backwards, which is how a chat is marked unread. Without it the mark still only moves forward.

### Fixed

- Derive the remote install and update upload deadline from the payload instead of a fixed 120 seconds: a two-minute floor plus one second per 128 KiB, capped at thirty minutes, with SSH compression enabled. A ~100 MB bundle over a slow uplink no longer fails at "Upload release" with "SSH command timed out", leaving an abandoned staging directory behind.

[0.66.0]: https://github.com/alekzonder/tariboy/compare/v0.65.0...v0.66.0

## [0.65.0] - 2026-09-18

### Added

- Update a remote host from the host itself: `tariboy update [VERSION|latest]` resolves a release, downloads `tariboy_<version>_linux-x86_64.tar.gz` and `SHA256SUMS`, verifies the archive before unpacking it, and runs the release's own `remote-install.sh`, so checksums, locking, atomic activation and rollback stay with the installer. A running daemon is restarted through the managed `tariboyd` link and must report the installed version; a stopped daemon is left stopped. The release now publishes that server archive, and the artifact gate checks its entries, VERSION, internal checksums, installer identity and binary architecture.
- Tag iterations and search them: `tariboy iteration tag add|rm|set` applies a batch of ids and tags in one transaction, `tariboy iteration ls` gains `--tag`, `--started-after` and `--started-before`, and list and inspect report a sorted tags list. The Activity tab filters by the same criteria, keeps them in the URL, shows each row's tags, and edits the selected iteration's tags. Tags live in their own table and are retired with the iteration, so an iteration row stays immutable evidence of an execution.
- Move a task tree between servers: a tree can be exported from one daemon and imported into another under the same keys, driven from the desktop task menu, which exports, imports, comments and cancels the copy left behind. The target refuses an import when it has no queue with that prefix, when a key is already in use, or when the queue runs a workflow.

### Changed

- Mint task keys as the queue prefix plus four random characters instead of a per-queue counter, so two daemons no longer mint the same keys and a task keeps its identity across hosts. Existing numeric keys are rewritten on daemon start and recorded as aliases, so a key already written into an agent context, a branch name or a comment keeps resolving, and lookup normalizes casing.
- Drop the Notifications view from the Tasks workspace. Customer questions stay visible as messages in the agent Chat and as the indicator on the task row, and opening a task marks its unread question notifications read. Agent rows keep only the unread message count, an unanswered question still lifts a row to the top, and a chat draws a New messages line above the first unseen message.

### Fixed

- Render the task panel opened from a chat message, which appeared as an empty sheet and left the Tasks tab unable to load. The dialog drops its resize-grip column when there is no grip, and the panel opens the tasks socket at the host current sequence instead of replaying the whole task event log.
- Keep trailing blank lines out of the Markdown the rich text editor emits, so pressing Enter twice at the end of a description no longer produces a code block holding `&nbsp;`. The blank lines and the caret stay where the author put them.

[0.65.0]: https://github.com/alekzonder/tariboy/compare/v0.64.0...v0.65.0

## [0.64.0] - 2026-09-17

### Added

- Open a task straight from its chat message: a task key attached to a task notification, or written in the message text, is now a link that opens the full task panel over the conversation, with its own load, tasks-socket refresh, and writes for status, priority, assignee, pull request, description, comments, and relations.
- Show the configured runtime in the agent header as one compact `harness · model · effort` chip, so the operator no longer has to open Configuration to see it. Values the selected harness does not report are omitted, and the chip disappears when none remain.

### Changed

- Order the agent workspace tabs as Chat, Tasks, Console, Autopilot, Activity, Configuration, Advanced. Tab routes, tab contents, and the default entry tab are unchanged.
- Keep the open agent tab when switching agents in the sidebar instead of always reopening Console, falling back to Console for Workspace, a team, or a freshly created agent. A selected task is intentionally not carried over.

### Fixed

- Exclude a root-level `evals` directory from a built image when packaging a skill, so authoring evidence no longer adds bytes, file count, and hash input to the immutable image. Nested paths such as `references/evals/` are unaffected.

[0.64.0]: https://github.com/alekzonder/tariboy/compare/v0.63.0...v0.64.0

## [0.63.0] - 2026-09-16

### Added

- Read an agent inbox as a chat with the customer: a Chat tab beside Tasks with Chat and Channels views, a sidebar that orders unpinned agents by their most recent conversation and carries per-agent unread counts, and `GET /api/chats`, `GET /api/chats/{agent}`, and `POST /api/chats/{agent}/read` served from the existing messages table.
- Stream one publication hint per message for every agent on the host over `GET /api/messages/ws`, so Desktop refetches on delivery instead of polling while HTTP stays authoritative.

### Changed

- Fix the customer login at `customer`, overridable through the `customer_login` daemon config key, instead of following the account that runs the daemon. The first start after upgrading carries existing tasks, comments, waits, notification state, and the old `user:<$USER>` channel over to the new principal in one transaction.
- Attribute task notifications to the causing principal in `data.from` while keeping `source` as `system:tasks`, and attribute an operator publish to the customer principal with `reply_to` support, so an agent reply stays in the conversation instead of in its own inbox.

### Fixed

- Correct the channel bus reference and messaging architecture pages, which still described the removed automatic acknowledgement path, together with the channel prefix list, `group request` semantics, the subscription dedup key, and the repeated-message troubleshooting entry.

[0.63.0]: https://github.com/alekzonder/tariboy/compare/v0.62.3...v0.63.0

## [0.62.3] - 2026-09-16

### Fixed

- Keep macOS publication focused on version validation, signed packaging, and release assets while completing repository checks before the release tag is pushed.

[0.62.3]: https://github.com/alekzonder/tariboy/compare/v0.62.2...v0.62.3

## [0.62.2] - 2026-09-16

### Fixed

- Complete macOS release-check portability with cross-platform harness fixtures, Darwin non-terminal handling, canonical frozen skill paths, and BSD-compatible Desktop test path resolution while preserving live-state protections.

[0.62.2]: https://github.com/alekzonder/tariboy/compare/v0.62.1...v0.62.2

## [0.62.1] - 2026-09-15

### Fixed

- Make repository and release checks portable to macOS by accepting canonical image source roots through system symlinks, enforcing Darwin Unix-socket limits, handling non-terminal shim probes, and supplying test-only installer command fallbacks without weakening production requirements.

[0.62.1]: https://github.com/alekzonder/tariboy/compare/v0.62.0...v0.62.1

## [0.62.0] - 2026-09-15

### Added

- Organize the console sidebar into Agents, Groups, and Servers tabs, with pinned agents, unread-question priority, cross-server group membership, and server update actions.
- Allow trusted image-creator agents to build images from any source directory readable by the daemon.

### Changed

- Let builds, imports, retags, and registry publication advance any non-reserved image tag while retaining prior archives by digest for pinned assignments.
- Run the repository checks before packaging a tagged Desktop release.

### Removed

- Remove the Evals subsystem and its image manifest, plugin, CLI, API, UI, and runtime surfaces while retaining legacy persisted data for safe upgrades.
- Remove AI proxy Rules, including model routing, model allow/deny policy, rate limits, and their CLI, API, UI, and runtime surfaces.

### Fixed

- Flush queued AI usage before terminal iteration span aggregation.
- Hide stale image provenance and reject stale team snapshots after a tag advances to different image bytes.

[0.62.0]: https://github.com/alekzonder/tariboy/compare/v0.61.0...v0.62.0

## [0.61.0] - 2026-09-14

### Added

- Retry Store image builds after immutable-tag conflicts with an alternative target image name or tag.
- Show dependency titles and statuses in task details.

### Changed

- Simplify the task detail panel around the compact operator template.

### Removed

- Remove Judge reviews, automation, controlled improvements, and their CLI, API, UI, documentation, and persisted state.

### Fixed

- Reject immutable Store image targets before restoring skill locks, release build controls promptly, and prevent stale inventory reads from replacing newer build results.
- Allow Goal wake delivery to recover after an earlier delivery reaches the dead-letter queue.

[0.61.0]: https://github.com/alekzonder/tariboy/compare/v0.60.0...v0.61.0

## [0.60.0] - 2026-09-14

### Added

- Copy the original Markdown from task comments.
- Browse image registry names and tags with coherent details for the selected image version.
- Show each agent’s active image version and the next available image version.
- Filter console agents by agent name or server label.

### Changed

- Redesign the console shell, task table, task detail panel, and application theme around the denser metal-and-blue operator interface.
- Publish documentation automatically after a successful Desktop release.

### Fixed

- Preserve unsupported Markdown content when editing task text in the rich editor.
- Accept `argument-hint` in Agent Skill frontmatter.
- Report latest-image inspection errors and retain existing agent image status when inventory refresh fails.
- Use English labels in application settings.
- Keep task detail controls usable at narrow widths and preserve Queue settings labels.

[0.60.0]: https://github.com/alekzonder/tariboy/compare/v0.59.0...v0.60.0

## [0.59.0] - 2026-09-11

### Added

- Connect a saved SSH host to an existing Tariboy daemon without installing, starting, or restarting remote software.

### Fixed

- Preserve the selected SSH setup mode while retrying an unavailable daemon or falling back to installation.

[0.59.0]: https://github.com/alekzonder/tariboy/compare/v0.58.1...v0.59.0

## [0.58.1] - 2026-09-11

### Fixed

- Restore Desktop builds after adding the application updater.

[0.58.1]: https://github.com/alekzonder/tariboy/compare/v0.58.0...v0.58.1

## [0.58.0] - 2026-09-11

### Added

- Download, signature-verify, and explicitly install Desktop updates from application settings.
- Present one-shot instructions, messages, and Native Task goals in a platform-owned processing order for every non-bare agent.

### Changed

- Publish the checked DMG together with the signed updater archive, signature, and update catalog through an atomic draft-to-public release flow.
- Prepare locked Go, UI, and documentation dependencies automatically before fast checks.

### Fixed

- Compare Desktop update versions by SemVer precedence.
- Preserve Markdown headings in Native Task questions.
- Remove the release artifact scanner’s ripgrep runtime dependency.

[0.58.0]: https://github.com/alekzonder/tariboy/compare/v0.57.5...v0.58.0

## [0.57.5] - 2026-09-11

### Fixed

- Avoid false credential detections while verifying macOS release artifacts.

[0.57.5]: https://github.com/alekzonder/tariboy/compare/v0.57.4...v0.57.5

## [0.57.4] - 2026-09-11

### Fixed

- Publish macOS release artifacts without requiring Linux binary emulation.

[0.57.4]: https://github.com/alekzonder/tariboy/compare/v0.57.3...v0.57.4

## [0.57.3] - 2026-09-11

### Fixed

- Use the installed QEMU runner when verifying Linux x86_64 release binaries on macOS.

[0.57.3]: https://github.com/alekzonder/tariboy/compare/v0.57.2...v0.57.3

## [0.57.2] - 2026-09-11

### Fixed

- Publish signed macOS artifacts without running the isolated desktop smoke suite.

[0.57.2]: https://github.com/alekzonder/tariboy/compare/v0.57.1...v0.57.2

## [0.57.1] - 2026-09-11

### Fixed

- Validate schema-v2 editable image sources before building them.

[0.57.1]: https://github.com/alekzonder/tariboy/compare/v0.57.0...v0.57.1

## [0.57.0] - 2026-09-11

### Added

- Run eligible saved scripts immediately, including idle recurring scripts.
- Mark all unread task notifications as read from the Tasks inbox.
- Register editable image-source API routes.

### Fixed

- Confine agent-authored image builds to each agent's managed workdir.
- Register editable image-source routes.

[0.57.0]: https://github.com/alekzonder/tariboy/compare/v0.56.0...v0.57.0

## [0.56.0] - 2026-09-11

### Added

- Agent and global shell-script editors, validated and sourced before harness launch.
- Image version visibility and Store image update status.
- Persistent, keyboard-accessible sidebar ordering for servers, teams, and agents.
- Stalled AI-iteration reporting and configurable stall timeout.
- Tag-triggered macOS release publication.

### Fixed

- Shell-script loading and launch-argument preservation.
- Audit-export streaming and transcript deduplication.
- Task and goal selection with pull-request URLs.

[0.56.0]: https://github.com/alekzonder/tariboy/compare/v0.55.0...v0.56.0
