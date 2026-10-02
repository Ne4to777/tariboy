import { apiOn, resolveTarget, type ApiTarget } from "./api"

export type TaskStatus = "open" | "in_progress" | "wait_customer" | "done" | "cancelled"
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
  status: TaskStatus
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
