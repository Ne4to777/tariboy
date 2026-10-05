import { describe, expect, it } from "vitest";
import { buildFeed, taskKeysOf, unreadCount, type FeedItem } from "./chatFeed";
import type { ChatMessage } from "@/lib/api";

const CUSTOMER = "user:customer";
const PARTICIPANTS = [CUSTOMER, "agent:worker"];
// A fixed "now" so day labels are asserted against a known today.
const NOW = new Date("2026-09-15T18:00:00Z");

function message(id: string, from: string, ts: string, text = id): ChatMessage {
  return { id, channel: "user:customer", ts, from, source: from, type: "message", text };
}

const kinds = (items: FeedItem[]) => items.map((item) => item.kind);
const labels = (items: FeedItem[]) =>
  items.flatMap((item) => (item.kind === "message" ? [] : [item.label]));

describe("buildFeed", () => {
  it("draws the unread rule once, above the first agent message newer than the mark", () => {
    const feed = buildFeed([
      message("a", "agent:worker", "2026-09-15T10:00:00Z"),
      message("b", CUSTOMER, "2026-09-15T10:01:00Z"),
      message("c", "agent:worker", "2026-09-15T10:02:00Z"),
      message("d", "agent:worker", "2026-09-15T10:03:00Z"),
    ], CUSTOMER, "2026-09-15T10:01:00Z", PARTICIPANTS, NOW);
    expect(kinds(feed)).toEqual(["day", "message", "message", "unread", "message", "message"]);
    expect(labels(feed)).toEqual(["Today", "2 new messages"]);
  });

  it("treats a chat that has never been read as entirely new, the way the daemon counts it", () => {
    const feed = buildFeed([message("a", "agent:worker", "2026-09-15T10:00:00Z")], CUSTOMER, "", PARTICIPANTS, NOW);
    expect(kinds(feed)).toEqual(["day", "unread", "message"]);
    expect(labels(feed)).toEqual(["Today", "1 new message"]);
  });

  it("draws the unread rule above the first participant message, past a system notice", () => {
    const feed = buildFeed([
      message("a", "system:workflow", "2026-09-15T10:00:00Z"),
      message("b", "agent:worker", "2026-09-15T10:01:00Z"),
    ], CUSTOMER, "", PARTICIPANTS, NOW);
    expect(kinds(feed)).toEqual(["day", "message", "unread", "message"]);
    expect(labels(feed)).toEqual(["Today", "1 new message"]);
  });

  it("draws no unread rule when only the customer has spoken since the mark", () => {
    const feed = buildFeed([
      message("a", "agent:worker", "2026-09-15T10:00:00Z"),
      message("b", CUSTOMER, "2026-09-15T10:05:00Z"),
    ], CUSTOMER, "2026-09-15T10:00:00Z", PARTICIPANTS, NOW);
    expect(kinds(feed)).toEqual(["day", "message", "message"]);
  });

  it("labels each day and restarts author grouping across the rule", () => {
    const feed = buildFeed([
      message("a", "agent:worker", "2026-09-14T10:00:00Z"),
      message("b", "agent:worker", "2026-09-14T10:01:00Z"),
      message("c", "agent:worker", "2026-09-15T09:00:00Z"),
    ], CUSTOMER, "2026-09-15T23:00:00Z", PARTICIPANTS, NOW);
    expect(labels(feed)).toEqual(["Yesterday", "Today"]);
    expect(feed.filter((item) => item.kind === "message").map((item) => item.head))
      .toEqual([true, false, true]);
  });

  it("reports the agent's reply as the read receipt once per run of own messages", () => {
    const feed = buildFeed([
      message("a", CUSTOMER, "2026-09-15T10:00:00Z"),
      message("b", CUSTOMER, "2026-09-15T10:01:00Z"),
      message("c", "agent:worker", "2026-09-15T10:30:00Z"),
      message("d", CUSTOMER, "2026-09-15T11:00:00Z"),
    ], CUSTOMER, "2026-09-15T23:00:00Z", PARTICIPANTS, NOW);
    const own = feed.filter((item) => item.kind === "message" && item.mine);
    expect(own.map((item) => item.kind === "message" && item.receipt))
      .toEqual(["read", "read", "delivered"]);
    expect(own.map((item) => item.kind === "message" && item.readLabel))
      .toEqual([undefined, "Read by worker · 10:30", undefined]);
  });
});

describe("unreadCount", () => {
  it("counts only the agent's messages newer than the mark", () => {
    const messages = [
      message("a", "agent:worker", "2026-09-15T10:00:00Z"),
      message("b", CUSTOMER, "2026-09-15T10:05:00Z"),
      message("c", "agent:worker", "2026-09-15T10:06:00Z"),
    ];
    expect(unreadCount(messages, CUSTOMER, "2026-09-15T10:00:00Z", PARTICIPANTS)).toBe(1);
    expect(unreadCount(messages, CUSTOMER, "2026-09-15T10:06:00Z", PARTICIPANTS)).toBe(0);
  });

  it("ignores a notice from a sender that is not a participant, as the daemon does", () => {
    const messages = [
      message("a", "system:workflow", "2026-09-15T10:01:00Z"),
      message("b", "agent:worker", "2026-09-15T10:02:00Z"),
    ];
    expect(unreadCount(messages, CUSTOMER, "", PARTICIPANTS)).toBe(1);
  });
});

describe("taskKeysOf", () => {
  it("lists the attached key first and every key written in the text once", () => {
    const listed = taskKeysOf({
      ...message("a", "agent:worker", "2026-09-15T10:00:00Z", "see TB-142 and TB-142 and TB-9f3a"),
      data: { task_key: "TB-7" },
    });
    expect(listed).toEqual(["TB-7", "TB-142", "TB-9f3a"]);
  });
});
