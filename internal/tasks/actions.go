package tasks

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

func (s *Service) AgentAction(ctx context.Context, actor Actor, action string, body map[string]any) (any, error) {
	if actor.IsCustomer || !strings.HasPrefix(actor.Principal, "agent:") {
		return nil, domainError(http.StatusForbidden, "forbidden", "agent identity required")
	}
	switch action {
	case "mine":
		waiting := actionString(body, "waiting_for")
		if waiting == "me" {
			waiting = actor.Principal
		}
		return s.ListTasks(ctx, actor, ListFilter{
			Queue: actionString(body, "queue"), Status: actionString(body, "status"),
			Assignee: actionString(body, "assignee"), WaitingFor: waiting,
			Text: actionString(body, "text"),
		})
	case "ready":
		filter := ReadyFilter{Queue: actionString(body, "queue"), Limit: actionInt(body, "limit")}
		if actionBool(body, "claim") {
			return s.ClaimReady(ctx, actor, filter, actionString(body, "idempotency_key"))
		}
		return s.Ready(ctx, actor, filter)
	case "show":
		return s.GetTask(ctx, actor, actionString(body, "key"))
	case "create":
		return s.CreateTask(ctx, actor, CreateTaskInput{
			Queue: actionString(body, "queue"), ParentKey: actionString(body, "parent_key"),
			Title: actionString(body, "title"), Description: actionString(body, "description"),
			PullRequest: actionString(body, "pull_request"), Assignee: actionString(body, "assignee"), Group: actionString(body, "group"),
			Priority:       Priority(actionString(body, "priority")),
			IdempotencyKey: actionString(body, "idempotency_key"),
		})
	case "update":
		key := actionString(body, "key")
		revision, err := s.actionRevision(ctx, actor, key, body)
		if err != nil {
			return nil, err
		}
		return s.UpdateTask(ctx, actor, key, UpdateTaskInput{
			Title: actionOptionalString(body, "title"), Description: actionOptionalString(body, "description"),
			Status: actionOptionalString(body, "status"), PullRequest: actionOptionalString(body, "pull_request"), Assignee: actionOptionalString(body, "assignee"),
			ManualBlockReason: actionOptionalString(body, "manual_block_reason"), Priority: actionOptionalPriority(body, "priority"), Revision: revision,
		})
	case "assign":
		key := actionString(body, "key")
		revision, err := s.actionRevision(ctx, actor, key, body)
		if err != nil {
			return nil, err
		}
		assignee := actionString(body, "assignee")
		return s.UpdateTask(ctx, actor, key, UpdateTaskInput{Assignee: &assignee, Revision: revision})
	case "comment":
		return s.AddComment(ctx, actor, actionString(body, "key"), AddCommentInput{
			Body: actionString(body, "body"), IdempotencyKey: actionString(body, "idempotency_key"),
		})
	case "ask":
		principal := actionString(body, "principal")
		if principal != "" && !strings.Contains(principal, ":") {
			principal = agentPrincipal(principal)
		}
		if principal == "" {
			return nil, domainError(http.StatusBadRequest, "missing_principal", "question principal is required")
		}
		return s.AddComment(ctx, actor, actionString(body, "key"), AddCommentInput{
			Body:           "@" + principal + "\n\n" + actionString(body, "body"),
			IdempotencyKey: actionString(body, "idempotency_key"),
		})
	case "move":
		key := actionString(body, "key")
		revision, err := s.actionRevision(ctx, actor, key, body)
		if err != nil {
			return nil, err
		}
		return s.MoveTask(ctx, actor, key, MoveInput{
			ParentKey: actionString(body, "parent_key"),
			BeforeKey: actionString(body, "before_key"),
			Revision:  revision,
		})
	case "block":
		blocked := actionString(body, "key")
		blocker := actionString(body, "blocker_key")
		revision, err := s.actionRevision(ctx, actor, blocker, body)
		if err != nil {
			return nil, err
		}
		return s.AddRelation(ctx, actor, blocker, RelationInput{
			TargetKey: blocked, Type: "blocks", Revision: revision,
			IdempotencyKey: actionString(body, "idempotency_key"),
		})
	case "relate":
		key := actionString(body, "key")
		revision, err := s.actionRevision(ctx, actor, key, body)
		if err != nil {
			return nil, err
		}
		return s.AddRelation(ctx, actor, key, RelationInput{
			TargetKey: actionString(body, "target_key"), Type: "related", Revision: revision,
			IdempotencyKey: actionString(body, "idempotency_key"),
		})
	case "done":
		key := actionString(body, "key")
		revision, err := s.actionRevision(ctx, actor, key, body)
		if err != nil {
			return nil, err
		}
		return s.CompleteTask(ctx, actor, key, CompleteInput{
			Revision: revision, CompleteAnyway: actionBool(body, "complete_anyway"),
		})
	case "advance":
		return s.Advance(ctx, actor, actionString(body, "key"), AdvanceInput{
			Outcome: actionString(body, "outcome"), Message: actionRawString(body, "message"),
			From: actionString(body, "from"),
		})
	case "artifact_set":
		return s.SetArtifact(ctx, actor, actionString(body, "key"), actionString(body, "name"), actionRawString(body, "value"))
	case "artifact_ls":
		artifacts, err := s.ListArtifacts(ctx, actor, actionString(body, "key"))
		return map[string]any{"artifacts": artifacts, "count": len(artifacts)}, err
	case "artifact_show":
		artifact, history, err := s.GetArtifact(ctx, actor, actionString(body, "key"), actionString(body, "name"))
		return map[string]any{"artifact": artifact, "history": history}, err
	case "workflow_get":
		return s.GetWorkflow(ctx, actor, actionString(body, "key"))
	case "request_get":
		id, err := actionID(body)
		if err != nil {
			return nil, err
		}
		return s.GetTransitionRequest(ctx, actor, actionString(body, "key"), id)
	case "workflow_runs":
		runs, err := s.ListScriptRuns(ctx, actor, actionString(body, "key"))
		return map[string]any{"runs": runs, "count": len(runs)}, err
	case "workflow_run_log":
		id, err := actionID(body)
		if err != nil {
			return nil, err
		}
		text, truncated, err := s.ScriptRunLog(ctx, actor, actionString(body, "key"), id, actionInt(body, "max_bytes"))
		return map[string]any{"run_id": id, "text": text, "truncated": truncated}, err
	default:
		return nil, domainError(http.StatusBadRequest, "invalid_action",
			fmt.Sprintf("unknown task action %q", action))
	}
}

// actionID is the positive numeric "id" of a request or run.
func actionID(body map[string]any) (int64, error) {
	id := int64(actionInt(body, "id"))
	if id <= 0 {
		return 0, domainError(http.StatusBadRequest, "invalid_id", "a positive numeric id is required")
	}
	return id, nil
}

func (s *Service) actionRevision(ctx context.Context, actor Actor, key string, body map[string]any) (int64, error) {
	if revision := int64(actionInt(body, "revision")); revision > 0 {
		return revision, nil
	}
	detail, err := s.GetTask(ctx, actor, key)
	return detail.Task.Revision, err
}

func actionString(body map[string]any, key string) string {
	value, _ := body[key].(string)
	return strings.TrimSpace(value)
}

// actionRawString is actionString without the trimming, for values stored as
// given such as artifact text.
func actionRawString(body map[string]any, key string) string {
	value, _ := body[key].(string)
	return value
}

func actionOptionalString(body map[string]any, key string) *string {
	value, exists := body[key]
	if !exists {
		return nil
	}
	text, ok := value.(string)
	if !ok {
		return nil
	}
	return &text
}

func actionOptionalPriority(body map[string]any, key string) *Priority {
	value, exists := body[key]
	if !exists {
		return nil
	}
	text, ok := value.(string)
	if !ok {
		return nil
	}
	priority := Priority(strings.TrimSpace(text))
	return &priority
}

func actionBool(body map[string]any, key string) bool {
	value, _ := body[key].(bool)
	return value
}

func actionInt(body map[string]any, key string) int {
	switch value := body[key].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	case string:
		parsed, _ := strconv.Atoi(value)
		return parsed
	default:
		return 0
	}
}
