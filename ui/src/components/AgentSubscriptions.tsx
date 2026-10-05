import { useEffect, useMemo, useState } from "react";
import { toast } from "sonner";
import { apiGet, apiPost, apiDelete, ApiError } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";
import {
  Command, CommandInput, CommandItem, CommandList,
} from "@/components/ui/command";

interface Sub { name: string; kind: string; protected: boolean }
interface Chan { name: string; kind: string }

const errmsg = (e: unknown) => (e instanceof ApiError ? e.message : String(e));

// validChannel mirrors the daemon's bus.ValidChannel (internal/bus/bus.go): a
// known kind prefix plus every ':'-separated segment matching the name rule. It
// gates free-text input client-side so a typo never POSTs a junk channel — the
// daemon still re-validates, this just surfaces the error inline before submit.
const CHANNEL_SEG = /^[a-z0-9][a-z0-9_-]*$/;
const CHANNEL_PREFIXES = new Set(["agent", "group", "user", "chat", "plugin", "system"]);
export function validChannel(name: string): boolean {
  if (!name || !name.includes(":")) return false;
  const parts = name.split(":");
  if (!CHANNEL_PREFIXES.has(parts[0])) return false;
  return parts.every((p) => CHANNEL_SEG.test(p));
}

// AgentSubscriptions manages the per-agent bus subscriptions: protected rows
// (own inbox, group:*) are read-only; ad-hoc rows get an Unsubscribe button. The
// Subscribe control is an editable combobox — it offers every existing channel
// the agent is not already subscribed to (including bound-but-idle chats, which
// GET /api/channels now surfaces), and also accepts a free-text channel name so
// an operator can subscribe to a channel that is not yet in the list.
export function AgentSubscriptions(
  { name, selected = null, onSelect }:
  { name: string; selected?: string | null; onSelect?: (ch: string) => void },
) {
  const [subs, setSubs] = useState<Sub[]>([]);
  const [channels, setChannels] = useState<Chan[]>([]);
  const [pick, setPick] = useState("");
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);

  const load = () => {
    apiGet<{ channels: Sub[] }>(`/api/agents/${encodeURIComponent(name)}/subscriptions`)
      .then((r) => setSubs(r.channels)).catch(() => setSubs([]));
    apiGet<{ channels: Chan[] }>("/api/channels")
      .then((r) => setChannels(r.channels)).catch(() => setChannels([]));
  };
  useEffect(() => { load(); }, [name]);

  // Channels not already subscribed, filtered by what the operator has typed so
  // the dropdown narrows as a substring match (case-insensitive).
  const options = useMemo(() => {
    const subscribed = new Set(subs.map((s) => s.name));
    const q = pick.trim().toLowerCase();
    return channels.filter((c) => !subscribed.has(c.name) && c.name.toLowerCase().includes(q));
  }, [channels, subs, pick]);

  const trimmed = pick.trim();
  const invalid = trimmed !== "" && !validChannel(trimmed);

  const run = async (label: string, fn: () => Promise<unknown>) => {
    setBusy(true);
    try { await fn(); toast.success(label); load(); }
    catch (e) { toast.error(`${label} failed: ${errmsg(e)}`); }
    finally { setBusy(false); }
  };

  const subscribe = () => {
    if (!trimmed || invalid) return;
    void run("subscribed", () => apiPost(`/api/agents/${encodeURIComponent(name)}/subscriptions`, { channel: trimmed }))
      .then(() => { setPick(""); setOpen(false); });
  };
  const unsubscribe = (ch: string) =>
    void run("unsubscribed", () => apiDelete(`/api/agents/${encodeURIComponent(name)}/subscriptions?channel=${encodeURIComponent(ch)}`));

  return (
    <div className="w-2/5 min-w-[12rem] shrink-0 overflow-auto">
      <div className="p-2 text-xs font-medium text-muted-foreground">SUBSCRIPTIONS</div>
      {/* These are the agent's event triggers: Autopilot links here rather
          than keeping a second editor. */}
      <p className="px-2 pb-2 text-[11.5px] leading-[1.45] text-muted-foreground">
        Every message on these channels wakes this agent in Autopilot.
      </p>
      {subs.map((s) => (
        // The channel name is a native <button>, so keyboard users can activate
        // it (Enter/Space select the row → populate the right pane) with no
        // custom key handling. The Unsubscribe control is a SIBLING of that
        // button, never nested inside it: an interactive element may not contain
        // another (invalid ARIA), which is why the row wrapper itself is a plain,
        // non-interactive container that only carries the hover/selected accent.
        <div key={s.name}
          className={cn("flex items-center gap-2 px-2 py-1.5 text-sm hover:bg-accent",
            selected === s.name && "bg-accent")}>
          <button type="button" onClick={() => onSelect?.(s.name)}
            className="flex min-w-0 flex-1 cursor-pointer items-center text-left">
            <span className="truncate font-mono text-xs">{s.name}</span>
          </button>
          {s.protected ? (
            <Badge variant="outline" className="ml-auto shrink-0" title="managed by system/group">🔒</Badge>
          ) : (
            <Button size="sm" variant="outline" className="ml-auto h-6 shrink-0" disabled={busy}
              onClick={() => unsubscribe(s.name)}>Unsubscribe</Button>
          )}
        </div>
      ))}
      {subs.length === 0 && <p className="p-2 text-sm text-muted-foreground">No subscriptions.</p>}
      <div className="mt-2 flex flex-col gap-2 border-t p-2">
        <Command shouldFilter={false} className="relative overflow-visible bg-transparent">
          <CommandInput
            value={pick}
            onValueChange={(v) => { setPick(v); setOpen(true); }}
            onFocus={() => setOpen(true)}
            // Close on blur so the dropdown does not linger; item selection uses
            // onMouseDown/preventDefault to fire before this blur closes the list.
            onBlur={() => setOpen(false)}
            onKeyDown={(e) => { if (e.key === "Enter") { e.preventDefault(); e.stopPropagation(); subscribe(); } }}
            placeholder="pick or type a channel…"
            aria-label="channel"
            aria-invalid={invalid}
          />
          {open && options.length > 0 && (
            <CommandList
              className="absolute top-full left-0 z-50 mt-1 w-full rounded-lg border bg-popover shadow-md"
            >
              {options.map((c) => (
                <CommandItem key={c.name} value={c.name}
                  onMouseDown={(ev) => ev.preventDefault()}
                  onSelect={() => { setPick(c.name); setOpen(false); }}>
                  <span className="truncate font-mono text-xs">{c.name}</span>
                  {c.kind === "chat" && (
                    <Badge variant="outline" className="ml-auto shrink-0">chat</Badge>
                  )}
                </CommandItem>
              ))}
            </CommandList>
          )}
        </Command>
        {invalid && (
          <p role="alert" className="text-xs text-destructive">
            Invalid channel name — use prefix:segment (e.g. chat:room), lowercase a–z, 0–9, _ or -.
          </p>
        )}
        <Button size="sm" disabled={busy || !trimmed || invalid} onClick={subscribe}>Subscribe</Button>
      </div>
    </div>
  );
}
