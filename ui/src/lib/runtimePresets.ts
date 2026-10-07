export type RuntimePresetField = "models" | "efforts";

export const RUNTIME_PRESETS_STORAGE_KEY = "tariboy:runtime-presets:v1";
export const EFFORT_PRESETS = [
  "low",
  "medium",
  "high",
  "xhigh",
  "max",
  "ultracode",
] as const;
export const MODEL_PRESETS_BY_HARNESS: Readonly<
  Record<string, readonly string[]>
> = {
  claude: [
    "claude-opus-4-8",
    "claude-sonnet-5",
    "claude-haiku-4-5",
    "claude-fable-5",
  ],
  codex: ["gpt-5"],
  opencode: [],
  cursor: [
    "auto",
    "gpt-5.3-codex-high",
    "gpt-5.2",
    "composer-2.5",
    "claude-opus-5-thinking-high",
    "gpt-5.6-sol-high",
    "claude-fable-5-thinking-high",
    "cursor-grok-4.5-high",
    "gemini-3.7-flash-high",
    "claude-sonnet-5-thinking-high",
    "gpt-5.6-luna-high",
    "grok-4.7-high",
  ],
  stub: [],
};

const LEARNED_PRESET_LIMIT = 20;

type LearnedHarnessPresets = Partial<Record<RuntimePresetField, string[]>>;
type LearnedRuntimePresets = Record<string, LearnedHarnessPresets>;

function storage(): Storage | null {
  try {
    return globalThis.localStorage ?? null;
  } catch {
    return null;
  }
}

function stringValues(value: unknown): string[] {
  if (!Array.isArray(value)) return [];
  return value
    .filter((entry): entry is string => typeof entry === "string")
    .map((entry) => entry.trim())
    .filter(Boolean)
    .slice(-LEARNED_PRESET_LIMIT);
}

function loadLearnedPresets(): LearnedRuntimePresets {
  try {
    const raw = storage()?.getItem(RUNTIME_PRESETS_STORAGE_KEY);
    if (!raw) return {};
    const parsed: unknown = JSON.parse(raw);
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed))
      return {};

    const learned: LearnedRuntimePresets = {};
    for (const [harness, value] of Object.entries(parsed)) {
      if (!value || typeof value !== "object" || Array.isArray(value)) continue;
      const entry = value as Record<string, unknown>;
      const models = stringValues(entry.models);
      const efforts = stringValues(entry.efforts);
      if (models.length || efforts.length)
        learned[harness] = { models, efforts };
    }
    return learned;
  } catch {
    return {};
  }
}

function builtInPresets(
  harness: string,
  field: RuntimePresetField,
): readonly string[] {
  return field === "models"
    ? (MODEL_PRESETS_BY_HARNESS[harness] ?? [])
    : EFFORT_PRESETS;
}

function uniqueTrimmed(groups: readonly (readonly string[])[]): string[] {
  const seen = new Set<string>();
  const result: string[] = [];
  for (const group of groups) {
    for (const raw of group) {
      const value = raw.trim();
      if (!value || seen.has(value)) continue;
      seen.add(value);
      result.push(value);
    }
  }
  return result;
}

export function runtimePresetOptions(
  harness: string,
  field: RuntimePresetField,
  extras: readonly string[] = [],
): string[] {
  const learned = loadLearnedPresets()[harness]?.[field] ?? [];
  return uniqueTrimmed([builtInPresets(harness, field), learned, extras]);
}

export function rememberRuntimePreset(
  harness: string,
  field: RuntimePresetField,
  rawValue: string,
): void {
  const value = rawValue.trim();
  if (!harness || !value || builtInPresets(harness, field).includes(value))
    return;

  const learned = loadLearnedPresets();
  const current = learned[harness]?.[field] ?? [];
  const next = [...current.filter((entry) => entry !== value), value].slice(
    -LEARNED_PRESET_LIMIT,
  );
  learned[harness] = {
    ...learned[harness],
    [field]: next,
  };

  try {
    storage()?.setItem(RUNTIME_PRESETS_STORAGE_KEY, JSON.stringify(learned));
  } catch {
    // Presets are a convenience. Storage failures must not block creation.
  }
}
