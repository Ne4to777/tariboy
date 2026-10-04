import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { expect, test, type APIRequestContext, type Page } from "playwright/test";

const daemonURL = "http://127.0.0.1:4176";

async function taskFromAPI(request: APIRequestContext, key: string) {
  const response = await request.get(`${daemonURL}/api/tasks/${key}`);
  expect(response.ok()).toBe(true);
  const envelope = await response.json();
  expect(envelope.ok).toBe(true);
  return envelope.result.task as {
    key: string;
    parent_key: string;
    position: number;
    priority: "P0" | "P1" | "P2" | "P3";
    title: string;
    description: string;
    status: string;
    assignee: string;
    manual_block_reason: string;
    pull_request: string;
  };
}

async function createTask(
  request: APIRequestContext,
  data: { title: string; priority: "P0" | "P1" | "P2" | "P3"; parent_key?: string },
) {
  const response = await request.post(`${daemonURL}/api/tasks`, {
    data: {
      queue: "TEST",
      idempotency_key: `tasks-browser-priority-${data.title.toLowerCase().replaceAll(" ", "-")}`,
      ...data,
    },
  });
  expect(response.ok()).toBe(true);
  const envelope = await response.json();
  expect(envelope.ok).toBe(true);
  return envelope.result as { key: string };
}

async function keyByTitle(request: APIRequestContext, title: string) {
  let key = "";
  await expect.poll(async () => {
    const response = await request.get(`${daemonURL}/api/tasks?status_view=all&limit=500`);
    if (!response.ok()) return "";
    const envelope = await response.json();
    const match = (envelope.result?.tasks ?? []).find((task: { title: string }) => task.title === title);
    key = match?.key ?? "";
    return key;
  }, { timeout: 10_000 }).not.toBe("");
  return key;
}

async function visibleTaskKeys(page: Page) {
  return page.locator("[data-testid^='task-row-']").evaluateAll((rows) =>
    rows.map((row) => row.getAttribute("data-testid")?.replace("task-row-", "")),
  );
}

async function pointerDrag(page: Page, source: ReturnType<Page["getByRole"]>, target: ReturnType<Page["getByTestId"]>) {
  const sourceBox = await source.boundingBox();
  const targetBox = await target.boundingBox();
  expect(sourceBox).not.toBeNull();
  expect(targetBox).not.toBeNull();
  const startX = sourceBox!.x + sourceBox!.width / 2;
  const startY = sourceBox!.y + sourceBox!.height / 2;
  await page.mouse.move(startX, startY);
  await page.mouse.down();
  await page.mouse.move(startX + 8, startY, { steps: 2 });
  await page.mouse.move(targetBox!.x + targetBox!.width / 2, targetBox!.y + targetBox!.height / 2, { steps: 20 });
  // dnd-kit derives the drop destination from React pointer-move state. Do not
  // release the pointer until that state has observed the intended zone.
  const targetID = await target.getAttribute("data-testid");
  if (targetID?.startsWith("drop-inside-")) {
    // A row marks the re-parent target with its own ring, not a tree class.
    await expect(target.locator("..")).toHaveClass(/ring-primary/);
  } else {
    await expect(target).toHaveClass(/is-over/);
  }
  await page.mouse.up();
}

async function assertNoLoadFailedToast(page: Page) {
  await expect(page.getByText(/network error: Load failed/i)).toHaveCount(0);
}

test("Tasks production workspace opens, closes, resizes, and restores the detail sheet", async ({ page, request }) => {
  let response = await request.post(`${daemonURL}/api/task-queues`, { data: { prefix: "RESIZE", name: "Resize browser" } });
  expect(response.ok()).toBe(true);
  response = await request.post(`${daemonURL}/api/tasks`, { data: {
    queue: "RESIZE", title: "Resize detail sheet", idempotency_key: "tasks-browser-resize-detail",
  } });
  expect(response.ok()).toBe(true);
  const resizeKey = (await response.json()).result.key as string;
  await page.goto("/tests/tasks-fixture.html#/servers/local/tasks");
  await expect(page.getByLabel("Search tasks")).toBeVisible();
  await expect(page.getByRole("separator", { name: "Resize task details" })).toHaveCount(0);

  await page.getByTestId(`task-row-${resizeKey}`).locator(".task-row-main").click();
  const detailHandle = page.getByRole("separator", { name: "Resize task details" });
  await expect(detailHandle).toBeVisible();

  await expect.poll(async () => (await page.getByRole("dialog").boundingBox())?.width).toBe(720);
  await expect.poll(async () => {
    const box = await page.getByRole("dialog").boundingBox();
    return box && box.x + box.width;
    // The sheet is an island: it stops 8px short of the window edge.
  }).toBe(1432);

  const detailBox = await detailHandle.boundingBox();
  expect(detailBox).not.toBeNull();
  await page.mouse.move(detailBox!.x + detailBox!.width / 2, detailBox!.y + 60);
  await page.mouse.down();
  await page.mouse.move(detailBox!.x - 62, detailBox!.y + 60, { steps: 8 });
  await page.mouse.up();

  const persisted = await page.evaluate(() => JSON.parse(localStorage.getItem("tasks:workspace:v1") ?? "{}"));
  expect(persisted).toMatchObject({ schemaVersion: 1 });
  expect(persisted.detailWidth).toBeGreaterThan(460);

  await page.setViewportSize({ width: 900, height: 900 });
  await expect(detailHandle).toBeHidden();
  await page.setViewportSize({ width: 1440, height: 900 });
  await expect(detailHandle).toHaveAttribute("aria-valuenow", String(persisted.detailWidth));

  await page.locator('[data-slot="dialog-overlay"]').click({ position: { x: 10, y: 10 } });
  await expect(page.getByRole("dialog")).toHaveCount(0);

  await page.reload();
  await expect(page.getByLabel("Search tasks")).toBeVisible();
  await page.getByTestId(`task-row-${resizeKey}`).locator(".task-row-main").click();
  await expect(page.getByRole("separator", { name: "Resize task details" }))
    .toHaveAttribute("aria-valuenow", String(persisted.detailWidth));

  await page.locator('[data-slot="dialog-overlay"]').click({ position: { x: 10, y: 10 } });
  await expect(page.getByRole("dialog")).toHaveCount(0);
  await page.getByTestId(`task-row-${resizeKey}`).locator(".task-row-main").click();
  await page.getByRole("dialog").getByLabel("Title").fill("Unsaved resize detail");
  await page.locator('[data-slot="dialog-overlay"]').click({ position: { x: 10, y: 10 } });
  await expect(page.getByText("Are you sure you want to close this task? Unsaved changes will be discarded.")).toBeVisible();
});

test("Tasks production workspace lists the agent pools of a queue", async ({ page, request }) => {
  const response = await request.post(`${daemonURL}/api/task-queues`, { data: { prefix: "FLOW", name: "Pool browser" } })
  expect(response.ok()).toBe(true)

  await page.goto("/tests/tasks-fixture.html#/servers/local/tasks")
  await page.getByRole("button", { name: "Queue: all" }).click()
  await page.getByRole("menuitem", { name: "Manage queues…" }).click()
  await page.getByRole("button", { name: "Agent pools FLOW" }).click()
  await expect(page.getByLabel("Agent pools FLOW").getByRole("heading", { name: "Agent pools" })).toBeVisible()
  await expect(page.getByLabel("Pool name FLOW")).toBeVisible()
  await assertNoLoadFailedToast(page)
})

test("Tasks production workspace persists PATCH saves, release fields, and the full tree workflow", async ({
  page,
  request,
}) => {
  test.setTimeout(60_000);
  await page.goto("/tests/tasks-fixture.html#/servers/local/tasks");
  await expect(page.getByLabel("Search tasks")).toBeVisible();

  await page.getByRole("button", { name: "Queue: all" }).click();
  await page.getByRole("menuitem", { name: "Manage queues…" }).click();
  await page.getByRole("button", { name: "New queue" }).click();
  await page.getByLabel("Queue prefix").fill("test");
  await page.getByLabel("New queue name").fill("Tasks E2E");
  await page.getByRole("button", { name: "Add queue" }).click();
  await expect(page.getByRole("heading", { name: "Tasks E2E" })).toBeVisible();

  await page.getByRole("button", { name: "Rename TEST" }).click();
  await page.getByLabel("Queue name TEST").fill("Tasks E2E updated");
  await page.getByLabel("Queue description TEST").fill("Saved through PATCH from a browser origin");
  await page.getByRole("button", { name: "Save TEST" }).click();
  await expect(page.getByText("Queue updated", { exact: true })).toBeVisible();
  await assertNoLoadFailedToast(page);

  await page.getByRole("button", { name: "Close" }).click();
  await page.getByRole("button", { name: "New task" }).click();
  await page.getByLabel("Task queue").selectOption("TEST");
  await page.getByLabel("Task title").fill("Root task");
  await page.getByRole("button", { name: "Create task" }).click();
  // The daemon mints a random key suffix, so the spec asks it which key it made.
  const rootKey = await keyByTitle(request, "Root task");
  await expect(page.getByTestId(`task-row-${rootKey}`)).toBeVisible();
  await page.getByRole("button", { name: "Close task detail" }).click();

  await page.getByRole("button", { name: `Add child to ${rootKey}` }).click();
  await page.getByLabel("Task title").fill("First child");
  await page.getByRole("button", { name: "Create task" }).click();
  const childKey = await keyByTitle(request, "First child");
  await expect(page.getByTestId(`task-row-${childKey}`)).toBeVisible();
  await page.getByRole("button", { name: "Close task detail" }).click();

  await page.getByRole("button", { name: "New task" }).click();
  await page.getByLabel("Task queue").selectOption("TEST");
  await page.getByLabel("Task title").fill("Second root");
  await page.getByRole("button", { name: "Create task" }).click();
  const secondRootKey = await keyByTitle(request, "Second root");
  await expect(page.getByTestId(`task-row-${secondRootKey}`)).toBeVisible();
  await page.getByRole("button", { name: "Close task detail" }).click();

  await page.getByTestId(`task-row-${rootKey}`).locator(".task-row-main").click();
  const detail = page.locator(".task-detail-panel");
  await expect(detail.getByRole("heading", { name: rootKey })).toBeVisible();
  await detail.getByLabel("Title").fill("Root task updated");
  await detail.getByRole("textbox", { name: "Description" }).fill("Edited from the production Tasks form");
  await detail.getByLabel("Status").selectOption("in_progress");
  await detail.getByLabel("Assignee").fill("worker-e2e");
  await detail.getByLabel("Manual block reason").fill("waiting on E2E fixture");
  const taskPatchRequest = page.waitForRequest((request) =>
    request.method() === "PATCH" && request.url() === `${daemonURL}/api/tasks/${rootKey}`);
  await detail.getByRole("button", { name: "Save task" }).click();
  expect((await taskPatchRequest).postDataJSON()).toMatchObject({
    title: "Root task updated",
    description: "Edited from the production Tasks form",
    status: "in_progress",
    assignee: "worker-e2e",
    manual_block_reason: "waiting on E2E fixture",
  });
  await expect(page.getByText("Task updated", { exact: true }).last()).toBeVisible();
  await expect(page.getByTestId(`task-row-${rootKey}`)).toContainText("Root task updated");
  await expect(page.getByTestId(`task-row-${rootKey}`)).toContainText("blocked");
  await expect(page.getByTestId(`task-row-${rootKey}`)).toContainText("worker-e2e");
  await assertNoLoadFailedToast(page);

  await detail.getByLabel("Status").selectOption("wait_customer");
  await detail.getByLabel("Pull request").fill("https://github.com/acme/tariboy/pull/43");
  await detail.getByRole("button", { name: "Save task" }).click();
  await expect(page.getByText("Task updated", { exact: true }).last()).toBeVisible();
  expect(await taskFromAPI(request, rootKey)).toMatchObject({
    status: "wait_customer",
    pull_request: "https://github.com/acme/tariboy/pull/43",
  });

  await page.reload();
  await page.getByTestId(`task-row-${rootKey}`).locator(".task-row-main").click();
  await expect(detail.getByLabel("Status")).toHaveValue("wait_customer");
  await expect(detail.getByLabel("Pull request")).toHaveValue("https://github.com/acme/tariboy/pull/43");
  await detail.getByLabel("Pull request").fill("");
  await detail.getByRole("button", { name: "Save task" }).click();
  await expect(page.getByText("Task updated", { exact: true }).last()).toBeVisible();

  await page.reload();
  await page.getByTestId(`task-row-${rootKey}`).locator(".task-row-main").click();
  await expect(detail.getByLabel("Status")).toHaveValue("wait_customer");
  await expect(detail.getByLabel("Pull request")).toHaveValue("");

  const saved = await taskFromAPI(request, rootKey);
  expect(saved).toMatchObject({
    title: "Root task updated",
    description: "Edited from the production Tasks form",
    status: "wait_customer",
    assignee: "agent:worker-e2e",
    manual_block_reason: "waiting on E2E fixture",
    pull_request: "",
  });

  await detail.getByLabel("Ask", { exact: true }).selectOption({ index: 1 });
  await detail.getByLabel("Comment", { exact: true }).fill("Please confirm the browser workflow");
  await detail.getByRole("button", { name: "Send comment" }).click();
  await expect(detail.getByText(/Waiting for an answer from user:/)).toBeVisible();
  await expect(detail.getByText("Please confirm the browser workflow")).toBeVisible();

  await page.reload();
  await expect(page.getByLabel("Search tasks")).toBeVisible();
  await page.getByRole("button", { name: "Waiting for me" }).click();
  await expect(page.getByTestId(`task-row-${rootKey}`)).toBeVisible();
  await page.getByTestId(`task-row-${rootKey}`).locator(".task-row-main").click();
  await detail.getByLabel("Comment", { exact: true }).fill("Confirmed from the customer");
  await detail.getByRole("button", { name: "Send comment" }).click();
  await expect(detail.getByText(/Waiting for an answer from user:/)).toHaveCount(0);
  await expect(detail.getByText("Confirmed from the customer")).toBeVisible();

  await detail.getByRole("button", { name: "Close task detail" }).click();
  await page.getByRole("button", { name: "Waiting for me" }).click();
  await page.getByTestId(`task-row-${rootKey}`).locator(".task-row-main").click();
  await detail.getByLabel("Relation type").selectOption("related");
  await detail.getByLabel("Related task key").fill(secondRootKey);
  await detail.getByRole("button", { name: "Add relation" }).click();
  // The dependency row spells its type and key in separate cells.
  const relationRow = detail.getByRole("button", { name: `Remove relation to ${secondRootKey}` });
  await expect(relationRow).toBeVisible();
  await expect(detail.getByText("related", { exact: true })).toBeVisible();
  await relationRow.click();
  await expect(relationRow).toHaveCount(0);
  await detail.getByRole("button", { name: "Close task detail" }).click();

  const criticalRoot = await createTask(request, { title: "Critical root", priority: "P0" });
  const secondCriticalRoot = await createTask(request, { title: "Second critical root", priority: "P0" });
  const lowRoot = await createTask(request, { title: "Low root", priority: "P3" });
  const criticalChild = await createTask(request, {
    title: "Critical child",
    priority: "P0",
    parent_key: rootKey,
  });
  const lowChild = await createTask(request, {
    title: "Low child",
    priority: "P3",
    parent_key: rootKey,
  });
  const expandRoot = page.getByRole("button", { name: `Expand ${rootKey}` });
  if (await expandRoot.isVisible()) await expandRoot.click();
  await expect(page.getByTestId(`task-row-${lowChild.key}`)).toBeVisible({ timeout: 10_000 });

  let ordered = await visibleTaskKeys(page);
  expect(ordered.indexOf(criticalRoot.key)).toBeLessThan(ordered.indexOf(rootKey));
  expect(ordered.indexOf(rootKey)).toBeLessThan(ordered.indexOf(lowRoot.key));
  expect(ordered.indexOf(criticalChild.key)).toBeLessThan(ordered.indexOf(childKey));
  expect(ordered.indexOf(childKey)).toBeLessThan(ordered.indexOf(lowChild.key));
  await expect(page.getByTestId(`task-row-${criticalRoot.key}`).getByLabel("P0 Critical")).toHaveText("P0");
  await expect(page.getByTestId(`task-row-${lowRoot.key}`).getByLabel("P3 Low")).toHaveText("P3");

  await pointerDrag(page, page.getByRole("button", { name: `Move ${secondCriticalRoot.key}` }), page.getByTestId(`drop-before-${criticalRoot.key}`));
  await expect.poll(async () => {
    const second = await taskFromAPI(request, secondCriticalRoot.key);
    const first = await taskFromAPI(request, criticalRoot.key);
    return second.position < first.position;
  }).toBe(true);

  const lowPosition = (await taskFromAPI(request, lowRoot.key)).position;
  await pointerDrag(page, page.getByRole("button", { name: `Move ${lowRoot.key}` }), page.getByTestId(`drop-before-${criticalRoot.key}`));
  await expect.poll(async () => (await taskFromAPI(request, lowRoot.key)).position).toBe(lowPosition);
  ordered = await visibleTaskKeys(page);
  expect(ordered.indexOf(criticalRoot.key)).toBeLessThan(ordered.indexOf(lowRoot.key));

  await pointerDrag(page, page.getByRole("button", { name: `Move ${lowRoot.key}` }), page.getByTestId(`drop-inside-${rootKey}`));
  await expect.poll(async () => (await taskFromAPI(request, lowRoot.key)).parent_key).toBe(rootKey);
  ordered = await visibleTaskKeys(page);
  expect(ordered.indexOf(childKey)).toBeLessThan(ordered.indexOf(lowChild.key));
  expect(ordered.indexOf(childKey)).toBeLessThan(ordered.indexOf(lowRoot.key));

  await pointerDrag(page, page.getByRole("button", { name: `Move ${secondRootKey}` }), page.getByTestId(`drop-inside-${rootKey}`));
  await expect.poll(async () => (await taskFromAPI(request, secondRootKey)).parent_key).toBe(rootKey);
  await expect(page.getByTestId(`task-row-${secondRootKey}`)).toBeVisible();

  await pointerDrag(page, page.getByRole("button", { name: `Move ${secondRootKey}` }), page.getByTestId(`drop-before-${childKey}`));
  await expect.poll(async () => {
    const moved = await taskFromAPI(request, secondRootKey);
    const first = await taskFromAPI(request, childKey);
    return moved.position < first.position;
  }).toBe(true);

  await page.getByRole("button", { name: "My tasks" }).click();
  await expect(page.getByRole("button", { name: "My tasks" })).toHaveAttribute("aria-pressed", "true");
  await page.getByRole("button", { name: "My tasks" }).click();
  await page.getByLabel("Search tasks").fill("Root task updated");
  await expect(page.getByTestId(`task-row-${rootKey}`)).toBeVisible();
  await expect(page.getByTestId(`task-row-${secondRootKey}`)).toHaveCount(0);
  await page.getByLabel("Search tasks").fill("");

  const createResponse = await request.post(`${daemonURL}/api/tasks`, {
    data: {
      queue: "TEST",
      title: "Realtime injected task",
      idempotency_key: "tasks-browser-e2e-realtime",
    },
  });
  expect(createResponse.ok()).toBe(true);
  await expect(page.getByText("Realtime injected task")).toBeVisible({ timeout: 10_000 });

  await page.reload();
  await expect(page.getByText("Realtime injected task")).toBeVisible();
  await expect(page.getByText("Root task updated")).toBeVisible();
  await assertNoLoadFailedToast(page);
});

const approvalWorkflow = `schema_version: 1
name: wfui
workflow_version: 0.1.0
initial_status: approval
statuses:
  - id: approval
    owner: customer
    instructions: ./statuses/approval.md
    transitions:
      - { on: approved, to: done }
  - id: done
    terminal: true
`;

test("Tasks production workspace drives a workflow queue: bind, approve, secrets", async ({ page, request }) => {
  test.setTimeout(90_000);
  const started = Date.now();
  const step = (name: string) => console.log(`[workflow-ui] ${((Date.now() - started) / 1000).toFixed(1)}s ${name}`);

  // Step 1: publish the fixture image through the daemon's build route, then
  // bind the queue from the queue settings.
  const source = mkdtempSync(path.join(tmpdir(), "tasks-e2e-workflow-"));
  mkdirSync(path.join(source, "statuses"));
  writeFileSync(path.join(source, "Workflowfile.yaml"), approvalWorkflow);
  writeFileSync(path.join(source, "statuses", "approval.md"), "Approve the work when it looks right.\n");
  let response = await request.post(`${daemonURL}/api/workflow-images/build`, { data: { path: source } });
  rmSync(source, { recursive: true, force: true });
  expect(response.ok(), "step 1: the fixture workflow image must build").toBe(true);
  response = await request.post(`${daemonURL}/api/task-queues`, { data: { prefix: "WFUI", name: "Workflow browser" } });
  expect(response.ok()).toBe(true);
  response = await request.post(`${daemonURL}/api/task-queues`, { data: { prefix: "WFLEX", name: "Flexible browser" } });
  expect(response.ok()).toBe(true);

  await page.goto("/tests/tasks-fixture.html#/servers/local/tasks");
  await expect(page.getByLabel("Search tasks")).toBeVisible();
  await page.getByRole("button", { name: "Queue: all" }).click();
  await page.getByRole("menuitem", { name: "Manage queues…" }).click();
  await page.getByRole("button", { name: "Workflow WFUI" }).click();
  const settings = page.getByRole("region", { name: "Workflow WFUI" });
  await expect(settings.getByText("No workflow")).toBeVisible();
  await settings.getByLabel("Workflow image").selectOption("wfui:latest");
  await settings.getByRole("button", { name: "Bind", exact: true }).click();
  await expect(settings.getByTestId("binding"), "step 1: the binding shows after Bind").toContainText("wfui");
  step("1 published and bound");

  // Step 2: a task in the bound queue.
  await page.getByRole("button", { name: "Close", exact: true }).click();
  await page.getByRole("button", { name: "New task" }).click();
  await page.getByLabel("Task queue").selectOption("WFUI");
  await page.getByLabel("Task title").fill("Approve the plan");
  await page.getByRole("button", { name: "Create task" }).click();
  const key = await keyByTitle(request, "Approve the plan");
  expect((await taskFromAPI(request, key)).status).toBe("approval");
  step("2 task created");

  // Step 3: the list shows the label and the waiting indicator.
  await page.getByRole("button", { name: "Close task detail" }).click();
  const row = page.getByTestId(`task-row-${key}`);
  await expect(row, "step 3: the row shows the workflow status label").toContainText("Approval");
  await expect(row, "step 3: the row shows who is awaited").toContainText("customer");
  step("3 list label and indicator");

  // Step 4: the drawer shows the workflow panel and no status or assignee control.
  await row.locator(".task-row-main").click();
  const detail = page.locator(".task-detail-panel");
  await expect(detail.getByText("wfui@0.1.0"), "step 4: the panel header").toBeVisible();
  await expect(detail.getByText("status approval")).toBeVisible();
  await expect(detail.getByRole("button", { name: "approved", exact: true })).toBeEnabled();
  await expect(detail.getByRole("combobox", { name: "Status", exact: true }), "step 4: no status select").toHaveCount(0);
  await expect(detail.getByLabel("Assignee", { exact: true }), "step 4: no assignee control").toHaveCount(0);
  step("4 drawer panel");

  // Step 5: approve; the task closes.
  await detail.getByRole("button", { name: "approved", exact: true }).click();
  await expect(detail.getByText("status done"), "step 5: the panel shows the terminal status").toBeVisible({ timeout: 15_000 });
  await expect.poll(async () => (await taskFromAPI(request, key)).status).toBe("done");
  await detail.getByRole("button", { name: "Close task detail" }).click();
  await page.getByRole("radio", { name: "Closed", exact: true }).click();
  await expect(page.getByTestId(`task-row-${key}`), "step 5: the closed task is listed").toContainText("Done");
  step("5 approved and closed");

  // Step 5b: the customer moves one task by hand and cancels another. The
  // confirmation opens above the detail sheet, so its buttons take the click.
  const createWorkflowTask = async (title: string) => {
    const created = await request.post(`${daemonURL}/api/tasks`, { data: { queue: "WFUI", title, idempotency_key: `tasks-browser-${title}` } });
    expect(created.ok()).toBe(true);
    return (await created.json()).result.key as string;
  };
  const moveKey = await createWorkflowTask("Move by hand");
  const cancelKey = await createWorkflowTask("Cancel by hand");
  await page.getByRole("radio", { name: "Active", exact: true }).click();
  await page.getByTestId(`task-row-${moveKey}`).locator(".task-row-main").click();
  await detail.getByRole("button", { name: "Move to status…" }).click();
  await detail.getByLabel("Target status").selectOption("done");
  await detail.getByLabel("Reason", { exact: true }).fill("closed by hand");
  await detail.getByRole("button", { name: "Move", exact: true }).click();
  await page.getByRole("alertdialog").getByRole("button", { name: "Move task" }).click({ timeout: 5_000 });
  await expect.poll(async () => (await taskFromAPI(request, moveKey)).status, "step 5b: the move lands").toBe("done");
  await detail.getByRole("button", { name: "Close task detail" }).click();
  await page.getByTestId(`task-row-${cancelKey}`).locator(".task-row-main").click();
  await detail.getByRole("button", { name: "Cancel task" }).click();
  await page.getByRole("alertdialog").getByRole("button", { name: "Cancel task" }).click({ timeout: 5_000 });
  await expect.poll(async () => ((await taskFromAPI(request, cancelKey)) as { category?: string }).category,
    "step 5b: the cancel lands").toBe("cancelled");
  await detail.getByRole("button", { name: "Close task detail" }).click();
  step("5b moved and cancelled by hand");

  // Step 6: queue settings show the binding and manage secrets.
  await page.getByRole("button", { name: "Queue: all" }).click();
  await page.getByRole("menuitem", { name: "Manage queues…" }).click();
  await page.getByRole("button", { name: "Workflow WFUI" }).click();
  const queueSettings = page.getByRole("region", { name: "Workflow WFUI" });
  await expect(queueSettings.getByTestId("binding")).toContainText(/wfui\s+0\.1\.0\s+[0-9a-f]{12}/);
  const secrets = queueSettings.getByRole("region", { name: "Secrets WFUI" });
  await expect(secrets.getByText("No secrets.")).toBeVisible();
  await secrets.getByLabel("Secret key").fill("GH_TOKEN");
  await secrets.getByLabel("Secret value").fill("ghp_browser-secret-value");
  await secrets.getByRole("button", { name: "Set", exact: true }).click();
  await expect(secrets.getByText("GH_TOKEN", { exact: true }), "step 6: the key is listed").toBeVisible();
  await expect(page.locator("body"), "step 6: no value on the page").not.toContainText("ghp_browser-secret-value");
  await expect(secrets.getByLabel("Secret value")).toHaveValue("");
  // After a reload the key is read back from the daemon, and the value never is.
  await page.reload();
  await expect(page.getByLabel("Search tasks")).toBeVisible();
  await page.getByRole("button", { name: "Queue: all" }).click();
  await page.getByRole("menuitem", { name: "Manage queues…" }).click();
  await page.getByRole("button", { name: "Workflow WFUI" }).click();
  const reloaded = page.getByRole("region", { name: "Workflow WFUI" }).getByRole("region", { name: "Secrets WFUI" });
  await expect(reloaded.getByText("GH_TOKEN", { exact: true }), "step 6: the key is listed after a reload").toBeVisible();
  expect(await page.content(), "step 6: no value in the reloaded page").not.toContain("ghp_browser-secret-value");
  await reloaded.getByRole("button", { name: "Remove GH_TOKEN" }).click();
  await page.getByRole("alertdialog").getByRole("button", { name: "Remove secret" }).click();
  await expect(reloaded.getByText("No secrets."), "step 6: the removed key is gone").toBeVisible();
  step("6 binding and secrets");

  // Step 7: a flexible queue keeps its status select and its assignee control.
  await page.getByRole("button", { name: "Close", exact: true }).click();
  response = await request.post(`${daemonURL}/api/tasks`, { data: {
    queue: "WFLEX", title: "Flexible stays flexible", idempotency_key: "tasks-browser-wfui-flex",
  } });
  expect(response.ok()).toBe(true);
  const flexKey = (await response.json()).result.key as string;
  await page.getByRole("radio", { name: "Active", exact: true }).click();
  await page.getByTestId(`task-row-${flexKey}`).locator(".task-row-main").click();
  const flexDetail = page.locator(".task-detail-panel");
  await expect(flexDetail.getByRole("combobox", { name: "Status", exact: true }), "step 7: flexible task keeps its status select").toBeVisible();
  await expect(flexDetail.getByLabel("Assignee", { exact: true }), "step 7: flexible task keeps its assignee control").toBeVisible();
  // The header menu opens above the detail sheet, so its item takes the click.
  await flexDetail.getByRole("button", { name: "Task actions" }).click();
  await page.getByRole("menuitem", { name: "Move to another server…" }).click({ timeout: 5_000 });
  await expect(page.getByRole("dialog", { name: "Move to another server" }), "step 7: the menu item opens its dialog").toBeVisible();
  step("7 flexible unchanged");
});
