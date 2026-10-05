import { useCallback, useEffect, useState, type ReactNode } from "react";
import { toast } from "sonner";
import { useAgentName } from "@/lib/agent";
import {
  ApiError,
  agentInboxClear,
  agentInboxList,
  agentInboxProcessed,
  agentInboxReply,
  agentInboxRequeue,
  subscribeAgentEvents,
  type InboxItem,
  type InboxStatus,
} from "@/lib/api";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { fmtDateTime } from "@/lib/time";

// The three sub-views map onto the P5 inbox status filter. Queue is the default.
type View = "queue" | "archive" | "dlq";
const VIEWS: [View, string][] = [["queue", "Queue"], ["archive", "Archive"], ["dlq", "DLQ"]];
const VIEW_STATUS: Record<View, InboxStatus> = {
  queue: "pending",
  archive: "processed",
  dlq: "dlq",
};

// A pending Mark-processed / Reply dialog. A processed dialog without an item
// applies to the captured pending queue.
type DialogState =
  | { mode: "clear" }
  | { mode: "processed"; item: InboxItem }
  | { mode: "reply"; item: InboxItem }
  | null;

// One inbox row. Actions differ per view: Queue rows can be processed or
// replied to, DLQ rows requeued; Archive rows are read-only and additionally
// show their result + processed time.
function MessageRow({
  item,
  view,
  expanded,
  onToggle,
  onProcess,
  onReply,
  onRequeue,
}: {
  item: InboxItem;
  view: View;
  expanded: boolean;
  onToggle: () => void;
  onProcess: () => void;
  onReply: () => void;
  onRequeue: () => void;
}) {
  const kind = item.kind && item.kind !== "event" ? item.kind : "";
  const hasDetails = !!item.subject || !!item.data;
  return (
    <div className="border-b px-4 py-2 last:border-0">
      <div className="flex items-start gap-2">
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
            <span className="font-mono" title={item.ts}>{fmtDateTime(item.ts)}</span>
            <span>·</span>
            <span className="font-mono">{item.source || "?"}</span>
            <Badge variant="secondary">{item.type || "note"}</Badge>
            {kind && <Badge>{kind}</Badge>}
            {item.attempts > 1 && <span>attempts: {item.attempts}</span>}
            {item.in_reply_to && (
              <span className="font-mono">↩ {item.in_reply_to}</span>
            )}
          </div>
          <div className="mt-1 text-sm break-words whitespace-pre-wrap">{item.text}</div>
          {view === "archive" && item.processed_at && (
            <div className="mt-1 text-xs text-muted-foreground">
              <span className="font-mono" title={item.processed_at}>
                processed {fmtDateTime(item.processed_at)}
              </span>
              {item.result && <span className="ml-2">— {item.result}</span>}
            </div>
          )}
          {hasDetails && (
            <button
              onClick={onToggle}
              className="mt-1 text-xs text-muted-foreground underline-offset-2 hover:underline"
            >
              {expanded ? "hide details" : "show details"}
            </button>
          )}
          {expanded && hasDetails && (
            <pre className="mt-1 overflow-auto rounded border bg-muted/40 p-2 text-xs">
              {JSON.stringify({ subject: item.subject, data: item.data }, null, 2)}
            </pre>
          )}
        </div>
        <div className="flex shrink-0 gap-1">
          {view === "queue" && (
            <>
              <Button size="sm" variant="outline" onClick={onProcess}>Mark processed</Button>
              <Button size="sm" variant="outline" onClick={onReply}>Reply</Button>
            </>
          )}
          {view === "dlq" && (
            <Button size="sm" variant="outline" onClick={onRequeue}>Requeue</Button>
          )}
        </div>
      </div>
    </div>
  );
}

// Per-agent delivery queue (Chat → Queue): Queue | Archive | DLQ sub-views over
// the P5 inbox endpoints. `leading` is the caller's view switch, rendered at the
// start of the same 48px toolbar as the sub-views. All lists are newest-first
// (the backend orders them). Operator actions (mark-processed, reply, requeue)
// mutate then reload the active view; live refresh rides the SSE `message`
// stream with a poll fallback.
export default function AgentMessages({ leading }: { leading?: ReactNode } = {}) {
  const name = useAgentName();
  const [view, setView] = useState<View>("queue");
  const [page, setPage] = useState<{ view: View; items: InboxItem[] }>({ view: "queue", items: [] });
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const [dialog, setDialog] = useState<DialogState>(null);
  const [dialogText, setDialogText] = useState("");
  const [busy, setBusy] = useState(false);

  const load = useCallback(
    (v: View) =>
      agentInboxList(name, VIEW_STATUS[v])
        .then((r) => setPage({ view: v, items: r.messages ?? [] }))
        .catch(() => { /* keep last rows on a transient failure */ }),
    [name],
  );
  const items = page.view === view ? page.items : [];

  // Reload the active view; refresh on SSE `message` events with a 3s poll
  // fallback (the SSE hub drops on a full buffer, so it is only a hint).
  useEffect(() => {
    if (!name) return;
    void load(view);
    const t = window.setInterval(() => void load(view), 3000);
    const off = subscribeAgentEvents(name, ["message"], () => void load(view));
    return () => { window.clearInterval(t); off(); };
  }, [name, view, load]);

  const toggle = (id: string) =>
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id); else next.add(id);
      return next;
    });

  const openDialog = (mode: "processed" | "reply", item: InboxItem) => {
    setDialogText("");
    setDialog({ mode, item });
  };

  const openBulkDialog = () => {
    setDialog({ mode: "clear" });
  };

  const submitDialog = async () => {
    if (!dialog) return;
    const text = dialogText.trim();
    if (dialog.mode !== "clear" && !text) {
      toast.error(dialog.mode === "processed" ? "result is required" : "reply text is required");
      return;
    }
    setBusy(true);
    try {
      if (dialog.mode === "clear") {
        const result = await agentInboxClear(name);
        toast.success(`deleted ${result.deleted_deliveries} deliveries and ${result.deleted_messages} messages`);
      } else if (dialog.mode === "processed") {
        await agentInboxProcessed(name, dialog.item.id, text);
        toast.success("marked processed");
      } else {
        await agentInboxReply(name, dialog.item.id, text);
        toast.success("replied");
      }
      setDialog(null);
    } catch (e) {
      toast.error(`failed: ${e instanceof ApiError ? e.message : String(e)}`);
    } finally {
      await load(view);
      setBusy(false);
    }
  };

  const requeue = async (item: InboxItem) => {
    try {
      await agentInboxRequeue(name, item.id);
      toast.success("requeued");
      await load(view);
    } catch (e) {
      toast.error(`requeue failed: ${e instanceof ApiError ? e.message : String(e)}`);
    }
  };

  const empty = {
    queue: "Queue is empty.",
    archive: "No archived messages.",
    dlq: "Dead-letter queue is empty.",
  }[view];

  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="flex h-12 shrink-0 items-center gap-2 pr-3 pl-4">
        {leading}
        <div role="tablist" aria-label="Queue view" className="flex shrink-0 gap-0.5 rounded-[9px] bg-muted p-0.5">
          {VIEWS.map(([key, label]) => (
            <button
              key={key}
              type="button"
              role="tab"
              aria-selected={view === key}
              onClick={() => setView(key)}
              className={`h-[22px] rounded-[7px] px-[9px] text-[11.5px] ${view === key
                ? "bg-card font-medium text-foreground shadow-[var(--raise)]"
                : "text-muted-foreground hover:text-foreground"}`}
            >
              {label}
            </button>
          ))}
        </div>
        <span className="text-[11.5px] text-muted-foreground">
          {items.length} message{items.length === 1 ? "" : "s"}
        </span>
        {view === "queue" && items.length > 0 && (
          <Button size="sm" variant="outline" className="ml-auto" onClick={openBulkDialog} disabled={busy}>
            Clear queue
          </Button>
        )}
      </div>
      <div role="tabpanel" className="min-h-0 flex-1 overflow-auto border-t">
        {items.length === 0 && (
          <p className="px-4 py-3 text-sm text-muted-foreground">{empty}</p>
        )}
        {items.map((it) => (
          <MessageRow
            key={it.id}
            item={it}
            view={view}
            expanded={expanded.has(it.id)}
            onToggle={() => toggle(it.id)}
            onProcess={() => openDialog("processed", it)}
            onReply={() => openDialog("reply", it)}
            onRequeue={() => void requeue(it)}
          />
        ))}
      </div>

      <Dialog open={dialog !== null} onOpenChange={(o) => { if (!o && !busy) setDialog(null); }}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>
              {dialog?.mode === "reply" ? "Reply" : dialog?.mode === "clear" ? "Clear queue" : "Mark processed"}
            </DialogTitle>
            <DialogDescription>
              {dialog?.mode === "clear"
                ? "Every pending delivery for this agent is physically deleted and cannot be recovered. Archive and DLQ are not changed; shared messages needed by other agents are retained."
                : dialog?.mode === "reply"
                ? "Publish a reply — this also marks the message processed."
                : "A non-empty result is required."}
            </DialogDescription>
          </DialogHeader>
          {dialog?.mode !== "clear" && (
            <Textarea
              autoFocus
              value={dialogText}
              onChange={(e) => setDialogText(e.target.value)}
              placeholder={dialog?.mode === "reply" ? "reply text" : "result"}
              rows={4}
            />
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setDialog(null)} disabled={busy}>Cancel</Button>
            <Button variant={dialog?.mode === "clear" ? "destructive" : "default"} onClick={() => void submitDialog()} disabled={busy}>
              {dialog?.mode === "reply" ? "Reply" : dialog?.mode === "clear" ? "Clear queue" : "Mark processed"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
