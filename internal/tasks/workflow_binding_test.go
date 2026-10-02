package tasks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/alekzonder/tariboy/internal/workflowfile"
	"github.com/alekzonder/tariboy/internal/workflowimage"
)

// seedBindingImage records a manifest whose pool-owned statuses use pools and
// returns its digest. It installs a resolver on svc that knows the refs seeded
// so far.
func seedBindingImage(t *testing.T, svc *Service, name, version string, pools ...string) string {
	t.Helper()
	def := workflowfile.File{SchemaVersion: 1, Name: name, WorkflowVersion: version, InitialStatus: "done"}
	for _, p := range pools {
		def.Statuses = append(def.Statuses, workflowfile.Status{
			ID: "in-" + p, Owner: workflowfile.Owner{Kind: workflowfile.OwnerPool, Pool: p},
		})
	}
	def.Statuses = append(def.Statuses, workflowfile.Status{ID: "done", Terminal: true})
	sum := sha256.Sum256([]byte(name + "|" + version + "|" + strings.Join(pools, ",")))
	digest := hex.EncodeToString(sum[:])
	raw, err := json.Marshal(workflowimage.Manifest{
		SchemaVersion: 1, Name: name, Version: version, Digest: digest, BuiltAt: "2026-10-02T00:00:00Z", Definition: def,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`INSERT INTO task_workflow_images(digest, name, version, manifest, built_at) VALUES (?, ?, ?, ?, ?)`,
		digest, name, version, string(raw), "2026-10-02T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	svc.SetWorkflowResolver(func(ref string) (string, error) {
		n, tag := ref, ""
		if i := strings.LastIndex(ref, ":"); i >= 0 {
			n, tag = ref[:i], ref[i+1:]
		}
		var d string
		err := svc.db.QueryRow(`SELECT digest FROM task_workflow_images
			WHERE (name = ? AND (? = '' OR version = ?)) OR digest = ? ORDER BY built_at DESC, rowid DESC LIMIT 1`,
			n, tag, tag, tag).Scan(&d)
		if err != nil {
			return "", fmt.Errorf("%w: %s", workflowimage.ErrNotFound, ref)
		}
		return d, nil
	})
	return digest
}

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

func TestQueueWorkflowBindingRules(t *testing.T) {
	ctx := context.Background()
	svc, actor := workflowFixture(t)
	if _, err := svc.SetQueueWorkflow(ctx, actor, "DEV", "flow", 0); ErrorCode(err) != "workflow_unavailable" {
		t.Fatalf("no resolver error = %v; want workflow_unavailable", err)
	}
	d1 := seedBindingImage(t, svc, "flow", "1", "developers", "reviewers")
	for _, call := range []func() error{
		func() error { _, err := svc.SetQueueWorkflow(ctx, AgentActor("dev-1"), "DEV", "flow", 0); return err },
		func() error { return svc.ClearQueueWorkflow(ctx, AgentActor("dev-1"), "DEV", 1) },
		func() error { _, err := svc.GetQueueWorkflow(ctx, AgentActor("dev-1"), "DEV"); return err },
	} {
		if err := call(); ErrorCode(err) != "forbidden" {
			t.Fatalf("agent error = %v; want forbidden", err)
		}
	}
	if _, err := svc.GetQueueWorkflow(ctx, actor, "DEV"); ErrorCode(err) != "queue_workflow_not_found" {
		t.Fatalf("unbound get error = %v; want queue_workflow_not_found", err)
	}
	if _, err := svc.SetQueueWorkflow(ctx, actor, "NOPE", "flow", 0); ErrorCode(err) != "queue_not_found" {
		t.Fatalf("unknown queue error = %v", err)
	}
	if _, err := svc.SetQueueWorkflow(ctx, actor, "DEV", "missing", 0); ErrorCode(err) != "not_found" {
		t.Fatalf("unknown ref error = %v; want not_found", err)
	}
	// Both pools are missing.
	_, err := svc.SetQueueWorkflow(ctx, actor, "DEV", "flow:1", 0)
	var domain *Error
	if !errors.As(err, &domain) || domain.Code != "workflow_pool_empty" || domain.Status != http.StatusConflict ||
		!reflect.DeepEqual(domain.Data["pools"], []string{"developers", "reviewers"}) {
		t.Fatalf("missing pools error = %#v", err)
	}
	// One pool exists but is empty, the other has a member.
	mustRebindPool(t, svc, actor, "developers", nil, 0)
	mustRebindPool(t, svc, actor, "reviewers", []string{"reviewer-1"}, 0)
	_, err = svc.SetQueueWorkflow(ctx, actor, "DEV", "flow:1", 0)
	if !errors.As(err, &domain) || domain.Code != "workflow_pool_empty" || !reflect.DeepEqual(domain.Data["pools"], []string{"developers"}) {
		t.Fatalf("empty pool error = %#v", err)
	}
	developers, err := svc.GetAgentPool(ctx, actor, "DEV", "developers")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RebindAgentPool(ctx, actor, "DEV", "developers", []string{"dev-1"}, developers.Revision, "fill-developers"); err != nil {
		t.Fatal(err)
	}

	bound, err := svc.SetQueueWorkflow(ctx, actor, "DEV", "flow:1", 0)
	if err != nil || bound.Digest != d1 || bound.Name != "flow" || bound.Version != "1" || bound.Revision != 1 || bound.Queue != "DEV" {
		t.Fatalf("bind = %#v, %v", bound, err)
	}
	got, err := svc.GetQueueWorkflow(ctx, actor, "dev")
	if err != nil || got != bound {
		t.Fatalf("get = %#v, %v; want %#v", got, err, bound)
	}
	// The same digest by digest ref is a no-op that keeps the revision.
	same, err := svc.SetQueueWorkflow(ctx, actor, "DEV", "flow:"+d1, 1)
	if err != nil || same != bound {
		t.Fatalf("rebind same digest = %#v, %v", same, err)
	}
	// A stale revision, or 0 once bound, conflicts and reports the current one.
	for _, stale := range []int64{0, 7} {
		_, err = svc.SetQueueWorkflow(ctx, actor, "DEV", "flow:1", stale)
		if !errors.As(err, &domain) || domain.Code != "revision_conflict" || domain.Status != http.StatusConflict || domain.Data["current_revision"] != int64(1) {
			t.Fatalf("stale revision %d error = %#v", stale, err)
		}
	}
	d2 := seedBindingImage(t, svc, "flow", "2", "developers")
	next, err := svc.SetQueueWorkflow(ctx, actor, "DEV", "flow:2", 1)
	if err != nil || next.Digest != d2 || next.Revision != 2 || next.Version != "2" {
		t.Fatalf("bind v2 = %#v, %v", next, err)
	}
	var kinds []string
	rows, err := svc.db.Query(`SELECT kind FROM task_events WHERE queue_prefix = 'DEV' AND kind LIKE 'queue.workflow_%' ORDER BY sequence`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, k)
	}
	rows.Close()
	if !reflect.DeepEqual(kinds, []string{"queue.workflow_bound", "queue.workflow_bound"}) {
		t.Fatalf("events = %v; want one per real change", kinds)
	}

	// A task in flight keeps its pinned digest across a clear.
	if _, err := svc.db.Exec(`
		INSERT INTO tasks(task_key, queue_prefix, title, status, author, customer, created_at, updated_at, workflow_digest, workflow_status)
		VALUES ('DEV-1', 'DEV', 't', 'open', 'u', 'u', 'n', 'n', ?, 'in-developers')`, d2); err != nil {
		t.Fatal(err)
	}
	if err := svc.ClearQueueWorkflow(ctx, actor, "DEV", 1); ErrorCode(err) != "revision_conflict" {
		t.Fatalf("stale clear error = %v", err)
	}
	if err := svc.ClearQueueWorkflow(ctx, actor, "DEV", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetQueueWorkflow(ctx, actor, "DEV"); ErrorCode(err) != "queue_workflow_not_found" {
		t.Fatalf("get after clear = %v", err)
	}
	if err := svc.ClearQueueWorkflow(ctx, actor, "DEV", 2); ErrorCode(err) != "queue_workflow_not_found" {
		t.Fatalf("clear unbound = %v", err)
	}
	var pinned string
	if err := svc.db.QueryRow(`SELECT workflow_digest FROM tasks WHERE task_key = 'DEV-1'`).Scan(&pinned); err != nil || pinned != d2 {
		t.Fatalf("pinned = %q, %v; want %q", pinned, err, d2)
	}
	if err := svc.db.QueryRow(`SELECT kind FROM task_events WHERE queue_prefix = 'DEV' ORDER BY sequence DESC LIMIT 1`).Scan(&pinned); err != nil || pinned != "queue.workflow_cleared" {
		t.Fatalf("last event = %q, %v", pinned, err)
	}
}

func TestRebindAgentPoolKeepsBoundPoolsNonEmpty(t *testing.T) {
	ctx := context.Background()
	svc, actor := workflowFixture(t)
	seedBindingImage(t, svc, "flow", "1", "developers")
	developers := mustRebindPool(t, svc, actor, "developers", []string{"dev-1"}, 0)
	other := mustRebindPool(t, svc, actor, "other", []string{"dev-2"}, 0)
	if _, err := svc.SetQueueWorkflow(ctx, actor, "DEV", "flow", 0); err != nil {
		t.Fatal(err)
	}
	_, err := svc.RebindAgentPool(ctx, actor, "DEV", "developers", nil, developers.Revision, "empty-bound")
	var domain *Error
	if !errors.As(err, &domain) || domain.Code != "workflow_pool_empty" || domain.Status != http.StatusConflict {
		t.Fatalf("empty bound pool error = %#v", err)
	}
	if got, err := svc.GetAgentPool(ctx, actor, "DEV", "developers"); err != nil || !reflect.DeepEqual(got, developers) {
		t.Fatalf("pool after refused rebind = %#v, %v", got, err)
	}
	if _, err := svc.RebindAgentPool(ctx, actor, "DEV", "developers", []string{"dev-2"}, developers.Revision, "swap"); err != nil {
		t.Fatalf("non-empty rebind: %v", err)
	}
	if _, err := svc.RebindAgentPool(ctx, actor, "DEV", "other", nil, other.Revision, "empty-unused"); err != nil {
		t.Fatalf("empty rebind of an unused pool: %v", err)
	}
}
