package tasks

import (
	"context"
	"testing"
)

func TestQuestionNotificationProjectsRequestingPrincipal(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	_, _ = svc.CreateQueue(ctx, customer, CreateQueueInput{Prefix: "ASK", Name: "Questions"})
	task, _ := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "ASK", Title: "decision", Assignee: "requester"})
	if _, err := svc.AddComment(ctx, AgentActor("requester"), task.Key, AddCommentInput{
		Body: "Need input from @user:customer",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.ListNotifications(ctx, customer, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RequestingPrincipal != "agent:requester" {
		t.Fatalf("notifications = %#v", got)
	}
}

func TestAnswerMarksResolvedQuestionNotificationRead(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	_, _ = svc.CreateQueue(ctx, customer, CreateQueueInput{Prefix: "ASK", Name: "Questions"})
	task, _ := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "ASK", Title: "decision", Assignee: "requester"})
	if _, err := svc.AddComment(ctx, AgentActor("requester"), task.Key, AddCommentInput{
		Body: "Need input from @user:customer",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddComment(ctx, customer, task.Key, AddCommentInput{Body: "Approved"}); err != nil {
		t.Fatal(err)
	}
	notifications, err := svc.ListNotifications(ctx, customer, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(notifications) != 1 || notifications[0].ReadAt == "" {
		t.Fatalf("notifications = %#v; want resolved question marked read", notifications)
	}
}

func TestAnswerResolvesOnlyAuthorsOpenWaitsAndMentionGrantsResponseAccess(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	_, _ = svc.CreateQueue(ctx, customer, CreateQueueInput{Prefix: "ASK", Name: "Questions"})
	task, _ := svc.CreateTask(ctx, customer, CreateTaskInput{
		Queue: "ASK", Title: "decision", Assignee: "requester",
	})

	question, err := svc.AddComment(ctx, AgentActor("requester"), task.Key, AddCommentInput{
		Body: "Need input from @agent:alice and @user:customer",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(question.CreatedWaits) != 2 {
		t.Fatalf("created waits = %d; want 2", len(question.CreatedWaits))
	}
	aliceView, err := svc.GetTask(ctx, AgentActor("alice"), task.Key)
	if err != nil {
		t.Fatalf("mentioned agent cannot open task: %v", err)
	}
	if aliceView.Task.Access != "respond" {
		t.Fatalf("mentioned agent access = %q; want respond", aliceView.Task.Access)
	}
	renamed := "must not be allowed"
	if _, err := svc.UpdateTask(ctx, AgentActor("alice"), task.Key, UpdateTaskInput{
		Title: &renamed, Revision: aliceView.Task.Revision,
	}); ErrorCode(err) != "not_found" {
		t.Fatalf("mentioned agent update error = %v; want not_found", err)
	}

	answer, err := svc.AddComment(ctx, AgentActor("alice"), task.Key, AddCommentInput{
		Body: "My answer",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(answer.ResolvedWaits) != 1 ||
		answer.ResolvedWaits[0].ExpectedPrincipal != "agent:alice" {
		t.Fatalf("resolved waits = %#v; want only agent:alice", answer.ResolvedWaits)
	}
	detail, err := svc.GetTask(ctx, customer, task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.WaitingFor) != 1 ||
		detail.WaitingFor[0].ExpectedPrincipal != "user:customer" {
		t.Fatalf("remaining waits = %#v; want only user:customer", detail.WaitingFor)
	}

	userAnswer, err := svc.AddComment(ctx, customer, task.Key, AddCommentInput{
		Body: "Customer answer",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(userAnswer.ResolvedWaits) != 1 {
		t.Fatalf("customer resolved waits = %d; want 1", len(userAnswer.ResolvedWaits))
	}
	detail, err = svc.GetTask(ctx, customer, task.Key)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.WaitingFor) != 0 || len(detail.Comments) != 3 {
		t.Fatalf("detail waits/comments = %d/%d; want 0/3",
			len(detail.WaitingFor), len(detail.Comments))
	}
}

func TestMalformedMentionRemainsPlainComment(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	_, _ = svc.CreateQueue(ctx, customer, CreateQueueInput{Prefix: "TEXT", Name: "Text"})
	task, _ := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "TEXT", Title: "text"})

	result, err := svc.AddComment(ctx, customer, task.Key, AddCommentInput{
		Body: "not principals: @alice @agent: @unknown:name",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.CreatedWaits) != 0 {
		t.Fatalf("malformed mentions created waits: %#v", result.CreatedWaits)
	}
}

func TestAddCommentIdempotencyReplaysWithoutDuplicateWaitOrRevision(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	_, _ = svc.CreateQueue(ctx, customer, CreateQueueInput{Prefix: "ASK", Name: "Ask"})
	task, _ := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "ASK", Title: "question"})

	first, err := svc.AddComment(ctx, customer, task.Key, AddCommentInput{
		Body: "@agent:alice need input", IdempotencyKey: "comment-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := svc.AddComment(ctx, customer, task.Key, AddCommentInput{
		Body: "different retry body", IdempotencyKey: "comment-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Comment.ID != first.Comment.ID || replayed.Comment.Body != first.Comment.Body {
		t.Fatalf("replayed = %#v, first = %#v", replayed, first)
	}
	var comments, waits, revision int
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM task_comments WHERE task_id = ?`, task.ID).Scan(&comments); err != nil {
		t.Fatal(err)
	}
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM task_waiting_for WHERE task_id = ?`, task.ID).Scan(&waits); err != nil {
		t.Fatal(err)
	}
	if err := svc.db.QueryRow(`SELECT revision FROM tasks WHERE id = ?`, task.ID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if comments != 1 || waits != 1 || revision != 2 {
		t.Fatalf("comments/waits/revision = %d/%d/%d, want 1/1/2", comments, waits, revision)
	}
}

func TestAssignedAgentCustomerQuestionTransitionsToWaitCustomer(t *testing.T) {
	svc, task := assignedTask(t, "worker", StatusInProgress)
	ctx := context.Background()
	result, err := svc.AddComment(ctx, AgentActor("worker"), task.Key, AddCommentInput{
		Body: "@user:customer choose one", IdempotencyKey: "ask-1",
	})
	if err != nil || len(result.CreatedWaits) != 1 {
		t.Fatalf("add comment = %#v, %v", result, err)
	}
	detail := assertTaskStatus(t, svc, task.Key, StatusWaitCustomer)
	if detail.Task.Revision != task.Revision+1 {
		t.Fatalf("revision = %d; want %d", detail.Task.Revision, task.Revision+1)
	}
	events, err := svc.ListEvents(ctx, CustomerActor("customer"), task.Key, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	updated, commented := events[len(events)-2], events[len(events)-1]
	if updated.Kind != "task.updated" || updated.TaskRevision != detail.Task.Revision ||
		updated.Payload["status"] != StatusWaitCustomer {
		t.Fatalf("updated event = %#v", updated)
	}
	if commented.Kind != "task.comment_added" || commented.TaskRevision != detail.Task.Revision ||
		updated.Sequence >= commented.Sequence {
		t.Fatalf("comment event = %#v after %#v", commented, updated)
	}
	var outboxSequence int64
	if err := svc.db.QueryRow(`
		SELECT event_sequence FROM task_notification_outbox
		WHERE message_type = 'task.question'`).Scan(&outboxSequence); err != nil {
		t.Fatal(err)
	}
	if outboxSequence != commented.Sequence {
		t.Fatalf("question outbox sequence = %d; want comment event %d", outboxSequence, commented.Sequence)
	}

	if _, err := svc.AddComment(ctx, AgentActor("worker"), task.Key, AddCommentInput{
		Body: "different retry body", IdempotencyKey: "ask-1",
	}); err != nil {
		t.Fatal(err)
	}
	replayed := assertTaskStatus(t, svc, task.Key, StatusWaitCustomer)
	replayedEvents, err := svc.ListEvents(ctx, CustomerActor("customer"), task.Key, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Task.Revision != detail.Task.Revision || len(replayedEvents) != len(events) {
		t.Fatalf("replay revision/events = %d/%d; want %d/%d",
			replayed.Task.Revision, len(replayedEvents), detail.Task.Revision, len(events))
	}
}

func TestFinalCustomerAnswerReturnsWaitCustomerToInProgress(t *testing.T) {
	svc, task := assignedTask(t, "worker", StatusInProgress)
	ctx := context.Background()
	if _, err := svc.AddComment(ctx, AgentActor("worker"), task.Key, AddCommentInput{
		Body: "@user:customer first", IdempotencyKey: "ask-1",
	}); err != nil {
		t.Fatal(err)
	}
	result, err := svc.AddComment(ctx, CustomerActor("customer"), task.Key, AddCommentInput{
		Body: "answered", IdempotencyKey: "answer-1",
	})
	if err != nil || len(result.ResolvedWaits) != 1 {
		t.Fatalf("answer = %#v, %v", result, err)
	}
	detail := assertTaskStatus(t, svc, task.Key, StatusInProgress)
	if detail.Task.Revision != task.Revision+2 {
		t.Fatalf("revision = %d; want %d", detail.Task.Revision, task.Revision+2)
	}
	events, err := svc.ListEvents(ctx, CustomerActor("customer"), task.Key, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	updated, commented := events[len(events)-2], events[len(events)-1]
	if updated.Kind != "task.updated" || updated.Payload["status"] != StatusInProgress ||
		commented.Kind != "task.comment_added" || updated.TaskRevision != detail.Task.Revision ||
		commented.TaskRevision != detail.Task.Revision || updated.Sequence >= commented.Sequence {
		t.Fatalf("answer event tail = %#v, %#v", updated, commented)
	}
}

func TestCustomerAnswerTransitionsWithOtherWaitRemaining(t *testing.T) {
	svc, task := assignedTask(t, "worker", StatusInProgress)
	ctx := context.Background()
	if _, err := svc.AddComment(ctx, AgentActor("worker"), task.Key, AddCommentInput{
		Body: "@user:customer decide and @agent:reviewer review",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddComment(ctx, CustomerActor("customer"), task.Key, AddCommentInput{
		Body: "customer answer",
	}); err != nil {
		t.Fatal(err)
	}
	detail := assertTaskStatus(t, svc, task.Key, StatusInProgress)
	if len(detail.WaitingFor) != 1 || detail.WaitingFor[0].ExpectedPrincipal != "agent:reviewer" {
		t.Fatalf("remaining waits = %#v; want agent:reviewer", detail.WaitingFor)
	}
}

func TestUnrelatedCommentDoesNotResumeManualWaitCustomer(t *testing.T) {
	svc, task := assignedTask(t, "worker", StatusWaitCustomer)
	ctx := context.Background()
	before, err := svc.ListEvents(ctx, CustomerActor("customer"), task.Key, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddComment(ctx, AgentActor("worker"), task.Key, AddCommentInput{
		Body: "work continues elsewhere",
	}); err != nil {
		t.Fatal(err)
	}
	assertTaskStatus(t, svc, task.Key, StatusWaitCustomer)
	after, err := svc.ListEvents(ctx, CustomerActor("customer"), task.Key, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before)+1 || after[len(after)-1].Kind != "task.comment_added" {
		t.Fatalf("event tail after unrelated comment = %#v", after[len(before):])
	}
}

func TestCustomerWaitStatusNonTransitions(t *testing.T) {
	tests := []struct {
		name                string
		actor               Actor
		mention             string
		start               string
		grantResponseAccess bool
		manualBeforeAnswer  bool
		want                string
	}{
		{name: "other agent question", actor: AgentActor("other"), mention: "@user:customer", start: StatusInProgress, grantResponseAccess: true, want: StatusInProgress},
		{name: "wait for another principal", actor: AgentActor("worker"), mention: "@agent:reviewer", start: StatusInProgress, want: StatusInProgress},
		{name: "done task", actor: AgentActor("worker"), mention: "@user:customer", start: StatusDone, want: StatusDone},
		{name: "cancelled task", actor: AgentActor("worker"), mention: "@user:customer", start: StatusCancelled, want: StatusCancelled},
		{name: "manual status before answer", actor: CustomerActor("customer"), start: StatusInProgress, manualBeforeAnswer: true, want: StatusOpen},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, task := assignedTask(t, "worker", tt.start)
			ctx := context.Background()
			if tt.grantResponseAccess {
				if _, err := svc.AddComment(ctx, CustomerActor("customer"), task.Key, AddCommentInput{
					Body: "@agent:other please respond",
				}); err != nil {
					t.Fatal(err)
				}
			}
			if tt.manualBeforeAnswer {
				if _, err := svc.AddComment(ctx, AgentActor("worker"), task.Key, AddCommentInput{
					Body: "@user:customer question",
				}); err != nil {
					t.Fatal(err)
				}
				waiting := assertTaskStatus(t, svc, task.Key, StatusWaitCustomer)
				status := StatusOpen
				if _, err := svc.UpdateTask(ctx, CustomerActor("customer"), task.Key, UpdateTaskInput{
					Status: &status, Revision: waiting.Task.Revision,
				}); err != nil {
					t.Fatal(err)
				}
			}
			before, err := svc.ListEvents(ctx, CustomerActor("customer"), task.Key, 0, 20)
			if err != nil {
				t.Fatal(err)
			}
			body := tt.mention
			if tt.manualBeforeAnswer {
				body = "answer after manual status"
			}
			if _, err := svc.AddComment(ctx, tt.actor, task.Key, AddCommentInput{Body: body}); err != nil {
				t.Fatal(err)
			}
			assertTaskStatus(t, svc, task.Key, tt.want)
			after, err := svc.ListEvents(ctx, CustomerActor("customer"), task.Key, 0, 20)
			if err != nil {
				t.Fatal(err)
			}
			if len(after) != len(before)+1 || after[len(after)-1].Kind != "task.comment_added" {
				t.Fatalf("event tail after non-transition = %#v", after[len(before):])
			}
		})
	}
}

func assignedTask(t *testing.T, assignee, status string) (*Service, Task) {
	t.Helper()
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	if _, err := svc.CreateQueue(ctx, customer, CreateQueueInput{Prefix: "ASK", Name: "Questions"}); err != nil {
		t.Fatal(err)
	}
	task, err := svc.CreateTask(ctx, customer, CreateTaskInput{
		Queue: "ASK", Title: "decision", Assignee: assignee,
	})
	if err != nil {
		t.Fatal(err)
	}
	if status != StatusOpen {
		updated, err := svc.UpdateTask(ctx, customer, task.Key, UpdateTaskInput{
			Status: &status, Revision: task.Revision,
		})
		if err != nil {
			t.Fatal(err)
		}
		task = updated
	}
	return svc, task
}

func assertTaskStatus(t *testing.T, svc *Service, key, want string) TaskDetail {
	t.Helper()
	detail, err := svc.GetTask(context.Background(), CustomerActor("customer"), key)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Task.Status != want {
		t.Fatalf("status = %q; want %q", detail.Task.Status, want)
	}
	return detail
}
