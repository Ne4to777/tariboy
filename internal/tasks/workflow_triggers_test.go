package tasks

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alekzonder/tariboy/internal/bus"
	basestore "github.com/alekzonder/tariboy/internal/store"
)

func triggerFixture(t *testing.T) (*Service, Actor) {
	t.Helper()
	svc := newTestService(t)
	operator := CustomerActor("customer")
	if _, err := svc.CreateQueue(context.Background(), operator, CreateQueueInput{Prefix: "DEV", Name: "Development"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`INSERT INTO agents(name, image_ref, image_digest) VALUES ('dev-a', 'basic:latest', 'digest')`); err != nil {
		t.Fatal(err)
	}
	return svc, operator
}

func mustCreateTrigger(t *testing.T, svc *Service, operator Actor, pattern string) QueueWorkflowTrigger {
	t.Helper()
	trigger, err := svc.CreateQueueWorkflowTrigger(context.Background(), operator, "DEV", CreateQueueWorkflowTriggerInput{Pattern: pattern, Action: "create_task"})
	if err != nil {
		t.Fatal(err)
	}
	return trigger
}

func countTasksMentioning(t *testing.T, svc *Service, text string) int {
	t.Helper()
	var count int
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE description LIKE ?`, "%"+text+"%").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestReconcileWorkflowObservationsRecoversCommittedBusMessage(t *testing.T) {
	svc, operator := triggerFixture(t)
	mustCreateTrigger(t, svc, operator, "metrics:api")
	if _, err := svc.db.Exec(`INSERT INTO channels(name,kind,created_at) VALUES('metrics:api','chat',?) ON CONFLICT DO NOTHING`, svc.now()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`INSERT INTO messages(id,channel,ts,source,type,subject,text,data,kind) VALUES('durable-1','metrics:api',?,'plugin:metrics','metric.alert','{"service":"api"}','API alert','{"value":7}','event')`, "2099-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if n, err := svc.ReconcileWorkflowObservations(context.Background(), 100); err != nil || n != 1 {
		t.Fatalf("reconcile = %d, %v", n, err)
	}
	if n, err := svc.ReconcileWorkflowObservations(context.Background(), 100); err != nil || n != 0 {
		t.Fatalf("second reconcile = %d, %v", n, err)
	}
	if got := countTasksMentioning(t, svc, "durable-1"); got != 1 {
		t.Fatalf("triggered tasks = %d; want 1", got)
	}
}

func TestClearPendingPreservesUnreconciledTriggerMessage(t *testing.T) {
	svc, operator := triggerFixture(t)
	mustCreateTrigger(t, svc, operator, "metrics:api")
	b := bus.New(&basestore.Store{DB: svc.db}, svc.clock)
	if _, err := b.Subscribe("dev-a", "metrics:api", nil, nil); err != nil {
		t.Fatal(err)
	}
	published, err := b.Publish(bus.Message{Channel: "metrics:api", Source: "plugin:metrics", Type: "metric.alert", Text: "queue pressure"})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := b.ClearPending("dev-a"); err != nil || got.DeletedDeliveries != 1 || got.DeletedMessages != 0 {
		t.Fatalf("clear result = %+v, err=%v", got, err)
	}
	if n, err := svc.ReconcileWorkflowObservations(context.Background(), 100); err != nil || n != 1 {
		t.Fatalf("reconcile = %d, %v", n, err)
	}
	if got := countTasksMentioning(t, svc, published.ID); got != 1 {
		t.Fatalf("triggered tasks = %d; want 1", got)
	}
}

func TestReconcileWorkflowObservationsUsesCommitSequenceNotTimestampOrID(t *testing.T) {
	svc, operator := triggerFixture(t)
	mustCreateTrigger(t, svc, operator, "metrics:*")
	ts := "2099-01-01T00:00:00Z"
	for _, message := range []struct{ id, channel string }{{"z-first", "metrics:z"}, {"a-second", "metrics:a"}} {
		if _, err := svc.db.Exec(`INSERT INTO channels(name,kind,created_at) VALUES(?,'chat',?) ON CONFLICT DO NOTHING`, message.channel, ts); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.db.Exec(`INSERT INTO messages(id,channel,ts,source,type,subject,kind) VALUES(?,?,?,'plugin:metrics','alert','{}','event')`, message.id, message.channel, ts); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.db.Exec(`INSERT INTO task_workflow_message_sequence(message_id) VALUES(?)`, message.id); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := svc.ReconcileWorkflowObservations(context.Background(), 10); err != nil || n != 2 {
		t.Fatalf("reconcile = %d, %v", n, err)
	}
	var cursor int64
	if err := svc.db.QueryRow(`SELECT last_message_sequence FROM task_workflow_ingress_state WHERE singleton=1`).Scan(&cursor); err != nil || cursor != 2 {
		t.Fatalf("cursor = %d, %v", cursor, err)
	}
	var first, second int64
	_ = svc.db.QueryRow(`SELECT id FROM tasks WHERE description LIKE '%z-first%'`).Scan(&first)
	_ = svc.db.QueryRow(`SELECT id FROM tasks WHERE description LIKE '%a-second%'`).Scan(&second)
	if first == 0 || second == 0 || first > second {
		t.Fatalf("task ids first/second = %d/%d; want commit order", first, second)
	}
}

func insertWorkflowIngressMessage(t *testing.T, svc *Service, id, channel, eventAt string) int64 {
	t.Helper()
	if _, err := svc.db.Exec(`INSERT INTO channels(name,kind,created_at) VALUES(?,'chat',?) ON CONFLICT DO NOTHING`, channel, svc.now()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`INSERT INTO messages(id,channel,ts,source,type,subject,text,data,kind) VALUES(?,?,?,'plugin:test','event','{}',?,'{}','event')`, id, channel, eventAt, id); err != nil {
		t.Fatal(err)
	}
	res, err := svc.db.Exec(`INSERT INTO task_workflow_message_sequence(message_id) VALUES(?)`, id)
	if err != nil {
		t.Fatal(err)
	}
	sequence, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return sequence
}

func TestTriggerActivationSequenceExcludesEarlierCommittedMessage(t *testing.T) {
	svc, operator := triggerFixture(t)
	svc.db.SetMaxOpenConns(4)
	tx, err := svc.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := reserveWorkflowIngressWriter(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO channels(name,kind,created_at) VALUES('issue-provider:issues','chat',?) ON CONFLICT DO NOTHING`, svc.now()); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO messages(id,channel,ts,source,type,subject,text,data,kind) VALUES('trigger-before','issue-provider:issues','2099-01-01T00:00:00Z','plugin:test','event','{}','trigger-before','{}','event')`); err != nil {
		t.Fatal(err)
	}
	res, err := tx.Exec(`INSERT INTO task_workflow_message_sequence(message_id) VALUES('trigger-before')`)
	if err != nil {
		t.Fatal(err)
	}
	oldSequence, _ := res.LastInsertId()
	created := make(chan struct {
		trigger QueueWorkflowTrigger
		err     error
	}, 1)
	go func() {
		trigger, err := svc.CreateQueueWorkflowTrigger(context.Background(), operator, "DEV", CreateQueueWorkflowTriggerInput{Pattern: "issue-provider:*", Action: "create_task"})
		created <- struct {
			trigger QueueWorkflowTrigger
			err     error
		}{trigger, err}
	}()
	select {
	case result := <-created:
		t.Fatalf("activation committed ahead of earlier message: %#v, %v", result.trigger, result.err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	result := <-created
	trigger, err := result.trigger, result.err
	if err != nil {
		t.Fatal(err)
	}
	newSequence := insertWorkflowIngressMessage(t, svc, "trigger-after", "issue-provider:issues", "2099-01-01T00:00:00Z")
	if trigger.CreatedAfterSequence != oldSequence || newSequence <= trigger.CreatedAfterSequence {
		t.Fatalf("trigger watermark=%d old/new=%d/%d", trigger.CreatedAfterSequence, oldSequence, newSequence)
	}
	if _, err := svc.ReconcileWorkflowObservations(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	if oldTasks, newTasks := countTasksMentioning(t, svc, "trigger-before"), countTasksMentioning(t, svc, "trigger-after"); oldTasks != 0 || newTasks != 1 {
		t.Fatalf("triggered tasks old/new=%d/%d; want 0/1", oldTasks, newTasks)
	}
}

func TestNewTriggerAtSequenceZeroUsesCommitOrderNotExternalTimestamp(t *testing.T) {
	svc, operator := triggerFixture(t)
	if _, err := svc.db.Exec(`DELETE FROM messages`); err != nil {
		t.Fatal(err)
	}
	trigger := mustCreateTrigger(t, svc, operator, "issue-provider:*")
	if trigger.CreatedAfterSequence != 0 || !trigger.ActivationSequenceSet {
		t.Fatalf("new zero-sequence trigger = %#v", trigger)
	}
	insertWorkflowIngressMessage(t, svc, "first-after-activation", "issue-provider:issues", "2000-01-01T00:00:00Z")
	if _, err := svc.ReconcileWorkflowObservations(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	if tasks := countTasksMentioning(t, svc, "first-after-activation"); tasks != 1 {
		t.Fatalf("first post-activation event with old external timestamp created %d tasks; want 1", tasks)
	}
}

func TestLegacyTargetWithoutActivationMarkerUsesTimestampBoundary(t *testing.T) {
	created := "2026-08-07T12:00:00Z"
	if workflowTargetEligible(10, 0, false, "2026-08-07T11:59:59Z", created) {
		t.Fatal("legacy target accepted an event older than its creation time")
	}
	if !workflowTargetEligible(10, 0, false, "2026-08-07T12:00:01Z", created) {
		t.Fatal("legacy target rejected an event newer than its creation time")
	}
}

func TestTriggerActivationBackfillsPreexistingUnsequencedMessageBeforeWatermark(t *testing.T) {
	svc, operator := triggerFixture(t)
	if _, err := svc.db.Exec(`INSERT INTO channels(name,kind,created_at) VALUES('issue-provider:issues','chat',?) ON CONFLICT DO NOTHING`, svc.now()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`INSERT INTO messages(id,channel,ts,source,type,subject,text,data,kind) VALUES('legacy-trigger-message','issue-provider:issues','2099-01-01T00:00:00Z','plugin:test','event','{}','legacy-trigger-message','{}','event')`); err != nil {
		t.Fatal(err)
	}
	trigger := mustCreateTrigger(t, svc, operator, "issue-provider:*")
	var repairedSequence int64
	if err := svc.db.QueryRow(`SELECT sequence FROM task_workflow_message_sequence WHERE message_id='legacy-trigger-message'`).Scan(&repairedSequence); err != nil {
		t.Fatal(err)
	}
	if repairedSequence > trigger.CreatedAfterSequence {
		t.Fatalf("repaired sequence %d is after activation watermark %d", repairedSequence, trigger.CreatedAfterSequence)
	}
	if _, err := svc.ReconcileWorkflowObservations(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	if tasks := countTasksMentioning(t, svc, "legacy-trigger-message"); tasks != 0 {
		t.Fatalf("pre-activation unsequenced message created %d tasks", tasks)
	}
}

func TestConcurrentWorkflowObservationReconcilersNeverRegressCursor(t *testing.T) {
	svc := newTestService(t)
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("message-%02d", 19-i)
		if _, err := svc.db.Exec(`INSERT INTO channels(name,kind,created_at) VALUES('metrics:x','chat',?) ON CONFLICT DO NOTHING`, svc.now()); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.db.Exec(`INSERT INTO messages(id,channel,ts,source,type,subject,kind) VALUES(?,'metrics:x',?,'plugin:metrics','alert','{}','event')`, id, svc.now()); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.db.Exec(`INSERT INTO task_workflow_message_sequence(message_id) VALUES(?)`, id); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 4 {
				if _, err := svc.ReconcileWorkflowObservations(context.Background(), 7); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent reconcile: %v", err)
	}
	var cursor int64
	if err := svc.db.QueryRow(`SELECT last_message_sequence FROM task_workflow_ingress_state WHERE singleton=1`).Scan(&cursor); err != nil || cursor != 20 {
		t.Fatalf("cursor = %d, %v; want 20", cursor, err)
	}
}

func TestZeroTargetCursorAdvanceSerializesAgainstTargetCreation(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.db.Exec(`INSERT INTO task_queues(prefix,name,created_at,updated_at) VALUES('DEV','Development',?,?)`, svc.now(), svc.now()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`INSERT INTO channels(name,kind,created_at) VALUES('issue-provider:issues','chat',?)`, svc.now()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`INSERT INTO messages(id,channel,ts,source,type,subject,text,data,kind) VALUES('overlap-event','issue-provider:issues',?,'plugin:issue-provider','issue.created','{}','overlap','{}','event')`, svc.now()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`INSERT INTO task_workflow_message_sequence(message_id) VALUES('overlap-event')`); err != nil {
		t.Fatal(err)
	}

	counted := make(chan struct{})
	release := make(chan struct{})
	svc.workflowIngressAfterTargetCount = func() {
		close(counted)
		<-release
	}
	reconciled := make(chan error, 1)
	go func() {
		_, err := svc.ReconcileWorkflowObservations(context.Background(), 100)
		reconciled <- err
	}()
	<-counted

	created := make(chan error, 1)
	go func() {
		_, err := svc.db.Exec(`INSERT INTO task_queue_workflow_triggers(queue_prefix,pattern,correlation_key,action,enabled,created_by,created_at,updated_at) VALUES('DEV','issue-provider:*','','create_task',1,'operator',?,?)`, svc.now(), svc.now())
		created <- err
	}()
	select {
	case err := <-created:
		t.Fatalf("target creation committed between zero-target count and cursor advance: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-reconciled; err != nil {
		t.Fatal(err)
	}
	if err := <-created; err != nil {
		t.Fatal(err)
	}
	var cursor int64
	if err := svc.db.QueryRow(`SELECT last_message_sequence FROM task_workflow_ingress_state WHERE singleton=1`).Scan(&cursor); err != nil || cursor != 1 {
		t.Fatalf("cursor = %d, %v; want 1", cursor, err)
	}
}

func TestQueueWorkflowTriggerCreatesFlexibleTaskIdempotently(t *testing.T) {
	svc, operator := triggerFixture(t)
	if _, err := svc.CreateQueueWorkflowTrigger(context.Background(), operator, "DEV", CreateQueueWorkflowTriggerInput{Pattern: "issue-provider:*", Action: "run_script"}); ErrorCode(err) != "invalid_trigger_action" {
		t.Fatalf("invalid action error = %v", err)
	}
	if _, err := svc.CreateQueueWorkflowTrigger(context.Background(), operator, "DEV", CreateQueueWorkflowTriggerInput{Pattern: "agent:*", Action: "create_task"}); ErrorCode(err) != "invalid_channel_pattern" {
		t.Fatalf("recursive internal trigger error = %v", err)
	}
	if _, err := svc.CreateQueueWorkflowTrigger(context.Background(), operator, "DEV", CreateQueueWorkflowTriggerInput{Pattern: "issue-provider:*", CorrelationKey: "issue-7", Action: "create_task"}); err != nil {
		t.Fatal(err)
	}
	notPlugin := ApplyWorkflowObservationInput{EventID: "user-event", Channel: "issue-provider:issues", Kind: "issue.created", CorrelationKey: "issue-7", Payload: map[string]any{"text": "From a user", "source": "user:someone"}}
	if err := svc.ApplyWorkflowObservation(context.Background(), notPlugin); err != nil {
		t.Fatal(err)
	}
	in := ApplyWorkflowObservationInput{EventID: "issue-provider-event-7", Channel: "issue-provider:issues", Kind: "issue.created", CorrelationKey: "issue-7", Payload: map[string]any{"text": "Fix production", "source": "plugin:issue-provider"}}
	for range 2 {
		if err := svc.ApplyWorkflowObservation(context.Background(), in); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE queue_prefix='DEV' AND title='Fix production' AND status='open' AND workflow_version_id IS NULL`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("trigger tasks = %d, %v", count, err)
	}
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE title='From a user'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("non-plugin message tasks = %d, %v; want 0", count, err)
	}
}

func TestQueueWorkflowTriggerIsOperatorOnlyAndPersistent(t *testing.T) {
	svc, operator := triggerFixture(t)
	in := CreateQueueWorkflowTriggerInput{Pattern: "issue-provider:*", CorrelationKey: "issue", Action: "create_task"}
	if _, err := svc.CreateQueueWorkflowTrigger(context.Background(), AgentActor("dev-a"), "DEV", in); ErrorCode(err) != "forbidden" {
		t.Fatalf("agent trigger error = %v; want forbidden", err)
	}
	created, err := svc.CreateQueueWorkflowTrigger(context.Background(), operator, "DEV", in)
	if err != nil {
		t.Fatal(err)
	}
	if !svc.WorkflowIngressEnabled() {
		t.Fatal("trigger did not enable workflow ingress")
	}
	listed, err := svc.ListQueueWorkflowTriggers(context.Background(), operator, "DEV")
	if err != nil || len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("listed = %#v, %v", listed, err)
	}
	restarted := NewService(svc.db, "customer", svc.clock)
	recovered, err := restarted.ListQueueWorkflowTriggers(context.Background(), operator, "DEV")
	if err != nil || len(recovered) != 1 || recovered[0].ID != created.ID {
		t.Fatalf("restart recovery = %#v, %v", recovered, err)
	}
	if !restarted.WorkflowIngressEnabled() {
		t.Fatal("restarted service did not enable workflow ingress for a persisted trigger")
	}
	if err := svc.DeleteQueueWorkflowTrigger(context.Background(), AgentActor("dev-a"), "DEV", created.ID); ErrorCode(err) != "forbidden" {
		t.Fatalf("agent delete = %v", err)
	}
	if err := svc.DeleteQueueWorkflowTrigger(context.Background(), operator, "DEV", created.ID); err != nil {
		t.Fatal(err)
	}
	if svc.WorkflowIngressEnabled() {
		t.Fatal("deleted last trigger left workflow ingress enabled")
	}
}
