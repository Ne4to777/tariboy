package bus

import (
	"sort"
	"strings"
)

// messageColumns is the message projection scanMessages expects, in its order.
const messageColumns = `id, channel, ts, source, type, subject, text, data,
	produced_by_agent, produced_in_iteration, produced_by_plugin,
	kind, correlation_id, in_reply_to, reply_to, deadline`

// ChatFeed returns one chat's messages, oldest first: the chat channel plus,
// for a chat that carries pre-migration history, the legacy two-inbox
// projection. Messages are never rewritten onto the new channel, so the old
// conversation stays readable exactly where it was written.
//
// before is a message TIMESTAMP, not an id, and pages backwards: ids embed
// their channel, so two channels merged into one feed do not sort by id.
func (b *Bus) ChatFeed(chat Chat, viewer string, types []string, limit int, before string) ([]Message, error) {
	if limit <= 0 {
		limit = 200
	}
	filter, filterArgs := typeFilterSQL(types)
	args := []any{chat.Channel}
	args = append(args, filterArgs...)
	page := ""
	if before != "" {
		page = " AND ts < ?"
		args = append(args, before)
	}
	// Newest page first so before can walk back; reversed to chronological
	// order once both sources are merged.
	rows, err := b.db.Query(`SELECT `+messageColumns+` FROM messages
		WHERE channel = ? AND `+filter+page+`
		ORDER BY ts DESC, id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	msgs, err := scanMessages(rows)
	if err != nil {
		return nil, err
	}
	if chat.LegacyAgent != "" {
		// The legacy projection is already tested and returns chronological
		// order; both sides are capped at limit before the merge, so a page
		// never scans more than two pages worth of rows.
		legacy, err := b.ChatMessages(viewer, chat.LegacyAgent, types, limit, before)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, legacy...)
		sort.SliceStable(msgs, func(i, j int) bool {
			if msgs[i].TS != msgs[j].TS {
				return msgs[i].TS > msgs[j].TS
			}
			return msgs[i].ID > msgs[j].ID
		})
		if len(msgs) > limit {
			msgs = msgs[:limit]
		}
	}
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, nil
}

// ChatList returns every chat principal takes part in, most recently active
// first, with the last message and the unread count measured against that
// participant own read mark. A chat with no messages yet is still listed: the
// chat exists as an entity, not as a side effect of traffic.
func (b *Bus) ChatList(principal string, types []string) ([]ChatSummary, error) {
	rows, err := b.db.Query(`SELECT `+chatColumns+`, p.read_ts
		FROM chats AS c JOIN chat_participants AS p ON p.chat_id = c.id
		WHERE p.principal = ? AND c.archived_at = ''
		ORDER BY c.id`, principal)
	if err != nil {
		return nil, err
	}
	type entry struct {
		chat   Chat
		readTS string
	}
	var entries []entry
	func() {
		defer rows.Close()
		for rows.Next() {
			var e entry
			if err = rows.Scan(&e.chat.ID, &e.chat.Kind, &e.chat.Title, &e.chat.Channel,
				&e.chat.LegacyAgent, &e.chat.CreatedAt, &e.chat.ArchivedAt, &e.readTS); err != nil {
				return
			}
			entries = append(entries, e)
		}
		err = rows.Err()
	}()
	if err != nil {
		return nil, err
	}

	// The legacy per-agent summaries are one query for all chats that carry
	// pre-migration history, keyed by the read mark of the chat that owns it.
	reads := map[string]string{}
	for _, e := range entries {
		if e.chat.LegacyAgent != "" {
			reads[e.chat.LegacyAgent] = e.readTS
		}
	}
	legacy := map[string]ChatSummary{}
	if len(reads) > 0 {
		summaries, err := b.ChatSummaries(principal, types, reads)
		if err != nil {
			return nil, err
		}
		for _, s := range summaries {
			legacy[s.Agent] = s
		}
	}

	out := make([]ChatSummary, 0, len(entries))
	for _, e := range entries {
		summary, err := b.chatChannelSummary(e.chat, principal, e.readTS, types)
		if err != nil {
			return nil, err
		}
		if old, ok := legacy[e.chat.LegacyAgent]; ok && e.chat.LegacyAgent != "" {
			summary.Unread += old.Unread
			if old.LastTS > summary.LastTS {
				summary.LastTS, summary.LastType = old.LastTS, old.LastType
				summary.LastText, summary.LastFrom = old.LastText, old.LastFrom
			}
		}
		summary.ID, summary.Kind, summary.Title = e.chat.ID, e.chat.Kind, e.chat.Title
		summary.Channel, summary.ReadTS = e.chat.Channel, e.readTS
		summary.Agent = ChatAgent(e.chat)
		parts, err := b.Participants(e.chat.ID)
		if err != nil {
			return nil, err
		}
		summary.Participants = make([]string, 0, len(parts))
		for _, part := range parts {
			summary.Participants = append(summary.Participants, part.Principal)
		}
		out = append(out, summary)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastTS > out[j].LastTS })
	return out, nil
}

// chatChannelSummary aggregates one chat own channel: its last message and how
// many messages from other participants arrived after the read mark. A sender
// that is not a participant, such as a system:* notice addressed to the agent,
// is shown in the feed but never counts as unread.
func (b *Bus) chatChannelSummary(chat Chat, principal, readTS string, types []string) (ChatSummary, error) {
	filter, filterArgs := typeFilterSQL(types)
	var summary ChatSummary
	var lastTS, lastType, lastText, lastFrom *string
	// The bare columns beside max(ts) are SQLite documented "row that holds the
	// maximum" behaviour, so the last message comes back in one pass.
	err := b.db.QueryRow(`
		SELECT max(ts), type, text, sender,
		       COALESCE(sum(CASE WHEN sender <> ? AND ts > ? AND sender IN (
		           SELECT principal FROM chat_participants WHERE chat_id = ?) THEN 1 ELSE 0 END), 0)
		FROM messages WHERE channel = ? AND `+filter,
		append([]any{principal, readTS, chat.ID, chat.Channel}, filterArgs...)...,
	).Scan(&lastTS, &lastType, &lastText, &lastFrom, &summary.Unread)
	if err != nil {
		return ChatSummary{}, err
	}
	summary.LastTS, summary.LastType = deref(lastTS), deref(lastType)
	summary.LastText, summary.LastFrom = deref(lastText), deref(lastFrom)
	return summary, nil
}

// deref reads an optional text column: an empty chat has no last message, so
// every one of them comes back NULL.
func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// ChatAgent is the agent a per-agent chat belongs to, or "" for a chat that is
// not about one agent (a group or an ad-hoc multi-agent chat).
func ChatAgent(chat Chat) string {
	switch chat.Kind {
	case ChatKindDirect, ChatKindTasks, ChatKindService:
		if _, suffix, ok := strings.Cut(chat.ID, ":"); ok {
			return suffix
		}
	}
	return ""
}

// SetReadTS moves a participant read mark. It only moves forward: a late or
// duplicated request from a second window must not resurrect messages already
// seen. exact sets the mark even when that moves it backwards, which is how
// marking a chat unread is expressed.
func (b *Bus) SetReadTS(chatID, principal, ts string, exact bool) error {
	query := `UPDATE chat_participants SET read_ts = ? WHERE chat_id = ? AND principal = ? AND read_ts < ?`
	args := []any{ts, chatID, principal, ts}
	if exact {
		query = `UPDATE chat_participants SET read_ts = ? WHERE chat_id = ? AND principal = ?`
		args = args[:3]
	}
	_, err := b.db.Exec(query, args...)
	return err
}
