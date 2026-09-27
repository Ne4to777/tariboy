import { useCallback, useEffect, useRef, useState } from "react";
import { toast } from "sonner";
import {
  ApiError,
  agentGetOn,
  createAgent,
  imageManifestGet,
  listImages,
  startAgent,
  type CreateAgentSpec,
  type ImageManifest,
  type ImageRow,
} from "@/lib/api";
import type { AgentView } from "@/lib/types";
import { resolveDaemon, unresolvedDaemon, type Daemon } from "@/lib/daemons";
import { HARNESSES, ImageCombobox } from "@/components/AgentFormFields";
import { EditablePresetCombobox } from "@/components/EditablePresetCombobox";
import { PathAutocomplete } from "@/components/PathAutocomplete";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  rememberRuntimePreset,
  runtimePresetOptions,
} from "@/lib/runtimePresets";
import {
  cloneAgentDraft,
  newAgentDraft,
  type AgentCreateDraft,
  type AgentPolicy,
} from "./agentCreateDraft";

const LOCAL_HOST_VALUE = "__local__";

export interface CloneAgentSource {
  hostId: string;
  agentName: string;
  hostLabel: string;
}

export interface CreateAgentDialogProps {
  open: boolean;
  hostId: string;
  imageRef?: string;
  cloneSource?: CloneAgentSource;
  hosts: { id: string; label: string; revision?: string }[];
  onOpenChange: (open: boolean) => void;
  onCreated: (hostId: string, name: string) => void;
}

function errorMessage(error: unknown): string {
  return error instanceof ApiError ? error.message : String(error);
}

function integerField(raw: string, label: string, minimum: number): number {
  const value = Number(raw.trim());
  if (!Number.isSafeInteger(value) || value < minimum) {
    throw new Error(`${label} must be a whole number of at least ${minimum}`);
  }
  return value;
}

function parseEnvironment(text: string): Record<string, string> {
  let value: unknown;
  try {
    value = JSON.parse(text || "{}");
  } catch {
    throw new Error("Environment must be valid JSON");
  }
  if (
    value === null ||
    Array.isArray(value) ||
    typeof value !== "object" ||
    Object.values(value).some((entry) => typeof entry !== "string")
  ) {
    throw new Error(
      "Environment must be a JSON object whose values are strings",
    );
  }
  return value as Record<string, string>;
}

export function CreateAgentDialog(props: CreateAgentDialogProps) {
  if (!props.open) return null;
  const sourceKey = props.cloneSource
    ? `${props.cloneSource.hostId}\u0000${props.cloneSource.agentName}`
    : "new";
  return (
    <CreateAgentDialogForm
      key={`${sourceKey}\u0000${props.hostId}\u0000${props.imageRef ?? ""}`}
      {...props}
    />
  );
}

function CreateAgentDialogForm({
  open,
  hostId,
  imageRef = "",
  cloneSource,
  hosts,
  onOpenChange,
  onCreated,
}: CreateAgentDialogProps) {
  const [host, setHost] = useState(hostId);
  const [draft, setDraft] = useState<AgentCreateDraft>(() =>
    newAgentDraft(imageRef),
  );
  const [target, setTarget] = useState<Daemon | null>(null);
  const [resolvedRevision, setResolvedRevision] = useState("");
  const [images, setImages] = useState<ImageRow[]>([]);
  const [manifest, setManifest] = useState<ImageManifest | null>(null);
  const [manifestKey, setManifestKey] = useState("");
  const [pluginDraft, setPluginDraft] = useState("");
  const runtimeInitializedForTarget = useRef("");
  const [busy, setBusy] = useState(false);
  const [sourceState, setSourceState] = useState<
    "idle" | "loading" | "ready" | "error"
  >(cloneSource ? "loading" : "idle");
  const [sourceError, setSourceError] = useState("");
  const [formError, setFormError] = useState("");
  const [startError, setStartError] = useState("");
  const [created, setCreated] = useState<{ host: string; name: string } | null>(
    null,
  );

  const selectedHostRevision =
    hosts.find((entry) => entry.id === host)?.revision ?? "missing";
  const targetIsCurrent = resolvedRevision === selectedHostRevision;
  const targetIsUsable = host === "" || target !== null;
  const loadingImages = !targetIsCurrent;
  const targetImageAvailable = images.some(
    (entry) => `${entry.name}:${entry.tag}` === draft.image,
  );
  const expectedManifestKey = `${selectedHostRevision}\u0000${draft.image}`;
  const runtimeTargetKey = `${host}\u0000${draft.image}`;
  const manifestIsCurrent =
    draft.image !== "" &&
    targetImageAvailable &&
    manifestKey === expectedManifestKey &&
    manifest !== null;
  const loadingManifest =
    draft.image !== "" &&
    targetIsCurrent &&
    targetIsUsable &&
    targetImageAvailable &&
    manifestKey !== expectedManifestKey;
  const targetImageError =
    draft.image !== "" &&
    targetIsCurrent &&
    targetIsUsable &&
    !targetImageAvailable
      ? `Image ${draft.image} is not built on the selected host`
      : "";
  const currentManifest = manifestIsCurrent ? manifest : null;
  const bare = currentManifest?.bare === true;
  const schemaV2 = currentManifest?.schema_version === 2;
  const defaultModel =
    currentManifest?.schema_version === 1
      ? (currentManifest.harness?.model ?? "")
      : "";
  const defaultEffort =
    currentManifest?.schema_version === 1
      ? (currentManifest.harness?.effort ?? "")
      : "";
  const modelPresets = runtimePresetOptions(draft.harness, "models", [
    defaultModel,
    draft.model,
  ]);
  const effortPresets = runtimePresetOptions(draft.harness, "efforts", [
    defaultEffort,
    draft.effort,
  ]);
  const displayedPlugins = schemaV2
    ? (currentManifest?.plugins?.map((plugin) => plugin.name) ?? [])
    : draft.plugins;
  const sourceReady = !cloneSource || sourceState === "ready";
  const formDisabled = busy || created !== null || sourceState === "loading";
  const visibleFormError = formError || targetImageError;

  const updateDraft = <K extends keyof AgentCreateDraft>(
    key: K,
    value: AgentCreateDraft[K],
  ) => {
    setDraft((current) => ({ ...current, [key]: value }));
  };

  const fetchSource =
    useCallback(async (): Promise<AgentCreateDraft | null> => {
      if (!cloneSource) return null;
      const sourceTarget = cloneSource.hostId
        ? await resolveDaemon(cloneSource.hostId)
        : null;
      if (cloneSource.hostId && !sourceTarget) {
        throw new Error(`host ${cloneSource.hostId} was not found`);
      }
      const source = await agentGetOn<AgentView>(
        sourceTarget,
        cloneSource.agentName,
        "",
      );
      return cloneAgentDraft(source);
    }, [cloneSource]);

  useEffect(() => {
    let alive = true;
    void fetchSource().then(
      (nextDraft) => {
        if (!alive || !nextDraft) return;
        setDraft(nextDraft);
        setSourceState("ready");
      },
      (error) => {
        if (!alive) return;
        setSourceState("error");
        setSourceError(`Could not load source agent: ${errorMessage(error)}`);
      },
    );
    return () => {
      alive = false;
    };
  }, [fetchSource]);

  const retrySource = () => {
    setSourceState("loading");
    setSourceError("");
    void fetchSource().then(
      (nextDraft) => {
        if (!nextDraft) return;
        setDraft(nextDraft);
        setSourceState("ready");
      },
      (error) => {
        setSourceState("error");
        setSourceError(`Could not load source agent: ${errorMessage(error)}`);
      },
    );
  };

  useEffect(() => {
    let alive = true;
    void (async () => {
      try {
        const nextTarget = host ? await resolveDaemon(host) : null;
        if (host && !nextTarget) throw new Error(`host ${host} was not found`);
        const result = await listImages(nextTarget);
        if (!alive) return;
        setTarget(nextTarget);
        setResolvedRevision(selectedHostRevision);
        setImages(result.images ?? []);
        setFormError("");
      } catch (error) {
        if (!alive) return;
        setTarget(null);
        setResolvedRevision(selectedHostRevision);
        setImages([]);
        setFormError(`Could not load images: ${errorMessage(error)}`);
      }
    })();
    return () => {
      alive = false;
    };
  }, [host, selectedHostRevision]);

  useEffect(() => {
    if (!draft.image || loadingImages || !targetIsCurrent || !targetIsUsable)
      return;
    if (!targetImageAvailable) return;
    let alive = true;
    void imageManifestGet(draft.image, target)
      .then((next) => {
        if (!alive) return;
        setManifest(next);
        setManifestKey(expectedManifestKey);
        setFormError("");
        if (
          !cloneSource &&
          runtimeInitializedForTarget.current !== runtimeTargetKey
        ) {
          runtimeInitializedForTarget.current = runtimeTargetKey;
          setDraft((current) => ({
            ...current,
            harness:
              next.schema_version === 1
                ? (next.harness?.type ?? "")
                : current.harness,
            model:
              next.schema_version === 1
                ? (next.harness?.model ?? "")
                : current.model,
            effort:
              next.schema_version === 1
                ? (next.harness?.effort ?? "")
                : current.effort,
            interactive:
              next.schema_version === 1
                ? (next.harness?.interactive ?? false)
                : false,
          }));
        }
      })
      .catch((error) => {
        if (!alive) return;
        setManifest(null);
        setManifestKey(expectedManifestKey);
        setFormError(`Could not load ${draft.image}: ${errorMessage(error)}`);
      });
    return () => {
      alive = false;
    };
  }, [
    cloneSource,
    draft.image,
    expectedManifestKey,
    loadingImages,
    runtimeTargetKey,
    target,
    targetImageAvailable,
    targetIsCurrent,
    targetIsUsable,
  ]);

  const changeHost = (value: string) => {
    const next = value === LOCAL_HOST_VALUE ? "" : value;
    if (next === host) return;
    setHost(next);
    setTarget(null);
    setResolvedRevision("");
    setImages([]);
    setManifest(null);
    setManifestKey("");
    setFormError("");
    if (!cloneSource) updateDraft("image", "");
  };

  const changeImage = (image: string) => {
    updateDraft("image", image);
    setManifest(null);
    setManifestKey("");
    setFormError("");
  };

  const addPlugin = () => {
    const plugin = pluginDraft.trim();
    if (plugin && !draft.plugins.includes(plugin)) {
      updateDraft("plugins", [...draft.plugins, plugin]);
    }
    setPluginDraft("");
  };

  const submit = async () => {
    if (
      !sourceReady ||
      !draft.image ||
      !targetIsCurrent ||
      !targetIsUsable ||
      !manifestIsCurrent
    ) {
      setFormError(
        "Wait for the source, selected host, and image to finish loading",
      );
      return;
    }
    let env: Record<string, string>;
    let intervalS: number;
    let timeoutS: number;
    let hardTimeoutS: number;
    let maxIdleIterations: number;
    let aiStallTimeoutS: number;
    let messagesBatch: number;
    let messagesMaxQueue: number;
    let goalWaitCustomerTimeoutS: number;
    let goalDeliveryCooldownS: number;
    try {
      env = parseEnvironment(draft.envText);
      intervalS = integerField(draft.intervalS, "Interval seconds", 0);
      timeoutS = integerField(draft.timeoutS, "Soft timeout seconds", 0);
      hardTimeoutS = integerField(
        draft.hardTimeoutS,
        "Hard timeout seconds",
        0,
      );
      maxIdleIterations = integerField(
        draft.maxIdleIterations,
        "Maximum idle iterations",
        0,
      );
      aiStallTimeoutS = integerField(
        draft.aiStallTimeoutS,
        "AI inactivity timeout seconds",
        1,
      );
      messagesBatch = integerField(
        draft.messagesBatch,
        "Message batch size",
        1,
      );
      messagesMaxQueue = integerField(
        draft.messagesMaxQueue,
        "Maximum queued messages",
        1,
      );
      goalWaitCustomerTimeoutS = integerField(
        draft.goalWaitCustomerTimeoutS,
        "Wait customer timeout seconds",
        1,
      );
      goalDeliveryCooldownS = integerField(
        draft.goalDeliveryCooldownS,
        "Goal delivery cooldown seconds",
        1,
      );
    } catch (error) {
      setFormError(errorMessage(error));
      return;
    }

    const spec: CreateAgentSpec = {
      image: draft.image,
      cwd: draft.cwd.trim(),
      harness: draft.harness.trim(),
      model: draft.model.trim(),
      effort: draft.effort.trim(),
      interactive: bare ? true : draft.interactive,
      loop: bare ? false : draft.loop,
      env,
      interval_s: intervalS,
      timeout_s: timeoutS,
      hard_timeout_s: hardTimeoutS,
      on_timeout: draft.onTimeout,
      on_error: draft.onError,
      max_idle_iterations: maxIdleIterations,
      ai_stall_timeout_s: aiStallTimeoutS,
      user_prompt: draft.userPrompt,
      messages_batch: messagesBatch,
      messages_max_queue: messagesMaxQueue,
      goal_enabled: draft.goalEnabled,
      goal_wait_customer_timeout_s: goalWaitCustomerTimeoutS,
      goal_delivery_cooldown_s: goalDeliveryCooldownS,
      group: draft.group.trim(),
      alias: draft.alias,
      notes: draft.notes,
      color: draft.color.trim(),
    };
    if (draft.name.trim()) spec.name = draft.name.trim();
    if (!schemaV2) spec.plugins = [...draft.plugins];

    setBusy(true);
    setFormError("");
    setStartError("");
    try {
      const result = await createAgent(spec, target);
      if (draft.model.trim() && draft.model.trim() !== defaultModel) {
        rememberRuntimePreset(draft.harness, "models", draft.model);
      }
      if (draft.effort.trim() && draft.effort.trim() !== defaultEffort) {
        rememberRuntimePreset(draft.harness, "efforts", draft.effort);
      }
      const nextCreated = { host, name: result.name };
      setCreated(nextCreated);
      onCreated(host, result.name);
      if (!draft.startNow) {
        onOpenChange(false);
        return;
      }
      try {
        await startAgent(result.name, target);
        onOpenChange(false);
      } catch (error) {
        setStartError(
          `Agent created but could not be started: ${errorMessage(error)}. Check the host and retry start.`,
        );
      }
    } catch (error) {
      const message = errorMessage(error);
      setFormError(message);
      toast.error(`create failed: ${message}`);
    } finally {
      setBusy(false);
    }
  };

  const retryStart = async () => {
    if (!created) return;
    setBusy(true);
    setStartError("");
    try {
      await startAgent(created.name, target);
      onOpenChange(false);
    } catch (error) {
      setStartError(
        `Agent is still stopped: ${errorMessage(error)}. Check the host and retry start.`,
      );
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>{cloneSource ? "Clone agent" : "New agent"}</DialogTitle>
          <DialogDescription>
            {cloneSource
              ? `Copy configuration from ${cloneSource.agentName} on ${cloneSource.hostLabel}. Choose a unique name and review every field.`
              : "Choose a target and image, then configure identity, runtime, Autopilot, and lifecycle settings."}
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-4">
          {sourceState === "loading" && (
            <p className="text-xs text-muted-foreground">
              Loading source configuration…
            </p>
          )}
          {sourceState === "error" && (
            <div
              role="alert"
              className="space-y-2 rounded border border-destructive p-3"
            >
              <p className="text-sm text-destructive">{sourceError}</p>
              <Button size="sm" variant="outline" onClick={retrySource}>
                Retry source
              </Button>
            </div>
          )}

          <section
            aria-labelledby="create-agent-target"
            className="space-y-3 rounded border p-3"
          >
            <h3 id="create-agent-target" className="font-medium">
              Target
            </h3>
            <div className="grid gap-3 sm:grid-cols-2">
              <div className="space-y-1">
                <Label htmlFor="create-agent-host">Host</Label>
                <Select
                  value={host || LOCAL_HOST_VALUE}
                  onValueChange={changeHost}
                  disabled={formDisabled}
                >
                  <SelectTrigger id="create-agent-host" className="h-8">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {hosts.map((entry) => (
                      <SelectItem
                        key={entry.id || LOCAL_HOST_VALUE}
                        value={entry.id || LOCAL_HOST_VALUE}
                      >
                        {entry.label}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-1">
                <div className="flex items-center gap-2">
                  <Label htmlFor="create-agent-image">Image</Label>
                  {bare && <Badge variant="secondary">Terminal only</Badge>}
                </div>
                <ImageCombobox
                  id="create-agent-image"
                  images={images}
                  value={draft.image}
                  onChange={changeImage}
                  ariaLabel="image"
                />
                {loadingImages && (
                  <p className="text-xs text-muted-foreground">
                    Loading built images…
                  </p>
                )}
              </div>
            </div>
          </section>

          <section
            aria-labelledby="create-agent-identity"
            className="space-y-3 rounded border p-3"
          >
            <h3 id="create-agent-identity" className="font-medium">
              Identity
            </h3>
            <div className="grid gap-3 sm:grid-cols-2">
              <Field label="name" id="create-agent-name">
                <Input
                  id="create-agent-name"
                  aria-label="name"
                  value={draft.name}
                  onChange={(event) => updateDraft("name", event.target.value)}
                  placeholder="empty = generated"
                  disabled={formDisabled}
                  className="h-8"
                />
              </Field>
              <Field label="alias" id="create-agent-alias">
                <Input
                  id="create-agent-alias"
                  aria-label="alias"
                  value={draft.alias}
                  onChange={(event) => updateDraft("alias", event.target.value)}
                  disabled={formDisabled}
                  className="h-8"
                />
              </Field>
              <Field label="group" id="create-agent-group">
                <Input
                  id="create-agent-group"
                  aria-label="group"
                  value={draft.group}
                  onChange={(event) => updateDraft("group", event.target.value)}
                  disabled={formDisabled}
                  className="h-8"
                />
              </Field>
              <Field label="color" id="create-agent-color">
                <Input
                  id="create-agent-color"
                  aria-label="color"
                  value={draft.color}
                  onChange={(event) => updateDraft("color", event.target.value)}
                  placeholder="#rrggbb"
                  disabled={formDisabled}
                  className="h-8"
                />
              </Field>
            </div>
            <Field label="notes" id="create-agent-notes">
              <Textarea
                id="create-agent-notes"
                aria-label="notes"
                value={draft.notes}
                onChange={(event) => updateDraft("notes", event.target.value)}
                disabled={formDisabled}
              />
            </Field>
          </section>

          <section
            aria-labelledby="create-agent-runtime"
            className="space-y-3 rounded border p-3"
          >
            <div>
              <h3 id="create-agent-runtime" className="font-medium">
                Runtime
              </h3>
              <p className="text-xs text-muted-foreground">
                Runtime settings stay editable independently of image content.
              </p>
            </div>
            <div className="grid gap-3 sm:grid-cols-2">
              <Field label="Harness" id="create-agent-harness">
                <select
                  id="create-agent-harness"
                  aria-label="harness"
                  value={draft.harness}
                  onChange={(event) =>
                    updateDraft("harness", event.target.value)
                  }
                  disabled={formDisabled || loadingManifest}
                  className="h-8 w-full rounded-lg border border-input bg-background px-2.5 text-sm"
                >
                  <option value="">image default</option>
                  {HARNESSES.map((value) => (
                    <option key={value} value={value}>
                      {value}
                    </option>
                  ))}
                </select>
              </Field>
              <Field label="Effort" id="create-agent-effort">
                <EditablePresetCombobox
                  id="create-agent-effort"
                  ariaLabel="effort"
                  value={draft.effort}
                  options={effortPresets}
                  onChange={(value) => updateDraft("effort", value)}
                  placeholder="image default"
                  disabled={formDisabled || loadingManifest}
                />
              </Field>
              <div className="sm:col-span-2">
                <Field label="Model" id="create-agent-model">
                  <EditablePresetCombobox
                    id="create-agent-model"
                    ariaLabel="model"
                    value={draft.model}
                    options={modelPresets}
                    onChange={(value) => updateDraft("model", value)}
                    placeholder="image default"
                    disabled={formDisabled || loadingManifest}
                  />
                </Field>
              </div>
              <div className="sm:col-span-2">
                <Field label="cwd" id="create-agent-cwd">
                  <PathAutocomplete
                    id="create-agent-cwd"
                    value={draft.cwd}
                    onChange={(value) => updateDraft("cwd", value)}
                    daemon={
                      host === ""
                        ? null
                        : targetIsCurrent && target
                          ? target
                          : unresolvedDaemon(
                              host,
                              hosts.find((entry) => entry.id === host)?.label,
                            )
                    }
                    placeholder="empty = managed workdir"
                    aria-label="cwd"
                  />
                </Field>
              </div>
            </div>
            <SwitchRow
              label="Interactive"
              id="create-agent-interactive"
              checked={bare ? true : draft.interactive}
              onCheckedChange={(value) => updateDraft("interactive", value)}
              disabled={formDisabled || loadingManifest || bare}
              help="Attach a terminal console."
            />
            <Field label="environment JSON" id="create-agent-env">
              <Textarea
                id="create-agent-env"
                aria-label="environment JSON"
                value={draft.envText}
                onChange={(event) => updateDraft("envText", event.target.value)}
                disabled={formDisabled}
                className="min-h-28 font-mono text-xs"
              />
            </Field>
            <div className="space-y-2">
              <Label>plugins</Label>
              <div className="flex flex-wrap gap-1">
                {displayedPlugins.length === 0 && (
                  <span className="text-xs text-muted-foreground">none</span>
                )}
                {displayedPlugins.map((plugin) => (
                  <Badge key={plugin} variant="secondary" className="gap-1">
                    {plugin}
                    {!schemaV2 && (
                      <button
                        type="button"
                        aria-label={`remove ${plugin}`}
                        onClick={() =>
                          updateDraft(
                            "plugins",
                            draft.plugins.filter((value) => value !== plugin),
                          )
                        }
                      >
                        ×
                      </button>
                    )}
                  </Badge>
                ))}
              </div>
              {schemaV2 ? (
                <p className="text-xs text-muted-foreground">
                  Plugins are owned by this schema-v2 image.
                </p>
              ) : (
                <div className="flex gap-2">
                  <Input
                    aria-label="plugin name"
                    value={pluginDraft}
                    onChange={(event) => setPluginDraft(event.target.value)}
                    onKeyDown={(event) => {
                      if (event.key === "Enter") {
                        event.preventDefault();
                        addPlugin();
                      }
                    }}
                    disabled={formDisabled}
                    className="h-8"
                  />
                  <Button
                    type="button"
                    size="sm"
                    variant="outline"
                    onClick={addPlugin}
                    disabled={formDisabled}
                  >
                    Add plugin
                  </Button>
                </div>
              )}
            </div>
          </section>

          <section
            aria-labelledby="create-agent-autopilot-section"
            className="space-y-3 rounded border p-3"
          >
            <h3 id="create-agent-autopilot-section" className="font-medium">
              Autopilot
            </h3>
            <SwitchRow
              label="Autopilot"
              id="create-agent-autopilot"
              checked={bare ? false : draft.loop}
              onCheckedChange={(value) => updateDraft("loop", value)}
              disabled={formDisabled || loadingManifest || bare}
              help={
                bare
                  ? "A terminal-only image does not contain agent instructions or tools."
                  : "Enable loop and event-driven work."
              }
            />
            <div className="grid gap-3 sm:grid-cols-2">
              <NumberField
                label="interval seconds"
                id="create-agent-interval"
                value={draft.intervalS}
                onChange={(value) => updateDraft("intervalS", value)}
                disabled={formDisabled}
                min={0}
              />
              <NumberField
                label="soft timeout seconds"
                id="create-agent-timeout"
                value={draft.timeoutS}
                onChange={(value) => updateDraft("timeoutS", value)}
                disabled={formDisabled}
                min={0}
              />
              <NumberField
                label="hard timeout seconds"
                id="create-agent-hard-timeout"
                value={draft.hardTimeoutS}
                onChange={(value) => updateDraft("hardTimeoutS", value)}
                disabled={formDisabled}
                min={0}
              />
              <PolicyField
                label="timeout policy"
                id="create-agent-on-timeout"
                value={draft.onTimeout}
                onChange={(value) => updateDraft("onTimeout", value)}
                disabled={formDisabled}
              />
              <PolicyField
                label="error policy"
                id="create-agent-on-error"
                value={draft.onError}
                onChange={(value) => updateDraft("onError", value)}
                disabled={formDisabled}
              />
              <NumberField
                label="maximum idle iterations"
                id="create-agent-max-idle"
                value={draft.maxIdleIterations}
                onChange={(value) => updateDraft("maxIdleIterations", value)}
                disabled={formDisabled}
                min={0}
              />
              <NumberField
                label="message batch size"
                id="create-agent-message-batch"
                value={draft.messagesBatch}
                onChange={(value) => updateDraft("messagesBatch", value)}
                disabled={formDisabled}
                min={1}
              />
              <NumberField
                label="maximum queued messages"
                id="create-agent-message-queue"
                value={draft.messagesMaxQueue}
                onChange={(value) => updateDraft("messagesMaxQueue", value)}
                disabled={formDisabled}
                min={1}
              />
            </div>
            <SwitchRow
              label="Goal"
              id="create-agent-goal"
              checked={draft.goalEnabled}
              onCheckedChange={(value) => updateDraft("goalEnabled", value)}
              disabled={formDisabled}
              help="Select and deliver the agent's current Native Task goal."
            />
            <NumberField
              label="wait customer timeout seconds"
              id="create-agent-goal-wait-customer-timeout"
              value={draft.goalWaitCustomerTimeoutS}
              onChange={(value) =>
                updateDraft("goalWaitCustomerTimeoutS", value)
              }
              disabled={formDisabled}
              min={1}
            />
            <NumberField
              label="goal delivery cooldown seconds"
              id="create-agent-goal-delivery-cooldown"
              value={draft.goalDeliveryCooldownS}
              onChange={(value) => updateDraft("goalDeliveryCooldownS", value)}
              disabled={formDisabled}
              min={1}
            />
            <Field label="standing user prompt" id="create-agent-user-prompt">
              <Textarea
                id="create-agent-user-prompt"
                aria-label="standing user prompt"
                value={draft.userPrompt}
                onChange={(event) =>
                  updateDraft("userPrompt", event.target.value)
                }
                disabled={formDisabled}
              />
            </Field>
          </section>

          <section
            aria-labelledby="create-agent-lifecycle"
            className="space-y-3 rounded border p-3"
          >
            <h3 id="create-agent-lifecycle" className="font-medium">
              Lifecycle
            </h3>
            <SwitchRow
              label="Start now"
              id="create-agent-start"
              checked={draft.startNow}
              onCheckedChange={(value) => updateDraft("startNow", value)}
              disabled={formDisabled}
              help="Off creates the agent in stopped state."
            />
          </section>

          {visibleFormError && (
            <p role="alert" className="text-sm text-destructive">
              {visibleFormError}
            </p>
          )}
          {startError && (
            <div
              role="alert"
              className="space-y-2 rounded border border-destructive p-3"
            >
              <p className="text-sm text-destructive">{startError}</p>
              <Button
                disabled={busy || !targetIsCurrent || !targetIsUsable}
                size="sm"
                onClick={() => void retryStart()}
              >
                Retry start
              </Button>
            </div>
          )}
        </div>

        <DialogFooter>
          {!created && (
            <Button
              disabled={
                busy ||
                !sourceReady ||
                loadingImages ||
                loadingManifest ||
                !targetIsCurrent ||
                !targetIsUsable ||
                !draft.image ||
                !manifestIsCurrent
              }
              onClick={() => void submit()}
            >
              {busy ? "Creating…" : "Create agent"}
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function Field({
  label,
  id,
  children,
}: {
  label: string;
  id: string;
  children: React.ReactNode;
}) {
  return (
    <div className="space-y-1">
      <Label htmlFor={id}>{label}</Label>
      {children}
    </div>
  );
}

function SwitchRow({
  label,
  id,
  checked,
  onCheckedChange,
  disabled,
  help,
}: {
  label: string;
  id: string;
  checked: boolean;
  onCheckedChange: (value: boolean) => void;
  disabled: boolean;
  help: string;
}) {
  return (
    <div className="flex items-center justify-between gap-3">
      <div>
        <Label htmlFor={id}>{label}</Label>
        <p className="text-xs text-muted-foreground">{help}</p>
      </div>
      <Switch
        id={id}
        checked={checked}
        onCheckedChange={onCheckedChange}
        disabled={disabled}
      />
    </div>
  );
}

function NumberField({
  label,
  id,
  value,
  onChange,
  disabled,
  min,
}: {
  label: string;
  id: string;
  value: string;
  onChange: (value: string) => void;
  disabled: boolean;
  min: number;
}) {
  return (
    <Field label={label} id={id}>
      <Input
        id={id}
        aria-label={label}
        type="number"
        min={min}
        step={1}
        value={value}
        onChange={(event) => onChange(event.target.value)}
        disabled={disabled}
        className="h-8"
      />
    </Field>
  );
}

function PolicyField({
  label,
  id,
  value,
  onChange,
  disabled,
}: {
  label: string;
  id: string;
  value: AgentPolicy;
  onChange: (value: AgentPolicy) => void;
  disabled: boolean;
}) {
  return (
    <Field label={label} id={id}>
      <select
        id={id}
        aria-label={label}
        value={value}
        onChange={(event) => onChange(event.target.value as AgentPolicy)}
        disabled={disabled}
        className="h-8 w-full rounded-lg border border-input bg-background px-2.5 text-sm"
      >
        <option value="restart">restart</option>
        <option value="stop">stop</option>
      </select>
    </Field>
  );
}
