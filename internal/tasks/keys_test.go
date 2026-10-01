package tasks

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"
)

var randomKeyRE = regexp.MustCompile(`^KEYS-[23456789abcdefghjkmnpqrstuvwxyz]{4}$`)

// randomKeyPattern matches a random key in any queue.
var randomKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]*-[23456789abcdefghjkmnpqrstuvwxyz]{4}$`)

func TestCreateTaskGeneratesRandomLowercaseKeys(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	actor := CustomerActor("customer")
	if _, err := svc.CreateQueue(ctx, actor, CreateQueueInput{Prefix: "KEYS", Name: "Keys"}); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for i := 0; i < 25; i++ {
		task, err := svc.CreateTask(ctx, actor, CreateTaskInput{Queue: "KEYS", Title: "t"})
		if err != nil {
			t.Fatal(err)
		}
		if !randomKeyRE.MatchString(task.Key) {
			t.Fatalf("key %q is not a random short key", task.Key)
		}
		if seen[task.Key] {
			t.Fatalf("duplicate key %q", task.Key)
		}
		seen[task.Key] = true
	}
}

func TestGetTaskAcceptsAnyKeyCasing(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	actor := CustomerActor("customer")
	if _, err := svc.CreateQueue(ctx, actor, CreateQueueInput{Prefix: "KEYS", Name: "Keys"}); err != nil {
		t.Fatal(err)
	}
	task, err := svc.CreateTask(ctx, actor, CreateTaskInput{Queue: "KEYS", Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	spellings := []string{
		task.Key,
		strings.ToUpper(task.Key),
		strings.ToLower(task.Key),
		" " + task.Key + " ",
	}
	for _, spelling := range spellings {
		detail, err := svc.GetTask(ctx, actor, spelling)
		if err != nil {
			t.Fatalf("get %q: %v", spelling, err)
		}
		if detail.Task.Key != task.Key {
			t.Fatalf("get %q returned %q", spelling, detail.Task.Key)
		}
	}
}

func TestMigrateLegacyKeysRewritesKeysAndKeepsOldOnesResolvable(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	actor := CustomerActor("customer")
	if _, err := svc.CreateQueue(ctx, actor, CreateQueueInput{Prefix: "KEYS", Name: "Keys"}); err != nil {
		t.Fatal(err)
	}
	task, err := svc.CreateTask(ctx, actor, CreateTaskInput{Queue: "KEYS", Title: "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	// Reproduce a database written before random keys existed.
	if _, err := svc.db.Exec(`UPDATE tasks SET task_key = 'KEYS-17' WHERE task_key = ?`, task.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`INSERT INTO agents(name, image_ref, current_goal_task_key)
		VALUES ('a', 'bare:latest', 'KEYS-17')`); err != nil {
		t.Fatal(err)
	}

	now := func() time.Time { return time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC) }
	if err := MigrateLegacyKeys(svc.db, now); err != nil {
		t.Fatal(err)
	}

	var migrated string
	if err := svc.db.QueryRow(`SELECT task_key FROM tasks`).Scan(&migrated); err != nil {
		t.Fatal(err)
	}
	if !randomKeyRE.MatchString(migrated) {
		t.Fatalf("migrated key = %q", migrated)
	}
	detail, err := svc.GetTask(ctx, actor, "KEYS-17")
	if err != nil {
		t.Fatalf("old key no longer resolves: %v", err)
	}
	if detail.Task.Key != migrated {
		t.Fatalf("old key resolved to %q, want %q", detail.Task.Key, migrated)
	}
	var goal string
	if err := svc.db.QueryRow(`SELECT current_goal_task_key FROM agents WHERE name = 'a'`).Scan(&goal); err != nil {
		t.Fatal(err)
	}
	if goal != migrated {
		t.Fatalf("agent goal key = %q, want %q", goal, migrated)
	}

	// Idempotent: a second run has nothing left to rewrite.
	if err := MigrateLegacyKeys(svc.db, now); err != nil {
		t.Fatal(err)
	}
	var after string
	if err := svc.db.QueryRow(`SELECT task_key FROM tasks`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != migrated {
		t.Fatalf("second run rewrote %q to %q", migrated, after)
	}
}

func TestMigrateLegacyKeysKeepsLaterAllDigitRandomKeys(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	actor := CustomerActor("customer")
	if _, err := svc.CreateQueue(ctx, actor, CreateQueueInput{Prefix: "KEYS", Name: "Keys"}); err != nil {
		t.Fatal(err)
	}
	now := func() time.Time { return time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC) }
	if err := MigrateLegacyKeys(svc.db, now); err != nil {
		t.Fatal(err)
	}
	task, err := svc.CreateTask(ctx, actor, CreateTaskInput{Queue: "KEYS", Title: "digits"})
	if err != nil {
		t.Fatal(err)
	}
	// The random alphabet includes 2-9, so a new key can be all digits.
	if _, err := svc.db.Exec(`UPDATE tasks SET task_key = 'KEYS-3469' WHERE task_key = ?`, task.Key); err != nil {
		t.Fatal(err)
	}
	if err := MigrateLegacyKeys(svc.db, now); err != nil {
		t.Fatal(err)
	}
	var after string
	if err := svc.db.QueryRow(`SELECT task_key FROM tasks`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != "KEYS-3469" {
		t.Fatalf("a later run rewrote random key KEYS-3469 to %q", after)
	}
}
