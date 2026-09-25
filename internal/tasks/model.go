// Package tasks owns the native Tariboy task domain and is shared by customer
// HTTP routes and identity-bound agent tools.
package tasks

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const (
	StatusOpen         = "open"
	StatusInProgress   = "in_progress"
	StatusWaitCustomer = "wait_customer"
	StatusDone         = "done"
	StatusCancelled    = "cancelled"
)

type Priority string

const (
	PriorityP0 Priority = "P0"
	PriorityP1 Priority = "P1"
	PriorityP2 Priority = "P2"
	PriorityP3 Priority = "P3"
)

func NormalizePriority(priority Priority) (Priority, error) {
	if priority == "" {
		return PriorityP2, nil
	}
	switch priority {
	case PriorityP0, PriorityP1, PriorityP2, PriorityP3:
		return priority, nil
	default:
		return "", domainError(http.StatusBadRequest, "invalid_priority", "priority must be P0, P1, P2, or P3")
	}
}

func NormalizePullRequest(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || (!strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https")) || u.Host == "" || u.User != nil {
		return "", domainError(http.StatusBadRequest, "invalid_pull_request", "pull request must be an absolute http or https URL without credentials")
	}
	u.Scheme, u.Host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	return u.String(), nil
}

type Actor struct {
	Principal  string
	IsCustomer bool
}

// DefaultCustomerLogin is the customer principal every daemon uses unless an
// operator overrides it. It is deliberately not derived from $USER: the same
// customer name on every server keeps tasks, mentions and the customer channel
// interchangeable across hosts.
const DefaultCustomerLogin = "customer"

func CustomerActor(login string) Actor {
	return Actor{Principal: userPrincipal(login), IsCustomer: true}
}

func AgentActor(name string) Actor {
	return Actor{Principal: agentPrincipal(name)}
}

func userPrincipal(login string) string {
	login = strings.TrimSpace(login)
	if strings.HasPrefix(login, "user:") {
		return login
	}
	return "user:" + login
}

func agentPrincipal(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || strings.HasPrefix(name, "agent:") {
		return name
	}
	return "agent:" + name
}

type Error struct {
	Status int
	Code   string
	Msg    string
	Data   map[string]any
}

func (e *Error) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	return e.Code
}

func domainError(status int, code, msg string) error {
	return &Error{Status: status, Code: code, Msg: msg}
}

func ErrorCode(err error) string {
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	return ""
}

func ErrorStatus(err error) int {
	if e, ok := err.(*Error); ok && e.Status != 0 {
		return e.Status
	}
	return http.StatusInternalServerError
}

type Queue struct {
	Prefix           string   `json:"prefix"`
	Name             string   `json:"name"`
	Description      string   `json:"description"`
	Owners           []string `json:"owners"`
	ResponsibleAgent string   `json:"responsible_agent"`
	Revision         int64    `json:"revision"`
	CreatedAt        string   `json:"created_at"`
	UpdatedAt        string   `json:"updated_at"`
}

type Task struct {
	ID                int64    `json:"-"`
	Key               string   `json:"key"`
	Queue             string   `json:"queue"`
	ParentKey         string   `json:"parent_key"`
	Position          int64    `json:"position"`
	Priority          Priority `json:"priority"`
	Title             string   `json:"title"`
	Description       string   `json:"description"`
	Status            string   `json:"status"`
	PullRequest       string   `json:"pull_request"`
	Author            string   `json:"author"`
	Customer          string   `json:"customer"`
	Group             string   `json:"group"`
	Assignee          string   `json:"assignee"`
	ManualBlockReason string   `json:"manual_block_reason"`
	Blocked           bool     `json:"blocked"`
	WorkflowVersionID int64    `json:"workflow_version_id,omitempty"`
	WorkflowVersion   string   `json:"workflow_version,omitempty"`
	WorkflowStatus    string   `json:"workflow_status,omitempty"`
	WorkflowRevision  int64    `json:"workflow_revision,omitempty"`
	Revision          int64    `json:"revision"`
	CreatedAt         string   `json:"created_at"`
	StartedAt         string   `json:"started_at"`
	UpdatedAt         string   `json:"updated_at"`
	CompletedAt       string   `json:"completed_at"`
	Access            string   `json:"access,omitempty"`
	// Filed marks a task that was just created as a report into a queue its author does not
	// own: unassigned, ungrouped, and no longer visible to that author. Set only on the
	// create response, so the caller can say so instead of leaving the agent to discover it
	// as a not_found on its own key.
	Filed bool `json:"filed,omitempty"`
}

type Comment struct {
	ID        int64  `json:"id"`
	TaskKey   string `json:"task_key"`
	Author    string `json:"author"`
	Body      string `json:"body"`
	Revision  int64  `json:"revision"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type WaitingFor struct {
	ID                  int64  `json:"id"`
	TaskKey             string `json:"task_key"`
	ExpectedPrincipal   string `json:"expected_principal"`
	RequestingPrincipal string `json:"requesting_principal"`
	RequestingCommentID int64  `json:"requesting_comment_id"`
	RequestedAt         string `json:"requested_at"`
	ResolvingCommentID  int64  `json:"resolving_comment_id,omitempty"`
	ResolvedAt          string `json:"resolved_at,omitempty"`
}

type Relation struct {
	ID           int64  `json:"id"`
	SourceKey    string `json:"source_key"`
	SourceTitle  string `json:"source_title"`
	SourceStatus string `json:"source_status"`
	TargetKey    string `json:"target_key"`
	TargetTitle  string `json:"target_title"`
	TargetStatus string `json:"target_status"`
	Type         string `json:"type"`
	CreatedBy    string `json:"created_by"`
	CreatedAt    string `json:"created_at"`
}

type Event struct {
	Sequence     int64          `json:"sequence"`
	EventID      string         `json:"event_id"`
	TaskKey      string         `json:"task_key,omitempty"`
	Queue        string         `json:"queue"`
	Kind         string         `json:"kind"`
	Actor        string         `json:"actor"`
	TaskRevision int64          `json:"task_revision"`
	Payload      map[string]any `json:"payload"`
	CreatedAt    string         `json:"created_at"`
}

type TaskDetail struct {
	Task     Task      `json:"task"`
	Comments []Comment `json:"comments"`
	// Descendants and ActiveDescendants count the whole subtree, not just direct children.
	// A root cannot be completed while ActiveDescendants > 0, so showing both is what lets
	// the reader notice a tree that keeps growing instead of closing.
	Descendants       int          `json:"descendants"`
	ActiveDescendants int          `json:"active_descendants"`
	WaitingFor        []WaitingFor `json:"waiting_for"`
	Relations         []Relation   `json:"relations"`
}

type CreateQueueInput struct {
	Prefix           string   `json:"prefix"`
	Name             string   `json:"name"`
	Description      string   `json:"description"`
	Owners           []string `json:"owners"`
	ResponsibleAgent string   `json:"responsible_agent"`
}

type UpdateQueueInput struct {
	Name             *string   `json:"name"`
	Description      *string   `json:"description"`
	Owners           *[]string `json:"owners"`
	ResponsibleAgent *string   `json:"responsible_agent"`
	Revision         int64     `json:"revision"`
}

type CreateTaskInput struct {
	Queue          string   `json:"queue"`
	ParentKey      string   `json:"parent_key"`
	Title          string   `json:"title"`
	Description    string   `json:"description"`
	PullRequest    string   `json:"pull_request"`
	Assignee       string   `json:"assignee"`
	Group          string   `json:"group"`
	Priority       Priority `json:"priority"`
	IdempotencyKey string   `json:"idempotency_key"`
}

type ListFilter struct {
	Queue      string
	Status     string
	StatusView string
	Assignee   string
	Author     string
	Group      string
	Text       string
	WaitingFor string
	ScopeAgent string
	Blocked    *bool
	Limit      int
	AfterKey   string
}

type TaskPage struct {
	Tasks      []Task `json:"tasks"`
	NextCursor string `json:"next_cursor,omitempty"`
	Sequence   int64  `json:"sequence"`
}

type MoveInput struct {
	ParentKey string `json:"parent_key"`
	BeforeKey string `json:"before_key"`
	Revision  int64  `json:"revision"`
}

type ReadyFilter struct {
	Queue string `json:"queue"`
	Limit int    `json:"limit"`
}

type UpdateTaskInput struct {
	Title             *string   `json:"title"`
	Description       *string   `json:"description"`
	Status            *string   `json:"status"`
	PullRequest       *string   `json:"pull_request"`
	Assignee          *string   `json:"assignee"`
	ManualBlockReason *string   `json:"manual_block_reason"`
	Priority          *Priority `json:"priority"`
	Revision          int64     `json:"revision"`
}

type CompleteInput struct {
	Revision       int64 `json:"revision"`
	CompleteAnyway bool  `json:"complete_anyway"`
}

type RelationInput struct {
	TargetKey      string `json:"target_key"`
	Type           string `json:"type"`
	Revision       int64  `json:"revision"`
	IdempotencyKey string `json:"idempotency_key"`
}

type DeleteRelationInput struct {
	RelationID     int64  `json:"relation_id"`
	Revision       int64  `json:"revision"`
	IdempotencyKey string `json:"idempotency_key"`
}

type AddCommentInput struct {
	Body           string `json:"body"`
	IdempotencyKey string `json:"idempotency_key"`
}

type CommentResult struct {
	Comment       Comment      `json:"comment"`
	CreatedWaits  []WaitingFor `json:"created_waits"`
	ResolvedWaits []WaitingFor `json:"resolved_waits"`
}

type PrincipalInfo struct {
	Customer string   `json:"customer"`
	Agents   []string `json:"agents"`
	Groups   []string `json:"groups"`
}

type Notification struct {
	ID                  string `json:"id"`
	Channel             string `json:"channel"`
	Type                string `json:"type"`
	Text                string `json:"text"`
	RequestingPrincipal string `json:"requesting_principal"`
	TaskKey             string `json:"task_key"`
	EventSeq            int64  `json:"event_sequence"`
	CreatedAt           string `json:"created_at"`
	PublishedAt         string `json:"published_at"`
	ReadAt              string `json:"read_at"`
	DismissedAt         string `json:"dismissed_at"`
}

func normalizeAssignee(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.Contains(value, ":") {
		return value
	}
	return agentPrincipal(value)
}

func validateActor(actor Actor) error {
	if strings.TrimSpace(actor.Principal) == "" {
		return domainError(http.StatusForbidden, "forbidden", "actor identity is required")
	}
	return nil
}

func notFound(key string) error {
	return domainError(http.StatusNotFound, "not_found", fmt.Sprintf("task %q not found", key))
}
