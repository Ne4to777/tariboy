import type { ChatMessage } from "@/lib/api";

/** A task key as the daemon writes it: queue prefix, dash, random lowercase
 *  suffix. The digits-only form is the retired numeric key, which still
 *  resolves through its alias and still appears in older messages. */
export const TASK_KEY = /\b[A-Z][A-Z0-9]*-(?:[0-9a-z]{4}|\d+)\b/g;

/** What a message the customer sent has reached.
 *
 *  There is no per-message delivery record for the customer's own messages, so
 *  only the two ends are observed and `read` is derived:
 *  - `sent` — published optimistically, the request is still in flight;
 *  - `delivered` — the daemon accepted and stored it;
 *  - `read` — the agent has spoken after it, which is the only evidence this
 *    side has that the agent consumed it.
 *  A true receipt would need the agent to record message consumption. */
export type Receipt = "sent" | "delivered" | "read";

export interface FeedMessage {
  kind: "message";
  message: ChatMessage;
  /** The customer's own message, drawn with a receipt and a person avatar. */
  mine: boolean;
  /** First message of a run by one author: it carries the header. */
  head: boolean;
  receipt?: Receipt;
  /** `Read by <agent> · HH:MM`, on the last read message of a run only. */
  readLabel?: string;
}

export interface FeedDivider {
  kind: "day" | "unread";
  /** Stable enough for a React key: a day repeats at most once. */
  id: string;
  label: string;
}

export type FeedItem = FeedMessage | FeedDivider;

function stringField(record: Record<string, unknown> | undefined, field: string): string {
  const value = record?.[field];
  return typeof value === "string" ? value : "";
}

/** Every task the message is about: the key the daemon attached to it first,
 *  then any key written in its text, each listed once. */
export function taskKeysOf(message: ChatMessage): string[] {
  const attached = stringField(message.data, "task_key") || stringField(message.subject, "task_key");
  return [...new Set([attached, ...(message.text.match(TASK_KEY) ?? [])].filter(Boolean))];
}

/** The agent behind a chat principal: `agent:builder` reads as `builder`. */
export function principalName(principal: string): string {
  return principal.replace(/^(agent|user|system):/, "");
}

function dayKey(iso: string, now: Date): { key: string; label: string } {
  const at = new Date(iso);
  if (Number.isNaN(at.getTime())) return { key: iso, label: iso };
  const pad = (n: number) => String(n).padStart(2, "0");
  const key = `${at.getFullYear()}-${pad(at.getMonth() + 1)}-${pad(at.getDate())}`;
  const today = `${now.getFullYear()}-${pad(now.getMonth() + 1)}-${pad(now.getDate())}`;
  const yesterday = new Date(now);
  yesterday.setDate(now.getDate() - 1);
  const before = `${yesterday.getFullYear()}-${pad(yesterday.getMonth() + 1)}-${pad(yesterday.getDate())}`;
  if (key === today) return { key, label: "Today" };
  if (key === before) return { key, label: "Yesterday" };
  return {
    key,
    label: at.toLocaleDateString(undefined, {
      day: "numeric", month: "short",
      ...(at.getFullYear() === now.getFullYear() ? {} : { year: "numeric" }),
    }),
  };
}

export function shortTime(iso: string): string {
  const at = new Date(iso);
  if (Number.isNaN(at.getTime())) return iso;
  return `${String(at.getHours()).padStart(2, "0")}:${String(at.getMinutes()).padStart(2, "0")}`;
}

/** Whether a message can be unread: another participant of the chat wrote it
 *  after the mark. A system notice addressed to the agent is not from a
 *  participant, so it never is. */
function isUnread(
  message: ChatMessage, customer: string, readTS: string, participants: readonly string[],
): boolean {
  return message.from !== customer && message.ts > readTS && participants.includes(message.from);
}

/** How many of the agent's messages are newer than the mark.
 *
 *  An empty mark means the customer has read nothing here, so every message the
 *  agent sent is new. That is the same rule the daemon counts by, which is what
 *  keeps this number equal to the sidebar's. */
export function unreadCount(
  messages: ChatMessage[], customer: string, readTS: string, participants: readonly string[],
): number {
  return messages.filter((message) => isUnread(message, customer, readTS, participants)).length;
}

/**
 * The rendered conversation: day rules, one unread rule, author grouping and
 * the receipt on each of the customer's own messages.
 *
 * `readTS` is the mark the chat was opened at, not the live one — the unread
 * rule holds its place while the customer reads. It is drawn once, above the
 * first agent message newer than that mark; an empty mark means nothing here
 * has been read, exactly as the daemon counts it, so the rule then sits above
 * the agent's first word.
 */
export function buildFeed(
  messages: ChatMessage[],
  customer: string,
  readTS: string,
  participants: readonly string[],
  now: Date = new Date(),
): FeedItem[] {
  const items: FeedItem[] = [];
  const firstUnread = messages.findIndex(
    (message) => isUnread(message, customer, readTS, participants),
  );
  let day = "";
  let previousAuthor = "";
  messages.forEach((message, index) => {
    const { key, label } = dayKey(message.ts, now);
    if (key !== day) {
      day = key;
      previousAuthor = "";
      items.push({ kind: "day", id: `day:${key}`, label });
    }
    if (index === firstUnread) {
      const count = messages.length - index;
      previousAuthor = "";
      items.push({
        kind: "unread", id: "unread",
        label: `${count} new message${count === 1 ? "" : "s"}`,
      });
    }
    const mine = message.from === customer;
    // The agent's first word after this message is the receipt: nothing after
    // it means the agent has not shown up since, so it is only delivered.
    const answer = mine
      ? messages.find((later) => later.from !== customer && later.ts > message.ts)
      : undefined;
    // One label per run of own messages: the label belongs to the last read
    // one, so a burst of three messages reports being read once.
    const nextMine = messages[index + 1];
    const runEnds = !nextMine || nextMine.from !== customer
      || !messages.find((later) => later.from !== customer && later.ts > nextMine.ts);
    items.push({
      kind: "message",
      message,
      mine,
      head: message.from !== previousAuthor,
      receipt: mine ? (answer ? "read" : "delivered") : undefined,
      readLabel: mine && answer && runEnds
        ? `Read by ${principalName(answer.from)} · ${shortTime(answer.ts)}`
        : undefined,
    });
    previousAuthor = message.from;
  });
  return items;
}
