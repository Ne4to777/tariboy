import { useCallback, useEffect, useMemo, useState } from "react";
import type { ReactNode } from "react";
import { useAgentName } from "@/lib/agent";
import {
  agentGetOn,
  agentPostOn,
  getAgentShellScriptOn,
  getActiveDaemon,
  setAgentShellScriptOn,
  type ApiTarget,
} from "@/lib/api";
import { guard } from "@/lib/toast-guard";
import type { AgentView } from "@/lib/types";
import {
  Card,
  CardContent,
  CardAction,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { GoalHelp } from "@/components/GoalHelp";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { SecretsPanel } from "@/components/SecretsPanel";
import { RetentionPanel } from "@/components/RetentionPanel";
import { ShellScriptEditor } from "@/components/ShellScriptEditor";

// Operator-selectable harnesses. This is intentionally a SUBSET of the backend
// allowlist (internal/harness.Get() = claude | codex | opencode | cursor | stub): 'stub'
// is a test-only harness that the compose validator rejects, so it is omitted
// here to keep operators from picking a value that can't be applied. The
// /harness endpoint stays permissive (still accepts 'stub' for unit tests);
// this list only governs what the dropdown offers as a new choice. An agent
// already on 'stub' still shows it as its current value — see harnessOptions.
const HARNESS_OPTIONS = ["claude", "codex", "opencode", "cursor"] as const;

// Field aligns a label above its control (input/select + button) so every row
// in the Settings page shares the same vertical rhythm. The control row keeps
// the input and its Set button as direct siblings — tests and CSS both rely on
// that grouping.
function Field({
  label,
  htmlFor,
  hint,
  children,
}: {
  label: string;
  htmlFor?: string;
  hint?: ReactNode;
  children: ReactNode;
}) {
  return (
    <div className="space-y-1.5">
      <Label htmlFor={htmlFor}>{label}</Label>
      {children}
      {hint ? <p className="text-xs text-muted-foreground">{hint}</p> : null}
    </div>
  );
}

// Mirror the shadcn Input look (rounded-lg, same border + focus ring) so
// selects and text inputs line up as one visual family, in light and dark.
const selectClass =
  "h-9 w-full rounded-lg border border-input bg-background px-2.5 text-sm outline-none " +
  "transition-colors focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 " +
  "dark:bg-input/30";

const NEXT_ITERATION = "Takes effect on the next iteration.";
const NEXT_START_SECTION = "Restart the agent yourself when you're ready.";
const NEXT_START_FIELD =
  "Saved immediately. Takes effect the next time the agent starts.";

// ---- Batched section drafts -------------------------------------------------
//
// Loop and the model/effort half of Runtime each own one draft with a single
// commit point instead of a Set button per field. The backend has no batch
// endpoint, so a save fans out over the CHANGED fields only, SERIALLY, in the
// section's render order, using the same per-field calls as before: what
// changes is when and how many requests are sent, never which ones exist.

// A field descriptor is pure and static, so the section arrays live at module
// scope: nothing in them depends on a render, which keeps the draft hook's
// dependencies stable.
interface SectionField {
  // key is both the draft key and the control's DOM id, so a failed save can
  // move focus to the control it belongs to.
  key: string;
  label: string;
  helper: string;
  options?: readonly string[];
  numeric?: boolean;
  minimum?: number;
  toggle?: boolean;
  read: (view: AgentView) => string;
  // normalize maps raw input text to the value that is compared against the
  // baseline AND sent to the server, so "30" and " 30 " are the same edit.
  normalize: (raw: string) => string;
  validate?: (value: string) => string;
  submit: (target: ApiTarget, name: string, value: string) => Promise<unknown>;
}

type Values = Record<string, string>;

// Whole seconds/counts: a non-numeric draft is left alone so the server, not
// the page, decides that it is invalid.
const normalizeInt = (min?: number) => (raw: string) => {
  const n = Number(raw.trim());
  if (raw.trim() === "" || !Number.isFinite(n)) return raw.trim();
  const truncated = Math.trunc(n);
  return String(min === undefined ? truncated : Math.max(min, truncated));
};
const normalizeStr = (raw: string) => raw.trim();

const loopInt = (
  field: string,
  min?: number,
): Pick<SectionField, "normalize" | "submit"> => ({
  normalize: normalizeInt(min),
  submit: (target, name, value) =>
    agentPostOn(target, name, `loop/${field}`, { value: Number(value) }),
});
const loopStr = (
  field: string,
): Pick<SectionField, "normalize" | "submit"> => ({
  normalize: normalizeStr,
  submit: (target, name, value) =>
    agentPostOn(target, name, `loop/${field}`, { value }),
});

const LOOP_FIELDS: readonly SectionField[] = [
  {
    key: "interval",
    label: "Interval",
    helper: `Seconds. ${NEXT_ITERATION}`,
    read: (v) => String(v.interval_s),
    ...loopInt("interval"),
  },
  {
    key: "timeout",
    label: "Timeout",
    helper: `Seconds. ${NEXT_ITERATION}`,
    read: (v) => String(v.timeout_s),
    ...loopInt("timeout"),
  },
  {
    key: "hard-timeout",
    label: "Hard timeout",
    helper: `Seconds. ${NEXT_ITERATION}`,
    read: (v) => String(v.hard_timeout_s),
    ...loopInt("hard-timeout"),
  },
  {
    key: "on-timeout",
    label: "On timeout",
    helper: NEXT_ITERATION,
    options: ["restart", "stop"],
    read: (v) => v.on_timeout || "restart",
    ...loopStr("on-timeout"),
  },
  {
    key: "on-error",
    label: "On error",
    helper: NEXT_ITERATION,
    options: ["restart", "stop"],
    read: (v) => v.on_error || "restart",
    ...loopStr("on-error"),
  },
  {
    key: "max-idle",
    label: "Maximum idle iterations",
    helper: `0 means never. ${NEXT_ITERATION}`,
    numeric: true,
    read: (v) => String(v.max_idle_iterations ?? 0),
    ...loopInt("max-idle", 0),
  },
  {
    key: "ai-stall-timeout",
    label: "AI inactivity timeout",
    helper: "Seconds without an AI-proxy request before showing an informational error.",
    numeric: true,
    minimum: 1,
    read: (v) => String(v.ai_stall_timeout_s ?? 1800),
    ...loopInt("ai-stall-timeout", 1),
  },
];

const RUNTIME_FIELDS: readonly SectionField[] = [
  {
    key: "model",
    label: "Model",
    helper: NEXT_ITERATION,
    read: (v) => v.model || "",
    normalize: normalizeStr,
    submit: (target, name, value) =>
      agentPostOn(target, name, "model", { value }),
  },
  {
    key: "effort",
    label: "Effort",
    helper: NEXT_ITERATION,
    read: (v) => v.effort || "",
    normalize: normalizeStr,
    submit: (target, name, value) =>
      agentPostOn(target, name, "effort", { value }),
  },
];

const POSITIVE_INTEGER = "Enter a positive whole number of seconds.";
const GOAL_FIELDS: readonly SectionField[] = [
  {
    key: "goal-enabled",
    label: "Enable Goal",
    helper: "Select and deliver this agent's current Native Task goal.",
    toggle: true,
    read: (v) => String(v.goal_enabled),
    normalize: normalizeStr,
    submit: (target, name, value) =>
      agentPostOn(target, name, "goal-enabled", { enabled: value === "true" }),
  },
  {
    key: "goal-wait-customer-timeout",
    label: "Wait customer timeout seconds",
    helper: "Use a positive whole number of seconds.",
    numeric: true,
    minimum: 1,
    read: (v) => String(v.goal_wait_customer_timeout_s),
    normalize: normalizeStr,
    validate: (value) =>
      Number.isInteger(Number(value)) && Number(value) > 0
        ? ""
        : POSITIVE_INTEGER,
    submit: (target, name, value) =>
      agentPostOn(target, name, "goal-wait-customer-timeout", {
        seconds: Number(value),
      }),
  },
  {
    key: "goal-delivery-cooldown",
    label: "Goal delivery cooldown seconds",
    helper: "Wait before sending another unprocessed Goal message.",
    numeric: true,
    minimum: 1,
    read: (v) => String(v.goal_delivery_cooldown_s),
    normalize: normalizeStr,
    validate: (value) =>
      Number.isInteger(Number(value)) && Number(value) > 0
        ? ""
        : POSITIVE_INTEGER,
    submit: (target, name, value) =>
      agentPostOn(target, name, "goal-delivery-cooldown", {
        seconds: Number(value),
      }),
  },
];

const MESSAGE_FIELDS: readonly SectionField[] = [
  {
    key: "messages-batch",
    label: "Messages per iteration",
    helper: "Maximum pending messages added to one iteration.",
    numeric: true,
    minimum: 1,
    read: (v) => String(v.messages_batch),
    normalize: normalizeStr,
    validate: (value) =>
      Number.isInteger(Number(value)) && Number(value) > 0
        ? ""
        : "Enter a positive whole number.",
    submit: (target, name, value) =>
      agentPostOn(target, name, "messages/batch", { value: Number(value) }),
  },
  {
    key: "messages-max-queue",
    label: "Maximum pending queue",
    helper: "New deliveries beyond this limit enter DLQ.",
    numeric: true,
    minimum: 1,
    read: (v) => String(v.messages_max_queue),
    normalize: normalizeStr,
    validate: (value) =>
      Number.isInteger(Number(value)) && Number(value) > 0
        ? ""
        : "Enter a positive whole number.",
    submit: (target, name, value) =>
      agentPostOn(target, name, "messages/max-queue", {
        value: Number(value),
      }),
  },
];

const PARTIAL_FAILURE =
  "Some changes were not saved. Review the highlighted fields and try again.";

function readSection(fields: readonly SectionField[], view: AgentView): Values {
  return Object.fromEntries(
    fields.map((f) => [f.key, f.normalize(f.read(view))]),
  );
}

interface SectionDraft {
  draft: Values;
  dirtyKeys: string[];
  saving: boolean;
  error: string;
  fieldErrors: Values;
  notice: string;
  setField: (key: string, value: string) => void;
  save: () => Promise<void>;
  discard: () => void;
}

// useSectionDraft owns one section's save contract: changed-only serial
// fan-out, stop-at-the-failure, and reconciliation from a full reload.
//
// The baseline is not state: it is always the loaded view, so a reload is
// reconciled by construction. Only edited fields are held as drafts, and a
// field drops its draft exactly when the server acknowledges it — which is why
// a partial failure keeps the failed and unattempted drafts while the
// acknowledged ones adopt the canonical, possibly renormalized, values.
function useSectionDraft(
  target: ApiTarget,
  name: string,
  view: AgentView,
  fields: readonly SectionField[],
  reload: () => Promise<AgentView | null>,
  savedMessage: string,
): SectionDraft {
  const baseline = useMemo(() => readSection(fields, view), [fields, view]);
  const [drafts, setDrafts] = useState<Values>({});
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [fieldErrors, setFieldErrors] = useState<Values>({});
  const [notice, setNotice] = useState("");

  const draft: Values = Object.fromEntries(
    fields.map((f) => [f.key, drafts[f.key] ?? baseline[f.key]]),
  );
  const dirtyKeys = fields
    .filter(
      (f) => f.key in drafts && f.normalize(drafts[f.key]) !== baseline[f.key],
    )
    .map((f) => f.key);

  const setField = useCallback((key: string, value: string) => {
    setNotice("");
    setError("");
    setFieldErrors((prev) => {
      if (!(key in prev)) return prev;
      const next = { ...prev };
      delete next[key];
      return next;
    });
    setDrafts((prev) => ({ ...prev, [key]: value }));
  }, []);

  const discard = useCallback(() => {
    setError("");
    setFieldErrors({});
    setNotice("Changes discarded");
    setDrafts({});
  }, []);

  const save = async () => {
    if (saving || dirtyKeys.length === 0) return;
    for (const f of fields) {
      if (!dirtyKeys.includes(f.key)) continue;
      const validation = f.validate?.(f.normalize(drafts[f.key])) ?? "";
      if (validation) {
        setError("");
        setFieldErrors({ [f.key]: validation });
        setNotice("");
        document.getElementById(f.key)?.focus();
        return;
      }
    }
    setSaving(true);
    setError("");
    setFieldErrors({});
    setNotice("");
    const acknowledged: string[] = [];
    let failedKey = "";
    let failedMessage = "";
    for (const f of fields) {
      if (!dirtyKeys.includes(f.key)) continue;
      try {
        await f.submit(target, name, f.normalize(drafts[f.key]));
        acknowledged.push(f.key);
      } catch (cause) {
        // Stop here: every field after this one stays an unsent dirty draft.
        failedKey = f.key;
        failedMessage = cause instanceof Error ? cause.message : String(cause);
        break;
      }
    }
    // Reload first, then release the acknowledged drafts, so those fields move
    // straight from the operator's value to the server's canonical one.
    await reload();
    setDrafts((prev) => {
      const next = { ...prev };
      for (const key of acknowledged) delete next[key];
      return next;
    });
    if (failedKey) {
      setError(PARTIAL_FAILURE);
      setFieldErrors({ [failedKey]: failedMessage });
      // Focus only after the request resolved, so the announcement and the
      // focus move do not race.
      document.getElementById(failedKey)?.focus();
    } else {
      setNotice(savedMessage);
    }
    setSaving(false);
  };

  return {
    draft,
    dirtyKeys,
    saving,
    error,
    fieldErrors,
    notice,
    setField,
    save,
    discard,
  };
}

// One control plus its label, timing helper and — when a save left it behind —
// the server's reason, wired to the control through aria-describedby.
function DraftField({
  field,
  section,
}: {
  field: SectionField;
  section: SectionDraft;
}) {
  const value = section.draft[field.key] ?? "";
  const fieldError = section.fieldErrors[field.key] ?? "";
  const describedBy = `${field.key}-help${fieldError ? ` ${field.key}-error` : ""}`;
  return (
    <div className="space-y-1.5">
      <Label htmlFor={field.key}>{field.label}</Label>
      {field.options ? (
        <select
          id={field.key}
          value={value}
          className={selectClass}
          aria-describedby={describedBy}
          aria-invalid={fieldError ? true : undefined}
          onChange={(e) => section.setField(field.key, e.target.value)}
        >
          {field.options.map((o) => (
            <option key={o} value={o}>
              {o}
            </option>
          ))}
        </select>
      ) : field.toggle ? (
        <Switch
          id={field.key}
          checked={value === "true"}
          aria-describedby={describedBy}
          aria-invalid={fieldError ? true : undefined}
          onCheckedChange={(checked) =>
            section.setField(field.key, String(checked))
          }
        />
      ) : (
        <Input
          id={field.key}
          value={value}
          className="h-9"
          type={field.numeric ? "number" : undefined}
          min={field.numeric ? (field.minimum ?? 0) : undefined}
          aria-describedby={describedBy}
          aria-invalid={fieldError ? true : undefined}
          onChange={(e) => section.setField(field.key, e.target.value)}
        />
      )}
      {fieldError && (
        <p
          id={`${field.key}-error`}
          role="alert"
          className="text-xs text-destructive"
        >
          {fieldError}
        </p>
      )}
      <p id={`${field.key}-help`} className="text-xs text-muted-foreground">
        {field.helper}
      </p>
    </div>
  );
}

// The footer is the section's only commit point, and it exists only while there
// is something to commit.
function DraftFooter({
  section,
  saveLabel,
}: {
  section: SectionDraft;
  saveLabel: string;
}) {
  return (
    <>
      {section.error && (
        <p role="alert" className="text-xs text-destructive">
          {section.error}
        </p>
      )}
      <p
        role="status"
        aria-live="polite"
        className="text-xs text-muted-foreground"
      >
        {section.notice}
      </p>
      {section.dirtyKeys.length > 0 && (
        <div className="flex flex-wrap items-center gap-3 border-t pt-4">
          <p
            role="status"
            aria-live="assertive"
            className="text-xs text-muted-foreground"
          >
            Unsaved changes
          </p>
          <Button
            size="sm"
            variant="secondary"
            className="h-9"
            disabled={section.saving}
            onClick={section.discard}
          >
            Discard changes
          </Button>
          <Button
            size="sm"
            className="h-9"
            disabled={section.saving}
            onClick={() => void section.save()}
          >
            {saveLabel}
          </Button>
        </div>
      )}
    </>
  );
}

function LoopEditor({
  name,
  view,
  reload,
  target,
}: {
  name: string;
  view: AgentView;
  reload: () => Promise<AgentView | null>;
  target: ApiTarget;
}) {
  const section = useSectionDraft(
    target,
    name,
    view,
    LOOP_FIELDS,
    reload,
    "Loop settings saved",
  );
  const [interval, timeout, hard, onTimeout, onError, maxIdle, aiStall] = LOOP_FIELDS;

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">Loop</CardTitle>
        <CardDescription>
          Choose when this agent runs and how it recovers.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-5">
        {/* Loop enable/disable is NOT here: it lives in the Configuration page's
            run-state strip, alongside the master switch, so an operator sees both
            run flags together. It is rendered exactly once, and it keeps its own
            immediate operation rather than joining this batch. */}
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-3">
          <DraftField field={interval} section={section} />
          <DraftField field={timeout} section={section} />
          <DraftField field={hard} section={section} />
        </div>

        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
          <DraftField field={onTimeout} section={section} />
          <DraftField field={onError} section={section} />
        </div>

        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
          <DraftField field={maxIdle} section={section} />
          <DraftField field={aiStall} section={section} />
        </div>

        <DraftFooter section={section} saveLabel="Save loop settings" />
      </CardContent>
    </Card>
  );
}

function GoalEditor({
  name,
  view,
  reload,
  target,
}: {
  name: string;
  view: AgentView;
  reload: () => Promise<AgentView | null>;
  target: ApiTarget;
}) {
  const section = useSectionDraft(
    target,
    name,
    view,
    GOAL_FIELDS,
    reload,
    "Goal settings saved",
  );
  const [enabled, timeout, cooldown] = GOAL_FIELDS;
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">Goal</CardTitle>
        <CardDescription>
          Choose whether this agent follows one sticky Native Task goal.
        </CardDescription>
        <CardAction><GoalHelp /></CardAction>
      </CardHeader>
      <CardContent className="space-y-5">
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
          <DraftField field={enabled} section={section} />
          <DraftField field={timeout} section={section} />
          <DraftField field={cooldown} section={section} />
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="current-goal-task">Current goal task</Label>
          <Input
            id="current-goal-task"
            value={view.current_goal_task_key || "No current goal"}
            disabled
          />
        </div>
        <DraftFooter section={section} saveLabel="Save Goal settings" />
      </CardContent>
    </Card>
  );
}

function MessageEditor({
  name,
  view,
  reload,
  target,
}: {
  name: string;
  view: AgentView;
  reload: () => Promise<AgentView | null>;
  target: ApiTarget;
}) {
  const section = useSectionDraft(
    target,
    name,
    view,
    MESSAGE_FIELDS,
    reload,
    "Message settings saved",
  );
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">Messages & Channels</CardTitle>
        <CardDescription>Control queue capacity and iteration batches.</CardDescription>
      </CardHeader>
      <CardContent className="space-y-5">
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
          {MESSAGE_FIELDS.map((field) => (
            <DraftField key={field.key} field={field} section={section} />
          ))}
        </div>
        <DraftFooter section={section} saveLabel="Save message settings" />
      </CardContent>
    </Card>
  );
}

function RuntimeConfigEditor({
  name,
  view,
  reload,
  target,
}: {
  name: string;
  view: AgentView;
  reload: () => Promise<AgentView | null>;
  target: ApiTarget;
}) {
  const section = useSectionDraft(
    target,
    name,
    view,
    RUNTIME_FIELDS,
    reload,
    "Runtime settings saved",
  );
  const [model, effort] = RUNTIME_FIELDS;

  // Options the dropdown renders: the operator subset, plus the agent's current
  // harness if it falls outside that subset (e.g. a test agent already on
  // 'stub'). This keeps the select from showing blank for an out-of-list value
  // without ever offering that value as a fresh choice.
  const harnessOptions = (HARNESS_OPTIONS as readonly string[]).includes(
    view.harness,
  )
    ? (HARNESS_OPTIONS as readonly string[])
    : [...HARNESS_OPTIONS, view.harness];

  const applyInteractive = (value: boolean) =>
    guard("interactive", async () => {
      await agentPostOn(target, name, "interactive", { value });
    }).then(reload);

  const applyHarness = (value: string) =>
    guard("harness", async () => {
      await agentPostOn(target, name, "harness", { value });
    }).then(reload);

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">Runtime</CardTitle>
        <CardDescription>
          Choose the model and effort for future iterations.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-5">
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
          <DraftField field={model} section={section} />
          <DraftField field={effort} section={section} />
        </div>

        <DraftFooter section={section} saveLabel="Save runtime settings" />

        {/* Harness and interactive stay OUT of the batch: each keeps its own
            immediate persistence request and takes effect on the next start. */}
        <section
          aria-labelledby="next-start-settings"
          className="space-y-4 border-t pt-4"
        >
          <div className="space-y-1">
            <h3 id="next-start-settings" className="text-sm font-medium">
              Next-start settings
            </h3>
            <p className="text-xs text-muted-foreground">
              {NEXT_START_SECTION}
            </p>
          </div>
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <Field label="Harness" htmlFor="harness" hint={NEXT_START_FIELD}>
              <select
                id="harness"
                value={view.harness}
                className={selectClass}
                onChange={(e) => {
                  if (e.target.value !== view.harness)
                    void applyHarness(e.target.value);
                }}
              >
                {harnessOptions.map((h) => (
                  <option key={h} value={h}>
                    {h}
                  </option>
                ))}
              </select>
            </Field>
            <div className="space-y-1.5">
              <div className="flex items-center gap-3 pt-1.5">
                <Switch
                  id="interactive"
                  checked={view.interactive}
                  onCheckedChange={(value) => void applyInteractive(value)}
                />
                <Label htmlFor="interactive" className="cursor-pointer">
                  Interactive (tmux TUI)
                </Label>
              </div>
              <p className="text-xs text-muted-foreground">
                {NEXT_START_FIELD}
              </p>
            </div>
          </div>
        </section>
      </CardContent>
    </Card>
  );
}

export default function AgentSettings({
  target = getActiveDaemon(),
}: {
  target?: ApiTarget;
}) {
  const name = useAgentName();
  const [view, setView] = useState<AgentView | null>(null);
  // reload resolves with the reloaded view so a section can reconcile its
  // baseline against the canonical values it just wrote.
  const reload = useCallback(async (): Promise<AgentView | null> => {
    if (!name) return null;
    try {
      const next = await agentGetOn<AgentView>(target, name, "");
      setView(next);
      return next;
    } catch {
      setView(null);
      return null;
    }
  }, [name, target]);
  const loadShellScript = useCallback(
    async () => (await getAgentShellScriptOn(target, name)).script,
    [name, target],
  );
  const saveShellScript = useCallback(
    (script: string) => setAgentShellScriptOn(target, name, script),
    [name, target],
  );
  // Deferred a microtask so the initial load is not a synchronous setState
  // inside the effect (the same pattern the Configuration tab uses).
  useEffect(() => {
    void Promise.resolve().then(reload);
  }, [reload]);
  if (!view) return <p className="text-sm text-muted-foreground">Loading…</p>;

  return (
    // A single readable column: DOM order is visual order, so Tab and
    // Shift+Tab move through the sections in the order they are read. The
    // max-w-5xl cap already applies from the Configuration tab wrapper, so no
    // second cap is introduced here.
    <div className="space-y-4">
      <GoalEditor name={name} view={view} reload={reload} target={target} />
      <LoopEditor name={name} view={view} reload={reload} target={target} />
      <MessageEditor name={name} view={view} reload={reload} target={target} />
      {/* No remount key here: a reload must reconcile the Runtime draft rather
          than throw it away, which is what a key would do to a field whose save
          failed. */}
      <RuntimeConfigEditor
        name={name}
        view={view}
        reload={reload}
        target={target}
      />
      <ShellScriptEditor
        title="Agent Shell Script"
        description="Bash commands sourced after the global script before this agent starts an iteration."
        load={loadShellScript}
        save={saveShellScript}
      />
      <SecretsPanel name={name} />
      <RetentionPanel name={name} />
    </div>
  );
}
