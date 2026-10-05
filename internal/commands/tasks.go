package commands

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/alekzonder/tariboy/internal/api"
	"github.com/alekzonder/tariboy/internal/registry"
	"github.com/alekzonder/tariboy/internal/tasks"
)

// TaskOperatorCommands returns a copy of the native Tasks operator commands.
// Other CLIs can reuse their canonical HTTP schemas without maintaining a second
// task command registry.
func TaskOperatorCommands() []registry.Command {
	return []registry.Command{
		taskRoute("tasks.queue.list", "GET", "/api/task-queues", "List task queues",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				queues, err := control.ListQueues(ctx, actor)
				return map[string]any{"queues": queues, "count": len(queues)}, err
			}),
		taskRoute("tasks.queue.create", "POST", "/api/task-queues", "Create a task queue",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				return control.CreateQueue(ctx, actor, tasks.CreateQueueInput{
					Prefix:           stringParam(p, "prefix"),
					Name:             stringParam(p, "name"),
					Description:      stringParam(p, "description"),
					Owners:           stringSliceParam(p, "owners"),
					ResponsibleAgent: stringParam(p, "responsible_agent"),
				})
			}),
		taskRoute("tasks.queue.get", "GET", "/api/task-queues/{prefix}", "Inspect a task queue",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				return control.GetQueue(ctx, actor, stringParam(p, "prefix"))
			}),
		taskRoute("tasks.queue.update", "PATCH", "/api/task-queues/{prefix}", "Update a task queue",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				var owners *[]string
				if _, ok := p["owners"]; ok {
					value := stringSliceParam(p, "owners")
					owners = &value
				}
				return control.UpdateQueue(ctx, actor, stringParam(p, "prefix"), tasks.UpdateQueueInput{
					Name:             optionalStringParam(p, "name"),
					Description:      optionalStringParam(p, "description"),
					Owners:           owners,
					ResponsibleAgent: optionalStringParam(p, "responsible_agent"),
					Revision:         int64Param(p, "revision"),
				})
			}),
		taskRoute("tasks.list", "GET", "/api/tasks", "List native tasks",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				filter := tasks.ListFilter{
					Queue:      stringParam(p, "queue"),
					Status:     stringParam(p, "status"),
					StatusView: stringParam(p, "status_view"),
					Assignee:   stringParam(p, "assignee"),
					Author:     stringParam(p, "author"),
					Group:      stringParam(p, "group"),
					Text:       stringParam(p, "text"),
					WaitingFor: stringParam(p, "waiting_for"),
					ScopeAgent: stringParam(p, "scope_agent"),
					Limit:      int(int64Param(p, "limit")),
					AfterKey:   stringParam(p, "after"),
				}
				if value, ok := boolParam(p, "blocked"); ok {
					filter.Blocked = &value
				}
				return control.ListTasks(ctx, actor, filter)
			}),
		taskRoute("tasks.create", "POST", "/api/tasks", "Create a native task",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				return control.CreateTask(ctx, actor, tasks.CreateTaskInput{
					Queue:          stringParam(p, "queue"),
					ParentKey:      stringParam(p, "parent_key"),
					Title:          stringParam(p, "title"),
					Description:    stringParam(p, "description"),
					PullRequest:    stringParam(p, "pull_request"),
					Assignee:       stringParam(p, "assignee"),
					Group:          stringParam(p, "group"),
					Priority:       tasks.Priority(stringParam(p, "priority")),
					IdempotencyKey: stringParam(p, "idempotency_key"),
				})
			}),
		taskRoute("tasks.get", "GET", "/api/tasks/{key}", "Inspect a native task",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				return control.GetTask(ctx, actor, stringParam(p, "key"))
			}),
		taskRoute("tasks.update", "PATCH", "/api/tasks/{key}", "Update a native task",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				return control.UpdateTask(ctx, actor, stringParam(p, "key"), tasks.UpdateTaskInput{
					Title:             optionalStringParam(p, "title"),
					Description:       optionalStringParam(p, "description"),
					Status:            optionalStringParam(p, "status"),
					PullRequest:       optionalStringParam(p, "pull_request"),
					Assignee:          optionalStringParam(p, "assignee"),
					ManualBlockReason: optionalStringParam(p, "manual_block_reason"),
					Priority:          optionalPriorityParam(p, "priority"),
					Revision:          int64Param(p, "revision"),
				})
			}),
		taskRoute("tasks.export", "GET", "/api/tasks/{key}/export", "Export a task tree for transfer to another daemon",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				return control.ExportTask(ctx, actor, stringParam(p, "key"))
			}),
		taskRoute("tasks.import", "POST", "/api/tasks/import", "Import a task tree exported from another daemon",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				var bundle tasks.TransferBundle
				if err := decodeTaskParam(p, "bundle", &bundle); err != nil {
					return nil, err
				}
				return control.ImportTask(ctx, actor, bundle)
			}),
		taskRoute("tasks.claim", "POST", "/api/tasks/{key}/claim", "Claim a native task",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				return control.ClaimTask(ctx, actor, stringParam(p, "key"), int64Param(p, "revision"))
			}),
		taskRoute("tasks.move", "POST", "/api/tasks/{key}/move", "Move a task in its queue tree",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				return control.MoveTask(ctx, actor, stringParam(p, "key"), tasks.MoveInput{
					ParentKey: stringParam(p, "parent_key"),
					BeforeKey: stringParam(p, "before_key"),
					Revision:  int64Param(p, "revision"),
				})
			}),
		taskRoute("tasks.complete", "POST", "/api/tasks/{key}/complete", "Complete a task",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				completeAnyway, _ := boolParam(p, "complete_anyway")
				return control.CompleteTask(ctx, actor, stringParam(p, "key"), tasks.CompleteInput{
					Revision: int64Param(p, "revision"), CompleteAnyway: completeAnyway,
				})
			}),
		taskRoute("tasks.comments.list", "GET", "/api/tasks/{key}/comments", "List task comments",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				detail, err := control.GetTask(ctx, actor, stringParam(p, "key"))
				return map[string]any{"comments": detail.Comments, "count": len(detail.Comments)}, err
			}),
		taskRoute("tasks.comments.add", "POST", "/api/tasks/{key}/comments", "Add a task comment",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				return control.AddComment(ctx, actor, stringParam(p, "key"), tasks.AddCommentInput{
					Body: stringParam(p, "body"), IdempotencyKey: stringParam(p, "idempotency_key"),
				})
			}),
		taskRoute("tasks.relations.list", "GET", "/api/tasks/{key}/relations", "List task relations",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				detail, err := control.GetTask(ctx, actor, stringParam(p, "key"))
				return map[string]any{"relations": detail.Relations, "count": len(detail.Relations)}, err
			}),
		taskRoute("tasks.relations.add", "POST", "/api/tasks/{key}/relations", "Add a task relation",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				return control.AddRelation(ctx, actor, stringParam(p, "key"), tasks.RelationInput{
					TargetKey: stringParam(p, "target_key"), Type: stringParam(p, "type"),
					Revision: int64Param(p, "revision"), IdempotencyKey: stringParam(p, "idempotency_key"),
				})
			}),
		taskRoute("tasks.relations.delete", "DELETE", "/api/tasks/{key}/relations", "Delete a task relation",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				in := tasks.DeleteRelationInput{
					RelationID: int64Param(p, "relation_id"), Revision: int64Param(p, "revision"),
					IdempotencyKey: stringParam(p, "idempotency_key"),
				}
				if err := control.DeleteRelation(ctx, actor, stringParam(p, "key"), in); err != nil {
					return nil, err
				}
				return map[string]any{"deleted": true, "relation_id": in.RelationID}, nil
			}),
		taskRoute("tasks.events", "GET", "/api/tasks/{key}/events", "List task events",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				events, err := control.ListEvents(ctx, actor, stringParam(p, "key"),
					int64Param(p, "after"), int(int64Param(p, "limit")))
				return map[string]any{"events": events, "count": len(events)}, err
			}),
		taskRoute("tasks.principals", "GET", "/api/task-principals", "List task principals",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				return control.Principals(ctx, actor)
			}),
		taskRoute("tasks.notifications.list", "GET", "/api/task-notifications", "List customer task notifications",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				include, _ := boolParam(p, "include_dismissed")
				list, err := control.ListNotifications(ctx, actor, include)
				return map[string]any{"notifications": list, "count": len(list)}, err
			}),
		taskRoute("tasks.notifications.read", "POST", "/api/task-notifications/{id}/read", "Mark a task notification read",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				return control.MarkNotification(ctx, actor, stringParam(p, "id"), "read")
			}),
		taskRoute("tasks.notifications.dismiss", "POST", "/api/task-notifications/{id}/dismiss", "Dismiss a task notification",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				return control.MarkNotification(ctx, actor, stringParam(p, "id"), "dismiss")
			}),
		taskRoute("tasks.queue.pool.set", "PATCH", "/api/task-queues/{queue}/pools/{pool}", "Bind agents to a logical workflow pool",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				return control.RebindAgentPool(ctx, actor, stringParam(p, "queue"), stringParam(p, "pool"), stringSliceParam(p, "agents"), int64Param(p, "revision"), stringParam(p, "idempotency_key"))
			}),
		taskRoute("tasks.queue.pool.list", "GET", "/api/task-queues/{queue}/pools", "List logical workflow pools",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				items, err := control.ListAgentPools(ctx, actor, stringParam(p, "queue"))
				return map[string]any{"items": items, "count": len(items)}, err
			}),
		taskRoute("tasks.queue.pool.get", "GET", "/api/task-queues/{queue}/pools/{pool}", "Inspect a logical workflow pool",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				return control.GetAgentPool(ctx, actor, stringParam(p, "queue"), stringParam(p, "pool"))
			}),
		taskRoute("tasks.queue.trigger.list", "GET", "/api/task-queues/{queue}/workflow-triggers", "List queue workflow triggers",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				items, err := control.ListQueueWorkflowTriggers(ctx, actor, stringParam(p, "queue"))
				return map[string]any{"items": items, "count": len(items)}, err
			}),
		taskRoute("tasks.queue.trigger.create", "POST", "/api/task-queues/{queue}/workflow-triggers", "Create a queue workflow trigger",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				return control.CreateQueueWorkflowTrigger(ctx, actor, stringParam(p, "queue"), tasks.CreateQueueWorkflowTriggerInput{Pattern: stringParam(p, "pattern"), CorrelationKey: stringParam(p, "correlation_key"), Action: stringParam(p, "action")})
			}),
		taskRoute("tasks.queue.trigger.delete", "DELETE", "/api/task-queues/{queue}/workflow-triggers/{id}", "Delete a queue workflow trigger",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				id := int64Param(p, "id")
				if err := control.DeleteQueueWorkflowTrigger(ctx, actor, stringParam(p, "queue"), id); err != nil {
					return nil, err
				}
				return map[string]any{"deleted": true, "id": id}, nil
			}),
		taskRoute("tasks.advance", "POST", "/api/tasks/{key}/advance", "Declare an outcome for a workflow task's current status",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				return control.Advance(ctx, actor, stringParam(p, "key"), tasks.AdvanceInput{
					Outcome: stringParam(p, "outcome"), Message: rawStringParam(p, "message"), From: stringParam(p, "from"),
				})
			}),
		taskRoute("tasks.artifacts.set", "PUT", "/api/tasks/{key}/artifacts/{name}", "Set a workflow task artifact",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				return control.SetArtifact(ctx, actor, stringParam(p, "key"), stringParam(p, "name"), rawStringParam(p, "value"))
			}),
		taskRoute("tasks.artifacts.list", "GET", "/api/tasks/{key}/artifacts", "List the current artifacts of a workflow task",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				artifacts, err := control.ListArtifacts(ctx, actor, stringParam(p, "key"))
				return map[string]any{"artifacts": artifacts, "count": len(artifacts)}, err
			}),
		taskRoute("tasks.artifacts.get", "GET", "/api/tasks/{key}/artifacts/{name}", "Show a workflow task artifact and its history",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				artifact, history, err := control.GetArtifact(ctx, actor, stringParam(p, "key"), stringParam(p, "name"))
				return map[string]any{"artifact": artifact, "history": history}, err
			}),
		taskRoute("tasks.workflow.get", "GET", "/api/tasks/{key}/workflow", "Show where a workflow task is and what it may do next",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				return control.GetWorkflow(ctx, actor, stringParam(p, "key"))
			}),
		taskRoute("tasks.workflow.requests.get", "GET", "/api/tasks/{key}/workflow/requests/{id}", "Show one transition request of a workflow task",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				return control.GetTransitionRequest(ctx, actor, stringParam(p, "key"), int64Param(p, "id"))
			}),
		taskRoute("tasks.workflow.runs.list", "GET", "/api/tasks/{key}/workflow/runs", "List the script runs of a workflow task",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				runs, err := control.ListScriptRuns(ctx, actor, stringParam(p, "key"))
				return map[string]any{"runs": runs, "count": len(runs)}, err
			}),
		taskRoute("tasks.workflow.runs.get", "GET", "/api/tasks/{key}/workflow/runs/{id}", "Show one script run of a workflow task",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				return control.GetScriptRun(ctx, actor, stringParam(p, "key"), int64Param(p, "id"))
			}),
		taskRoute("tasks.workflow.runs.log", "GET", "/api/tasks/{key}/workflow/runs/{id}/log", "Read the tail of a script run's log",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				id := int64Param(p, "id")
				maxBytes, err := tasks.ParseMaxBytes(p["max_bytes"])
				if err != nil {
					return nil, err
				}
				text, truncated, err := control.ScriptRunLog(ctx, actor, stringParam(p, "key"), id, maxBytes)
				return map[string]any{"run_id": id, "text": text, "truncated": truncated}, err
			}),
		taskRoute("tasks.workflow.move", "POST", "/api/tasks/{key}/workflow/move", "Move a workflow task to another status",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				return control.MoveWorkflow(ctx, actor, stringParam(p, "key"), stringParam(p, "to"), stringParam(p, "reason"))
			}),
		taskRoute("tasks.workflow.resume", "POST", "/api/tasks/{key}/workflow/resume", "Resolve the pause of a workflow task",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				return control.ResumeWorkflow(ctx, actor, stringParam(p, "key"), stringParam(p, "decision"))
			}),
		taskRoute("tasks.cancel", "POST", "/api/tasks/{key}/cancel", "Cancel a workflow task",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				return control.CancelWorkflowTask(ctx, actor, stringParam(p, "key"))
			}),
		taskRoute("tasks.queue.workflow.set", "PUT", "/api/task-queues/{queue}/workflow", "Bind a workflow image to a queue",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				return control.SetQueueWorkflow(ctx, actor, stringParam(p, "queue"), stringParam(p, "ref"), int64Param(p, "revision"))
			}),
		taskRoute("tasks.queue.workflow.get", "GET", "/api/task-queues/{queue}/workflow", "Show the workflow image a queue is bound to",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				return control.GetQueueWorkflow(ctx, actor, stringParam(p, "queue"))
			}),
		taskRoute("tasks.queue.workflow.clear", "DELETE", "/api/task-queues/{queue}/workflow", "Unbind the workflow image from a queue",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				queue := stringParam(p, "queue")
				if err := control.ClearQueueWorkflow(ctx, actor, queue, int64Param(p, "revision")); err != nil {
					return nil, err
				}
				return map[string]any{"queue": queue, "cleared": true}, nil
			}),
		withMaxBody(taskRoute("tasks.queue.secret.set", "PUT", "/api/task-queues/{queue}/secrets/{key}", "Set a queue secret for workflow scripts",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				queue, key := stringParam(p, "queue"), stringParam(p, "key")
				info, err := control.SetQueueSecretInfo(ctx, actor, queue, key, rawStringParam(p, "value"))
				if err != nil {
					return nil, err
				}
				return map[string]any{"queue": strings.ToUpper(queue), "key": key, "updated_at": info.UpdatedAt}, nil
			}), maxQueueSecretBody),
		taskRoute("tasks.queue.secret.ls", "GET", "/api/task-queues/{queue}/secrets", "List the keys of a queue's secrets",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				items, err := control.ListQueueSecrets(ctx, actor, stringParam(p, "queue"))
				return map[string]any{"secrets": items, "count": len(items)}, err
			}),
		taskRoute("tasks.queue.source.ls", "GET", "/api/task-queues/{queue}/sources", "List the workflow sources of a queue and their last runs",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				items, err := control.ListQueueSources(ctx, actor, stringParam(p, "queue"))
				return map[string]any{"sources": items, "count": len(items)}, err
			}),
		taskRoute("tasks.queue.source.log", "GET", "/api/task-queues/{queue}/source-runs/{id}/log", "Read the tail of a workflow source run's log",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				id := int64Param(p, "id")
				maxBytes, err := tasks.ParseMaxBytes(p["max_bytes"])
				if err != nil {
					return nil, err
				}
				text, truncated, err := control.QueueSourceRunLog(ctx, actor, stringParam(p, "queue"), id, maxBytes)
				return map[string]any{"run_id": id, "text": text, "truncated": truncated}, err
			}),
		taskRoute("tasks.queue.secret.rm", "DELETE", "/api/task-queues/{queue}/secrets/{key}", "Remove a queue secret",
			func(ctx context.Context, control registry.TaskControl, actor tasks.Actor, p registry.Params) (any, error) {
				queue, key := stringParam(p, "queue"), stringParam(p, "key")
				if err := control.RemoveQueueSecret(ctx, actor, queue, key); err != nil {
					return nil, err
				}
				return map[string]any{"queue": strings.ToUpper(queue), "key": key, "removed": true}, nil
			}),
	}
}

func taskCommands() []registry.Command { return TaskOperatorCommands() }

// maxQueueSecretBody bounds the body of a queue secret PUT: a 64 KiB value
// with JSON escaping fits well within it.
const maxQueueSecretBody = 512 << 10

// withMaxBody sets the request body bound of an HTTP command.
func withMaxBody(cmd registry.Command, limit int64) registry.Command {
	route := *cmd.HTTP
	route.MaxBodyBytes = limit
	cmd.HTTP = &route
	return cmd
}

type taskHandler func(context.Context, registry.TaskControl, tasks.Actor, registry.Params) (any, error)

func taskRoute(path, method, route, summary string, handler taskHandler) registry.Command {
	bodyArgs := taskHTTPArgs(path)
	var args []registry.Arg
	for _, segment := range strings.Split(route, "/") {
		if !strings.HasPrefix(segment, "{") || !strings.HasSuffix(segment, "}") {
			continue
		}
		name := segment[1 : len(segment)-1]
		arg := registry.Arg{Name: name, Required: true, Help: "Resource " + name}
		for i, candidate := range bodyArgs {
			if candidate.Name == name {
				arg = candidate
				bodyArgs = append(bodyArgs[:i], bodyArgs[i+1:]...)
				break
			}
		}
		args = append(args, arg)
	}
	args = append(args, bodyArgs...)
	for i := range args {
		if args[i].Flag == "" {
			args[i].Flag = strings.ReplaceAll(args[i].Name, "_", "-")
		}
	}
	return registry.Command{
		Path: path, Summary: summary, CLIHidden: path != "tasks.queue.create",
		Args: args, ResultSchema: taskHTTPResultSchema(path),
		Schemas: taskOpenAPISchemas(),
		HTTP:    &registry.HTTPRoute{Method: method, Path: route},
		Handler: func(c *registry.Ctx, p registry.Params) (any, error) {
			if c.Tasks == nil {
				return nil, api.UserError{Status: http.StatusServiceUnavailable,
					Code: "tasks_unavailable", Msg: "native Tasks service is unavailable"}
			}
			actor := tasks.CustomerActor(c.Tasks.CustomerLogin())
			result, err := handler(registry.RequestContext(p), c.Tasks, actor, p)
			if err == nil {
				return result, nil
			}
			var domain *tasks.Error
			if errors.As(err, &domain) {
				return nil, api.UserError{
					Status: domain.Status, Code: domain.Code, Msg: domain.Msg, Data: domain.Data,
				}
			}
			return nil, err
		},
	}
}

func taskHTTPArgs(path string) []registry.Arg {
	switch path {
	case "tasks.queue.update":
		return []registry.Arg{
			{Name: "name", Help: "Queue name"},
			{Name: "description", Help: "Queue description"},
			{Name: "owners", Help: "Comma-separated owner agents"},
			{Name: "responsible_agent", Help: "Responsible agent"},
			{Name: "revision", Type: registry.Int, Required: true, Help: "Expected current revision"},
		}
	case "tasks.notifications.list":
		return []registry.Arg{{Name: "include_dismissed", Type: registry.Bool, Help: "Include dismissed notifications"}}
	case "tasks.queue.create":
		return []registry.Arg{
			{Name: "prefix", Required: true, Help: "Queue key prefix"},
			{Name: "name", Required: true, Help: "Queue name"},
			{Name: "description", Help: "Queue description"},
			{Name: "owners", Help: "Comma-separated owner agents"},
			{Name: "responsible_agent", Flag: "responsible-agent", Help: "Responsible agent"},
		}
	case "tasks.create":
		return []registry.Arg{
			{Name: "queue", Help: "Queue prefix for a root task"},
			{Name: "parent_key", Flag: "parent-key", Help: "Parent task key"},
			{Name: "title", Required: true, Help: "Task title"},
			{Name: "description", Help: "Task description"},
			{Name: "pull_request", Flag: "pull-request", Help: "Canonical pull request URL"},
			{Name: "assignee", Help: "Task assignee"},
			{Name: "group", Help: "Task group"},
			{Name: "priority", Help: "Task priority", Schema: map[string]any{
				"type": "string", "enum": []string{string(tasks.PriorityP0), string(tasks.PriorityP1), string(tasks.PriorityP2), string(tasks.PriorityP3)},
			}},
			{Name: "idempotency_key", Flag: "idempotency-key", Help: "Stable retry key"},
		}
	case "tasks.update":
		return []registry.Arg{
			{Name: "title", Help: "Task title"},
			{Name: "description", Help: "Task description"},
			{Name: "status", Help: "Task status", Schema: map[string]any{
				"type": "string", "enum": []string{tasks.StatusOpen, tasks.StatusInProgress, tasks.StatusWaitCustomer, tasks.StatusDone, tasks.StatusCancelled},
			}},
			{Name: "pull_request", Flag: "pull-request", Help: "Canonical pull request URL (empty clears it)"},
			{Name: "assignee", Help: "Task assignee"},
			{Name: "manual_block_reason", Flag: "manual-block-reason", Help: "Manual block reason"},
			{Name: "priority", Help: "Task priority"},
			{Name: "revision", Type: registry.Int, Required: true, Help: "Expected current revision"},
		}
	case "tasks.import":
		return []registry.Arg{{Name: "bundle", Type: registry.JSONObject, Required: true,
			Help:   "Bundle returned by tasks.export on the source daemon",
			Schema: map[string]any{"type": "object"}}}
	case "tasks.queue.pool.set":
		return poolMutationArgs(registry.Arg{Name: "agents", Required: true, Help: "Explicit agent names", Schema: map[string]any{"type": "array", "items": map[string]any{"type": "string"}}})
	case "tasks.queue.trigger.create":
		return []registry.Arg{{Name: "pattern", Required: true, Help: "Allowed channel pattern"}, {Name: "correlation_key", Help: "Correlation selector"}, {Name: "action", Required: true, Help: "Declared workflow trigger action"}}
	case "tasks.queue.trigger.delete":
		return []registry.Arg{{Name: "id", Type: registry.Int, Required: true, Help: "Resource id"}}
	case "tasks.advance":
		return []registry.Arg{{Name: "outcome", Required: true, Help: "Outcome to declare"}, {Name: "message", Help: "Message recorded with the transition"}, {Name: "from", Help: "Status the caller believes the task is in; a different current status is refused with status_changed"}}
	case "tasks.artifacts.set":
		return []registry.Arg{{Name: "value", Help: "Artifact value, stored as given"}}
	case "tasks.workflow.runs.log", "tasks.queue.source.log":
		return []registry.Arg{{Name: "max_bytes", Type: registry.Int, Help: "Largest log tail to return in bytes (default 65536, maximum 1048576)"}}
	case "tasks.workflow.move":
		return []registry.Arg{{Name: "to", Required: true, Help: "Target status id"}, {Name: "reason", Required: true, Help: "Why the task is moved"}}
	case "tasks.workflow.resume":
		return []registry.Arg{{Name: "decision", Required: true, Help: "continue resumes the task in its current status with counters reset; release dispatches the task to another pool member", Schema: map[string]any{
			"type": "string", "enum": []string{tasks.ResumeContinue, tasks.ResumeRelease},
		}}}
	case "tasks.queue.workflow.set":
		return []registry.Arg{{Name: "ref", Required: true, Help: "Workflow image reference, name:tag"}, {Name: "revision", Type: registry.Int, Help: "Expected current binding revision (zero for a new binding)"}}
	case "tasks.queue.workflow.clear":
		return []registry.Arg{{Name: "revision", Type: registry.Int, Required: true, Help: "Expected current binding revision"}}
	case "tasks.queue.secret.set":
		return []registry.Arg{{Name: "value", Help: "Secret value, stored as given; the CLI reads stdin when --value is absent"}}
	case "tasks.events":
		return []registry.Arg{{Name: "after", Type: registry.Int, Help: "Resume after sequence"}, {Name: "limit", Type: registry.Int, Help: "Maximum events"}}
	default:
		return nil
	}
}

func taskHTTPResultSchema(path string) map[string]any {
	switch path {
	case "tasks.queue.pool.set", "tasks.queue.pool.get":
		return schemaRef("AgentPool")
	case "tasks.queue.pool.list":
		return listSchema("AgentPool")
	case "tasks.queue.trigger.create":
		return schemaRef("QueueWorkflowTrigger")
	case "tasks.queue.trigger.list":
		return listSchema("QueueWorkflowTrigger")
	case "tasks.advance", "tasks.workflow.requests.get":
		return schemaRef("TransitionRequest")
	case "tasks.workflow.runs.list":
		return objectSchema([]string{"runs", "count"}, map[string]any{"runs": arrayOf("ScriptRun"), "count": map[string]any{"type": "integer"}})
	case "tasks.workflow.runs.get":
		return schemaRef("ScriptRun")
	case "tasks.queue.source.ls":
		return objectSchema([]string{"sources", "count"}, map[string]any{"sources": arrayOf("QueueSource"), "count": map[string]any{"type": "integer"}})
	case "tasks.workflow.runs.log", "tasks.queue.source.log":
		return objectSchema([]string{"run_id", "text", "truncated"}, map[string]any{"run_id": map[string]any{"type": "integer"}, "text": map[string]any{"type": "string"}, "truncated": map[string]any{"type": "boolean"}})
	case "tasks.artifacts.set":
		return schemaRef("Artifact")
	case "tasks.artifacts.list":
		return objectSchema([]string{"artifacts", "count"}, map[string]any{"artifacts": arrayOf("Artifact"), "count": map[string]any{"type": "integer"}})
	case "tasks.artifacts.get":
		return objectSchema([]string{"artifact", "history"}, map[string]any{"artifact": schemaRef("Artifact"), "history": arrayOf("Artifact")})
	case "tasks.workflow.get":
		return schemaRef("WorkflowView")
	case "tasks.workflow.move", "tasks.workflow.resume", "tasks.cancel":
		return schemaRef("Task")
	case "tasks.queue.workflow.set", "tasks.queue.workflow.get":
		return schemaRef("QueueWorkflow")
	case "tasks.queue.secret.set":
		return objectSchema([]string{"queue", "key", "updated_at"}, map[string]any{"queue": map[string]any{"type": "string"}, "key": map[string]any{"type": "string"}, "updated_at": map[string]any{"type": "string"}})
	case "tasks.queue.secret.ls":
		return objectSchema([]string{"secrets", "count"}, map[string]any{"secrets": arrayOf("QueueSecret"), "count": map[string]any{"type": "integer"}})
	case "tasks.queue.secret.rm":
		return objectSchema([]string{"queue", "key", "removed"}, map[string]any{"queue": map[string]any{"type": "string"}, "key": map[string]any{"type": "string"}, "removed": map[string]any{"type": "boolean"}})
	case "tasks.queue.workflow.clear":
		return objectSchema([]string{"queue", "cleared"}, map[string]any{"queue": map[string]any{"type": "string"}, "cleared": map[string]any{"type": "boolean"}})
	default:
		return map[string]any{"type": "object"}
	}
}

func poolMutationArgs(primary registry.Arg) []registry.Arg {
	return []registry.Arg{primary, {Name: "revision", Type: registry.Int, Required: true, Help: "Expected current revision (zero for a new pool)"}, {Name: "idempotency_key", Required: true, Help: "Stable retry key"}}
}

func stringParam(p registry.Params, key string) string {
	value, _ := p[key].(string)
	return strings.TrimSpace(value)
}

// rawStringParam is stringParam without the trimming, for values stored as
// given such as artifact text.
func rawStringParam(p registry.Params, key string) string {
	value, _ := p[key].(string)
	return value
}

func optionalStringParam(p registry.Params, key string) *string {
	value, ok := p[key]
	if !ok || value == nil {
		return nil
	}
	text, ok := value.(string)
	if !ok {
		return nil
	}
	return &text
}

func optionalPriorityParam(p registry.Params, key string) *tasks.Priority {
	value := optionalStringParam(p, key)
	if value == nil {
		return nil
	}
	priority := tasks.Priority(*value)
	return &priority
}

func stringSliceParam(p registry.Params, key string) []string {
	switch values := p[key].(type) {
	case []string:
		return values
	case []any:
		out := make([]string, 0, len(values))
		for _, value := range values {
			if text, ok := value.(string); ok {
				out = append(out, text)
			}
		}
		return out
	case string:
		if strings.TrimSpace(values) == "" {
			return nil
		}
		return strings.Split(values, ",")
	default:
		return nil
	}
}

func int64Param(p registry.Params, key string) int64 {
	switch value := p[key].(type) {
	case int:
		return int64(value)
	case int64:
		return value
	case float64:
		return int64(value)
	case string:
		n, _ := strconv.ParseInt(value, 10, 64)
		return n
	default:
		return 0
	}
}

func boolParam(p registry.Params, key string) (bool, bool) {
	value, exists := p[key]
	if !exists {
		return false, false
	}
	switch typed := value.(type) {
	case bool:
		return typed, true
	case string:
		parsed, err := strconv.ParseBool(typed)
		return parsed, err == nil
	default:
		return false, false
	}
}

func decodeTaskParam(p registry.Params, key string, out any) error {
	value, ok := p[key]
	if !ok {
		value = p
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return api.UserError{Status: http.StatusBadRequest, Code: "invalid_request", Msg: "request body must be valid JSON"}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return api.UserError{Status: http.StatusBadRequest, Code: "invalid_request", Msg: "request body has an invalid schema"}
	}
	return nil
}
