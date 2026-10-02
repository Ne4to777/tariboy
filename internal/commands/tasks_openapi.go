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
		"AgentPool":            objectSchema([]string{"id", "queue", "name", "agents", "revision", "created_at", "updated_at"}, map[string]any{"id": integer, "queue": str, "name": str, "agents": stringArray(), "revision": integer, "created_at": str, "updated_at": str}),
		"Task":                 objectSchema([]string{"key", "queue", "title", "status", "revision"}, map[string]any{"key": str, "queue": str, "title": str, "description": str, "status": status, "pull_request": str, "revision": integer}),
		"QueueWorkflowTrigger": objectSchema([]string{"id", "queue", "pattern", "action", "enabled", "created_by", "created_at", "updated_at"}, map[string]any{"id": integer, "queue": str, "pattern": str, "correlation_key": str, "action": str, "enabled": boolean, "created_by": str, "created_at": str, "updated_at": str}),
		"TaskEvent":            objectSchema([]string{"sequence", "event_id", "queue", "kind", "actor", "task_revision", "payload", "created_at"}, map[string]any{"sequence": integer, "event_id": str, "task_key": str, "queue": str, "kind": str, "actor": str, "task_revision": integer, "payload": free, "created_at": str}),
	}
}
