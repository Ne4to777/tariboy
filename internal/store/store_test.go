package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenMigrates(t *testing.T) {
	s := open(t)
	v, err := s.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v < 1 {
		t.Fatalf("schema version = %d, want >= 1", v)
	}
}

func TestOpenCreatesWorkflowImages(t *testing.T) {
	s := open(t)
	requireTable(t, s.DB, "task_workflow_images")
	var unique int
	err := s.DB.QueryRow(`SELECT "unique" FROM pragma_index_list('task_workflow_images') WHERE name = 'idx_task_workflow_images_name_version'`).Scan(&unique)
	if err != nil {
		t.Fatalf("index idx_task_workflow_images_name_version is missing: %v", err)
	}
	if unique != 1 {
		t.Fatal("index idx_task_workflow_images_name_version is not unique")
	}
	rows, err := s.DB.Query(`SELECT name FROM pragma_index_info('idx_task_workflow_images_name_version') ORDER BY seqno`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	if strings.Join(cols, ",") != "name,version" {
		t.Fatalf("index columns = %v, want name,version", cols)
	}
	insert := `INSERT INTO task_workflow_images (digest, name, version, manifest, built_at) VALUES (?, 'demo', '1.0.0', '{}', 'now')`
	if _, err := s.DB.Exec(insert, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(insert, "b"); err == nil {
		t.Fatal("a second digest for the same name and version was accepted")
	}
}

func TestOpenCreatesWorkflowEngineSchema(t *testing.T) {
	s := open(t)
	for _, table := range []string{"task_queue_workflows", "task_workflow_holders", "task_status_visits", "task_transition_requests", "task_artifacts"} {
		requireTable(t, s.DB, table)
	}
	requireColumn(t, s.DB, "tasks", "workflow_digest")
	requireColumn(t, s.DB, "tasks", "workflow_paused_reason")
	requireColumn(t, s.DB, "tasks", "workflow_status")
	for _, index := range []string{"idx_tasks_workflow_digest", "idx_task_artifacts_current", "idx_task_transition_requests_one_pending"} {
		var name string
		if err := s.DB.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`, index).Scan(&name); err != nil {
			t.Fatalf("index %s is missing: %v", index, err)
		}
	}

	exec := func(query string, args ...any) error {
		_, err := s.DB.Exec(query, args...)
		return err
	}
	if err := exec(`INSERT INTO task_queues(prefix, name, created_at, updated_at) VALUES ('DEV', 'Dev', 'now', 'now')`); err != nil {
		t.Fatal(err)
	}
	if err := exec(`INSERT INTO tasks(task_key, queue_prefix, title, author, customer, created_at, updated_at) VALUES ('DEV-1', 'DEV', 'one', 'user:c', 'user:c', 'now', 'now')`); err != nil {
		t.Fatal(err)
	}
	if err := exec(`INSERT INTO task_status_visits(task_id, sequence, status_id, entered_at, entered_by) VALUES (1, 1, 'draft', 'now', 'system')`); err != nil {
		t.Fatal(err)
	}
	request := `INSERT INTO task_transition_requests(task_id, visit_id, outcome, actor, state, created_at) VALUES (1, 1, 'done', 'agent:a', ?, 'now')`
	if err := exec(request, "pending"); err != nil {
		t.Fatal(err)
	}
	if err := exec(request, "pending"); err == nil {
		t.Fatal("a second pending transition request for one task was accepted")
	}
	if err := exec(request, "applied"); err != nil {
		t.Fatalf("a finished request beside a pending one was refused: %v", err)
	}
	if err := exec(request, "bogus"); err == nil {
		t.Fatal("an unknown request state was accepted")
	}
}

func TestOpenCreatesScriptRunSchema(t *testing.T) {
	s := open(t)
	requireTable(t, s.DB, "task_script_runs")
	requireColumn(t, s.DB, "task_status_visits", "next_watch_at")
	exec := func(query string, args ...any) error {
		_, err := s.DB.Exec(query, args...)
		return err
	}
	for _, query := range []string{
		`INSERT INTO task_queues(prefix, name, created_at, updated_at) VALUES ('DEV', 'Dev', 'now', 'now')`,
		`INSERT INTO tasks(task_key, queue_prefix, title, author, customer, created_at, updated_at) VALUES ('DEV-1', 'DEV', 'one', 'user:c', 'user:c', 'now', 'now')`,
		`INSERT INTO task_status_visits(task_id, sequence, status_id, entered_at, entered_by) VALUES (1, 1, 'merge', 'now', 'system')`,
	} {
		if err := exec(query); err != nil {
			t.Fatal(err)
		}
	}
	var next string
	if err := s.DB.QueryRow(`SELECT next_watch_at FROM task_status_visits WHERE id = 1`).Scan(&next); err != nil || next != "" {
		t.Fatalf("next_watch_at = %q, %v", next, err)
	}
	run := `INSERT INTO task_script_runs(task_id, visit_id, kind, script, run_as, state, created_at) VALUES (1, 1, ?, 'w.sh', ?, ?, 'now')`
	if err := exec(run, "watch", "queue", "pending"); err != nil {
		t.Fatal(err)
	}
	if err := exec(run, "watch", "queue", "running"); err == nil {
		t.Fatal("a second active run for one task was accepted")
	}
	if err := exec(run, "watch", "queue", "finished"); err != nil {
		t.Fatalf("a finished run beside an active one was refused: %v", err)
	}
	for _, bad := range [][]any{{"other", "queue", "finished"}, {"check", "shell", "finished"}, {"check", "queue", "bogus"}} {
		if err := exec(run, bad...); err == nil {
			t.Fatalf("run %v was accepted", bad)
		}
	}
}

// holderMigrationFixture opens a database migrated up to before migration
// first, with one task and its holder row, and reopens it with every
// migration.
func holderMigrationFixture(t *testing.T, first string) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "holder-migration.db")
	legacy := openBeforeMigration(t, path, first)
	if _, err := legacy.DB.Exec(`
		INSERT INTO task_queues(prefix, name, created_at, updated_at) VALUES ('DEV', 'Dev', 'now', 'now');
		INSERT INTO tasks(task_key, queue_prefix, title, author, customer, created_at, updated_at)
		VALUES ('DEV-1', 'DEV', 'one', 'user:c', 'user:c', 'now', 'now');
		INSERT INTO task_workflow_holders(task_id, pool, agent, dispatched_at) VALUES (1, 'developers', 'dev-1', 'now');`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.DB.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestHolderUnavailableSinceMigrationDefaultsToEmpty(t *testing.T) {
	requireColumn(t, open(t).DB, "task_workflow_holders", "unavailable_since")
	s := holderMigrationFixture(t, "0061_task_workflow_holder_unavailable.sql")
	var since string
	if err := s.DB.QueryRow(`SELECT unavailable_since FROM task_workflow_holders WHERE task_id = 1`).Scan(&since); err != nil || since != "" {
		t.Fatalf("unavailable_since = %q, %v; want empty", since, err)
	}
}

func TestOpenRemovesJudgeTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remove-judge.db")
	db := createDatabaseBeforeMigration(t, path, "0043_remove_judge.sql")
	requireTable(t, db, "judge_runs")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, name := range []string{"judge_runs", "judge_automation_revisions", "improvement_proposals", "image_releases"} {
		var count int
		if err := s.DB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("removed table %q still exists", name)
		}
	}
}

func TestAgentGoalsMigrationPreservesTasksAndAddsReleaseFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-goals-upgrade.db")
	db := createDatabaseBeforeMigration(t, path, "0037_agent_goals.sql")
	if _, err := db.Exec(`
		INSERT INTO agents(name,image_ref) VALUES ('worker','basic:latest');
		INSERT INTO task_queues(prefix,name,created_at,updated_at)
		VALUES ('TEST','Tests','2026-09-03T00:00:00Z','2026-09-03T00:00:00Z');
		INSERT INTO task_workflow_versions(id,name,version,definition,state,created_at,updated_at)
		VALUES (1,'delivery',1,'{}','published','2026-09-03T00:00:00Z','2026-09-03T00:00:00Z');
		INSERT INTO tasks(
			id,task_key,queue_prefix,title,status,author,customer,
			workflow_version_id,workflow_status,workflow_revision,created_at,updated_at
		) VALUES (
			1,'TEST-1','TEST','ship','open','agent:worker','user:customer',
			1,'build',4,'2026-09-03T00:00:00Z','2026-09-03T00:00:00Z'
		);
		INSERT INTO task_events(event_id,task_id,queue_prefix,kind,actor,task_revision,created_at)
		VALUES ('event-1',1,'TEST','task.created','agent:worker',1,'2026-09-03T00:00:00Z');
	`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()

	var status, pullRequest string
	if err := upgraded.DB.QueryRow(`
		SELECT status,pull_request FROM tasks WHERE task_key='TEST-1'`,
	).Scan(&status, &pullRequest); err != nil {
		t.Fatal(err)
	}
	if status != "open" || pullRequest != "" {
		t.Fatalf("migrated task = %q %q", status, pullRequest)
	}
	// 0054 later removes the workflow engine; its event proves 0037 carried
	// the workflow columns through the tasks rebuild.
	var workflowStatus string
	if err := upgraded.DB.QueryRow(`
		SELECT json_extract(payload,'$.workflow_status') FROM task_events
		WHERE task_id=1 AND kind='workflow.removed'`,
	).Scan(&workflowStatus); err != nil || workflowStatus != "build" {
		t.Fatalf("workflow.removed status = %q, %v", workflowStatus, err)
	}
	var eventTaskID int64
	if err := upgraded.DB.QueryRow(`SELECT task_id FROM task_events WHERE event_id='event-1'`).Scan(&eventTaskID); err != nil || eventTaskID != 1 {
		t.Fatalf("inbound task event = %d, %v", eventTaskID, err)
	}
	var goalEnabled, timeout int
	var currentKey string
	if err := upgraded.DB.QueryRow(`
		SELECT goal_enabled,goal_wait_customer_timeout_s,current_goal_task_key
		FROM agents WHERE name='worker'`,
	).Scan(&goalEnabled, &timeout, &currentKey); err != nil || goalEnabled != 1 || timeout != 300 || currentKey != "" {
		t.Fatalf("agent goal defaults = %d %d %q, %v", goalEnabled, timeout, currentKey, err)
	}
}

func TestScriptRunsMigrationPreservesLegacyHistoryAndBusSchedules(t *testing.T) {
	path := filepath.Join(t.TempDir(), "script-runs-upgrade.db")
	db := createDatabaseBeforeMigration(t, path, "0034_script_runs_and_outbox.sql")
	if _, err := db.Exec(`
		INSERT INTO scripts(id,agent,name,description,command,mode,interval_seconds,status,pid,last_exit,created_at,last_started_at,last_finished_at,next_run_at,log_path) VALUES
		('once','alice','check','one shot','make check','once',NULL,'done',NULL,0,'2026-08-20T06:00:00Z','2026-08-20T06:01:00Z','2026-08-20T06:02:00Z',NULL,'/tmp/once.log'),
		('every','alice','watch','recurring','make watch','every',30,'waiting',NULL,2,'2026-08-20T06:03:00Z','2026-08-20T06:04:00Z','2026-08-20T06:05:00Z','2026-08-20T06:05:30Z','/tmp/every.log'),
		('running','alice','build','running','make build','once',NULL,'running',4242,NULL,'2026-08-20T06:06:00Z','2026-08-20T06:07:00Z',NULL,NULL,'/tmp/running.log');
		INSERT INTO schedules(id,agent,kind,spec,channel,message_template,next_fire_at,enabled,created_at,correlation_id)
		VALUES('schedule-1','alice','oneshot','2026-08-21T00:00:00Z','inbox:alice','{"text":"wake"}','2026-08-21T00:00:00Z',1,'2026-08-20T00:00:00Z','request-1');`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()

	requireTable(t, upgraded.DB, "script_runs")
	requireTable(t, upgraded.DB, "script_result_outbox")
	var command, mode, state, nextRun string
	var interval int
	if err := upgraded.DB.QueryRow(`SELECT command,mode,interval_seconds,state,COALESCE(next_run_at,'') FROM scripts WHERE id='every'`).Scan(&command, &mode, &interval, &state, &nextRun); err != nil {
		t.Fatal(err)
	}
	if command != "make watch" || mode != "every" || interval != 30 || state != "active" || nextRun != "2026-08-20T06:05:30Z" {
		t.Fatalf("migrated recurring definition = %q %q %d %q %q", command, mode, interval, state, nextRun)
	}
	var status, startedAt, finishedAt, logPath string
	var exitCode int
	if err := upgraded.DB.QueryRow(`SELECT status,exit_code,COALESCE(started_at,''),COALESCE(finished_at,''),log_path FROM script_runs WHERE script_id='every'`).Scan(&status, &exitCode, &startedAt, &finishedAt, &logPath); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || exitCode != 2 || startedAt != "2026-08-20T06:04:00Z" || finishedAt != "2026-08-20T06:05:00Z" || logPath != "/tmp/every.log" {
		t.Fatalf("migrated recurring run = %q %d %q %q %q", status, exitCode, startedAt, finishedAt, logPath)
	}
	var runningState string
	if err := upgraded.DB.QueryRow(`SELECT state FROM scripts WHERE id='running'`).Scan(&runningState); err != nil || runningState != "completed" {
		t.Fatalf("running definition state=%q err=%v", runningState, err)
	}
	var migratedRunID string
	if err := upgraded.DB.QueryRow(`SELECT id,status,COALESCE(started_at,''),log_path FROM script_runs WHERE script_id='running'`).Scan(&migratedRunID, &status, &startedAt, &logPath); err != nil {
		t.Fatal(err)
	}
	if migratedRunID[:5] != "srun-" || status != "interrupted" || startedAt != "2026-08-20T06:07:00Z" || logPath != "/tmp/running.log" {
		t.Fatalf("migrated interrupted run = %q %q %q %q", migratedRunID, status, startedAt, logPath)
	}
	var outboxRunID, payload string
	if err := upgraded.DB.QueryRow(`SELECT run_id,payload FROM script_result_outbox WHERE script_id='running'`).Scan(&outboxRunID, &payload); err != nil {
		t.Fatalf("migrated running result was not queued: %v", err)
	}
	if outboxRunID != migratedRunID || !strings.Contains(payload, `"status":"interrupted"`) || !strings.Contains(payload, `"log_path":"/tmp/running.log"`) {
		t.Fatalf("migrated interruption outbox = run %q payload %q", outboxRunID, payload)
	}
	var scheduleSnapshot string
	if err := upgraded.DB.QueryRow(`SELECT id||'|'||agent||'|'||kind||'|'||spec||'|'||channel||'|'||message_template||'|'||next_fire_at||'|'||enabled||'|'||created_at||'|'||correlation_id FROM schedules WHERE id='schedule-1'`).Scan(&scheduleSnapshot); err != nil {
		t.Fatal(err)
	}
	if want := `schedule-1|alice|oneshot|2026-08-21T00:00:00Z|inbox:alice|{"text":"wake"}|2026-08-21T00:00:00Z|1|2026-08-20T00:00:00Z|request-1`; scheduleSnapshot != want {
		t.Fatalf("schedule changed: got %q want %q", scheduleSnapshot, want)
	}
}

func TestPricingMigrationAddsSourcesAndGroupSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pricing-upgrade.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() >= "0033_pricing_catalog_group_usage.sql" {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			t.Fatalf("apply %s: %v", entry.Name(), err)
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations(name) VALUES (?)`, entry.Name()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO ai_pricing(model, input_per_mtok, output_per_mtok, cache_write_per_mtok, cache_read_per_mtok) VALUES
		('operator-model', 7, 8, 9, 10),
		('claude-opus-4-8', 5, 25, 6.25, 0.5)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	requireColumn(t, s.DB, "ai_pricing", "source")
	requireColumn(t, s.DB, "ai_requests", "group_id")
	requireColumn(t, s.DB, "ai_requests", "group_name")
	var index string
	if err := s.DB.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_ai_requests_group_ts'`).Scan(&index); err != nil {
		t.Fatalf("group snapshot index missing: %v", err)
	}
	var source string
	if err := s.DB.QueryRow(`SELECT source FROM ai_pricing WHERE model='operator-model'`).Scan(&source); err != nil || source != "manual" {
		t.Fatalf("operator row source=%q err=%v, want manual", source, err)
	}
	if _, err := s.DB.Exec(`INSERT INTO ai_pricing(model) VALUES ('default-source-model')`); err != nil {
		t.Fatalf("insert default-source row: %v", err)
	}
	if err := s.DB.QueryRow(`SELECT source FROM ai_pricing WHERE model='default-source-model'`).Scan(&source); err != nil || source != "manual" {
		t.Fatalf("default row source=%q err=%v, want manual", source, err)
	}
	var count int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM ai_pricing WHERE model='claude-opus-4-8'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("built-in seed rows retained=%d err=%v, want 0", count, err)
	}
}

func TestTaskWorkflowMigrationKeepsPoolsTriggersAndTaskColumns(t *testing.T) {
	s := open(t)
	for _, table := range []string{
		"task_workflow_versions", "task_agent_pools", "task_agent_pool_members",
		"task_queue_workflow_triggers", "task_workflow_ingress_state",
		"task_workflow_message_sequence",
	} {
		requireTable(t, s.DB, table)
	}
	for _, column := range []string{"workflow_version_id", "workflow_status", "workflow_revision"} {
		requireColumn(t, s.DB, "tasks", column)
	}
	requireColumn(t, s.DB, "task_queue_workflow_triggers", "created_after_sequence")
	requireColumn(t, s.DB, "task_queue_workflow_triggers", "activation_sequence_set")
}

func TestWorkflowEngineRemovalMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflow-engine-removal.db")
	legacy := openBeforeMigration(t, path, "0054_drop_task_workflow_engine.sql")
	const now = "2026-10-01T00:00:00Z"
	if _, err := legacy.DB.Exec(`
		INSERT INTO agents(name,image_ref) VALUES ('worker','basic:latest');
		INSERT INTO task_queues(prefix,name,created_at,updated_at) VALUES ('DEV','Dev',?1,?1);
		INSERT INTO task_workflow_versions(id,name,version,definition,state,created_at,updated_at,published_at)
		VALUES (1,'dev',1,'{}','published',?1,?1,?1);
		INSERT INTO task_queue_workflows(queue_prefix,workflow_version_id,bound_by,bound_at) VALUES ('DEV',1,'user:op',?1);
		INSERT INTO tasks(id,task_key,queue_prefix,title,author,customer,status,created_at,updated_at,
			workflow_version_id,workflow_status,workflow_revision)
		VALUES (1,'DEV-1','DEV','work','user:op','user:op','in_progress',?1,?1,1,'implement',3);
		INSERT INTO tasks(id,task_key,queue_prefix,title,author,customer,status,created_at,updated_at)
		VALUES (2,'DEV-2','DEV','flexible','user:op','user:op','open',?1,?1);
		INSERT INTO task_agent_pools(id,queue_prefix,name,created_at,updated_at) VALUES (1,'DEV','builders',?1,?1);
		INSERT INTO task_agent_pool_members(pool_id,agent,position) VALUES (1,'worker',0);
		INSERT INTO task_status_executions(id,task_id,workflow_version_id,status_id,sequence,task_revision,created_at)
		VALUES (1,1,1,'implement',1,1,?1);
		INSERT INTO task_requirement_executions(id,status_execution_id,requirement_id,pool_id,dispatch,created_at)
		VALUES (1,1,'code',1,'claim_one',?1);
		INSERT INTO task_assignments(id,requirement_execution_id,agent,attempt,state,created_at,updated_at)
		VALUES (1,1,'worker',1,'leased',?1,?1);
		INSERT INTO task_artifacts(task_id,assignment_id,name,type,created_by,created_at,updated_at)
		VALUES (1,1,'plan','markdown','agent:worker',?1,?1);
		INSERT INTO task_workflow_questions(id,task_id,assignment_id,question,context,blocking_scope,created_at)
		VALUES (1,1,1,'why','ctx','assignment',?1);
		INSERT INTO task_workflow_holds(task_id,assignment_id,question_id,scope,created_at) VALUES (1,1,1,'assignment',?1);
		INSERT INTO task_workflow_subscriptions(id,task_id,assignment_id,pattern,created_by,created_at)
		VALUES (1,1,1,'metrics:api','agent:worker',?1);
		INSERT INTO task_observations(task_id,subscription_id,assignment_id,kind,observed_at) VALUES (1,1,1,'alert',?1);
		INSERT INTO task_workflow_outbox(wake_id,task_id,assignment_id,kind,next_attempt_at) VALUES ('wake-1',1,1,'workflow.assignment_ready',?1);
		INSERT INTO task_queue_workflow_triggers(queue_prefix,pattern,action,enabled,created_by,created_at,updated_at)
		VALUES ('DEV','issue-provider:*','create_task',1,'user:op',?1,?1);`, now); err != nil {
		t.Fatal(err)
	}
	if err := legacy.DB.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var status string
	var versionID, workflowStatus, workflowRevision sql.NullString
	if err := s.DB.QueryRow(`SELECT status,workflow_version_id,workflow_status,workflow_revision FROM tasks WHERE id=1`).
		Scan(&status, &versionID, &workflowStatus, &workflowRevision); err != nil {
		t.Fatal(err)
	}
	if status != "in_progress" || versionID.Valid || workflowStatus.Valid || workflowRevision.Valid {
		t.Fatalf("migrated task status=%q workflow=%v/%v/%v; want in_progress and NULL workflow columns",
			status, versionID, workflowStatus, workflowRevision)
	}
	var removedStatus string
	if err := s.DB.QueryRow(`SELECT json_extract(payload,'$.workflow_status') FROM task_events WHERE task_id=1 AND kind='workflow.removed'`).
		Scan(&removedStatus); err != nil || removedStatus != "implement" {
		t.Fatalf("workflow.removed event status=%q err=%v; want implement", removedStatus, err)
	}
	var flexibleEvents int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM task_events WHERE task_id=2 AND kind='workflow.removed'`).Scan(&flexibleEvents); err != nil || flexibleEvents != 0 {
		t.Fatalf("flexible task workflow.removed events=%d err=%v; want 0", flexibleEvents, err)
	}
	for _, table := range []string{
		"task_observations", "task_workflow_subscriptions", "task_workflow_holds",
		"task_workflow_questions", "task_assignments",
		"task_requirement_executions", "task_status_executions",
		"task_workflow_outbox",
	} {
		var count int
		if err := s.DB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("dropped table %q still exists", table)
		}
	}
	requireTable(t, s.DB, "task_workflow_versions")
	for table, want := range map[string]int{
		"task_workflow_versions": 0,
		// 0055 recreates these two names with the new engine's shapes; the
		// removal must not have carried any old rows over.
		"task_artifacts":                 0,
		"task_queue_workflows":           0,
		"task_agent_pools":               1,
		"task_agent_pool_members":        1,
		"task_queue_workflow_triggers":   1,
		"task_workflow_ingress_state":    1,
		"task_workflow_message_sequence": 0,
	} {
		var count int
		if err := s.DB.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != want {
			t.Errorf("%s rows = %d; want %d", table, count, want)
		}
	}
}

func TestWorkflowActivationSequenceMigrationPreservesLegacyTargets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflow-sequence-upgrade.db")
	db := createDatabaseAppliedThrough0027(t, path)
	if _, err := db.Exec(`
		INSERT INTO task_queues(prefix,name,created_at,updated_at) VALUES ('DEV','Dev','2026-08-07T00:00:00Z','2026-08-07T00:00:00Z');
		INSERT INTO task_queue_workflow_triggers(queue_prefix,pattern,action,enabled,created_by,created_at,updated_at) VALUES ('DEV','issue-provider:*','create_task',1,'operator','2026-08-07T00:00:00Z','2026-08-07T00:00:00Z');`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	var watermark int64
	var sequenceSet bool
	if err := upgraded.DB.QueryRow(`SELECT created_after_sequence,activation_sequence_set FROM task_queue_workflow_triggers`).Scan(&watermark, &sequenceSet); err != nil || watermark != 0 || sequenceSet {
		t.Fatalf("legacy trigger watermark/set=%d/%v err=%v; want 0/false", watermark, sequenceSet, err)
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "x.db")
	s1, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	s1.Close()
	s2, err := Open(db) // re-running migrations must be a no-op
	if err != nil {
		t.Fatal(err)
	}
	s2.Close()
}

func TestConfigGetSet(t *testing.T) {
	s := open(t)
	if _, ok, _ := s.ConfigGet("k"); ok {
		t.Fatal("unset key reported ok")
	}
	if err := s.ConfigSet("k", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfigSet("k", "v2"); err != nil { // upsert
		t.Fatal(err)
	}
	v, ok, err := s.ConfigGet("k")
	if err != nil || !ok || v != "v2" {
		t.Fatalf("got %q ok=%v err=%v", v, ok, err)
	}
}

func TestAddEvent(t *testing.T) {
	s := open(t)
	if err := s.AddEvent("", "daemon_start", `{"pid":1}`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("events = %d, want 1", n)
	}
}

func TestOpenEscapesPathAndLimitsConns(t *testing.T) {
	// A base dir containing spaces and shell-special characters must still open:
	// SQLite URI filenames are percent-decoded, so url.PathEscape makes them safe.
	dir := filepath.Join(t.TempDir(), "a b & c")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(dir, "x.db"))
	if err != nil {
		t.Fatalf("open in tricky path: %v", err)
	}
	defer s.Close()
	if got := s.DB.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("MaxOpenConnections = %d, want 1", got)
	}
	if err := s.ConfigSet("k", "v"); err != nil {
		t.Fatal(err)
	}
}

func TestMigration0011DropsState(t *testing.T) {
	s := open(t)
	rows, err := s.DB.Query(`SELECT name FROM pragma_table_info('agents')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			t.Fatal(err)
		}
		cols[col] = true
	}
	if cols["state"] {
		t.Fatal("state column should be dropped by migration 0011")
	}
	if !cols["error_reason"] {
		t.Fatal("error_reason column should exist")
	}
}

func TestTaskPriorityMigrationPreservesLegacyTaskData(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	s := openBeforeTaskPriorityMigration(t, dbPath)
	now := "2026-08-03T00:00:00Z"
	if _, err := s.DB.Exec(`
		INSERT INTO task_queues(prefix, name, created_at, updated_at)
		VALUES ('TEST', 'Test', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`
		INSERT INTO tasks(task_key, queue_prefix, position, title, author, customer, created_at, updated_at)
		VALUES
			('TEST-1', 'TEST', 0, 'legacy parent', 'user:test', 'user:test', ?, ?),
			('TEST-2', 'TEST', 1, 'legacy child', 'user:test', 'user:test', ?, ?)`, now, now, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`
		INSERT INTO task_relations(source_id, target_id, type, created_by, created_at)
		VALUES (1, 2, 'related', 'user:test', ?);
		INSERT INTO task_comments(task_id, author, body, created_at, updated_at)
		VALUES (1, 'user:test', 'legacy comment', ?, ?);
		INSERT INTO task_waiting_for(task_id, expected_principal, requesting_principal, requesting_comment_id, requested_at)
		VALUES (1, 'agent:test', 'user:test', 1, ?);
		INSERT INTO task_events(event_id, task_id, queue_prefix, kind, actor, task_revision, created_at)
		VALUES ('event-1', 1, 'TEST', 'task.updated', 'user:test', 1, ?)
	`, now, now, now, now, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("upgrade legacy task data: %v", err)
	}
	defer s.Close()

	var taskCount, relationCount, commentCount, waitingCount, eventCount int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM tasks WHERE priority = 'P2'`).Scan(&taskCount); err != nil {
		t.Fatal(err)
	}
	for query, dest := range map[string]*int{
		`SELECT COUNT(*) FROM task_relations`:   &relationCount,
		`SELECT COUNT(*) FROM task_comments`:    &commentCount,
		`SELECT COUNT(*) FROM task_waiting_for`: &waitingCount,
		`SELECT COUNT(*) FROM task_events`:      &eventCount,
	} {
		if err := s.DB.QueryRow(query).Scan(dest); err != nil {
			t.Fatal(err)
		}
	}
	if taskCount != 2 || relationCount != 1 || commentCount != 1 || waitingCount != 1 || eventCount != 1 {
		t.Fatalf("row counts after migration = tasks:%d relations:%d comments:%d waiting:%d events:%d, want 2/1/1/1/1",
			taskCount, relationCount, commentCount, waitingCount, eventCount)
	}
	var fkViolation string
	if err := s.DB.QueryRow(`SELECT "table" FROM pragma_foreign_key_check LIMIT 1`).Scan(&fkViolation); err != sql.ErrNoRows {
		t.Fatalf("foreign key check returned table %q, err %v", fkViolation, err)
	}
	var foreignKeysEnabled int
	if err := s.DB.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeysEnabled); err != nil {
		t.Fatal(err)
	}
	if foreignKeysEnabled != 1 {
		t.Fatalf("foreign_keys = %d after migration, want 1", foreignKeysEnabled)
	}
	wantIndexes := map[string]bool{
		"idx_tasks_parent":   false,
		"idx_tasks_queue":    false,
		"idx_tasks_author":   false,
		"idx_tasks_assignee": false,
		"idx_tasks_group":    false,
		"idx_tasks_status":   false,
	}
	rows, err := s.DB.Query(`SELECT name FROM pragma_index_list('tasks')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if _, ok := wantIndexes[name]; ok {
			wantIndexes[name] = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for name, found := range wantIndexes {
		if !found {
			t.Errorf("index %s missing after migration", name)
		}
	}
}

func openBeforeTaskPriorityMigration(t *testing.T, path string) *Store {
	t.Helper()
	return openBeforeMigration(t, path, "0025_task_priority.sql")
}

// openBeforeMigration applies every migration ordered before first, so a test
// can seed legacy rows and then let Open run first and everything after it.
func openBeforeMigration(t *testing.T, path, first string) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{DB: db}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (
		name TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')))`); err != nil {
		t.Fatal(err)
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Name() < first {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			t.Fatalf("apply legacy migration %s: %v", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations(name) VALUES (?)`, name); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(fmt.Errorf("commit legacy migration %s: %w", name, err))
		}
	}
	return s
}

func TestTaskAssignmentIterationMigrationUpgradesApplied0026(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		if entry.Name() <= "0026_task_workflows.sql" {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations(name) VALUES (?)`, name); err != nil {
			t.Fatal(err)
		}
	}
	var before int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('task_assignments') WHERE name='lease_iteration'`).Scan(&before); err != nil || before != 0 {
		t.Fatalf("precondition column=%d err=%v", before, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	// 0054 drops task_assignments later; the upgrade must still apply 0027 once.
	var applied int
	if err := upgraded.DB.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name='0027_task_assignment_iteration.sql'`).Scan(&applied); err != nil || applied != 1 {
		t.Fatalf("0027 records=%d err=%v", applied, err)
	}
}

func TestTaskAssignmentIterationMigrationAcceptsIntermediate0026Column(t *testing.T) {
	path := filepath.Join(t.TempDir(), "intermediate.db")
	db := createDatabaseAppliedThrough0026(t, path)
	if _, err := db.Exec(`ALTER TABLE task_assignments ADD COLUMN lease_iteration TEXT NOT NULL DEFAULT ''`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO task_queues(prefix,name,created_at,updated_at) VALUES ('DEV','Dev','2026-08-07T00:00:00Z','2026-08-07T00:00:00Z');
		INSERT INTO task_workflow_versions(id,name,version,definition,state,created_at,updated_at) VALUES (1,'dev',1,'{}','published','2026-08-07T00:00:00Z','2026-08-07T00:00:00Z');
		INSERT INTO tasks(id,task_key,queue_prefix,title,author,customer,created_at,updated_at,workflow_version_id,workflow_status,workflow_revision) VALUES (1,'DEV-1','DEV','work','agent:a','user:u','2026-08-07T00:00:00Z','2026-08-07T00:00:00Z',1,'impl',1);
		INSERT INTO task_status_executions(id,task_id,workflow_version_id,status_id,sequence,task_revision,created_at) VALUES (1,1,1,'impl',1,1,'2026-08-07T00:00:00Z');
		INSERT INTO task_requirement_executions(id,status_execution_id,requirement_id,dispatch,created_at) VALUES (1,1,'code','claim_one','2026-08-07T00:00:00Z');
		INSERT INTO task_assignments(id,requirement_execution_id,attempt,state,lease_owner,lease_expires_at,created_at,updated_at,lease_iteration) VALUES (1,1,1,'leased','agent:a','2030-01-01T00:00:00Z','2026-08-07T00:00:00Z','2026-08-07T00:00:00Z','iter-preserved');`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO daemon_config(key,value) VALUES ('migration-marker','preserved')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	var applied int
	if err := upgraded.DB.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name='0027_task_assignment_iteration.sql'`).Scan(&applied); err != nil || applied != 1 {
		t.Fatalf("0027 records=%d err=%v", applied, err)
	}
	value, ok, err := upgraded.ConfigGet("migration-marker")
	if err != nil || !ok || value != "preserved" {
		t.Fatalf("preserved value=%q ok=%v err=%v", value, ok, err)
	}
}

func createDatabaseAppliedThrough0026(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		if entry.Name() <= "0026_task_workflows.sql" {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations(name) VALUES (?)`, name); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func createDatabaseBeforeMigration(t *testing.T, path, stopBefore string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Name() < stopBefore {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations(name) VALUES (?)`, name); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func createDatabaseAppliedThrough0027(t *testing.T, path string) *sql.DB {
	t.Helper()
	db := createDatabaseAppliedThrough0026(t, path)
	body, err := migrationsFS.ReadFile("migrations/0027_task_assignment_iteration.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(body)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations(name) VALUES ('0027_task_assignment_iteration.sql')`); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestTaskPriorityConstraint(t *testing.T) {
	s := open(t)
	now := "2026-08-03T00:00:00Z"
	if _, err := s.DB.Exec(`
		INSERT INTO task_queues(prefix, name, created_at, updated_at)
		VALUES ('TEST', 'Test', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	for i, priority := range []string{"P0", "P1", "P2", "P3"} {
		if _, err := s.DB.Exec(`
			INSERT INTO tasks(task_key, queue_prefix, position, title, author, customer, priority, created_at, updated_at)
			VALUES (?, 'TEST', ?, 'valid', 'user:test', 'user:test', ?, ?, ?)`,
			"TEST-"+priority, i, priority, now, now); err != nil {
			t.Fatalf("insert priority %s: %v", priority, err)
		}
	}
	if _, err := s.DB.Exec(`
		INSERT INTO tasks(task_key, queue_prefix, position, title, author, customer, priority, created_at, updated_at)
		VALUES ('TEST-X', 'TEST', 10, 'invalid', 'user:test', 'user:test', 'urgent', ?, ?)`, now, now); err == nil {
		t.Fatal("invalid priority write succeeded")
	} else if err == sql.ErrNoRows {
		t.Fatalf("unexpected invalid priority error: %v", err)
	}
}

func requireTable(t *testing.T, db *sql.DB, table string) {
	t.Helper()
	var name string
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name); err != nil {
		t.Fatalf("table %s is missing: %v", table, err)
	}
}

func requireColumn(t *testing.T, db *sql.DB, table, column string) {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if name == column {
			return
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	t.Fatalf("column %s.%s is missing", table, column)
}

func TestMessageSenderMigrationBackfillsExistingMessages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "message-sender.db")
	db := createDatabaseBeforeMigration(t, path, "0048_message_sender.sql")
	if _, err := db.Exec(`INSERT INTO messages(id, channel, ts, source, data, produced_by_agent) VALUES
		('m1', 'user:customer', '1', 'system:tasks', '{"from":"agent:worker"}', NULL),
		('m2', 'agent:worker:inbox', '2', 'user:customer', NULL, NULL),
		('m3', 'user:customer', '3', 'operator', 'not json', 'worker'),
		('m4', 'user:customer', '4', 'operator', '{"from":""}', NULL)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	want := map[string]string{"m1": "agent:worker", "m2": "user:customer", "m3": "agent:worker", "m4": "system"}
	for id, sender := range want {
		var got string
		if err := s.DB.QueryRow(`SELECT sender FROM messages WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != sender {
			t.Errorf("sender of %s = %q, want %q", id, got, sender)
		}
	}
}

func TestTaskStartedAtMigrationBackfillsTheFirstStartFromEvents(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	s := openBeforeMigration(t, dbPath, "0049_task_started_at.sql")
	if _, err := s.DB.Exec(`
		INSERT INTO task_queues(prefix, name, created_at, updated_at) VALUES ('T', 'T', 'c', 'c');
		INSERT INTO tasks(id, task_key, queue_prefix, title, status, author, customer, created_at, updated_at) VALUES
			(1, 'T-1', 'T', 'reopened', 'done', 'user:u', 'user:u', '2026-01-01T00:00:00Z', 'u'),
			(2, 'T-2', 'T', 'claimed', 'in_progress', 'user:u', 'user:u', '2026-01-01T00:00:00Z', 'u'),
			(3, 'T-3', 'T', 'never started', 'open', 'user:u', 'user:u', '2026-01-01T00:00:00Z', 'u'),
			(4, 'T-4', 'T', 'start purged', 'in_progress', 'user:u', 'user:u', '2026-01-01T00:00:00Z', 'u');
		INSERT INTO task_events(event_id, task_id, queue_prefix, kind, actor, payload, created_at) VALUES
			('e1', 1, 'T', 'task.updated', 'user:u', '{"status":"open"}', '2026-01-02T00:00:00Z'),
			('e2', 1, 'T', 'task.updated', 'user:u', '{"status":"in_progress"}', '2026-01-03T00:00:00Z'),
			('e3', 1, 'T', 'task.updated', 'user:u', '{"status":"in_progress"}', '2026-01-05T00:00:00Z'),
			('e4', 2, 'T', 'task.claimed', 'agent:a', '{"assignee":"agent:a"}', '2026-01-04T00:00:00Z'),
			('e5', 3, 'T', 'task.updated', 'user:u', '{"status":"open"}', '2026-01-04T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	want := map[string]string{"T-1": "2026-01-03T00:00:00Z", "T-2": "2026-01-04T00:00:00Z", "T-3": "", "T-4": ""}
	for key, started := range want {
		var got string
		if err := s.DB.QueryRow(`SELECT started_at FROM tasks WHERE task_key = ?`, key).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != started {
			t.Fatalf("%s started_at = %q; want %q", key, got, started)
		}
	}
	if _, err := s.DB.Exec(`UPDATE tasks SET status = 'in_progress', updated_at = '2026-02-01T00:00:00Z' WHERE task_key IN ('T-1', 'T-3', 'T-4')`); err != nil {
		t.Fatal(err)
	}
	want["T-3"] = "2026-02-01T00:00:00Z"
	for key, started := range want {
		var got string
		if err := s.DB.QueryRow(`SELECT started_at FROM tasks WHERE task_key = ?`, key).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != started {
			t.Fatalf("after update %s started_at = %q; want %q", key, got, started)
		}
	}
}

func TestScriptQuietExitMigrationWrapsLegacyDefinitions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quiet-exit.db")
	s := openBeforeMigration(t, path, "0052_script_quiet_exit_constant.sql")
	if _, err := s.DB.Exec(`INSERT INTO scripts(id, agent, name, description, command, mode, interval_seconds, quiet_exit, state, created_at, next_run_at) VALUES
		('scr-legacy', 'worker', 'legacy', 'd', 'poll --state ''a b''', 'every', 60, 2, 'active', '2026-09-01T00:00:00Z', '2026-09-01T00:01:00Z'),
		('scr-zero', 'worker', 'zero', 'd', 'poll', 'every', 60, 0, 'active', '2026-09-01T00:00:00Z', NULL),
		('scr-current', 'worker', 'current', 'd', 'poll', 'every', 60, 111, 'active', '2026-09-01T00:00:00Z', NULL),
		('scr-plain', 'worker', 'plain', 'd', 'poll', 'every', 60, NULL, 'completed', '2026-09-01T00:00:00Z', NULL),
		('scr-once', 'worker', 'once', 'd', 'make check', 'once', NULL, NULL, 'completed', '2026-09-01T00:00:00Z', NULL)`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	wrapped := func(command string, code string) string {
		return "sh -c '" + command + "'\n" +
			"__tariboy_rc=$?\n" +
			"[ \"$__tariboy_rc\" -eq " + code + " ] && exit 111\n" +
			"exit \"$__tariboy_rc\""
	}
	want := map[string]string{
		"scr-legacy":  wrapped(`poll --state '\''a b'\''`, "2"),
		"scr-zero":    wrapped("poll", "0"),
		"scr-current": "poll",
		"scr-plain":   "poll",
		"scr-once":    "make check",
	}
	for id, command := range want {
		var got string
		var quiet *int
		if err := s.DB.QueryRow(`SELECT command, quiet_exit FROM scripts WHERE id = ?`, id).Scan(&got, &quiet); err != nil {
			t.Fatal(err)
		}
		if got != command {
			t.Errorf("%s command:\n%s\nwant:\n%s", id, got, command)
		}
		if quiet != nil {
			t.Errorf("%s quiet_exit = %d, want NULL", id, *quiet)
		}
	}
	var state, next string
	if err := s.DB.QueryRow(`SELECT state, COALESCE(next_run_at, '') FROM scripts WHERE id = 'scr-legacy'`).Scan(&state, &next); err != nil {
		t.Fatal(err)
	}
	if state != "active" || next != "2026-09-01T00:01:00Z" {
		t.Fatalf("legacy schedule state=%q next_run_at=%q, want it untouched", state, next)
	}
}
