import { Fragment, useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { toast } from "sonner";
import { ArrowDown, Check, Download, Mail, MessageSquare, MoreHorizontal, Plus, Search } from "lucide-react";
import { useAgentName } from "@/lib/agent";
import {
  ApiError, chatListOn, chatMessagesOn, chatReadOn, messageSendOn,
  type ChatMessage, type ChatSummary,
} from "@/lib/api";
import { useMessagesSocket } from "@/hooks/useMessagesSocket";
import { targetFor } from "@/lib/terminalsHost";
import {
  DropdownMenu, DropdownMenuCheckItem, DropdownMenuContent, DropdownMenuItem,
  DropdownMenuLabel, DropdownMenuSeparator, DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import TaskDrawer from "@/pages/tasks/TaskDrawer";
import { cn } from "@/lib/utils";
import {
  DEFAULT_CHAT_TYPES, EXTRA_CHAT_TYPES, loadChatTypes, saveChatTypes,
} from "./chatTypes";
import { buildFeed, unreadCount, type FeedMessage } from "./chatFeed";
import ChatMessageRow from "./ChatMessage";
import ChatComposer from "./ChatComposer";
import ChatTaskFromMessage from "./ChatTaskFromMessage";
import ChatCreate from "./ChatCreate";
import { sharedChats } from "./chatModel";
import { DayDivider, UnreadDivider } from "./ChatDivider";

const ALL_TYPES = [...DEFAULT_CHAT_TYPES, ...EXTRA_CHAT_TYPES];

/** A question the agent asked and is waiting on, so the composer can say what
 *  the next message will do. */
const QUESTION_TYPE = "task.question";

/** One agent owns three chats: the conversation the customer holds with it, the
 *  task notifications addressed to it, and its service wake-ups. They are read
 *  here rather than listed apart, because the sidebar already groups by agent
 *  and these three are that agent's row expanded. */
const KINDS = [
  ["dm", "Chat"],
  ["tasks", "Tasks"],
  ["service", "Service"],
] as const;
type Kind = typeof KINDS[number][0];

function isKind(value: string): value is Kind {
  return KINDS.some(([key]) => key === value);
}

function transcript(messages: ChatMessage[], agent: string): string {
  return [`# Chat with ${agent}`, "", ...messages.map((message) =>
    `## ${message.from} · ${message.ts}\n\n${message.text}\n`)].join("\n");
}

/**
 * The conversation with one agent: the customer's messages in that agent's
 * inbox merged with the agent's messages in the customer's own channel. The
 * daemon owns that projection; this view renders it, sends into it, and marks
 * it read. Live updates arrive over the host's message socket, which is a hint
 * to refetch — the HTTP response stays authoritative.
 *
 * There is no conversation list here: the conversation is whichever agent the
 * sidebar has selected, one agent to one thread.
 */
export default function AgentChat({ hostId = "", leading, personalOnly = false }: {
  hostId?: string;
  /** Simple mode: the conversation only, without the chat tablist. */
  personalOnly?: boolean;
  /** Rendered at the left of the toolbar by whoever owns the section switch,
   *  so the tab keeps one toolbar row instead of two. */
  leading?: ReactNode;
}) {
  const name = useAgentName();
  const target = targetFor(hostId);
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [customer, setCustomer] = useState("user:customer");
  const [types, setTypes] = useState<string[]>(() => loadChatTypes());
  const [draft, setDraft] = useState("");
  const [query, setQuery] = useState("");
  const [sending, setSending] = useState(false);
  // The message being published, shown with a `sent` receipt until the refetch
  // brings back the stored one.
  const [pending, setPending] = useState<ChatMessage | null>(null);
  // The read mark this chat is rendered against. It is captured once, before
  // opening moves the server's mark forward, and only an explicit action moves
  // it afterwards, so the unread rule holds its place while the customer reads.
  const [anchor, setAnchor] = useState<string | null>(null);
  const anchorRef = useRef<string | null>(null);
  // Set by Mark unread: the chat must stay unread after this visit, so opening
  // must not quietly mark it read again.
  const keepUnread = useRef(false);
  // Either one of the agent's three kinds or the id of a shared chat it is in.
  const [selected, setSelected] = useState<string>("dm");
  const [shared, setShared] = useState<ChatSummary[]>([]);
  const [creating, setCreating] = useState(false);
  const [taskKey, setTaskKey] = useState("");
  const [taskFrom, setTaskFrom] = useState<ChatMessage | null>(null);
  const bottom = useRef<HTMLDivElement | null>(null);
  const unreadRule = useRef<HTMLDivElement | null>(null);
  const [atBottom, setAtBottom] = useState(true);
  const typesKey = types.join(",");
  // The chat id the endpoints are addressed by. An agent name still resolves to
  // the personal chat, so the sidebar keeps working with what it knows.
  const chatId = !name ? "" : isKind(selected) ? `${selected}:${name}` : selected;
  // Only the conversation and the shared chats are written into: the other two
  // are the agent's own notification feeds, and the customer is an observer in
  // the service one.
  const writable = selected === "dm" || !isKind(selected);
  const current = shared.find((chat) => chat.id === selected);
  // Only a participant's message can be unread, the rule the daemon counts by.
  const participants = useMemo(
    () => (isKind(selected) ? [customer, `agent:${name}`] : current?.participants ?? []),
    [selected, customer, name, current],
  );

  const load = useCallback(async () => {
    if (!name) return;
    try {
      const page = await chatMessagesOn(target, chatId, { types });
      if (anchorRef.current === null) {
        anchorRef.current = page.read_ts ?? "";
        setAnchor(anchorRef.current);
      }
      setMessages(page.messages ?? []);
      setCustomer(page.customer || "user:customer");
      const newest = page.messages?.[page.messages.length - 1];
      // The mark is the timestamp of a message actually shown, never "now", so
      // one arriving mid-render cannot be marked read without being seen. A
      // hidden tab is not "shown": a chat left open behind another window keeps
      // counting its messages as unread. A chat the customer deliberately
      // marked unread is not marked read again either.
      if (newest && !keepUnread.current && document.visibilityState !== "hidden") {
        await chatReadOn(target, chatId, newest.ts);
      }
    } catch {
      // Keep the last conversation on a transient failure.
    }
    // target is derived from hostId and typesKey stands for the type list; both
    // change identity on every render otherwise.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [name, hostId, typesKey, selected]);

  // A different agent is a different conversation: its own anchor, its own
  // unread rule, its own draft.
  useEffect(() => {
    anchorRef.current = null;
    keepUnread.current = false;
    setAnchor(null);
    setMessages([]);
    setPending(null);
  }, [name, hostId, selected]);

  // The shared chats are read from the same list the sidebar reads, so a chat
  // created in another window appears here without its own endpoint.
  const loadShared = useCallback(async () => {
    if (!name) return;
    try {
      const page = await chatListOn(target);
      setShared(sharedChats(page.chats ?? [], name));
    } catch {
      // A transient failure keeps the tabs that are already shown.
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [name, hostId]);

  useEffect(() => { void load(); }, [load]);
  useEffect(() => { void loadShared(); }, [loadShared]);
  useMessagesSocket({
    target,
    enabled: (!target || Boolean(target.baseURL)) && Boolean(name),
    // A frame that names its chat refetches only that one; a publication on a
    // channel no chat owns still falls back to the agent it concerns.
    onHint: (hint) => {
      if (hint.chat ? hint.chat === chatId : hint.agent === name) void load();
    },
    onOpen: () => { void load(); },
  });

  const feed = useMemo(
    () => buildFeed(
      pending ? [...messages, pending] : messages,
      customer,
      anchor ?? "",
      participants,
    ),
    [messages, pending, customer, anchor, participants],
  );
  const shown = useMemo(() => {
    const needle = query.trim().toLowerCase();
    if (!needle) return feed;
    return feed.filter((item) => item.kind !== "message" || item.message.text.toLowerCase().includes(needle));
  }, [feed, query]);
  const newCount = anchor === null ? 0 : unreadCount(messages, customer, anchor, participants);
  const openQuestion = messages.at(-1)?.type === QUESTION_TYPE
    && messages.at(-1)?.from !== customer;

  // Follow the conversation only when the customer is already at its end;
  // otherwise the arrival is announced and the reading position is left alone.
  useEffect(() => {
    if (atBottom) bottom.current?.scrollIntoView({ block: "end" });
    // atBottom must not itself trigger a scroll, only a new message may.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [shown.length]);

  function markRead() {
    const newest = messages.at(-1);
    keepUnread.current = false;
    anchorRef.current = newest?.ts ?? "";
    setAnchor(anchorRef.current);
    if (newest && name) void chatReadOn(target, chatId, newest.ts).catch(() => undefined);
  }

  async function markUnread() {
    // Unread means the agent's last word is new again, so the mark goes back to
    // the message before it. That is the one write that moves the mark
    // backwards, and it says so.
    const lastAgent = messages.findLastIndex((message) => message.from !== customer);
    if (lastAgent < 0 || !name) return;
    const back = messages[lastAgent - 1]?.ts ?? "";
    keepUnread.current = true;
    anchorRef.current = back;
    setAnchor(back);
    try {
      await chatReadOn(target, chatId, back, { exact: true });
    } catch (error) {
      toast.error(`Could not mark the chat unread: ${error instanceof ApiError ? error.message : String(error)}`);
    }
  }

  const toggleType = (type: string) => {
    const next = types.includes(type) ? types.filter((t) => t !== type) : [...types, type];
    if (next.length === 0) return;
    setTypes(next);
    saveChatTypes(next);
  };

  function exportTranscript() {
    const url = URL.createObjectURL(new Blob([transcript(messages, name)], { type: "text/markdown" }));
    const link = document.createElement("a");
    link.href = url;
    link.download = `chat-${name}.md`;
    link.click();
    URL.revokeObjectURL(url);
  }

  const send = async () => {
    const text = draft.trim();
    if (!text || sending) return;
    setSending(true);
    const chatChannel = `chat:${chatId}`;
    setPending({
      id: `pending:${Date.now()}`, channel: chatChannel, ts: new Date().toISOString(),
      from: customer, source: customer, type: "message", text,
    });
    try {
      // The chat owns this channel, so the agent reply lands here without the
      // client having to name a reply target.
      await messageSendOn(target, { channel: chatChannel, type: "message", text });
      setDraft("");
      // Speaking is reading: the customer has answered whatever was new.
      markRead();
      await load();
    } catch (e) {
      toast.error(`send failed: ${e instanceof ApiError ? e.message : String(e)}`);
    } finally {
      setPending(null);
      setSending(false);
    }
  };

  function reply(item: FeedMessage) {
    const quoted = item.message.text.split("\n").map((line) => `> ${line}`).join("\n");
    setDraft((current) => `${quoted}\n\n${current}`);
  }

  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="flex h-12 shrink-0 items-center gap-2 pr-3 pl-4">
        {leading}
        {!personalOnly && <div role="tablist" aria-label="Agent chats" className="flex shrink-0 gap-0.5 rounded-[9px] bg-muted p-0.5">
          {[...KINDS.map(([key, label]) => [key, label] as [string, string]),
            ...shared.map((chat) => [chat.id, chat.title || chat.id] as [string, string]),
          ].map(([key, label]) => (
            <button
              key={key}
              type="button"
              role="tab"
              aria-selected={selected === key}
              onClick={() => setSelected(key)}
              className={`h-[22px] max-w-[140px] truncate rounded-[7px] px-[9px] text-[11.5px] ${selected === key
                ? "bg-card font-medium text-foreground shadow-[var(--raise)]"
                : "text-muted-foreground hover:text-foreground"}`}
            >
              {label}
            </button>
          ))}
          <button
            type="button"
            aria-label="New chat"
            title="Start a chat with this agent and others"
            onClick={() => setCreating(true)}
            className="grid size-[22px] place-items-center rounded-[7px] text-muted-foreground hover:bg-card hover:text-foreground"
          >
            <Plus className="size-3.5" aria-hidden />
          </button>
        </div>}
        <label className="flex h-7 w-[232px] items-center gap-[7px] rounded-[8px] bg-muted px-[9px] text-muted-foreground">
          <Search className="size-3 shrink-0" aria-hidden />
          <input
            type="search"
            aria-label="Search messages"
            placeholder="Search messages"
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            className="w-full bg-transparent text-[12px] outline-none"
          />
        </label>
        <span className="truncate text-[11.5px] text-muted-foreground">
          {/* A shared chat says who is in it: in a multi-agent conversation the
              author of a message is not deducible from the thread it is in. */}
          {current
            ? (current.participants ?? []).map((principal) => principal.split(":")[1]).join(", ")
            : messages.length === 0
              ? "no messages"
              : `you + ${name} · ${messages.length} message${messages.length === 1 ? "" : "s"}`}
        </span>
        <div className="ml-auto flex items-center gap-1">
          {newCount > 0 ? (
            <button
              type="button"
              onClick={markRead}
              className="flex h-[26px] items-center gap-1.5 rounded-[7px] bg-primary/12 px-2.5 text-[12px] font-medium text-primary hover:bg-primary/18"
            >
              <Check className="size-3.5" aria-hidden />
              Mark read
            </button>
          ) : (
            <button
              type="button"
              onClick={() => void markUnread()}
              disabled={messages.length === 0}
              title="Mark this chat unread"
              className="flex h-[26px] items-center gap-1.5 rounded-[7px] px-2.5 text-[12px] text-muted-foreground hover:bg-accent hover:text-foreground disabled:opacity-50"
            >
              <Mail className="size-3.5" aria-hidden />
              Mark unread
            </button>
          )}
          <button
            type="button"
            aria-label="Export transcript"
            title="Export transcript"
            onClick={exportTranscript}
            disabled={messages.length === 0}
            className="grid size-[26px] place-items-center rounded-[7px] text-muted-foreground hover:bg-accent hover:text-foreground disabled:opacity-50"
          >
            <Download className="size-3.5" />
          </button>
          <DropdownMenu>
            <DropdownMenuTrigger
              aria-label="Chat menu"
              className="grid size-[26px] place-items-center rounded-[7px] text-muted-foreground hover:bg-accent hover:text-foreground"
            >
              <MoreHorizontal className="size-3.5" />
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end" className="w-56">
              <DropdownMenuLabel>Message types</DropdownMenuLabel>
              {ALL_TYPES.map((type) => (
                <DropdownMenuCheckItem
                  key={type}
                  checked={types.includes(type)}
                  onSelect={(event: Event) => { event.preventDefault(); toggleType(type); }}
                  className="font-mono text-xs"
                >
                  {type}
                </DropdownMenuCheckItem>
              ))}
              <DropdownMenuSeparator />
              <DropdownMenuItem onSelect={() => { setTypes([...DEFAULT_CHAT_TYPES]); saveChatTypes([...DEFAULT_CHAT_TYPES]); }}>
                Reset to the default types
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      </div>
      <div
        onScroll={(event) => {
          const box = event.currentTarget;
          setAtBottom(box.scrollHeight - box.scrollTop - box.clientHeight < 40);
        }}
        className="flex min-h-0 flex-1 flex-col overflow-auto border-t pt-0.5 pb-2.5"
      >
        {messages.length === 0 && (
          <div className="flex flex-1 flex-col items-center justify-center gap-1.5 px-6 py-10 text-center">
            <span className="grid size-8 place-items-center rounded-[9px] bg-muted text-muted-foreground">
              <MessageSquare className="size-4" aria-hidden />
            </span>
            <span className="text-[13px] font-medium">No messages yet</span>
            <span className="max-w-[300px] text-[12px] leading-[1.5] text-pretty text-muted-foreground">
              Write to this agent — it answers here and can open tasks straight from the thread.
            </span>
          </div>
        )}
        {shown.map((item) => (
          <Fragment key={item.kind === "message" ? item.message.id : item.id}>
            {item.kind === "day" && <DayDivider label={item.label} />}
            {item.kind === "unread" && (
              <div ref={unreadRule}>
                <UnreadDivider label={item.label} onMarkRead={markRead} />
              </div>
            )}
            {item.kind === "message" && (
              <ChatMessageRow
                item={item}
                onOpenTask={setTaskKey}
                onReply={reply}
                onCreateTask={(entry) => setTaskFrom(entry.message)}
              />
            )}
          </Fragment>
        ))}
        {openQuestion && (
          <div className="flex items-center gap-[9px] px-4 pt-1 pl-3.5">
            <span className="grid size-[22px] place-items-center rounded-[7px] bg-[color-mix(in_oklab,var(--status-running)_16%,transparent)]">
              <span className="size-[5px] rounded-full bg-[var(--status-running)]" />
            </span>
            <span className="text-[11.5px] text-muted-foreground">{name} is waiting for an answer…</span>
          </div>
        )}
        <div ref={bottom} />
      </div>
      {newCount > 0 && !atBottom && (
        <div className="relative z-2 flex h-0 shrink-0 justify-center">
          <button
            type="button"
            onClick={() => unreadRule.current?.scrollIntoView({ block: "center" })}
            className={cn(
              "absolute bottom-1.5 flex h-[26px] items-center gap-1.5 rounded-full bg-primary px-2.5",
              "text-[11.5px] font-medium text-primary-foreground shadow-[var(--lift)]",
            )}
          >
            <ArrowDown className="size-3" aria-hidden />
            {newCount} new
          </button>
        </div>
      )}
      {writable ? (
      <ChatComposer
        agent={name}
        target={target}
        value={draft}
        onChange={setDraft}
        onSend={() => void send()}
        sending={sending}
        hint={openQuestion ? "Answers the agent's open question" : undefined}
      />
      ) : (
        <p className="border-t px-4 py-3 text-[12px] text-muted-foreground">
          {selected === "tasks"
            ? `Task notifications addressed to ${name}. Answer a task in its own drawer.`
            : `Service wake-ups for ${name}: script results, goals and schedules.`}
        </p>
      )}
      {taskKey && (
        <TaskDrawer
          key={taskKey}
          taskKey={taskKey}
          target={target}
          onClose={() => setTaskKey("")}
        />
      )}
      {creating && (
        <ChatCreate
          agent={name}
          target={target}
          onClose={() => setCreating(false)}
          onCreated={(chat) => { setCreating(false); setSelected(chat); void loadShared(); }}
        />
      )}
      {taskFrom && (
        <ChatTaskFromMessage
          message={taskFrom}
          agent={name}
          target={target}
          onClose={() => setTaskFrom(null)}
          onCreated={(key) => { setTaskFrom(null); setTaskKey(key); }}
        />
      )}
    </div>
  );
}
