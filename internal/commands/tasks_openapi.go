package commands

import "github.com/alekzonder/tariboy/internal/tasks"

func schemaRef(name string) map[string]any {
	return map[string]any{"$ref": "#/components/schemas/" + name}
}
func arrayOf(name string) map[string]any {
	return map[string]any{"type": "array", "items": schemaRef(name)}
}
func stringArray() map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
}
func objectSchema(required []string, properties map[string]any) map[string]any {
	s := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}
func listSchema(item string) map[string]any {
	return objectSchema([]string{"items", "count"}, map[string]any{"items": arrayOf(item), "count": map[string]any{"type": "integer"}})
}

func taskOpenAPISchemas() map[string]map[string]any {
	str := map[string]any{"type": "string"}
	integer := map[string]any{"type": "integer"}
	boolean := map[string]any{"type": "boolean"}
	free := map[string]any{"type": "object", "additionalProperties": true}
	status := map[string]any{"type": "string", "enum": []string{tasks.StatusOpen, tasks.StatusInProgress, tasks.StatusWaitCustomer, tasks.StatusDone, tasks.StatusCancelled}}
	return map[string]map[string]any{
		"AgentPool": objectSchema([]string{"id", "queue", "name", "agents", "revision", "created_at", "updated_at"}, map[string]any{"id": integer, "queue": str, "name": str, "agents": stringArray(), "revision": integer, "created_at": str, "updated_at": str}),
		"Task": objectSchema([]string{"key", "queue", "title", "status", "category", "revision"}, map[string]any{
			"key": str, "queue": str, "title": str, "description": str,
			"status":          map[string]any{"type": "string", "description": "The flexible status, or the workflow status for a workflow task"},
			"category":        status,
			"waiting_on":      map[string]any{"type": "string", "enum": []string{tasks.WaitingOnCustomer, tasks.WaitingOnScript, tasks.WaitingOnPause}},
			"workflow_digest": str, "workflow_name": str, "workflow_version": str, "workflow_paused_reason": str,
			"pull_request": str, "revision": integer}),
		"OutcomeView": objectSchema([]string{"on", "to"}, map[string]any{"on": str, "to": str, "requires": stringArray(), "missing": stringArray(), "checks": stringArray()}),
		"StatusVisit": objectSchema([]string{"id", "sequence", "status", "entered_at", "entered_by"}, map[string]any{"id": integer, "sequence": integer, "status": str, "entered_at": str, "entered_by": str, "left_at": str, "outcome": str, "message": str}),
		"TransitionRequest": objectSchema([]string{"id", "task_key", "outcome", "actor", "state", "created_at"}, map[string]any{"id": integer, "task_key": str, "outcome": str, "message": str, "actor": str,
			"state": map[string]any{"type": "string", "enum": []string{"pending", "applied", "rejected", "failed", "cancelled"}, "description": "One of pending, applied, rejected, failed, cancelled"}, "result_message": str, "created_at": str, "finished_at": str,
			"wait_seconds": map[string]any{"type": "integer", "description": "Only while pending: how long to wait for the checks, in seconds"}}),
		"Artifact":      objectSchema([]string{"id", "name", "value", "author", "created_at"}, map[string]any{"id": integer, "name": str, "value": str, "author": str, "created_at": str}),
		"QueueWorkflow": objectSchema([]string{"queue", "name", "version", "digest", "revision", "updated_at"}, map[string]any{"queue": str, "name": str, "version": str, "digest": str, "revision": integer, "updated_at": str}),
		"QueueSecret":   objectSchema([]string{"key", "updated_at"}, map[string]any{"key": str, "updated_at": str}),
		"WorkflowView": objectSchema([]string{"name", "version", "digest", "status", "category", "owner", "outcomes", "artifacts", "visits", "declared_artifacts", "statuses"}, map[string]any{
			"name": str, "version": str, "digest": str, "status": str, "category": status,
			"waiting_on": str, "paused_reason": str, "owner": str, "holder": str, "instructions_path": str,
			"outcomes": arrayOf("OutcomeView"), "artifacts": arrayOf("Artifact"), "visits": arrayOf("StatusVisit"),
			"last_request": schemaRef("TransitionRequest"), "runs": arrayOf("ScriptRun"),
			"declared_artifacts": arrayOf("DeclaredArtifact"), "statuses": arrayOf("StatusView")}),
		"DeclaredArtifact": objectSchema([]string{"name", "description"}, map[string]any{"name": str, "description": str}),
		"StatusView": objectSchema([]string{"id", "owner", "terminal"}, map[string]any{"id": str,
			"owner":    map[string]any{"type": "string", "description": "pool:NAME, customer, script, or empty for a terminal status"},
			"terminal": map[string]any{"type": "boolean"}}),
		"ScriptRun": objectSchema([]string{"id", "task_key", "kind", "script", "run_as", "state", "created_at"}, map[string]any{
			"id": integer, "task_key": str,
			"kind":      map[string]any{"type": "string", "enum": []string{"check", "watch"}},
			"script":    str,
			"run_as":    map[string]any{"type": "string", "enum": []string{"queue", "agent"}},
			"state":     map[string]any{"type": "string", "enum": []string{"pending", "running", "finished", "interrupted", "cancelled"}},
			"verdict":   map[string]any{"type": "string", "enum": []string{"pass", "reject", "outcome", "quiet", "failure"}},
			"exit_code": integer, "holder": str, "message": str, "created_at": str, "started_at": str, "finished_at": str, "log_path": str}),
		"SourceRun": objectSchema([]string{"id", "queue", "source", "script", "state", "tasks_created", "started_at"}, map[string]any{
			"id": integer, "queue": str, "source": str, "script": str,
			"state":     map[string]any{"type": "string", "enum": []string{"running", "finished", "interrupted", "cancelled"}},
			"verdict":   map[string]any{"type": "string", "enum": []string{"items", "quiet", "failure"}},
			"exit_code": integer, "message": str, "tasks_created": integer, "started_at": str, "finished_at": str, "log_path": str}),
		"QueueSource": objectSchema([]string{"queue", "name", "script", "every", "failures"}, map[string]any{
			"queue": str, "name": str, "script": str, "every": str, "timeout": str,
			"next_run_at": map[string]any{"type": "string", "description": "When the source runs next; absent while it runs"},
			"failures":    map[string]any{"type": "integer", "description": "Failed or interrupted runs since the last good one"},
			"last_run":    schemaRef("SourceRun")}),
		"QueueWorkflowTrigger": objectSchema([]string{"id", "queue", "pattern", "action", "enabled", "created_by", "created_at", "updated_at"}, map[string]any{"id": integer, "queue": str, "pattern": str, "correlation_key": str, "action": str, "enabled": boolean, "created_by": str, "created_at": str, "updated_at": str}),
		"TaskEvent":            objectSchema([]string{"sequence", "event_id", "queue", "kind", "actor", "task_revision", "payload", "created_at"}, map[string]any{"sequence": integer, "event_id": str, "task_key": str, "queue": str, "kind": str, "actor": str, "task_revision": integer, "payload": free, "created_at": str}),
	}
}
