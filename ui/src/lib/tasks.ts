import { ApiError, apiOn, resolveTarget, type ApiTarget } from "./api"
import { taskStatusLabel as flexibleStatusLabel } from "./statusTone"

export type TaskStatus = "open" | "in_progress" | "wait_customer" | "done" | "cancelled"
export type TaskCategory = TaskStatus
export type WaitingOn = "customer" | "script" | "pause"
export type TaskStatusView = "active" | "closed" | "all"
export type TaskAccess = "write" | "respond" | "context"
export type TaskPriority = "P0" | "P1" | "P2" | "P3"

export interface TaskQueue {
  prefix: string
  name: string
  description: string
  owners: string[]
  responsible_agent: string
  revision: number
  created_at: string
  updated_at: string
}

export interface Task {
  key: string
  queue: string
  parent_key: string
  position: number
  priority: TaskPriority
  title: string
  description: string
  // The flexible status, or the workflow status ID for a workflow task.
  status: string
  category: TaskCategory
  waiting_on?: WaitingOn
  workflow_name?: string
  workflow_version?: string
  workflow_digest?: string
  workflow_paused_reason?: string
  pull_request?: string
  author: string
  customer: string
  group: string
  assignee: string
  manual_block_reason: string
  blocked: boolean
  revision: number
  created_at: string
  started_at?: string
  updated_at: string
  completed_at: string
  access?: TaskAccess
}

export interface AgentPool {
  id: number
  queue: string
  name: string
  agents: string[]
  revision: number
  created_at: string
  updated_at: string
}

export interface TaskComment {
  id: number
  task_key: string
  author: string
  body: string
  revision: number
  created_at: string
  updated_at: string
}

export interface TaskWait {
  id: number
  task_key: string
  expected_principal: string
  requesting_principal: string
  requesting_comment_id: number
  requested_at: string
  resolving_comment_id?: number
  resolved_at?: string
}

export type TaskRelationType = "blocks" | "related"
export interface TaskRelation {
  id: number
  source_key: string
  source_title: string
  source_status: TaskStatus
  target_key: string
  target_title: string
  target_status: TaskStatus
  type: TaskRelationType
  created_by: string
  created_at: string
}

export interface TaskDetail {
  task: Task
  comments: TaskComment[]
  waiting_for: TaskWait[]
  relations: TaskRelation[]
}

export interface TaskEvent {
  sequence: number
  event_id: string
  task_key?: string
  queue: string
  kind: string
  actor: string
  task_revision: number
  payload: Record<string, unknown>
  created_at: string
}

export interface TaskEventHint {
  type: "event"
  sequence: number
  kind: string
  task_key?: string
  queue?: string
  task_revision?: number
}

export interface TaskNotification {
  id: string
  channel: string
  type: string
  text: string
  requesting_principal: string
  task_key: string
  event_sequence: number
  created_at: string
  published_at: string
  read_at: string
  dismissed_at: string
}

export interface TaskPrincipals {
  customer: string
  agents: string[]
  groups: string[]
}

export interface TaskFilters {
  queue?: string
  status?: TaskStatus
  status_view?: TaskStatusView
  assignee?: string
  author?: string
  group?: string
  text?: string
  waiting_for?: string
  scope_agent?: string
  blocked?: boolean
  limit?: number
  after?: string
}

export interface TaskPage {
  tasks: Task[]
  next_cursor?: string
  sequence: number
}

export interface CreateQueueInput {
  prefix: string
  name: string
  description?: string
  owners?: string[]
  responsible_agent?: string
}

export interface UpdateQueueInput {
  name?: string
  description?: string
  owners?: string[]
  responsible_agent?: string
  revision: number
}

export interface CreateTaskInput {
  queue: string
  parent_key?: string
  title: string
  description?: string
  pull_request?: string
  assignee?: string
  group?: string
  priority?: TaskPriority
  idempotency_key?: string
}

export interface UpdateTaskInput {
  title?: string
  description?: string
  status?: TaskStatus
  pull_request?: string
  assignee?: string
  manual_block_reason?: string
  priority?: TaskPriority
  revision: number
}

export interface MoveTaskInput {
  parent_key?: string
  before_key?: string
  revision: number
}

export interface CommentResult {
  comment: TaskComment
  created_waits: TaskWait[]
  resolved_waits: TaskWait[]
}

function call<T>(
  target: ApiTarget,
  method: string,
  path: string,
  body?: unknown,
): Promise<T> {
  return apiOn<T>(resolveTarget(target), method, path, body)
}

function queryPath(path: string, values: object): string {
  const query = new URLSearchParams()
  for (const [key, value] of Object.entries(values) as [string, string | number | boolean | undefined][]) {
    if (value !== undefined && value !== "") query.set(key, String(value))
  }
  const encoded = query.toString()
  return encoded ? `${path}?${encoded}` : path
}

export const listTaskQueues = (target?: ApiTarget) =>
  call<{ queues: TaskQueue[]; count: number }>(target, "GET", "/api/task-queues")
export const getTaskQueue = (prefix: string, target?: ApiTarget) =>
  call<TaskQueue>(target, "GET", `/api/task-queues/${encodeURIComponent(prefix)}`)
export const createTaskQueue = (input: CreateQueueInput, target?: ApiTarget) =>
  call<TaskQueue>(target, "POST", "/api/task-queues", input)
export const updateTaskQueue = (prefix: string, input: UpdateQueueInput, target?: ApiTarget) =>
  call<TaskQueue>(target, "PATCH", `/api/task-queues/${encodeURIComponent(prefix)}`, input)

export const listAgentPools = (queue: string, target?: ApiTarget) =>
  call<{ items: AgentPool[]; count: number }>(target, "GET", `/api/task-queues/${encodeURIComponent(queue)}/pools`)
export const rebindAgentPool = (queue: string, pool: string, agents: string[], revision: number, idempotencyKey: string, target?: ApiTarget) =>
  call<AgentPool>(target, "PATCH", `/api/task-queues/${encodeURIComponent(queue)}/pools/${encodeURIComponent(pool)}`, {
    agents, revision, idempotency_key: idempotencyKey,
  })

export const listTasks = (filters: TaskFilters = {}, target?: ApiTarget) =>
  call<TaskPage>(target, "GET", queryPath("/api/tasks", filters))
export const getTask = (key: string, target?: ApiTarget) =>
  call<TaskDetail>(target, "GET", `/api/tasks/${encodeURIComponent(key)}`)
export const createTask = (input: CreateTaskInput, target?: ApiTarget) =>
  call<Task>(target, "POST", "/api/tasks", input)
export const updateTask = (key: string, input: UpdateTaskInput, target?: ApiTarget) =>
  call<Task>(target, "PATCH", `/api/tasks/${encodeURIComponent(key)}`, input)
export const claimTask = (key: string, revision: number, target?: ApiTarget) =>
  call<Task>(target, "POST", `/api/tasks/${encodeURIComponent(key)}/claim`, { revision })
export const moveTask = (key: string, input: MoveTaskInput, target?: ApiTarget) =>
  call<Task>(target, "POST", `/api/tasks/${encodeURIComponent(key)}/move`, input)
export const completeTask = (
  key: string,
  revision: number,
  completeAnyway = false,
  target?: ApiTarget,
) => call<Task>(target, "POST", `/api/tasks/${encodeURIComponent(key)}/complete`, {
  revision,
  complete_anyway: completeAnyway,
})

// A transfer bundle travels between daemons unchanged: the source exports it,
// the target imports it under the same keys. Only the desktop app is
// authenticated against both hosts, so it drives the two calls.
export interface TransferBundle {
  root_key: string
  queue: string
  tasks: unknown[]
  relations: unknown[]
}

export const exportTask = (key: string, target?: ApiTarget) =>
  call<TransferBundle>(target, "GET", `/api/tasks/${encodeURIComponent(key)}/export`)
export const importTask = (bundle: TransferBundle, target?: ApiTarget) =>
  call<Task>(target, "POST", "/api/tasks/import", { bundle })

// transferTask is the whole move, in the order that keeps a failure
// inspectable: read the tree on the source, write it on the destination, then
// close the copy that stays behind. Nothing is deleted, so a failure after the
// import leaves the task readable on both hosts.
export async function transferTask(
  key: string,
  source: ApiTarget,
  destination: ApiTarget,
  destinationLabel: string,
  idempotencyKey?: string,
): Promise<void> {
  const bundle = await exportTask(key, source)
  await importTask(bundle, destination)
  await addTaskComment(key, `Moved to ${destinationLabel} as ${key}.`, source, idempotencyKey)
  const current = await getTask(key, source)
  await updateTask(key, { status: "cancelled", revision: current.task.revision }, source)
}

export const listTaskComments = (key: string, target?: ApiTarget) =>
  call<{ comments: TaskComment[]; count: number }>(
    target,
    "GET",
    `/api/tasks/${encodeURIComponent(key)}/comments`,
  )
export const addTaskComment = (
  key: string,
  body: string,
  target?: ApiTarget,
  idempotencyKey?: string,
) => call<CommentResult>(target, "POST", `/api/tasks/${encodeURIComponent(key)}/comments`, {
  body,
  idempotency_key: idempotencyKey,
})

export const listTaskRelations = (key: string, target?: ApiTarget) =>
  call<{ relations: TaskRelation[]; count: number }>(
    target,
    "GET",
    `/api/tasks/${encodeURIComponent(key)}/relations`,
  )
export const addTaskRelation = (
  key: string,
  targetKey: string,
  type: TaskRelationType,
  revision: number,
  target?: ApiTarget,
  idempotencyKey?: string,
) => call<TaskRelation>(target, "POST", `/api/tasks/${encodeURIComponent(key)}/relations`, {
  target_key: targetKey,
  type,
  revision,
  idempotency_key: idempotencyKey,
})
export const deleteTaskRelation = (
  key: string,
  relationID: number,
  revision: number,
  target?: ApiTarget,
  idempotencyKey?: string,
) =>
  call<{ deleted: boolean; relation_id: number }>(
    target,
    "DELETE",
    queryPath(`/api/tasks/${encodeURIComponent(key)}/relations`, {
      relation_id: relationID,
      revision,
      idempotency_key: idempotencyKey,
    }),
  )

export const listTaskEvents = (
  key: string,
  after = 0,
  limit = 200,
  target?: ApiTarget,
) => call<{ events: TaskEvent[]; count: number }>(
  target,
  "GET",
  queryPath(`/api/tasks/${encodeURIComponent(key)}/events`, { after, limit }),
)

export const listTaskPrincipals = (target?: ApiTarget) =>
  call<TaskPrincipals>(target, "GET", "/api/task-principals")
export const listTaskNotifications = (includeDismissed = false, target?: ApiTarget) =>
  call<{ notifications: TaskNotification[]; count: number }>(
    target,
    "GET",
    queryPath("/api/task-notifications", { include_dismissed: includeDismissed || undefined }),
  )
export const markTaskNotificationRead = (id: string, target?: ApiTarget) =>
  call<TaskNotification>(
    target,
    "POST",
    `/api/task-notifications/${encodeURIComponent(id)}/read`,
  )

// Workflow tasks.

export interface WorkflowOutcome { on: string; to: string; requires?: string[]; missing?: string[]; checks?: string[] }
export interface WorkflowArtifact { id: number; name: string; value: string; author: string; created_at: string }
export interface WorkflowVisit {
  id: number
  sequence: number
  status: string
  entered_at: string
  entered_by: string
  left_at?: string
  outcome?: string
  message?: string
}
export type TransitionState = "pending" | "applied" | "rejected" | "failed" | "cancelled"
export interface TransitionRequest {
  id: number
  task_key: string
  outcome: string
  message?: string
  actor: string
  state: TransitionState
  result_message?: string
  created_at: string
  finished_at?: string
  // Only while pending: how long the checks may take, in seconds.
  wait_seconds?: number
}
export type ScriptRunState = "pending" | "running" | "finished" | "interrupted" | "cancelled"
export type ScriptRunVerdict = "pass" | "reject" | "outcome" | "quiet" | "failure"
export interface ScriptRun {
  id: number
  task_key: string
  kind: "check" | "watch"
  script: string
  run_as: "queue" | "agent"
  state: ScriptRunState
  verdict?: ScriptRunVerdict
  exit_code?: number
  holder?: string
  message?: string
  created_at: string
  started_at?: string
  finished_at?: string
  log_path?: string
}
export interface WorkflowView {
  name: string
  version: string
  digest: string
  status: string
  category: TaskCategory
  waiting_on?: string
  paused_reason?: string
  owner: string
  holder?: string
  instructions_path?: string
  outcomes: WorkflowOutcome[]
  artifacts: WorkflowArtifact[]
  visits: WorkflowVisit[]
  last_request?: TransitionRequest
  runs?: ScriptRun[]
  /** Every artifact the manifest declares, in manifest order. */
  declared_artifacts: WorkflowDeclaredArtifact[]
  /** Every status the manifest declares, in manifest order. */
  statuses: WorkflowStatusInfo[]
}
export interface WorkflowDeclaredArtifact { name: string; description: string }
/** `owner` is `pool:NAME`, `customer`, `script`, or "" for a terminal status. */
export interface WorkflowStatusInfo { id: string; owner: string; terminal: boolean }
export interface QueueWorkflow { queue: string; name: string; version: string; digest: string; revision: number; updated_at: string }
export interface QueueSecretInfo { key: string; updated_at: string }
export interface WorkflowImage { name: string; tag: string; version: string; digest: string; built_at: string }
/** `kind` is "" for a terminal status. */
export interface WorkflowOwner { kind: "pool" | "customer" | "script" | ""; pool?: string }
export interface WorkflowLimits { idle_iterations?: number; rejected_requests?: number; script_failures?: number; unavailable_grace?: string }
export interface WorkflowCheck { script: string; run_as?: string; timeout?: string }
export interface WorkflowWatch { script: string; every: string; timeout?: string }
export interface WorkflowTransition { on: string; to: string; requires?: string[]; checks?: WorkflowCheck[] }
export interface WorkflowDefinitionStatus {
  id: string
  owner: WorkflowOwner
  instructions?: string
  watch?: WorkflowWatch
  transitions?: WorkflowTransition[]
  limits?: WorkflowLimits
  terminal?: boolean
  cancelled?: boolean
}
/** A parsed Workflowfile.yaml as the manifest stores it. */
export interface WorkflowDefinition {
  schema_version: number
  name: string
  workflow_version: string
  initial_status: string
  requires_secrets?: string[]
  env?: Record<string, string>
  limits?: WorkflowLimits
  artifacts?: WorkflowDeclaredArtifact[]
  statuses: WorkflowDefinitionStatus[]
}
export interface WorkflowFileEntry { path: string; sha256: string; executable: boolean; size: number }
export interface WorkflowManifest {
  schema_version: number
  name: string
  version: string
  digest: string
  built_at: string
  definition: WorkflowDefinition
  files: WorkflowFileEntry[]
}
export interface WorkflowValidationError { code: string; path: string; message: string }
export interface WorkflowValidation {
  valid: boolean
  name: string
  version: string
  pools: string[]
  files: string[]
  errors: WorkflowValidationError[]
}
export interface WorkflowBuildResult { name: string; version: string; digest: string; tags: string[]; created: boolean }

/**
 * A workflow task reports a workflow status ID in `status`, a flexible task its
 * own status. The daemon marks one by its digest; the name is a fallback.
 */
export const isWorkflowTask = (task: Task): boolean => !!(task.workflow_digest || task.workflow_name)

/** The status as a person reads it, for either kind of task. */
export function taskStatusLabel(task: Task): string {
  if (!isWorkflowTask(task)) return flexibleStatusLabel(task.status)
  const text = task.status.replace(/[_-]+/g, " ")
  return text.charAt(0).toUpperCase() + text.slice(1)
}
export const taskCategoryLabel = (category: TaskCategory): string => flexibleStatusLabel(category)
export const isActiveCategory = (category: TaskCategory): boolean =>
  category === "open" || category === "in_progress"

const taskPath = (key: string) => `/api/tasks/${encodeURIComponent(key)}`
const queuePath = (queue: string) => `/api/task-queues/${encodeURIComponent(queue)}`

export const getTaskWorkflow = (key: string, target?: ApiTarget) =>
  call<WorkflowView>(target, "GET", `${taskPath(key)}/workflow`)
export const advanceTask = (
  key: string,
  outcome: string,
  message: string,
  from?: string,
  target?: ApiTarget,
) => call<TransitionRequest>(target, "POST", `${taskPath(key)}/advance`, {
  outcome,
  message,
  ...(from ? { from } : {}),
})
export const getTransitionRequest = (key: string, id: number, target?: ApiTarget) =>
  call<TransitionRequest>(target, "GET", `${taskPath(key)}/workflow/requests/${id}`)
export const setTaskArtifact = (key: string, name: string, value: string, target?: ApiTarget) =>
  call<WorkflowArtifact>(target, "PUT", `${taskPath(key)}/artifacts/${encodeURIComponent(name)}`, { value })
export const getTaskArtifactHistory = async (key: string, name: string, target?: ApiTarget) =>
  (await call<{ artifact: WorkflowArtifact; history: WorkflowArtifact[] }>(
    target,
    "GET",
    `${taskPath(key)}/artifacts/${encodeURIComponent(name)}`,
  )).history
export const listTaskScriptRuns = async (key: string, target?: ApiTarget) =>
  (await call<{ runs: ScriptRun[]; count: number }>(target, "GET", `${taskPath(key)}/workflow/runs`)).runs
export const getTaskScriptRunLog = async (
  key: string,
  id: number,
  maxBytes?: number,
  target?: ApiTarget,
): Promise<{ text: string; truncated: boolean }> => {
  const { text, truncated } = await call<{ run_id: number; text: string; truncated: boolean }>(
    target,
    "GET",
    queryPath(`${taskPath(key)}/workflow/runs/${id}/log`, { max_bytes: maxBytes }),
  )
  return { text, truncated }
}
export const moveTaskWorkflow = (key: string, to: string, reason: string, target?: ApiTarget) =>
  call<Task>(target, "POST", `${taskPath(key)}/workflow/move`, { to, reason })
export const cancelWorkflowTask = (key: string, target?: ApiTarget) =>
  call<Task>(target, "POST", `${taskPath(key)}/cancel`)
export const resumeTaskWorkflow = (
  key: string,
  decision: "continue" | "release",
  target?: ApiTarget,
) => call<Task>(target, "POST", `${taskPath(key)}/workflow/resume`, { decision })

/** The workflow image a queue is bound to, or null when it is unbound. */
export async function getQueueWorkflow(queue: string, target?: ApiTarget): Promise<QueueWorkflow | null> {
  try {
    return await call<QueueWorkflow>(target, "GET", `${queuePath(queue)}/workflow`)
  } catch (err) {
    if (err instanceof ApiError && err.code === "queue_workflow_not_found") return null
    throw err
  }
}
export const setQueueWorkflow = (queue: string, ref: string, revision: number, target?: ApiTarget) =>
  call<QueueWorkflow>(target, "PUT", `${queuePath(queue)}/workflow`, { ref, revision })
export const clearQueueWorkflow = async (queue: string, revision: number, target?: ApiTarget): Promise<void> => {
  await call(target, "DELETE", queryPath(`${queuePath(queue)}/workflow`, { revision }))
}

// Secret values are write-only: a client lists keys and sets or removes them,
// and never reads a value back.
export const listQueueSecrets = async (queue: string, target?: ApiTarget) =>
  (await call<{ secrets: QueueSecretInfo[]; count: number }>(target, "GET", `${queuePath(queue)}/secrets`)).secrets
export const setQueueSecret = (queue: string, key: string, value: string, target?: ApiTarget) =>
  call<{ queue: string; key: string; updated_at: string }>(
    target,
    "PUT",
    `${queuePath(queue)}/secrets/${encodeURIComponent(key)}`,
    { value },
  )
export const removeQueueSecret = async (queue: string, key: string, target?: ApiTarget): Promise<void> => {
  await call(target, "DELETE", `${queuePath(queue)}/secrets/${encodeURIComponent(key)}`)
}

export const listWorkflowImages = async (target?: ApiTarget) =>
  (await call<{ workflows: WorkflowImage[]; count: number }>(target, "GET", "/api/workflow-images")).workflows
const workflowImagePath = (name: string, tag: string) =>
  `/api/workflow-images/${encodeURIComponent(name)}/${encodeURIComponent(tag)}`
/** The manifest a tag or a full digest names. */
export const getWorkflowImage = (name: string, tag: string, target?: ApiTarget) =>
  call<WorkflowManifest>(target, "GET", workflowImagePath(name, tag))
export const removeWorkflowImage = (name: string, tag: string, target?: ApiTarget) =>
  call<{ name: string; tag: string; removed: boolean; content_removed: boolean }>(target, "DELETE", workflowImagePath(name, tag))
/** `path` is a Workflowfile.yaml, or its directory, on the host of the target. */
export const validateWorkflowDirectory = (path: string, target?: ApiTarget) =>
  call<WorkflowValidation>(target, "POST", "/api/workflow-images/validate", { path })
export const buildWorkflowDirectory = (path: string, target?: ApiTarget) =>
  call<WorkflowBuildResult>(target, "POST", "/api/workflow-images/build", { path })
