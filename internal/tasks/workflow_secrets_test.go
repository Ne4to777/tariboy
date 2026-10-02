package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/alekzonder/tariboy/internal/workflowimage"
)

// requireSecrets rewrites the manifest of the seeded image digest so it
// requires the named secrets.
func requireSecrets(t *testing.T, svc *Service, digest string, names ...string) {
	t.Helper()
	var raw string
	if err := svc.db.QueryRow(`SELECT manifest FROM task_workflow_images WHERE digest = ?`, digest).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var manifest workflowimage.Manifest
	if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Definition.RequiresSecrets = names
	updated, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`UPDATE task_workflow_images SET manifest = ? WHERE digest = ?`, string(updated), digest); err != nil {
		t.Fatal(err)
	}
}

func TestSetQueueSecretInfoReturnsTheStoredTime(t *testing.T) {
	ctx := context.Background()
	svc, actor := workflowFixture(t)
	info, err := svc.SetQueueSecretInfo(ctx, actor, "dev", "GH_TOKEN", "s3cr3t-value")
	if err != nil {
		t.Fatal(err)
	}
	items, err := svc.ListQueueSecrets(ctx, actor, "DEV")
	if err != nil || len(items) != 1 || info != items[0] || info.UpdatedAt == "" {
		t.Fatalf("info = %#v; listed %#v, %v", info, items, err)
	}
	if _, err := svc.SetQueueSecretInfo(ctx, AgentActor("dev-1"), "DEV", "GH_TOKEN", "x"); ErrorCode(err) != "forbidden" {
		t.Fatalf("agent: %v", err)
	}
}

func TestQueueSecretsSetListRemove(t *testing.T) {
	ctx := context.Background()
	svc, actor := workflowFixture(t)
	if err := svc.SetQueueSecret(ctx, actor, "DEV", "ZED_TOKEN", "first"); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetQueueSecret(ctx, actor, "dev", "GH_TOKEN", "s3cr3t-value"); err != nil {
		t.Fatal(err)
	}
	// Setting an existing key replaces the value.
	if err := svc.SetQueueSecret(ctx, actor, "DEV", "ZED_TOKEN", "second"); err != nil {
		t.Fatal(err)
	}
	keys, err := svc.ListQueueSecretKeys(ctx, actor, "DEV")
	if err != nil || !reflect.DeepEqual(keys, []string{"GH_TOKEN", "ZED_TOKEN"}) {
		t.Fatalf("keys = %v, %v; want sorted keys", keys, err)
	}
	values, err := svc.queueSecrets(ctx, "DEV")
	if err != nil || !reflect.DeepEqual(values, map[string]string{"GH_TOKEN": "s3cr3t-value", "ZED_TOKEN": "second"}) {
		t.Fatalf("queueSecrets = %v, %v", values, err)
	}
	if err := svc.RemoveQueueSecret(ctx, actor, "DEV", "ZED_TOKEN"); err != nil {
		t.Fatal(err)
	}
	if err := svc.RemoveQueueSecret(ctx, actor, "DEV", "ZED_TOKEN"); ErrorCode(err) != "not_found" {
		t.Fatalf("remove missing error = %v; want not_found", err)
	}
	keys, _ = svc.ListQueueSecretKeys(ctx, actor, "DEV")
	if !reflect.DeepEqual(keys, []string{"GH_TOKEN"}) {
		t.Fatalf("keys after remove = %v", keys)
	}
}

func TestQueueSecretsValidation(t *testing.T) {
	ctx := context.Background()
	svc, actor := workflowFixture(t)
	for _, tc := range []struct {
		queue, key, value, code string
		status                  int
	}{
		{"DEV", "1BAD", "v", "invalid_secret_key", http.StatusBadRequest},
		{"DEV", "has-dash", "v", "invalid_secret_key", http.StatusBadRequest},
		{"DEV", "", "v", "invalid_secret_key", http.StatusBadRequest},
		{"DEV", "TARIBOY_TOOLS_SOCKET", "v", "invalid_secret_key", http.StatusBadRequest},
		{"DEV", "GH_TOKEN", "", "invalid_secret", http.StatusBadRequest},
		{"DEV", "GH_TOKEN", "bad\xffutf8", "invalid_secret", http.StatusBadRequest},
		{"DEV", "GH_TOKEN", strings.Repeat("x", 64<<10+1), "secret_too_large", http.StatusRequestEntityTooLarge},
		{"NOPE", "GH_TOKEN", "v", "not_found", http.StatusNotFound},
	} {
		err := svc.SetQueueSecret(ctx, actor, tc.queue, tc.key, tc.value)
		var domain *Error
		if !errors.As(err, &domain) || domain.Code != tc.code || domain.Status != tc.status {
			t.Fatalf("set %q: error = %v; want %s (%d)", tc.key, err, tc.code, tc.status)
		}
		if len(tc.value) > 1 && strings.Contains(err.Error(), tc.value) {
			t.Fatalf("error message echoes the value: %v", err)
		}
	}
	if err := svc.SetQueueSecret(ctx, actor, "DEV", "OK_KEY", strings.Repeat("x", 64<<10)); err != nil {
		t.Fatalf("a value of exactly 64 KiB: %v", err)
	}
	if err := svc.SetQueueSecret(ctx, actor, "DEV", "_under1", "v"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ListQueueSecretKeys(ctx, actor, "NOPE"); ErrorCode(err) != "not_found" {
		t.Fatalf("list of an unknown queue error = %v; want not_found", err)
	}
	svcEmpty, actorEmpty := workflowFixture(t)
	keys, err := svcEmpty.ListQueueSecretKeys(ctx, actorEmpty, "DEV")
	if err != nil || keys == nil || len(keys) != 0 {
		t.Fatalf("keys of a queue with none = %#v, %v; want an empty list", keys, err)
	}
}

func TestQueueSecretsAreCustomerOnly(t *testing.T) {
	ctx := context.Background()
	svc, actor := workflowFixture(t)
	if err := svc.SetQueueSecret(ctx, actor, "DEV", "GH_TOKEN", "v"); err != nil {
		t.Fatal(err)
	}
	agent := AgentActor("dev-1")
	for name, call := range map[string]func() error{
		"set":    func() error { return svc.SetQueueSecret(ctx, agent, "DEV", "GH_TOKEN", "x") },
		"list":   func() error { _, err := svc.ListQueueSecretKeys(ctx, agent, "DEV"); return err },
		"remove": func() error { return svc.RemoveQueueSecret(ctx, agent, "DEV", "GH_TOKEN") },
	} {
		if err := call(); ErrorCode(err) != "forbidden" {
			t.Fatalf("agent %s error = %v; want forbidden", name, err)
		}
	}
	for _, action := range []string{"queue_secrets", "secret_set", "secrets"} {
		if _, err := svc.AgentAction(ctx, agent, action, map[string]any{"queue": "DEV"}); err == nil {
			t.Fatalf("AgentAction accepted %q", action)
		}
	}
}

func TestQueueSecretEventsNameTheKeyOnly(t *testing.T) {
	ctx := context.Background()
	svc, actor := workflowFixture(t)
	if err := svc.SetQueueSecret(ctx, actor, "DEV", "GH_TOKEN", "hunter2-value"); err != nil {
		t.Fatal(err)
	}
	if err := svc.RemoveQueueSecret(ctx, actor, "DEV", "GH_TOKEN"); err != nil {
		t.Fatal(err)
	}
	rows, err := svc.db.Query(`SELECT kind, actor, payload FROM task_events WHERE queue_prefix = 'DEV' AND kind LIKE 'queue.secret_%' ORDER BY sequence`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var kinds []string
	for rows.Next() {
		var kind, who, payload string
		if err := rows.Scan(&kind, &who, &payload); err != nil {
			t.Fatal(err)
		}
		if who != actor.Principal || payload != `{"key":"GH_TOKEN"}` {
			t.Fatalf("event %s by %s payload %s; want the key only", kind, who, payload)
		}
		kinds = append(kinds, kind)
	}
	if !reflect.DeepEqual(kinds, []string{"queue.secret_set", "queue.secret_removed"}) {
		t.Fatalf("events = %v", kinds)
	}
}

func TestSetQueueWorkflowRequiresItsSecrets(t *testing.T) {
	ctx := context.Background()
	svc, actor := workflowFixture(t)
	digest := seedBindingImage(t, svc, "flow", "1", "developers")
	requireSecrets(t, svc, digest, "ZED_TOKEN", "GH_TOKEN", "OTHER")
	mustRebindPool(t, svc, actor, "developers", []string{"dev-1"}, 0)
	if err := svc.SetQueueSecret(ctx, actor, "DEV", "OTHER", "v"); err != nil {
		t.Fatal(err)
	}
	_, err := svc.SetQueueWorkflow(ctx, actor, "DEV", "flow:1", 0)
	var domain *Error
	if !errors.As(err, &domain) || domain.Code != "workflow_secret_missing" || domain.Status != http.StatusConflict ||
		!reflect.DeepEqual(domain.Data["secrets"], []string{"GH_TOKEN", "ZED_TOKEN"}) {
		t.Fatalf("missing secrets error = %#v", err)
	}
	if _, err := svc.GetQueueWorkflow(ctx, actor, "DEV"); ErrorCode(err) != "queue_workflow_not_found" {
		t.Fatalf("a refused bind left a binding: %v", err)
	}
	for _, key := range []string{"GH_TOKEN", "ZED_TOKEN"} {
		if err := svc.SetQueueSecret(ctx, actor, "DEV", key, "v"); err != nil {
			t.Fatal(err)
		}
	}
	bound, err := svc.SetQueueWorkflow(ctx, actor, "DEV", "flow:1", 0)
	if err != nil {
		t.Fatal(err)
	}
	// A bound manifest keeps the secrets it requires.
	err = svc.RemoveQueueSecret(ctx, actor, "DEV", "GH_TOKEN")
	if !errors.As(err, &domain) || domain.Code != "workflow_secret_missing" || domain.Status != http.StatusConflict ||
		!reflect.DeepEqual(domain.Data["secrets"], []string{"GH_TOKEN"}) {
		t.Fatalf("remove of a required secret error = %#v", err)
	}
	if keys, _ := svc.ListQueueSecretKeys(ctx, actor, "DEV"); len(keys) != 3 {
		t.Fatalf("a refused remove changed the keys: %v", keys)
	}
	// Once the queue is unbound the key may go.
	if err := svc.ClearQueueWorkflow(ctx, actor, "DEV", bound.Revision); err != nil {
		t.Fatal(err)
	}
	if err := svc.RemoveQueueSecret(ctx, actor, "DEV", "GH_TOKEN"); err != nil {
		t.Fatal(err)
	}
}

func TestSetQueueWorkflowReportsPoolsBeforeSecrets(t *testing.T) {
	ctx := context.Background()
	svc, actor := workflowFixture(t)
	digest := seedBindingImage(t, svc, "flow", "1", "developers")
	requireSecrets(t, svc, digest, "GH_TOKEN")
	if _, err := svc.SetQueueWorkflow(ctx, actor, "DEV", "flow:1", 0); ErrorCode(err) != "workflow_pool_empty" {
		t.Fatalf("error = %v; want workflow_pool_empty first", err)
	}
}
