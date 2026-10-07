import { useState } from "react";
import { Input } from "@/components/ui/input";
import type { ImageRow } from "@/lib/api";

// Shared building blocks for the agent-create surfaces (single-agent form V3 and
// the group wizard V4). Kept here so both reuse one image picker and one env
// serializer instead of duplicating the logic per page.

export const HARNESSES = ["claude", "codex", "opencode", "cursor", "stub"];

export interface EnvRow { key: string; value: string }

// serializeEnv turns the key/value repeater into the daemon's `K=V,K=V` string,
// dropping rows with a blank key. Values keep their content verbatim.
export function serializeEnv(rows: EnvRow[]): string {
  return rows
    .filter((r) => r.key.trim() !== "")
    .map((r) => `${r.key.trim()}=${r.value}`)
    .join(",");
}

// commaFieldError guards the env/plugins wire format. The daemon parses both the
// `K=V,K=V` env string and the comma-list of plugins by splitting on ',' with no
// escaping (parseKV / parseList in internal/commands/agents.go, shared with the
// CLI --env/--plugins flags). A comma inside an env key/value or a plugin name
// would therefore be mis-split into extra bogus pairs. The format simply cannot
// represent it, so the create forms reject such input up front with a clear
// message instead of serializing a string the daemon silently corrupts. Returns
// null when every field is safe.
export function commaFieldError(rows: EnvRow[], plugins: string[]): string | null {
  for (const r of rows) {
    if (r.key.trim() === "") continue;
    if (r.key.includes(",")) return `env key "${r.key.trim()}": commas are not supported`;
    if (r.value.includes(",")) return `env value for "${r.key.trim()}": commas are not supported`;
  }
  const badPlugin = plugins.find((p) => p.includes(","));
  if (badPlugin) return `plugin "${badPlugin}": commas are not supported`;
  return null;
}

// ImageCombobox is a filterable, single-select picker over the built images.
// The value is the full `name:tag` ref. It is deliberately built on a plain
// input + option list (not radix Select / cmdk) so the required-image gate and
// selection are trivially exercisable in tests. onMouseDown preventDefault
// keeps focus on the input so a click-select fires before onBlur closes the
// list — the same trick PathAutocomplete uses. `ariaLabel` lets the group
// wizard give each row's image field a distinct accessible name.
export function ImageCombobox({
  images, value, onChange, id, invalid, ariaLabel = "image",
}: {
  images: ImageRow[];
  value: string;
  onChange: (v: string) => void;
  id?: string;
  invalid?: boolean;
  ariaLabel?: string;
}) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const filtered = images.filter((im) =>
    `${im.name}:${im.tag}`.toLowerCase().includes(query.toLowerCase()),
  );
  return (
    <div className="relative">
      <Input
        id={id}
        value={open ? query : value}
        onChange={(e) => { setQuery(e.target.value); setOpen(true); }}
        onFocus={() => { setQuery(""); setOpen(true); }}
        onBlur={() => setOpen(false)}
        placeholder="image:tag (required)"
        aria-label={ariaLabel}
        aria-invalid={invalid || undefined}
        className="h-8"
      />
      {open && (
        <div
          role="listbox"
          className="absolute top-full left-0 z-50 mt-1 max-h-64 w-full overflow-auto rounded-lg border bg-popover p-1 shadow-md"
        >
          {filtered.length === 0 && (
            <div className="px-2 py-2 text-sm text-muted-foreground">No images</div>
          )}
          {filtered.map((im) => {
            const ref = `${im.name}:${im.tag}`;
            return (
              <button
                key={ref}
                type="button"
                role="option"
                aria-selected={ref === value}
                onMouseDown={(e) => e.preventDefault()}
                onClick={() => { onChange(ref); setOpen(false); }}
                className="block w-full rounded px-2 py-1.5 text-left text-sm hover:bg-accent"
              >
                {ref}
              </button>
            );
          })}
        </div>
      )}
    </div>
  );
}
