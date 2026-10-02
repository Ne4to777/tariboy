package tasks

import (
	"context"
	"reflect"
	"testing"
)

func workflowFixture(t *testing.T) (*Service, Actor) {
	t.Helper()
	svc := newTestService(t)
	actor := CustomerActor("customer")
	if _, err := svc.CreateQueue(context.Background(), actor, CreateQueueInput{
		Prefix: "DEV", Name: "Development",
	}); err != nil {
		t.Fatal(err)
	}
	for _, agent := range []string{"dev-1", "dev-2", "reviewer-1"} {
		if _, err := svc.db.Exec(`
			INSERT INTO agents(name, image_ref, image_digest)
			VALUES (?, 'basic:latest', 'digest')`, agent); err != nil {
			t.Fatal(err)
		}
	}
	return svc, actor
}

func mustRebindPool(t *testing.T, svc *Service, actor Actor, pool string, agents []string, revision int64) AgentPool {
	t.Helper()
	got, err := svc.RebindAgentPool(context.Background(), actor, "DEV", pool, agents, revision, "pool-"+pool)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestRebindAgentPoolIsRevisionedAuditedAndIdempotent(t *testing.T) {
	svc, actor := workflowFixture(t)
	if _, err := svc.RebindAgentPool(context.Background(), AgentActor("dev-1"), "DEV", "developers", []string{"dev-1"}, 0, "agent-rebind"); ErrorCode(err) != "forbidden" {
		t.Fatalf("agent rebind error = %v; want forbidden", err)
	}
	if _, err := svc.RebindAgentPool(context.Background(), actor, "DEV", "developers", []string{"missing"}, 0, "missing-agent"); ErrorCode(err) != "agent_not_found" {
		t.Fatalf("unknown agent error = %v; want agent_not_found", err)
	}
	developers := mustRebindPool(t, svc, actor, "developers", []string{"dev-1"}, 0)

	rebound, err := svc.RebindAgentPool(context.Background(), actor, "DEV", "developers", []string{"agent:dev-2", "dev-2"}, developers.Revision, "rebind-developers")
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := svc.RebindAgentPool(context.Background(), actor, "DEV", "developers", []string{"dev-1"}, 999, "rebind-developers")
	if err != nil {
		t.Fatal(err)
	}
	if len(rebound.Agents) != 1 || rebound.Agents[0] != "dev-2" || replayed.Revision != rebound.Revision {
		t.Fatalf("rebound/replayed pool = %#v / %#v", rebound, replayed)
	}
	_, err = svc.RebindAgentPool(context.Background(), actor, "DEV", "developers", []string{"dev-1"}, developers.Revision, "stale-rebind")
	conflict, ok := err.(*Error)
	if !ok || conflict.Code != "revision_conflict" {
		t.Fatalf("stale rebind error = %#v; want revision_conflict", err)
	}
	if conflict.Data["current_revision"] != rebound.Revision || !reflect.DeepEqual(conflict.Data["current"], rebound) {
		t.Fatalf("stale rebind conflict data = %#v; want current pool %#v", conflict.Data, rebound)
	}
	emptied, err := svc.RebindAgentPool(context.Background(), actor, "DEV", "developers", nil, rebound.Revision, "empty-rebind")
	if err != nil || len(emptied.Agents) != 0 {
		t.Fatalf("empty rebind = %#v, %v; want an empty pool", emptied, err)
	}
	var eventRevision int64
	if err := svc.db.QueryRow(`
		SELECT task_revision FROM task_events
		WHERE queue_prefix = 'DEV' AND kind = 'task.agent_pool_rebound'
		ORDER BY sequence DESC LIMIT 1`).Scan(&eventRevision); err != nil {
		t.Fatal(err)
	}
	if eventRevision != emptied.Revision {
		t.Fatalf("pool audit revision = %d; want %d", eventRevision, emptied.Revision)
	}
}

func TestGetAndListAgentPools(t *testing.T) {
	svc, actor := workflowFixture(t)
	reviewers := mustRebindPool(t, svc, actor, "reviewers", []string{"reviewer-1"}, 0)
	developers := mustRebindPool(t, svc, actor, "developers", []string{"dev-2", "dev-1"}, 0)
	got, err := svc.GetAgentPool(context.Background(), actor, "dev", "developers")
	if err != nil || !reflect.DeepEqual(got, developers) {
		t.Fatalf("get pool = %#v, %v; want %#v", got, err, developers)
	}
	if _, err := svc.GetAgentPool(context.Background(), actor, "DEV", "missing"); ErrorCode(err) != "workflow_pool_not_found" {
		t.Fatalf("missing pool error = %v; want workflow_pool_not_found", err)
	}
	listed, err := svc.ListAgentPools(context.Background(), actor, "DEV")
	if err != nil || !reflect.DeepEqual(listed, []AgentPool{developers, reviewers}) {
		t.Fatalf("listed pools = %#v, %v", listed, err)
	}
	if _, err := svc.ListAgentPools(context.Background(), AgentActor("dev-1"), "DEV"); ErrorCode(err) != "forbidden" {
		t.Fatalf("agent list error = %v; want forbidden", err)
	}
}
