package tasknotify

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/alekzonder/tariboy/internal/bus"
	"github.com/alekzonder/tariboy/internal/store"
	"github.com/alekzonder/tariboy/internal/tasks"
)

type fakeBus struct {
	messages []bus.Message
	fail     bool
}

func (f *fakeBus) Publish(message bus.Message) (bus.Message, error) {
	f.messages = append(f.messages, message)
	if f.fail {
		return bus.Message{}, errors.New("bus unavailable")
	}
	message.ID = "message-" + message.IdempotencyKey
	return message, nil
}

func TestFlushPublishesAssignmentQuestionAnswerAndTriageChannels(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "tariboyd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Date(2026, 7, 31, 13, 0, 0, 0, time.UTC)
	svc := tasks.NewService(st.DB, "customer", func() time.Time { return now })
	ctx := context.Background()
	customer := tasks.CustomerActor("customer")
	if _, err := svc.CreateQueue(ctx, customer, tasks.CreateQueueInput{
		Prefix: "NOTE", Name: "Notify", ResponsibleAgent: "triager",
	}); err != nil {
		t.Fatal(err)
	}
	_, _ = svc.CreateTask(ctx, customer, tasks.CreateTaskInput{
		Queue: "NOTE", Title: "unassigned",
	})
	assigned, _ := svc.CreateTask(ctx, customer, tasks.CreateTaskInput{
		Queue: "NOTE", Title: "assigned", Assignee: "worker",
	})
	_, err = svc.AddComment(ctx, tasks.AgentActor("worker"), assigned.Key, tasks.AddCommentInput{
		Body: "Question for @user:customer",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddComment(ctx, customer, assigned.Key, tasks.AddCommentInput{
		Body: "Answer",
	}); err != nil {
		t.Fatal(err)
	}

	fake := &fakeBus{}
	publisher := New(st.DB, fake, func() time.Time { return now },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := publisher.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	got := map[string]string{}
	for _, message := range fake.messages {
		got[message.Type] = message.Channel
		if message.IdempotencyKey == "" {
			t.Fatalf("%s has no idempotency key", message.Type)
		}
	}
	want := map[string]string{
		"task.triage":   "chat:tasks:triager",
		"task.assigned": "chat:tasks:worker",
		"task.question": "user:customer",
		"task.answered": "chat:tasks:worker",
	}
	for typ, channel := range want {
		if got[typ] != channel {
			t.Fatalf("%s channel = %q; want %q (all=%v)", typ, got[typ], channel, got)
		}
	}
	count := len(fake.messages)
	if err := publisher.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fake.messages) != count {
		t.Fatalf("second flush published %d more messages", len(fake.messages)-count)
	}
}

func TestFailedPublishIsRetriedAfterBackoff(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "tariboyd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Date(2026, 7, 31, 13, 0, 0, 0, time.UTC)
	svc := tasks.NewService(st.DB, "customer", func() time.Time { return now })
	ctx := context.Background()
	customer := tasks.CustomerActor("customer")
	_, _ = svc.CreateQueue(ctx, customer, tasks.CreateQueueInput{
		Prefix: "RETRY", Name: "Retry", ResponsibleAgent: "triager",
	})
	_, _ = svc.CreateTask(ctx, customer, tasks.CreateTaskInput{
		Queue: "RETRY", Title: "retry me",
	})

	fake := &fakeBus{fail: true}
	publisher := New(st.DB, fake, func() time.Time { return now },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := publisher.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fake.messages) != 1 {
		t.Fatalf("attempts = %d; want 1", len(fake.messages))
	}
	fake.fail = false
	if err := publisher.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fake.messages) != 1 {
		t.Fatalf("retried before backoff elapsed")
	}
	now = now.Add(3 * time.Second)
	if err := publisher.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fake.messages) != 2 {
		t.Fatalf("attempts after backoff = %d; want 2", len(fake.messages))
	}
}
