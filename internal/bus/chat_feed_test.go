package bus

import "testing"

// chatFixture is a bus with one reconciled agent plus that agent's personal
// chat, which is the row every read-path test reads through.
func chatFixture(t *testing.T) (*Bus, Chat) {
	t.Helper()
	b := chatBus(t)
	if err := b.ReconcileChats([]string{"worker"}, "user:customer"); err != nil {
		t.Fatal(err)
	}
	chat, err := b.GetChat(ChatIDDirect("worker"))
	if err != nil {
		t.Fatal(err)
	}
	return b, chat
}

// The personal chat must keep showing history written before the migration:
// that history lives on the two merged inboxes, and messages are never
// rewritten onto the new chat channel.
func TestChatFeedUnionsLegacyHistoryAndChatChannel(t *testing.T) {
	b, chat := chatFixture(t)
	publish(t, b, Message{Channel: InboxChannel("worker"), Source: "user:customer", Type: "message", Text: "old customer"})
	publish(t, b, Message{Channel: "user:customer", Source: "agent:worker", Type: "message", Text: "old agent"})
	publish(t, b, Message{Channel: chat.Channel, Source: "user:customer", Type: "message", Text: "new"})

	msgs, err := b.ChatFeed(chat, "user:customer", DefaultChatTypes, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	texts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		texts = append(texts, m.Text)
	}
	if len(texts) != 3 || texts[0] != "old customer" || texts[1] != "old agent" || texts[2] != "new" {
		t.Fatalf("feed must merge legacy history with the chat channel, oldest first: %v", texts)
	}
}

// A chat without pre-migration history reads only its own channel, so a
// service or tasks chat never inherits the conversation.
func TestChatFeedWithoutLegacyAgentReadsOnlyItsChannel(t *testing.T) {
	b, _ := chatFixture(t)
	service, err := b.GetChat(ChatIDService("worker"))
	if err != nil {
		t.Fatal(err)
	}
	publish(t, b, Message{Channel: InboxChannel("worker"), Source: "user:customer", Type: "message", Text: "conversation"})
	publish(t, b, Message{Channel: service.Channel, Source: "system:schedule", Type: "message", Text: "alarm"})

	msgs, err := b.ChatFeed(service, "user:customer", DefaultChatTypes, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Text != "alarm" {
		t.Fatalf("service chat must read only its own channel: %+v", msgs)
	}
}

// before pages backwards by timestamp across both sources, and limit caps the
// page it returns.
func TestChatFeedPagesBackwardsByTimestamp(t *testing.T) {
	b, chat := chatFixture(t)
	publish(t, b, Message{Channel: InboxChannel("worker"), Source: "user:customer", Type: "message", Text: "first"})
	second := publish(t, b, Message{Channel: chat.Channel, Source: "user:customer", Type: "message", Text: "second"})
	publish(t, b, Message{Channel: chat.Channel, Source: "agent:worker", Type: "message", Text: "third"})

	older, err := b.ChatFeed(chat, "user:customer", DefaultChatTypes, 0, second.TS)
	if err != nil {
		t.Fatal(err)
	}
	if len(older) != 1 || older[0].Text != "first" {
		t.Fatalf("before must page strictly older messages: %+v", older)
	}
	newest, err := b.ChatFeed(chat, "user:customer", DefaultChatTypes, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(newest) != 1 || newest[0].Text != "third" {
		t.Fatalf("limit must return the newest page: %+v", newest)
	}
}

// The chat list is per participant: it carries the chat identity, its last
// message and the unread count measured against that participant read mark.
func TestChatListCarriesIdentityAndUnread(t *testing.T) {
	b, chat := chatFixture(t)
	publish(t, b, Message{Channel: chat.Channel, Source: "user:customer", Type: "message", Text: "mine"})
	agentSaid := publish(t, b, Message{Channel: chat.Channel, Source: "agent:worker", Type: "message", Text: "theirs"})

	list, err := b.ChatList("user:customer", DefaultChatTypes)
	if err != nil {
		t.Fatal(err)
	}
	var found *ChatSummary
	for i := range list {
		if list[i].ID == chat.ID {
			found = &list[i]
		}
	}
	if found == nil {
		t.Fatalf("personal chat missing from the list: %+v", list)
	}
	if found.Kind != ChatKindDirect || found.Agent != "worker" {
		t.Fatalf("chat identity missing: %+v", found)
	}
	if found.LastText != "theirs" || found.LastFrom != "agent:worker" || found.LastTS != agentSaid.TS {
		t.Fatalf("last message wrong: %+v", found)
	}
	if found.Unread != 1 {
		t.Fatalf("only the other participant messages count as unread, got %d", found.Unread)
	}

	if err := b.SetReadTS(chat.ID, "user:customer", agentSaid.TS, false); err != nil {
		t.Fatal(err)
	}
	list, err = b.ChatList("user:customer", DefaultChatTypes)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range list {
		if row.ID == chat.ID && row.Unread != 0 {
			t.Fatalf("reading to the last message must clear unread, got %d", row.Unread)
		}
	}
}

// Review Focus item 5: a message that arrives between render and read-mark.
// The mark moves forward only, so a second window marking an older timestamp
// must not resurrect messages that were read. Marking unread is the one
// deliberate exception and asks for it explicitly.
func TestChatReadMarkOnlyMovesForward(t *testing.T) {
	b, chat := chatFixture(t)
	if err := b.SetReadTS(chat.ID, "user:customer", "2026-09-22T10:00:00.000000000Z", false); err != nil {
		t.Fatal(err)
	}
	if err := b.SetReadTS(chat.ID, "user:customer", "2026-09-22T09:00:00.000000000Z", false); err != nil {
		t.Fatal(err)
	}
	if got := readTS(t, b, chat.ID, "user:customer"); got != "2026-09-22T10:00:00.000000000Z" {
		t.Fatalf("a late request must not resurrect read messages, got %q", got)
	}
	if err := b.SetReadTS(chat.ID, "user:customer", "2026-09-22T09:00:00.000000000Z", true); err != nil {
		t.Fatal(err)
	}
	if got := readTS(t, b, chat.ID, "user:customer"); got != "2026-09-22T09:00:00.000000000Z" {
		t.Fatalf("an explicit mark-unread must move the mark back, got %q", got)
	}
}

// A chat nobody has written in yet has no last message, so it has no sender
// either rather than an invented "system".
func TestChatListLeavesAnEmptyChatWithoutASender(t *testing.T) {
	b, _ := chatFixture(t)
	list, err := b.ChatList("user:customer", DefaultChatTypes)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list {
		if s.ID == ChatIDTasks("worker") {
			if s.LastTS != "" || s.LastFrom != "" {
				t.Fatalf("empty chat summary = %#v, want no last message", s)
			}
			return
		}
	}
	t.Fatalf("tasks chat missing from %#v", list)
}

// Only a participant of the chat can leave the customer something unread. A
// workflow notice in the tasks chat is addressed to the agent, so it must not
// light the sidebar badge; the agent own word in that chat still does.
func TestChatListCountsOnlyParticipantsAsUnread(t *testing.T) {
	b, _ := chatFixture(t)
	tasks, err := b.GetChat(ChatIDTasks("worker"))
	if err != nil {
		t.Fatal(err)
	}
	publish(t, b, Message{Channel: tasks.Channel, Source: "system:workflow", Type: "task.assigned", Text: "Task assigned: DEV-1"})
	publish(t, b, Message{Channel: tasks.Channel, Source: "agent:worker", Type: "message", Text: "on it"})

	list, err := b.ChatList("user:customer", DefaultChatTypes)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range list {
		if row.ID == tasks.ID {
			if row.Unread != 1 {
				t.Fatalf("only the agent message counts as unread, got %d", row.Unread)
			}
			return
		}
	}
	t.Fatalf("tasks chat missing from %#v", list)
}
