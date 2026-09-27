import type { ComponentProps } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { CreateAgentDialog } from "./CreateAgentDialog";
import {
  agentGetOn,
  createAgent,
  imageManifestGet,
  listImages,
  startAgent,
  type ImageManifest,
} from "@/lib/api";
import type { AgentView } from "@/lib/types";
import { targetFor } from "@/lib/terminalsHost";
import { resolveDaemon } from "@/lib/daemons";
import { RUNTIME_PRESETS_STORAGE_KEY } from "@/lib/runtimePresets";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    agentGetOn: vi.fn(),
    createAgent: vi.fn(),
    imageManifestGet: vi.fn(),
    listImages: vi.fn(),
    startAgent: vi.fn(),
  };
});

vi.mock("@/lib/daemons", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/daemons")>();
  return { ...actual, resolveDaemon: vi.fn() };
});

vi.mock("@/components/PathAutocomplete", () => ({
  PathAutocomplete: ({
    value,
    onChange,
    id,
    daemon,
    "aria-label": ariaLabel,
  }: {
    value: string;
    onChange: (value: string) => void;
    id?: string;
    daemon?: { id?: string } | null;
    "aria-label"?: string;
  }) => (
    <input
      id={id}
      aria-label={ariaLabel}
      data-host={daemon?.id ?? "local"}
      value={value}
      onChange={(event) => onChange(event.target.value)}
    />
  ),
}));

const hosts = [
  { id: "", label: "local" },
  { id: "d1", label: "prod" },
];

const manifest = (overrides: Partial<ImageManifest> = {}): ImageManifest => ({
  schema_version: 1,
  name: "worker",
  tag: "v1",
  built_at: "2026-07-29T00:00:00Z",
  parents: null,
  plugins: null,
  requires_secrets: null,
  harness: {
    type: "codex",
    model: "o3",
    effort: "high",
    interactive: false,
  },
  env: null,
  policy: {},
  layers: null,
  ...overrides,
  skills: overrides.skills ?? null,
});

const cloneSource: AgentView = {
  name: "source",
  image: "worker:v1",
  digest: "sha256",
  state: "stopped",
  cwd: "/managed/source/workdir",
  configured_cwd: "",
  harness: "codex",
  model: "gpt-5",
  effort: "high",
  interactive: true,
  loop_enabled: false,
  enabled: false,
  interval_s: 12,
  timeout_s: 34,
  hard_timeout_s: 56,
  on_timeout: "stop",
  on_error: "restart",
  max_idle_iterations: 7,
  ai_stall_timeout_s: 420,
  user_prompt: "standing prompt",
  env: { CSV: "a,b", EQ: "a=b", LINES: "one\ntwo" },
  plugins: ["context", "custom"],
  messages_batch: 8,
  messages_max_queue: 900,
  group: "reviewers",
  alias: "Source alias",
  notes: "source notes",
  color: "#123abc",
  goal_enabled: false,
  goal_wait_customer_timeout_s: 120,
  goal_delivery_cooldown_s: 60,
  current_goal_task_key: "TARI-43",
};

const completeOrdinarySpec = (overrides: Record<string, unknown> = {}) => ({
  image: "worker:v1",
  cwd: "",
  harness: "codex",
  model: "o3",
  effort: "high",
  interactive: false,
  loop: true,
  env: {},
  plugins: [],
  interval_s: 0,
  timeout_s: 7200,
  hard_timeout_s: 10800,
  on_timeout: "restart",
  on_error: "restart",
  max_idle_iterations: 0,
  ai_stall_timeout_s: 1800,
  user_prompt: "",
  messages_batch: 10,
  messages_max_queue: 100,
  group: "",
  alias: "",
  notes: "",
  color: "",
  goal_enabled: true,
  goal_wait_customer_timeout_s: 300,
  goal_delivery_cooldown_s: 60,
  ...overrides,
});

function renderDialog(
  props: Partial<ComponentProps<typeof CreateAgentDialog>> = {},
) {
  const onCreated = vi.fn();
  const onOpenChange = vi.fn();
  const view = render(
    <CreateAgentDialog
      open
      hostId="d1"
      imageRef="worker:v1"
      hosts={hosts}
      onOpenChange={onOpenChange}
      onCreated={onCreated}
      {...props}
    />,
  );
  return { ...view, onCreated, onOpenChange };
}

async function setSwitch(name: string, value: boolean) {
  const control = await screen.findByRole("switch", { name });
  if (control.getAttribute("aria-checked") === String(!value))
    fireEvent.click(control);
}

async function selectHarness(value: string) {
  const control = screen.getByRole("combobox", { name: "harness" });
  await waitFor(() => expect(control).toBeEnabled());
  fireEvent.change(control, { target: { value } });
  expect(control).toHaveValue(value);
}

beforeEach(() => {
  vi.clearAllMocks();
  localStorage.clear();
  vi.mocked(resolveDaemon).mockImplementation(async (id) => targetFor(id));
  vi.mocked(listImages).mockResolvedValue({
    images: [
      { name: "worker", tag: "v1", bare: false },
      { name: "bare", tag: "latest", bare: true },
    ],
    count: 2,
  });
  vi.mocked(imageManifestGet).mockResolvedValue(manifest());
  vi.mocked(agentGetOn).mockResolvedValue(cloneSource);
  vi.mocked(createAgent).mockResolvedValue({
    name: "created",
    state: "stopped",
  });
  vi.mocked(startAgent).mockResolvedValue({ name: "created", action: "start" });
});

afterEach(() => vi.restoreAllMocks());

it("loads a complete clone draft from the explicit source host and submits every field", async () => {
  const { onCreated } = renderDialog({
    imageRef: undefined,
    cloneSource: { hostId: "d1", agentName: "source", hostLabel: "prod" },
  });

  expect(
    await screen.findByRole("heading", { name: "Clone agent" }),
  ).toBeInTheDocument();
  expect(screen.getByText(/source.*prod/i)).toBeInTheDocument();
  expect(agentGetOn).toHaveBeenCalledWith(targetFor("d1"), "source", "");
  expect(screen.getByLabelText("name")).toHaveValue("");
  expect(screen.getByLabelText("cwd")).toHaveValue("");
  await waitFor(() =>
    expect(screen.getByLabelText("alias")).toHaveValue("Source alias"),
  );
  expect(screen.getByLabelText("group")).toHaveValue("reviewers");
  expect(screen.getByLabelText("color")).toHaveValue("#123abc");
  expect(screen.getByLabelText("notes")).toHaveValue("source notes");
  expect(screen.getByLabelText("model")).toHaveValue("gpt-5");
  expect(screen.getByLabelText("environment JSON")).toHaveValue(
    '{\n  "CSV": "a,b",\n  "EQ": "a=b",\n  "LINES": "one\\ntwo"\n}',
  );
  expect(screen.getByText("context")).toBeInTheDocument();
  expect(screen.getByLabelText("interval seconds")).toHaveValue(12);
  expect(screen.getByLabelText("soft timeout seconds")).toHaveValue(34);
  expect(screen.getByLabelText("hard timeout seconds")).toHaveValue(56);
  expect(screen.getByLabelText("timeout policy")).toHaveValue("stop");
  expect(screen.getByLabelText("error policy")).toHaveValue("restart");
  expect(screen.getByLabelText("maximum idle iterations")).toHaveValue(7);
  expect(screen.getByLabelText("standing user prompt")).toHaveValue(
    "standing prompt",
  );
  expect(screen.getByLabelText("message batch size")).toHaveValue(8);
  expect(screen.getByLabelText("maximum queued messages")).toHaveValue(900);
  expect(screen.getByRole("switch", { name: "Goal" })).not.toBeChecked();
  expect(screen.getByLabelText("wait customer timeout seconds")).toHaveValue(
    120,
  );
  expect(screen.getByRole("switch", { name: "Interactive" })).toBeChecked();
  expect(screen.getByRole("switch", { name: "Autopilot" })).not.toBeChecked();
  expect(screen.getByRole("switch", { name: "Start now" })).not.toBeChecked();

  fireEvent.change(screen.getByLabelText("name"), {
    target: { value: "copy" },
  });
  const create = screen.getByRole("button", { name: "Create agent" });
  await waitFor(() => expect(create).toBeEnabled());
  fireEvent.click(create);

  await waitFor(() => expect(createAgent).toHaveBeenCalledOnce());
  expect(createAgent).toHaveBeenCalledWith(
    {
      image: "worker:v1",
      name: "copy",
      cwd: "",
      harness: "codex",
      model: "gpt-5",
      effort: "high",
      interactive: true,
      loop: false,
      env: { CSV: "a,b", EQ: "a=b", LINES: "one\ntwo" },
      plugins: ["context", "custom"],
      interval_s: 12,
      timeout_s: 34,
      hard_timeout_s: 56,
      on_timeout: "stop",
      on_error: "restart",
      max_idle_iterations: 7,
      ai_stall_timeout_s: 420,
      user_prompt: "standing prompt",
      messages_batch: 8,
      messages_max_queue: 900,
      group: "reviewers",
      alias: "Source alias",
      notes: "source notes",
      color: "#123abc",
      goal_enabled: false,
      goal_wait_customer_timeout_s: 120,
      goal_delivery_cooldown_s: 60,
    },
    targetFor("d1"),
  );
  expect(startAgent).not.toHaveBeenCalled();
  expect(onCreated).toHaveBeenCalledWith("d1", "created");
});

it("retries a failed source load without allowing a partial clone", async () => {
  vi.mocked(agentGetOn)
    .mockRejectedValueOnce(new Error("source unavailable"))
    .mockResolvedValueOnce(cloneSource);
  renderDialog({
    imageRef: undefined,
    cloneSource: { hostId: "d1", agentName: "source", hostLabel: "prod" },
  });

  expect(await screen.findByRole("alert")).toHaveTextContent(
    "source unavailable",
  );
  expect(screen.getByRole("button", { name: "Create agent" })).toBeDisabled();
  expect(createAgent).not.toHaveBeenCalled();

  fireEvent.click(screen.getByRole("button", { name: "Retry source" }));

  await waitFor(() => expect(agentGetOn).toHaveBeenCalledTimes(2));
  expect(await screen.findByLabelText("alias")).toHaveValue("Source alias");
  await waitFor(() =>
    expect(screen.getByRole("button", { name: "Create agent" })).toBeEnabled(),
  );
});

it("requires a daemon with the complete clone projection", async () => {
  const oldProjection: AgentView = { ...cloneSource };
  delete oldProjection.configured_cwd;
  vi.mocked(agentGetOn).mockResolvedValueOnce(oldProjection);
  renderDialog({
    imageRef: undefined,
    cloneSource: { hostId: "d1", agentName: "source", hostLabel: "prod" },
  });

  expect(await screen.findByRole("alert")).toHaveTextContent(
    /update.*host.*complete clone/i,
  );
  expect(screen.getByRole("button", { name: "Create agent" })).toBeDisabled();
  expect(createAgent).not.toHaveBeenCalled();
});

it("retains clone drafts while target host and image readiness change", async () => {
  const source = { ...cloneSource, interactive: false, loop_enabled: true };
  vi.mocked(agentGetOn).mockResolvedValueOnce(source);
  renderDialog({
    imageRef: undefined,
    hosts: [...hosts, { id: "d2", label: "staging" }],
    cloneSource: { hostId: "d1", agentName: "source", hostLabel: "prod" },
  });

  expect(await screen.findByLabelText("alias")).toHaveValue("Source alias");
  fireEvent.change(screen.getByLabelText("model"), {
    target: { value: "clone-model" },
  });

  fireEvent.click(screen.getByRole("combobox", { name: "Host" }));
  fireEvent.click(await screen.findByRole("option", { name: "staging" }));
  expect(screen.getByRole("button", { name: "Create agent" })).toBeDisabled();
  await waitFor(() =>
    expect(imageManifestGet).toHaveBeenLastCalledWith(
      "worker:v1",
      targetFor("d2"),
    ),
  );
  expect(screen.getByLabelText("alias")).toHaveValue("Source alias");
  expect(screen.getByLabelText("model")).toHaveValue("clone-model");

  vi.mocked(imageManifestGet).mockResolvedValueOnce(
    manifest({
      name: "bare",
      tag: "latest",
      bare: true,
      harness: { type: "codex", interactive: false },
    }),
  );
  fireEvent.focus(screen.getByLabelText("image"));
  fireEvent.click(await screen.findByRole("option", { name: "bare:latest" }));
  expect(screen.getByRole("button", { name: "Create agent" })).toBeDisabled();
  expect(await screen.findByText("Terminal only")).toBeInTheDocument();
  expect(screen.getByRole("switch", { name: "Interactive" })).toBeChecked();
  expect(screen.getByRole("switch", { name: "Autopilot" })).not.toBeChecked();
  expect(screen.getByLabelText("alias")).toHaveValue("Source alias");
  expect(screen.getByLabelText("model")).toHaveValue("clone-model");

  vi.mocked(imageManifestGet).mockResolvedValueOnce(manifest());
  fireEvent.focus(screen.getByLabelText("image"));
  fireEvent.click(await screen.findByRole("option", { name: "worker:v1" }));
  await waitFor(() =>
    expect(screen.queryByText("Terminal only")).not.toBeInTheDocument(),
  );
  expect(screen.getByRole("switch", { name: "Interactive" })).not.toBeChecked();
  expect(screen.getByRole("switch", { name: "Autopilot" })).toBeChecked();
});

it("keeps schema-v1 plugins editable and makes schema-v2 plugins image-owned", async () => {
  vi.mocked(listImages).mockResolvedValue({
    images: [
      { name: "worker", tag: "v1", bare: false },
      { name: "worker", tag: "v2", bare: false },
    ],
    count: 2,
  });
  vi.mocked(imageManifestGet).mockImplementation(async (ref) =>
    ref === "worker:v2"
      ? manifest({
          schema_version: 2,
          tag: "v2",
          plugins: [{ name: "image-plugin" }],
        })
      : manifest(),
  );
  renderDialog({
    imageRef: undefined,
    cloneSource: { hostId: "d1", agentName: "source", hostLabel: "prod" },
  });

  const pluginInput = await screen.findByRole("textbox", {
    name: "plugin name",
  });
  fireEvent.change(pluginInput, { target: { value: "draft-plugin" } });
  fireEvent.click(screen.getByRole("button", { name: "Add plugin" }));
  expect(screen.getByText("draft-plugin")).toBeInTheDocument();

  fireEvent.focus(screen.getByLabelText("image"));
  fireEvent.click(await screen.findByRole("option", { name: "worker:v2" }));
  expect(await screen.findByText("image-plugin")).toBeInTheDocument();
  expect(
    screen.getByText(/plugins are owned by this schema-v2 image/i),
  ).toBeInTheDocument();
  expect(
    screen.queryByRole("textbox", { name: "plugin name" }),
  ).not.toBeInTheDocument();
  expect(screen.queryByText("draft-plugin")).not.toBeInTheDocument();

  fireEvent.change(screen.getByLabelText("name"), {
    target: { value: "copy" },
  });
  await waitFor(() =>
    expect(screen.getByRole("button", { name: "Create agent" })).toBeEnabled(),
  );
  fireEvent.click(screen.getByRole("button", { name: "Create agent" }));
  await waitFor(() => expect(createAgent).toHaveBeenCalledOnce());
  expect(createAgent).toHaveBeenCalledWith(
    expect.not.objectContaining({ plugins: expect.anything() }),
    targetFor("d1"),
  );
});

it("resets Interactive to the schema-v2 default after a bare image was selected", async () => {
  vi.mocked(listImages).mockResolvedValue({
    images: [
      { name: "bare", tag: "latest", bare: true },
      { name: "worker", tag: "v2", bare: false },
    ],
    count: 2,
  });
  vi.mocked(imageManifestGet).mockImplementation(async (ref) =>
    ref === "worker:v2"
      ? manifest({ schema_version: 2, tag: "v2" })
      : manifest({
          name: "bare",
          tag: "latest",
          bare: true,
          harness: { type: "codex", interactive: true },
        }),
  );
  renderDialog({ imageRef: "bare:latest" });
  expect(await screen.findByText("Terminal only")).toBeInTheDocument();
  expect(screen.getByRole("switch", { name: "Interactive" })).toBeChecked();

  fireEvent.focus(screen.getByLabelText("image"));
  fireEvent.click(await screen.findByRole("option", { name: "worker:v2" }));
  await waitFor(() =>
    expect(screen.queryByText("Terminal only")).not.toBeInTheDocument(),
  );
  expect(screen.getByRole("switch", { name: "Interactive" })).not.toBeChecked();
});

it("rejects invalid environment JSON before creation", async () => {
  renderDialog();
  await screen.findByDisplayValue("o3");
  fireEvent.change(screen.getByLabelText("environment JSON"), {
    target: { value: '{"COUNT": 3}' },
  });
  fireEvent.click(screen.getByRole("button", { name: "Create agent" }));

  expect(await screen.findByRole("alert")).toHaveTextContent(
    "Environment must be a JSON object whose values are strings",
  );
  expect(createAgent).not.toHaveBeenCalled();
});

it("rejects non-integral numeric configuration before creation", async () => {
  renderDialog();
  await screen.findByDisplayValue("o3");
  fireEvent.change(screen.getByLabelText("interval seconds"), {
    target: { value: "1.5" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Create agent" }));

  expect(await screen.findByRole("alert")).toHaveTextContent(
    "Interval seconds must be a whole number of at least 0",
  );
  expect(createAgent).not.toHaveBeenCalled();
});

describe.each([
  { startNow: false, interactive: false, autopilot: false },
  { startNow: false, interactive: false, autopilot: true },
  { startNow: false, interactive: true, autopilot: false },
  { startNow: false, interactive: true, autopilot: true },
  { startNow: true, interactive: false, autopilot: false },
  { startNow: true, interactive: false, autopilot: true },
  { startNow: true, interactive: true, autopilot: false },
  { startNow: true, interactive: true, autopilot: true },
])(
  "creation matrix $startNow/$interactive/$autopilot",
  ({ startNow, interactive, autopilot }) => {
    it("creates stopped with explicit independent controls and starts only on request", async () => {
      const { onCreated } = renderDialog();
      await screen.findByDisplayValue("worker:v1");
      await waitFor(() =>
        expect(
          screen.getByRole("button", { name: "Create agent" }),
        ).toBeEnabled(),
      );
      await setSwitch("Start now", startNow);
      await setSwitch("Interactive", interactive);
      await setSwitch("Autopilot", autopilot);

      fireEvent.click(screen.getByRole("button", { name: "Create agent" }));

      await waitFor(() => expect(createAgent).toHaveBeenCalledOnce());
      expect(createAgent).toHaveBeenCalledWith(
        completeOrdinarySpec({ interactive, loop: autopilot }),
        targetFor("d1"),
      );
      if (startNow) {
        await waitFor(() =>
          expect(startAgent).toHaveBeenCalledWith("created", targetFor("d1")),
        );
      } else {
        expect(startAgent).not.toHaveBeenCalled();
      }
      expect(onCreated).toHaveBeenCalledWith("d1", "created");
    });
  },
);

it("uses a newly selected host for image/default loading, cwd, creation, and start", async () => {
  renderDialog();

  expect(await screen.findByLabelText("cwd")).toHaveAttribute(
    "data-host",
    "d1",
  );
  expect(listImages).toHaveBeenCalledWith(targetFor("d1"));
  expect(imageManifestGet).toHaveBeenCalledWith("worker:v1", targetFor("d1"));

  fireEvent.click(screen.getByRole("combobox", { name: "Host" }));
  fireEvent.click(await screen.findByRole("option", { name: "local" }));
  await waitFor(() => expect(listImages).toHaveBeenLastCalledWith(null));
  expect(screen.getByLabelText("cwd")).toHaveAttribute("data-host", "local");
  expect(screen.getByLabelText("image")).toHaveValue("");

  fireEvent.focus(screen.getByLabelText("image"));
  fireEvent.click(await screen.findByRole("option", { name: "worker:v1" }));
  await waitFor(() =>
    expect(imageManifestGet).toHaveBeenLastCalledWith("worker:v1", null),
  );
  await waitFor(() =>
    expect(screen.getByRole("button", { name: "Create agent" })).toBeEnabled(),
  );
  fireEvent.click(screen.getByRole("button", { name: "Create agent" }));
  await waitFor(() => expect(createAgent).toHaveBeenCalled());
  expect(createAgent).toHaveBeenCalledWith(expect.any(Object), null);
  expect(startAgent).toHaveBeenCalledWith("created", null);
});

it("retries a cold remote Run Agent selection when the host becomes ready", async () => {
  vi.mocked(resolveDaemon)
    .mockRejectedValueOnce(new Error("host d1 is not ready"))
    .mockResolvedValueOnce(targetFor("d1"));
  const props: ComponentProps<typeof CreateAgentDialog> = {
    open: true,
    hostId: "d1",
    imageRef: "worker:v1",
    hosts: [{ id: "d1", label: "prod", revision: "pending" }],
    onOpenChange: vi.fn(),
    onCreated: vi.fn(),
  };
  const view = render(<CreateAgentDialog {...props} />);

  expect(await screen.findByRole("alert")).toHaveTextContent(
    /host d1 is not ready/i,
  );
  expect(screen.getByRole("button", { name: "Create agent" })).toBeDisabled();

  view.rerender(
    <CreateAgentDialog
      {...props}
      hosts={[
        { id: "d1", label: "prod", revision: "ready:http://127.0.0.1:19000" },
      ]}
    />,
  );

  await waitFor(() => expect(resolveDaemon).toHaveBeenCalledTimes(2));
  expect(listImages).toHaveBeenLastCalledWith(targetFor("d1"));
  expect(imageManifestGet).toHaveBeenLastCalledWith(
    "worker:v1",
    targetFor("d1"),
  );
  await waitFor(() =>
    expect(screen.getByRole("button", { name: "Create agent" })).toBeEnabled(),
  );
});

it("shows image defaults as editable values and submits the complete defaults", async () => {
  renderDialog();
  await screen.findByDisplayValue("o3");

  expect(screen.getByLabelText("harness")).toHaveValue("codex");
  expect(screen.getByLabelText("model")).toHaveValue("o3");
  expect(screen.getByLabelText("effort")).toHaveValue("high");
  fireEvent.focus(screen.getByLabelText("model"));
  expect(screen.getByRole("option", { name: "gpt-5" })).toBeVisible();
  expect(screen.getByRole("option", { name: "o3" })).toBeVisible();
  fireEvent.blur(screen.getByLabelText("model"));

  expect(screen.getByLabelText("environment JSON")).toHaveValue("{}");

  fireEvent.click(screen.getByRole("button", { name: "Create agent" }));
  await waitFor(() => expect(createAgent).toHaveBeenCalled());
  expect(createAgent).toHaveBeenCalledWith(
    completeOrdinarySpec(),
    targetFor("d1"),
  );
});

it("submits edited runtime values with the complete configuration", async () => {
  renderDialog();
  await screen.findByDisplayValue("o3");
  await selectHarness("claude");
  fireEvent.change(screen.getByLabelText("model"), {
    target: { value: "opus" },
  });
  fireEvent.change(screen.getByLabelText("effort"), {
    target: { value: "medium" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Create agent" }));

  await waitFor(() => expect(createAgent).toHaveBeenCalled());
  expect(createAgent).toHaveBeenCalledWith(
    completeOrdinarySpec({
      harness: "claude",
      model: "opus",
      effort: "medium",
    }),
    targetFor("d1"),
  );
});

it("changes suggestions with harness without clearing a custom model", async () => {
  renderDialog();
  await screen.findByDisplayValue("o3");
  fireEvent.change(screen.getByLabelText("model"), {
    target: { value: "private-model" },
  });

  await selectHarness("claude");

  expect(screen.getByLabelText("model")).toHaveValue("private-model");
  fireEvent.focus(screen.getByLabelText("model"));
  expect(screen.getByRole("option", { name: "claude-opus-4-8" })).toBeVisible();
  expect(screen.getByRole("option", { name: "private-model" })).toBeVisible();
  expect(
    screen.queryByRole("option", { name: "gpt-5" }),
  ).not.toBeInTheDocument();
});

it("keeps runtime drafts when the same selected image is reloaded", async () => {
  const { rerender } = renderDialog({
    hosts: [
      { id: "", label: "local" },
      { id: "d1", label: "prod", revision: "initial" },
    ],
  });
  await screen.findByDisplayValue("o3");
  fireEvent.change(screen.getByLabelText("model"), {
    target: { value: "private-model" },
  });
  fireEvent.change(screen.getByLabelText("effort"), {
    target: { value: "ultra-custom" },
  });

  rerender(
    <CreateAgentDialog
      open
      hostId="d1"
      imageRef="worker:v1"
      hosts={[
        { id: "", label: "local" },
        { id: "d1", label: "prod", revision: "ready" },
      ]}
      onOpenChange={vi.fn()}
      onCreated={vi.fn()}
    />,
  );

  await waitFor(() => expect(imageManifestGet).toHaveBeenCalledTimes(2));
  expect(screen.getByLabelText("model")).toHaveValue("private-model");
  expect(screen.getByLabelText("effort")).toHaveValue("ultra-custom");
});

it("initializes defaults again when the same image is selected on another host", async () => {
  vi.mocked(imageManifestGet)
    .mockResolvedValueOnce(manifest())
    .mockResolvedValueOnce(
      manifest({
        harness: {
          type: "claude",
          model: "claude-opus-4-8",
          effort: "max",
          interactive: true,
        },
      }),
    );
  renderDialog({
    hosts: [
      { id: "", label: "local" },
      { id: "d1", label: "prod" },
      { id: "d2", label: "staging" },
    ],
  });
  await screen.findByDisplayValue("o3");
  fireEvent.change(screen.getByLabelText("model"), {
    target: { value: "private-model" },
  });

  fireEvent.click(screen.getByRole("combobox", { name: "Host" }));
  fireEvent.click(await screen.findByRole("option", { name: "staging" }));
  await waitFor(() => expect(screen.getByLabelText("image")).toHaveValue(""));
  fireEvent.focus(screen.getByLabelText("image"));
  fireEvent.click(await screen.findByRole("option", { name: "worker:v1" }));

  expect(await screen.findByLabelText("harness")).toHaveValue("claude");
  expect(screen.getByLabelText("model")).toHaveValue("claude-opus-4-8");
  expect(screen.getByLabelText("effort")).toHaveValue("max");
});

it("remembers unknown model and effort after successful creation", async () => {
  renderDialog();
  await screen.findByDisplayValue("o3");
  fireEvent.change(screen.getByLabelText("model"), {
    target: { value: "private-model" },
  });
  fireEvent.change(screen.getByLabelText("effort"), {
    target: { value: "ultra-custom" },
  });

  fireEvent.click(screen.getByRole("button", { name: "Create agent" }));
  await waitFor(() => expect(createAgent).toHaveBeenCalledOnce());

  expect(
    JSON.parse(localStorage.getItem(RUNTIME_PRESETS_STORAGE_KEY) ?? "{}"),
  ).toEqual({
    codex: {
      models: ["private-model"],
      efforts: ["ultra-custom"],
    },
  });
});

it("does not remember custom runtime values when creation fails", async () => {
  vi.mocked(createAgent).mockRejectedValueOnce(new Error("rejected"));
  renderDialog();
  await screen.findByDisplayValue("o3");
  fireEvent.change(screen.getByLabelText("model"), {
    target: { value: "private-model" },
  });
  fireEvent.change(screen.getByLabelText("effort"), {
    target: { value: "ultra-custom" },
  });

  fireEvent.click(screen.getByRole("button", { name: "Create agent" }));

  expect(await screen.findByRole("alert")).toHaveTextContent("rejected");
  expect(localStorage.getItem(RUNTIME_PRESETS_STORAGE_KEY)).toBeNull();
});

describe.each([{ startNow: false }, { startNow: true }])(
  "bare terminal session with startNow=$startNow",
  ({ startNow }) => {
    it("keeps bare selected, locks its modes, and supports stopped or started creation", async () => {
      vi.mocked(imageManifestGet).mockResolvedValue(
        manifest({
          name: "bare",
          tag: "latest",
          bare: true,
          harness: { type: "claude", interactive: false },
        }),
      );
      renderDialog({ imageRef: "bare:latest" });

      expect(await screen.findByText("Terminal only")).toBeInTheDocument();
      expect(screen.getByLabelText("image")).toHaveValue("bare:latest");
      expect(screen.getByRole("switch", { name: "Interactive" })).toBeChecked();
      expect(
        screen.getByRole("switch", { name: "Interactive" }),
      ).toBeDisabled();
      expect(
        screen.getByRole("switch", { name: "Autopilot" }),
      ).not.toBeChecked();
      expect(screen.getByRole("switch", { name: "Autopilot" })).toBeDisabled();
      expect(
        screen.getByText(/does not contain agent instructions or tools/i),
      ).toBeInTheDocument();
      await setSwitch("Start now", startNow);

      fireEvent.click(screen.getByRole("button", { name: "Create agent" }));
      await waitFor(() => expect(createAgent).toHaveBeenCalled());
      expect(createAgent).toHaveBeenCalledWith(
        completeOrdinarySpec({
          image: "bare:latest",
          harness: "claude",
          model: "",
          effort: "",
          interactive: true,
          loop: false,
        }),
        targetFor("d1"),
      );
      if (startNow) {
        expect(startAgent).toHaveBeenCalledWith("created", targetFor("d1"));
      } else {
        expect(startAgent).not.toHaveBeenCalled();
      }
    });
  },
);

it("keeps the created stopped agent and retries only start after a start failure", async () => {
  vi.mocked(startAgent)
    .mockRejectedValueOnce(new Error("daemon unavailable"))
    .mockResolvedValueOnce({ name: "created", action: "start" });
  const { onCreated, onOpenChange } = renderDialog();
  await screen.findByDisplayValue("o3");
  fireEvent.change(screen.getByLabelText("model"), {
    target: { value: "private-model" },
  });

  const create = screen.getByRole("button", { name: "Create agent" });
  await waitFor(() => expect(create).toBeEnabled());
  fireEvent.click(create);

  expect(await screen.findByRole("alert")).toHaveTextContent(
    /agent created but could not be started.*daemon unavailable/i,
  );
  expect(onCreated).toHaveBeenCalledWith("d1", "created");
  expect(onOpenChange).not.toHaveBeenCalledWith(false);
  expect(
    JSON.parse(localStorage.getItem(RUNTIME_PRESETS_STORAGE_KEY) ?? "{}"),
  ).toMatchObject({ codex: { models: ["private-model"] } });

  fireEvent.click(screen.getByRole("button", { name: "Retry start" }));
  await waitFor(() => expect(startAgent).toHaveBeenCalledTimes(2));
  expect(createAgent).toHaveBeenCalledOnce();
  expect(onOpenChange).toHaveBeenCalledWith(false);
});
