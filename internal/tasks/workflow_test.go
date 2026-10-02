package tasks

import (
	"context"
	"testing"
)

func TestParentCompletionRequiresExplicitOverrideAndLeavesChildrenOpen(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	_, _ = svc.CreateQueue(ctx, customer, CreateQueueInput{Prefix: "FLOW", Name: "Flow"})
	parent, _ := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "FLOW", Title: "parent"})
	child, _ := svc.CreateTask(ctx, customer, CreateTaskInput{ParentKey: parent.Key, Title: "child"})
	grandchild, _ := svc.CreateTask(ctx, customer, CreateTaskInput{ParentKey: child.Key, Title: "grandchild"})

	if _, err := svc.CompleteTask(ctx, customer, parent.Key, CompleteInput{
		Revision: parent.Revision,
	}); ErrorCode(err) != "active_descendants" {
		t.Fatalf("complete error = %v; want active_descendants", err)
	}
	completed, err := svc.CompleteTask(ctx, customer, parent.Key, CompleteInput{
		Revision:       parent.Revision,
		CompleteAnyway: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != StatusDone {
		t.Fatalf("parent status = %q; want done", completed.Status)
	}
	for _, key := range []string{child.Key, grandchild.Key} {
		detail, err := svc.GetTask(ctx, customer, key)
		if err != nil {
			t.Fatal(err)
		}
		if detail.Task.Status != StatusOpen {
			t.Fatalf("%s status = %q; want open", key, detail.Task.Status)
		}
	}
}

func TestBlocksCycleIsRejectedAndActiveBlockerDerivesBlocked(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	_, _ = svc.CreateQueue(ctx, customer, CreateQueueInput{Prefix: "DEPS", Name: "Deps"})
	a, _ := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "DEPS", Title: "A"})
	b, _ := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "DEPS", Title: "B"})
	c, _ := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "DEPS", Title: "C"})

	if _, err := svc.AddRelation(ctx, customer, a.Key, RelationInput{
		TargetKey: b.Key, Type: "blocks", Revision: a.Revision,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddRelation(ctx, customer, b.Key, RelationInput{
		TargetKey: c.Key, Type: "blocks", Revision: b.Revision,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddRelation(ctx, customer, c.Key, RelationInput{
		TargetKey: a.Key, Type: "blocks", Revision: c.Revision,
	}); ErrorCode(err) != "blocking_cycle" {
		t.Fatalf("cycle error = %v; want blocking_cycle", err)
	}
	detail, err := svc.GetTask(ctx, customer, c.Key)
	if err != nil {
		t.Fatal(err)
	}
	if !detail.Task.Blocked {
		t.Fatalf("%s is not derived blocked", c.Key)
	}
}

func TestRelatedRelationIsSymmetricAndDeduplicated(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	_, _ = svc.CreateQueue(ctx, customer, CreateQueueInput{Prefix: "LINK", Name: "Links"})
	a, _ := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "LINK", Title: "A"})
	b, _ := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "LINK", Title: "B"})

	first, err := svc.AddRelation(ctx, customer, b.Key, RelationInput{
		TargetKey: a.Key, Type: "related", Revision: b.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.SourceKey != a.Key || first.TargetKey != b.Key {
		t.Fatalf("canonical related = %s -> %s; want %s -> %s",
			first.SourceKey, first.TargetKey, a.Key, b.Key)
	}
	if _, err := svc.AddRelation(ctx, customer, a.Key, RelationInput{
		TargetKey: b.Key, Type: "related", Revision: a.Revision,
	}); ErrorCode(err) != "relation_exists" {
		t.Fatalf("duplicate relation error = %v; want relation_exists", err)
	}
}

func TestRelationMutationUsesRevisionAndIdempotency(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	_, _ = svc.CreateQueue(ctx, customer, CreateQueueInput{Prefix: "CAS", Name: "CAS"})
	a, _ := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "CAS", Title: "A"})
	b, _ := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "CAS", Title: "B"})

	first, err := svc.AddRelation(ctx, customer, a.Key, RelationInput{
		TargetKey: b.Key, Type: "related", Revision: a.Revision, IdempotencyKey: "rel-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := svc.AddRelation(ctx, customer, a.Key, RelationInput{
		TargetKey: b.Key, Type: "related", Revision: a.Revision, IdempotencyKey: "rel-1",
	})
	if err != nil || replayed.ID != first.ID {
		t.Fatalf("replayed relation = %#v, err=%v; want id %d", replayed, err, first.ID)
	}
	afterAdd, _ := svc.GetTask(ctx, customer, a.Key)
	if afterAdd.Task.Revision != a.Revision+1 {
		t.Fatalf("revision after add = %d; want %d", afterAdd.Task.Revision, a.Revision+1)
	}
	if _, err := svc.AddRelation(ctx, customer, a.Key, RelationInput{
		TargetKey: b.Key, Type: "blocks", Revision: a.Revision,
	}); ErrorCode(err) != "revision_conflict" {
		t.Fatalf("stale relation add error = %v; want revision_conflict", err)
	}

	if err := svc.DeleteRelation(ctx, customer, a.Key, DeleteRelationInput{
		RelationID: first.ID, Revision: afterAdd.Task.Revision, IdempotencyKey: "del-1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteRelation(ctx, customer, a.Key, DeleteRelationInput{
		RelationID: first.ID, Revision: afterAdd.Task.Revision, IdempotencyKey: "del-1",
	}); err != nil {
		t.Fatalf("idempotent relation delete replay: %v", err)
	}
	afterDelete, _ := svc.GetTask(ctx, customer, a.Key)
	if afterDelete.Task.Revision != afterAdd.Task.Revision+1 {
		t.Fatalf("revision after delete = %d; want %d",
			afterDelete.Task.Revision, afterAdd.Task.Revision+1)
	}
}

func TestRelationDoesNotExposeOrPermitDeletingHiddenEndpoint(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	_, _ = svc.CreateQueue(ctx, customer, CreateQueueInput{Prefix: "ACLREL", Name: "ACL"})
	alice, _ := svc.CreateTask(ctx, customer, CreateTaskInput{
		Queue: "ACLREL", Title: "Alice", Assignee: "alice",
	})
	bob, _ := svc.CreateTask(ctx, customer, CreateTaskInput{
		Queue: "ACLREL", Title: "Bob", Assignee: "bob",
	})
	relation, err := svc.AddRelation(ctx, customer, alice.Key, RelationInput{
		TargetKey: bob.Key, Type: "blocks", Revision: alice.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	aliceView, err := svc.GetTask(ctx, AgentActor("alice"), alice.Key)
	if err != nil {
		t.Fatal(err)
	}
	if len(aliceView.Relations) != 0 {
		t.Fatalf("hidden endpoint leaked through relation: %#v", aliceView.Relations)
	}
	events, err := svc.ListEvents(ctx, AgentActor("alice"), alice.Key, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if _, exposed := event.Payload["target_key"]; exposed {
			t.Fatalf("hidden endpoint leaked through event: %#v", event)
		}
	}
	if err := svc.DeleteRelation(ctx, AgentActor("alice"), alice.Key, DeleteRelationInput{
		RelationID: relation.ID, Revision: aliceView.Task.Revision,
	}); ErrorCode(err) != "not_found" {
		t.Fatalf("delete relation to hidden endpoint error = %v; want not_found", err)
	}
}

func TestAddRelationAppendsEventForBothEndpoints(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	_, _ = svc.CreateQueue(ctx, customer, CreateQueueInput{Prefix: "ADDEVT", Name: "Events"})
	a, _ := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "ADDEVT", Title: "A"})
	b, _ := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "ADDEVT", Title: "B"})

	if _, err := svc.AddRelation(ctx, customer, a.Key, RelationInput{
		TargetKey: b.Key, Type: "related", Revision: a.Revision,
	}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{a.Key, b.Key} {
		events, err := svc.ListEvents(ctx, customer, key, 0, 20)
		if err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, event := range events {
			found = found || event.Kind == "task.relation_added"
		}
		if !found {
			t.Fatalf("%s events = %#v; want task.relation_added", key, events)
		}
	}
}

func TestDeleteRelationAppendsEventForBothEndpoints(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	_, _ = svc.CreateQueue(ctx, customer, CreateQueueInput{Prefix: "DELEVT", Name: "Events"})
	a, _ := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "DELEVT", Title: "A"})
	b, _ := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "DELEVT", Title: "B"})
	relation, err := svc.AddRelation(ctx, customer, a.Key, RelationInput{
		TargetKey: b.Key, Type: "related", Revision: a.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	afterAdd, _ := svc.GetTask(ctx, customer, a.Key)

	if err := svc.DeleteRelation(ctx, customer, a.Key, DeleteRelationInput{
		RelationID: relation.ID, Revision: afterAdd.Task.Revision,
	}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{a.Key, b.Key} {
		events, err := svc.ListEvents(ctx, customer, key, 0, 20)
		if err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, event := range events {
			found = found || event.Kind == "task.relation_removed"
		}
		if !found {
			t.Fatalf("%s events = %#v; want task.relation_removed", key, events)
		}
	}
}

func TestRelationCarriesEndpointTitleAndStatus(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	customer := CustomerActor("customer")
	_, _ = svc.CreateQueue(ctx, customer, CreateQueueInput{Prefix: "REL", Name: "Rel"})
	blocker, _ := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "REL", Title: "Ship worker:v2"})
	blocked, _ := svc.CreateTask(ctx, customer, CreateTaskInput{Queue: "REL", Title: "Packager drops skills"})
	if _, err := svc.AddRelation(ctx, customer, blocked.Key, RelationInput{
		TargetKey: blocker.Key, Type: "blocks", Revision: blocked.Revision,
	}); err != nil {
		t.Fatal(err)
	}

	detail, err := svc.GetTask(ctx, customer, blocked.Key)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Relations) != 1 {
		t.Fatalf("relations = %#v; want one", detail.Relations)
	}
	relation := detail.Relations[0]
	if relation.SourceTitle != blocked.Title || relation.TargetTitle != blocker.Title {
		t.Fatalf("relation titles = %q -> %q; want %q -> %q",
			relation.SourceTitle, relation.TargetTitle, blocked.Title, blocker.Title)
	}
	if relation.SourceStatus != string(blocked.Status) || relation.TargetStatus != string(blocker.Status) {
		t.Fatalf("relation statuses = %q -> %q; want %q -> %q",
			relation.SourceStatus, relation.TargetStatus, blocked.Status, blocker.Status)
	}
}
