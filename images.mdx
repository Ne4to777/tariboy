---
title: Images
description: Build plugin, Agent Skill, and ordered prompt artifacts, inspect them, and assign them to agents.
sidebar:
  label: Overview
  icon: layers
---

An image defines the plugins available to an agent, optional packaged Agent
Skills, and the exact prompt template rendered for each iteration. Harness,
model, effort, interactive mode, environment, policy, and secrets are
runtime or compose configuration. They are not schema-v2 image fields.

## Browse built images

The **Images** list has one row per image name on the selected server. **Latest
image_version** and **Latest built at** describe exactly `name:latest`;
**Newest built tag** and **Tag built at** describe the most recently built tag,
which may be different. Missing latest shows **No latest**, and an unversioned
manifest shows **Version not specified**. Newest is selected by parsed
`built_at`, not SemVer. Tied dates prefer the matching version tag, then stable
tag-name order; missing or invalid dates sort last. Imported archives retain
their original build time, rather than showing the import time.

Select an image name to see its tags, manifest versions, build dates, full
digests, and separate **Latest** and **Newest** badges. Current and pending agent
usage and Run Agent, Export, Upload to servers, and Remove remain per-tag
actions. Import stays on the Images root. Select a tag for its existing
Overview, Template, Skills, and Files tabs; breadcrumbs return to its name or
the root, and existing direct tag URLs keep working.

Detail reads carry the displayed manifest digest. If a ref changes
between reads, the UI reports the update, discards the old content, and reloads
once. A further change offers **Refresh image**, so repeated rebuilds cannot
cause an endless refresh loop. The read API accepts optional
`expected_digest` on manifest, template, prompt, provenance, file-list, and
file-content requests; a mismatched published digest returns HTTP 409
`image_changed`. CLI equivalents accept `--expected-digest`. Requests without
that value retain their existing behavior. Every detail read is serialized
with publication, so its digest check and content read describe one generation.

## Build from an original directory

Open **Images**, enter the directory containing `Tariboyfile.yaml`, choose a
required name and an optional tag, then select **Validate** or **Build**. The
default build publishes both the source `image_version` and `latest`, or only
`latest` when the source has no version. An explicit tag publishes only that
tag. Tariboy records the canonical source CWD as local provenance and retains
a frozen source snapshot for evidence. Rebuilding still reads the original
directory and its current paths.

**Validate** is read-only. Before a build it shows the schema version, explicit
plugin names, packaged skill metadata, template hash, and every prompt entry in
declared order. Static entries include their original source string, resolved
category, byte size, and full SHA-256; runtime entries remain named
placeholders. Errors block the build.
Warnings call out omissions such as identity or finishing instructions,
host-bound absolute paths, unusually large prompts, missing obvious plugin
instructions, and a static external-plugin version that differs from the active
runtime version. Validation never repairs or reorders the template.

Use **Open in VS Code** on an image detail page when the source CWD still exists
on the selected local or SSH host. Imported images have no source CWD.

**New Image** creates a managed schema-v2 source from only a name and optional
initial prompt. The generated project is prompt-only; edit its
`Tariboyfile.yaml` to declare packaged skills or plugins before building.

The equivalent operator command is:

```bash
tariboy image validate --path ./reviewer-image --name reviewer --tag v3
tariboy image build --path ./reviewer-image --name reviewer --tag v3
tariboy image build --path ./reviewer-image --name reviewer --tag latest --tag v4
tariboy image build --path ./reviewer-image --name reviewer --tag v4 \
  --repository-id agent-images --git-commit 91ab820
tariboy image ls
```

Provide `--repository-id` and `--git-commit` together when the source is an
exact Git revision. Tariboy records those explicit values in the frozen
source snapshot; it never infers a commit from the current working directory.

For CLI calls, a relative `--path` is resolved against the shell's current
working directory before the request is sent to the daemon.

### Refs, tags and identity

A **ref** is the stored image content. Its identity is derived from the image
name and the `image_version` declared in `Tariboyfile.yaml`, so rebuilding the
same version republishes the same ref and bumping the version creates another
one. A source with no declared version stays addressed by its archive bytes.
A **tag** is only a pointer to a ref, held in `images/<name>/tags/<tag>`; the
content lives once in `images/<name>/refs/<id>.tar.gz`.

Every build therefore publishes without restriction: rebuilding an existing
ref, including `latest`, always succeeds and moves the tag. Generations that
other tags no longer name stay on disk, so agents pinned to an earlier ref keep
running until their next launch gate.

There is one build mechanism. The operator CLI, the Store build, an agent's
own `image-creator`, and team import all publish the same way and behave
identically, including how they default tags. Repeat `--tag` to move several
tags onto one build; every requested tag names the same ref and reports the
same digest, so a versioned tag and `latest` can never disagree. The requested
refs and their snapshot/provenance metadata commit as one batch; a failure on
any tag restores every tag exactly. Duplicate tags are rejected. When `--tag`
is omitted, a source with `image_version` publishes both that version and
`latest`, and the single-result output reports the versioned ref; a source
without a version publishes only `latest`. Explicit tags, including
`--tag latest`, take priority and do not add another ref.

The daemon-managed `bare:latest` and `basic:latest` refs remain reserved and
cannot be built, imported over, or removed.

A store written by an earlier release is migrated once, when the daemon starts:
each archive keeps its content digest as its ref id, so every digest already
pinned on an agent continues to resolve. The migration is one-way.

## Stores on a server

Open **Stores** beside **Images** to register a Git repository or an absolute
directory on the selected server. Each `tariboyd` owns its own registrations
and performs clone, refresh, and build on that server. A Store contains image
sources at `images/<image_name>/Tariboyfile.yaml`.

```bash
tariboy store add team git@github.com:company/agent-images.git
tariboy store add local /srv/company-images
tariboy store list
tariboy store show team
tariboy store refresh team
tariboy store auto team --interval 60 --image reviewer --image writer
tariboy store auto team --interval 0
tariboy image build team/reviewer
tariboy image build team/reviewer --name company-reviewer
tariboy store remove team
```

Git sources are cloned under `<base-dir>/stores/<store_name>/`. For a daemon
running as `agent` with defaults, `team` lives at
`/home/agent/.tariboy/stores/team/`, and its reviewer source is
`/home/agent/.tariboy/stores/team/images/reviewer/Tariboyfile.yaml`.
`TARIBOY_BASE_DIR` or the daemon's `--base-dir` changes that root.
A local source such as `/srv/company-images` is used in place, without a copy.
Stores are the source of canonical agent images and skills; the daemon no
longer installs a separate built-in Store tree.

### Automatic refresh and build

A Store can refresh and rebuild itself on a schedule. `tariboy store auto NAME
--interval MINUTES --image IMAGE` stores a whole-minute interval and the images
selected for automatic builds; the Store detail exposes the same policy as an
interval field and one **Auto** checkbox per image. An interval of `0` disables
it, which is the default for a new Store.

Every minute the daemon runs the cycle for each Store whose interval has
elapsed: it refreshes that Store exactly as **Refresh** does, then builds each
selected image that reports an update, using the default tags so both
`<image_name>:<image_version>` and `<image_name>:latest` are published. An image
is built only when the source and latest both declare versions and those values
differ, so an unbuilt or unversioned image is never built automatically.

A failed refresh skips that Store's builds for the cycle, and a failed build
does not stop the remaining selections or Stores. Failures are logged and
retried at the next interval; missed cycles are not replayed, so a daemon
restart can delay a due cycle by up to one interval. Saving a policy restarts
the interval from that moment.

The registration's name and source are persisted in the daemon database
together with this policy (interval, selected images, and the last cycle time).
Viewing a Store still reads the current disk inventory and `image_version` values;
there is no saved image list to become stale. For each valid source, the detail
inspects `<image_name>:latest` directly and shows that manifest's
`image_version`. It marks an update only when the source and latest both declare
versions and those values differ. Missing latest, an unversioned source or
latest, and a damaged latest artifact remain distinct states. A damaged latest
does not hide healthy siblings or fall back to another tag. A successful
**Build** reloads the inventory and reports both the version tag and `latest`
when the source is versioned. A missing `images/` directory shows an empty
list. An invalid source reports its own error and disables its Build action
while valid sibling images remain available.

**Refresh** of a managed clone makes it match its upstream exactly: it runs
`git fetch --prune`, `git reset --hard @{upstream}` and `git clean -fd`, so
local edits, local commits and untracked files in `<base-dir>/stores/<name>/`
are discarded on every refresh; files ignored by `.gitignore` are kept. A local
Git checkout root, including a Git worktree, is refreshed with
`git pull --ff-only` and its local edits are never reset or stashed. A local
directory without its own `.git` entry is simply reread; Tariboy does not pull
an ancestor repository. A fetch, missing upstream, conflict or authentication
failure is returned as `store_refresh_failed` (HTTP 409) and carries the tail of
Git's error output, so the UI and CLI name the cause. For a local checkout,
resolve it there, for example after `git -C <path> status`, then refresh again.
**Remove** unregisters the Store and deletes its managed clone;
it preserves local source directories and already built images.

Store builds use the existing image builder. By default, a versioned source
publishes both `<image_name>:<image_version>` and `<image_name>:latest`, while a
source without a version publishes only `latest`. Use `--name` to avoid a name
collision between Stores, and `--tag` to publish only an explicit tag. Do not
combine a Store selector with `--path`.

Store Build advances every requested tag atomically after validation. Existing
version tags and `latest` do not require a different target name or tag. The
Store page's **Build** always publishes the defaults; to publish another name or
tag, use `tariboy image build <store>/<image> --name NAME --tag TAG`. An
explicit tag publishes only that ref; other refs remain unchanged.

Before freezing sources, the daemon runs `npx skills experimental_install` in
the Store root when `skills-lock.json` exists there, then in the image directory
when it has a separate lock. An image with `extends` instead installs each
layer's lock in its temporary assembly directory, in layer order, after the
Store root (see [Inherit from other images](#inherit-from-other-images-with-extends)).
The installer rewrites `skills-lock.json`, so the
daemon restores each lock file's original content after it runs, including on
failure; a build leaves the Store checkout's tracked lock files unchanged and
does not block a later Refresh. An installation failure stops the build before
publication. Git must be installed on the daemon host; lock restoration also
requires Node.js/npm and access to the skill sources. Private repositories use
the server account's configured SSH or Git credential helpers. Configure access
on that server before adding the Store; do not put credentials in its URL.

The Store detail labels **Source image_version** and **Latest image_version**
explicitly, highlights version mismatches, and keeps manual **Build** available
when the versions match. This comparison cannot detect changed source bytes
when `image_version` stays the same; use manual Build after such a change.
Built artifacts appear in the existing **Images** workspace. Stores do not yet
expose standalone skills or plugins.

## Image versions

Set `image_version: 0.1.0` in `Tariboyfile.yaml` to version the image
independently of `schema_version` and the Tariboy product version. Both source
schemas accept SemVer 2.0.0, including prerelease and build metadata, without a
`v` prefix. Complete SemVer versions are also valid image tags.

Read or increment the version locally, without a running daemon:

```bash
tariboy image version get
tariboy image version get --path ./reviewer-image/Tariboyfile.yaml
tariboy image version update patch --path ./reviewer-image
tariboy image build --path ./reviewer-image --name reviewer
```

Both version commands default to `./Tariboyfile.yaml`; `--path` accepts a
file or directory. They print only the resulting version (or a JSON string
with `--json`). `update major`, `update minor`, and `update patch` increment
that component, reset lower components to zero, and remove prerelease/build
suffixes. For example, `1.2.3-rc.1+build.7` becomes `1.3.0` after `update minor`.

Updates atomically replace the file while preserving its permissions, YAML
fields, and comments; YAML formatting may change. Missing or invalid versions
produce an error without changing the file. Old sources may omit the field
for build compatibility, but version commands require it.

The runnable manifest preserves `image_version`; the archive carries no tag,
since a tag only points at a ref. Every new iteration snapshots the source
version with the ref and digest, exposes it through `iteration inspect`, and
includes it in `runtime: identity` and iteration audit exports. Older runnable
images and historical iterations simply omit the version.

## Schema version 2

Schema v2 is strict and accepts only `schema_version`, `image_version`,
`extends`, `plugins`, `skills`, and `prompts`:

```yaml
schema_version: 2
image_version: 0.1.0
plugins:
  - name: whoami
  - name: messages
  - name: context
  - name: workdir
  - name: jira
skills:
  - dir: ../../skills/whoami
  - dir: ../../skills/messages
  - dir: ../../skills/context
  - dir: ../../skills/workdir
  - dir: ../shared-skills/code-review
  - dir: $PLUGINS/jira/2.5.0/skills/triage
prompts:
  - runtime: identity
  - runtime: one-shot
  - runtime: messages
  - file: $PLUGINS/jira/2.5.0/prompts/reviewer.md
  - runtime: context
  - runtime: workdir
  - file: ../../skills/loop/finish-iteration.md
  - runtime: user-prompt
  - file: /srv/tariboy/prompts/finish.md
```

The `prompts` sequence preserves the order of static files and ordinary runtime
values. For every non-bare agent, Tariboy prepends a platform-owned **Task
Processing Order** block containing one-shot, messages, and Goal. Older entries
for those inputs are accepted but do not render duplicate sections. Tariboy
does not add an identity header or finishing tail. An empty list produces an
empty image template, but the platform work block is still rendered at runtime.

Each static `file` is resolved and embedded at build time. Supported forms are:

| Form | Resolves from |
| --- | --- |
| `$PLUGINS/...` | The common external-plugin root |
| `./...` | The original directory containing `Tariboyfile.yaml` |
| `../...` below a declared skill | That skill's frozen source snapshot |
| `/absolute/path/...` | An operator-supplied absolute filesystem path |

The builder expands only those literal variables; it never performs shell or
environment expansion. A prompt may use `../` only to read a file below a
source-relative directory already declared in `skills`; validation reads that
directory directly and build reads its frozen snapshot. Other traversal,
symlinks, missing files, non-regular files, and oversized prompt files are
rejected.

### Inherit from other images with `extends`

`extends` lists parent image source directories. Each entry uses `./`, `../`,
or an absolute path and resolves from the directory of the image that declares
it:

```yaml
schema_version: 2
image_version: 0.2.0
extends:
  - ../basic
  - ../reviewer-base
plugins: [{name: jira}]
skills: [{dir: ./skills/triage}]
prompts: [{file: ./instructions.md}]
```

The build expands the chain into ordered layers: every parent's own parents
first, then the parent, in `extends` order, with the child last. An image
reached twice, such as a shared ancestor, is used once at its first position.
A cycle, a missing parent, a parent that is not schema v2, and a chain deeper
than 16 images are rejected before anything is built.

Tariboy assembles the layers in a temporary owner-only directory under
`<base-dir>/image-build/` and removes it after the build:

1. Each layer's directory is copied over the previous one. A file with the
   same path is replaced; other files remain.
2. If the copied layer contains `skills-lock.json`, a Store build runs
   `npx skills experimental_install` in the assembly directory and then removes
   the lock. A later layer's install replaces a skill of the same name
   installed by an earlier one.
3. Each layer's `./` prompt files are copied to
   `./.tariboy-extends/<layer>/<path>`, so equal prompt paths from different
   images stay separate files. The reserved `.tariboy-extends` directory must
   not exist in a source.
4. `../` skill and prompt paths are rewritten to absolute paths from the
   layer's original directory.

The lists are then concatenated in layer order. `plugins` keep the first entry
of each name. `skills` keep the **last** entry of each skill name, so a child
replaces a parent's skill. `prompts` keep the first entry of each runtime
placeholder and of each file content, so a shared ancestor's prompt is not
repeated. The child's `image_version` names the result. The merged
`Tariboyfile.yaml` has no `extends` and is validated, frozen, and built
exactly like any other source; Validate shows the merged result.

`extends` works for `--path` builds, Store builds, and an agent's own image
builds. Only Store builds run the per-layer skills installer; for the other
builds, restore the lock in each image directory first. Managed image sources
and team imports reject `extends`. A child can only append: it cannot remove
an inherited entry or insert a prompt between a parent's prompts. Layering
never deletes files, so a skill directory copied from a parent keeps files the
child's copy lacks. Rebuilding a parent does not rebuild its children.

## Packaged Agent Skills

Each `skills` entry explicitly packages one Agent Skill directory into the
image. The skill travels with runnable export/import. At iteration
launch Claude Code and OpenCode receive it through native skill configuration;
Codex receives a compact prompt catalog with its absolute `SKILL.md` path and
reads the file on demand. This is separate from image plugins and prompt
entries; none implies another.

See [Agent Skills in images](/docs/images/agent-skills) for the directory and
frontmatter contract, source paths, validation limits, manifest fields,
portability, harness adapters, discovery precedence, and activation failures.

## Runtime placeholders

Runtime entries remain visible placeholders inside the built image. Before
every iteration, the runner replaces ordinary values at their declared
positions. Work inputs are always grouped in the platform block:

| Placeholder | Value at iteration start | Skill named in the rendered instruction |
| --- | --- | --- |
| `identity` | Current agent name, active image ref and digest, CWD, and iteration identity | `whoami` |
| `goal` | Compatibility marker; selected Native Task fields appear in Task Processing Order | none; platform-owned |
| `workdir` | Absolute managed `agents/<agent>/workdir`, independent of the effective CWD | `workdir` |
| `context` | Durable agent context | `context` |
| `messages` | Compatibility marker; delivered messages and outstanding requests appear in Task Processing Order | none; platform-owned |
| `awaiting-replies` | Compatibility marker; outstanding requests appear under Messages in the platform block | none; platform-owned |
| `user-prompt` | The agent's standing prompt | none; this is task input |
| `one-shot` | Compatibility marker; single-execution input appears first in Task Processing Order | none; this is task input |

Each non-empty ordinary runtime value starts with `# [runtime: <name>]`. For skill-owned
values, the short instruction to use that skill follows the heading and names
the skill without embedding an installation path. Context data then starts with
`# Agent Context`. The platform block starts with `# Task Processing Order`,
followed by a fixed ordering instruction and `## One-shot`, `## Messages`,
and `## Goal`. Missing work inputs have explicit empty-state text and an
instruction not to search for them with commands. Outstanding replies use a
nested `### Awaiting replies` heading; message and task content is retained.
This grouping applies to existing images without rebuilding and to images
without work-input entries or plugins. It grants no additional capabilities
or packaged skills. Existing declared markers still undergo normal image
contract validation. Each singleton placeholder may appear at most once.
Empty ordinary runtime values add no text. The **Template** tab shows static paths, categories, sizes,
hashes, and runtime markers in their exact order without draining messages.

## External plugins

Image-owned static prompts and complete Agent Skill trees come from the image
source and are copied into the runnable artifact. Dynamic runtime placeholders
and built-in or external plugin implementations remain daemon-owned.

External plugins are separate:

```text
$PLUGINS/<plugin-name>/<plugin-version>/...
```

Versions can exist side by side, while one version per plugin name is active
and supervised. Images select plugin names, not plugin versions. Adding a
plugin never adds its prompt implicitly; include every required static prompt
path in `prompts`.

## Assign an image to an existing agent

Build the image in **Images**, then open the agent's **Configuration** tab and
select it under **Agent image**. The choice is persisted as pending. Tariboy
does not interrupt a running iteration: it validates and activates the image
through the next iteration's launch gate, then snapshots the image ref, source
version, digest, and prompt-template hash for that iteration.

Activation changes only image bytes, plugin capabilities, and image-owned
shims. It preserves harness, model, effort, environment, interactive and loop
settings, CWD, context, workdir, messages, history, audit, group, and
subscriptions. A validation or staging failure leaves the current image active
and exposes a retryable pending error. The pending selection can be replaced or
cancelled before activation.

The image tag list shows agent names separately under **Current** and
**Pending**, so an operator can see both active use and assignments waiting for
their next launch gate. An image in either list cannot be removed.

Configuration shows **Current** (or **Activated version** when stopped) with
the version read from the agent's pinned ref and digest. **Next iteration**
previews the explicit pending digest first, otherwise the content the active
ref points at now, otherwise the current pinned image. Viewing this
projection does not create pending state or activate anything. Inspection and
activation errors remain visible even when there is no pending ref; missing
version metadata is labelled **Version not specified**.

When an agent's active ref moves to another id, the launch gate stages and
promotes it at the next iteration. The running iteration keeps its original
prompt, version, and digest. A rebuild that does not change `image_version`
republishes the same ref, so there is nothing new to adopt and running agents
are undisturbed; bump the version to roll a change out. The preview is not a
reservation: the ref can move again before launch. Cancelling an explicit
pending assignment does not disable following the active ref. The preview refreshes with agent status and after UI image builds.

## Import and export

**Export** downloads a runnable image artifact named
`<name>-<tag>.tariboy-image.tar.gz`. It includes the runnable image
archive, including packaged skills, and its digest, but never original source
files or source CWD. After the browser download starts, Tariboy confirms the
saved image ref and portable filename in a toast. **Import runnable image** verifies both the portable wrapper and the inner runnable
archive before installation. Validation rejects unmanifested content, unsafe or
special-file members, duplicate paths, malformed schema-v2 metadata, prompt
layer/hash mismatches, and compressed or expanded limit violations. Import does
not rebuild the image. Preview, apply, assignment, and activation also validate
plugin, runtime, skill, launcher, and harness compatibility against the
destination daemon before publication. The import preview exposes editable **Import name** and
**Import tag** fields. The same ref and digest are idempotent; when the ref
already names different bytes, import atomically advances the tag and retains
the previous archive by digest. An explicit retag receives a newly rewritten
manifest and digest.

Keep the original directory and paths when future rebuilds are required. An
exported image is portable for use, not an editable source backup.

## Transfer a runnable image to servers

For an exportable, non-reserved built image, choose **Upload to servers** to
send its runnable artifact to other configured hosts. The dialog lists every
currently ready host except the captured source host; **All servers** selects
that all-ready, non-source set, and individual targets can be deselected. For a
remote source, this set includes **This daemon (local)** once; for a local
source, it never lists the local source as a destination.

Tariboy exports one runnable archive from the captured source and keeps it only
in the browser memory of the open dialog. It previews and applies that same
archive separately on each selected destination. A failure on one destination
does not prevent later destinations from continuing. A same-ref/same-digest
destination is shown as **Already present**; a different digest advances that
destination's tag and is shown as completed. A transfer can still be retagged
and retried for only one destination without exporting again. Closing the
dialog discards the archive and its transfer progress.

The dialog snapshots the eligible hosts and the chosen destinations when it
opens and starts the operation, so a later registry refresh cannot hide the
result for a requested host. It first shows **Exporting** while downloading the
single source archive; controls and cancellation remain unavailable until that
download completes. Afterwards, cancellation prevents only destinations that
have not started.

The source and every destination are explicit request targets throughout the
transfer. Starting, continuing, or retrying a transfer never changes the active
host or the selected source route. The in-memory archive is a runnable artifact
for this operation, not source backup or persistent state.

## Built-in images and compatibility

- `bare:latest` is daemon-synthesized with no plugins and an empty prompt. Its
  terminal-only behavior is runtime policy.
- The official Store's `basic` source explicitly declares every plugin,
  packaged skill, runtime placeholder, and mandatory finish prompt it uses.
  Register and refresh `tariboy-store`, then build `official/basic`; daemon
  upgrades do not publish or replace that image.
- New build and validation requests require schema v2. Existing self-contained
  schema-v1 runnable archives remain readable and runnable only when they carry
  every byte their historical contract requires; they are not valid editable
  sources for a new build.

Native Tasks remains daemon-owned. Add `plugins: [{name: tasks}]` and package
`../../skills/tasks` when a Store image should expose its agent command and
instructions. Add `plugins: [{name: goal}]` and package `../../skills/goal`
when the image should expose explicit Goal selection tools. The
daemon-authoritative selected Goal is included for every non-bare agent,
independently of those declarations. The rendered task title
and description are untrusted task input, not daemon instructions or lifecycle
authority.

The supplied `tariboy-developer` image source also packages the Tasks skill and
declares the compatible `runtime: goal` marker; custom images need no marker
to receive the platform work block.
