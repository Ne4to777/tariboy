import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useNavigate } from "react-router-dom";
import { DaemonProvider } from "@/components/DaemonProvider";
import { CustomerQuestionNotificationsContext } from "@/components/customerQuestionNotificationsContext";
import { SidebarStateProvider } from "@/pages/terminals/SidebarStateProvider";
import TerminalsPage from "@/pages/terminals/TerminalsPage";
import { fetchAllAgents } from "@/lib/aggregate";
import { setActiveDaemon } from "@/lib/api";
import { addDaemon } from "@/lib/daemons";

vi.mock("@/lib/aggregate", () => ({ fetchAllAgents: vi.fn() }));

interface Call {
  url: string;
  method: string;
  body?: string;
  authorization?: string;
}

const envelope = (result: unknown, ok = true): Response => ({
  ok,
  status: ok ? 200 : 500,
  text: async () => JSON.stringify(ok
    ? { ok: true, result }
    : { ok: false, error: { code: "store_failed", message: String(result) } }),
}) as Response;

function RouteButtons({ hostParam, otherHostParam }: { hostParam: string; otherHostParam?: string }) {
  const navigate = useNavigate();
  return <>
    <button onClick={() => navigate(`/servers/${hostParam}/stores/old`)}>Open old</button>
    <button onClick={() => navigate(`/servers/${otherHostParam ?? hostParam}/stores/new`)}>Open new</button>
    {otherHostParam && <button onClick={() => navigate(`/servers/${otherHostParam}/stores/old`)}>Switch host</button>}
  </>;
}

function renderPage(path: string, hostParam: string, otherHostParam?: string) {
  return render(
    <DaemonProvider>
      <CustomerQuestionNotificationsContext.Provider value={{ attention: new Map(), refreshHost: async () => {} }}>
        <SidebarStateProvider>
          <MemoryRouter initialEntries={[path]}>
            <RouteButtons hostParam={hostParam} otherHostParam={otherHostParam} />
            <Routes>
              <Route path="/servers/:hostId/stores" element={<TerminalsPage serverView="stores" />} />
              <Route path="/servers/:hostId/stores/:name" element={<TerminalsPage serverView="store-detail" />} />
            </Routes>
          </MemoryRouter>
        </SidebarStateProvider>
      </CustomerQuestionNotificationsContext.Provider>
    </DaemonProvider>,
  );
}

beforeEach(() => {
  localStorage.clear();
  // The server rows this page asserts on live in the sidebar's Servers tab.
  localStorage.setItem("terminals:sidebar-tab:v1", "servers");
  sessionStorage.clear();
});

afterEach(() => {
  setActiveDaemon(null);
  vi.restoreAllMocks();
});

describe("Stores workspace", () => {
  it("keeps list and lifecycle actions on the route-selected host", async () => {
    const host = await addDaemon({
      label: "Store host",
      baseURL: "https://store.example",
      token: "store-token",
    });
    vi.mocked(fetchAllAgents).mockResolvedValue([
      { host: { id: "", label: "This daemon (local)" }, agents: [] },
      { host: { id: host.id, label: host.label }, agents: [] },
    ]);
    const calls: Call[] = [];
    let removeAttempts = 0;
    let detailReads = 0;
    let detailReloadFails = false;
    let finishBuild!: (response: Response) => void;
    const buildResponse = new Promise<Response>((resolve) => { finishBuild = resolve; });
    const imageBuilt = vi.fn();
    window.addEventListener("tariboy:image-built", imageBuilt);
    vi.stubGlobal("fetch", vi.fn().mockImplementation((input: RequestInfo | URL, init: RequestInit = {}) => {
      const url = String(input);
      const method = init.method ?? "GET";
      const headers = new Headers(init.headers);
      calls.push({
        url,
        method,
        body: typeof init.body === "string" ? init.body : undefined,
        authorization: headers.get("Authorization") ?? undefined,
      });
      if (url.endsWith("/api/stores") && method === "GET") {
        return Promise.resolve(envelope([{ name: "team", source: "/srv/team", path: "/srv/team" }]));
      }
      if (url.endsWith("/api/stores") && method === "POST") {
        return Promise.resolve(envelope({ name: "design", source: "git@example.com:design.git", path: "/stores/design" }));
      }
      if (url.endsWith("/api/stores/design") && method === "GET") {
        if (detailReloadFails) return Promise.resolve(envelope("reload failed", false));
        detailReads++;
        return Promise.resolve(envelope({
          name: "design",
          source: "git@example.com:design.git",
          path: "/stores/design",
          images: [
            { name: "reviewer", version: "1.2.3", built_version: detailReads === 1 ? "1.0.0" : "1.2.3", update_needed: detailReads === 1, latest_status: "built" },
            { name: "equal", version: "1.0.0", built_version: "1.0.0", update_needed: false, latest_status: "built" },
            { name: "missing-latest", version: "2.0.0", built_version: "", update_needed: false, latest_status: "missing" },
            { name: "source-unversioned", version: "", built_version: "1.0.0", update_needed: false, latest_status: "built" },
            { name: "latest-unversioned", version: "1.0.0", built_version: "", update_needed: false, latest_status: "unversioned" },
            { name: "latest-broken", version: "1.0.0", built_version: "", update_needed: false, latest_status: "error", latest_error: "invalid latest manifest" },
            { name: "broken", version: "", built_version: "", update_needed: false, error: "invalid Tariboyfile", latest_status: "missing" },
          ],
        }));
      }
      if (url.endsWith("/api/stores/design/refresh")) {
        return Promise.resolve(envelope("refresh failed", false));
      }
      if (url.endsWith("/api/images/build")) return buildResponse;
      if (url.endsWith("/api/stores/design") && method === "DELETE") {
        removeAttempts++;
        return Promise.resolve(removeAttempts === 1
          ? envelope("remove failed", false)
          : envelope({ removed: true }));
      }
      return Promise.resolve(envelope({ agents: [], groups: [], count: 0 }));
    }));

    renderPage(`/servers/${encodeURIComponent(host.id)}/stores`, encodeURIComponent(host.id));

    expect(await screen.findByRole("heading", { name: "Stores" })).toBeInTheDocument();
    expect(await screen.findByText("/srv/team")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Images" })).toHaveAttribute(
      "href", `/servers/${encodeURIComponent(host.id)}/images`,
    );
    expect(screen.getByRole("link", { name: "Stores" })).toHaveAttribute(
      "href", `/servers/${encodeURIComponent(host.id)}/stores`,
    );

    fireEvent.change(screen.getByLabelText("Store name"), { target: { value: "design" } });
    fireEvent.change(screen.getByLabelText("Store source"), { target: { value: "git@example.com:design.git" } });
    fireEvent.click(screen.getByRole("button", { name: "Add store" }));

    expect(await screen.findByRole("heading", { name: "design" })).toBeInTheDocument();
    expect(await screen.findByRole("columnheader", { name: "Source image_version" })).toBeInTheDocument();
    expect(screen.getByRole("columnheader", { name: "Latest image_version" })).toBeInTheDocument();
    const reviewerRow = screen.getByText("reviewer").closest("tr");
    expect(reviewerRow).toHaveTextContent("1.0.0");
    expect(reviewerRow).toHaveTextContent("Update needed");
    expect(reviewerRow).toHaveClass("bg-amber-50");
    const equalRow = screen.getByText("equal").closest("tr")!;
    expect(equalRow).toHaveTextContent("Up to date");
    expect(within(equalRow).getByRole("button", { name: "Build equal" })).toBeEnabled();
    const missingLatestRow = screen.getByText("missing-latest").closest("tr")!;
    expect(missingLatestRow).toHaveTextContent("Not built");
    expect(missingLatestRow).toHaveTextContent("Latest not built");
    expect(within(missingLatestRow).getByRole("button", { name: "Build missing-latest" })).toBeEnabled();
    const sourceUnversionedRow = screen.getByText("source-unversioned").closest("tr")!;
    expect(sourceUnversionedRow).toHaveTextContent("Source image_version missing; comparison unavailable");
    expect(sourceUnversionedRow).not.toHaveTextContent("Up to date");
    const latestUnversionedRow = screen.getByText("latest-unversioned").closest("tr")!;
    expect(latestUnversionedRow).toHaveTextContent("Latest image_version missing; comparison unavailable");
    expect(latestUnversionedRow).not.toHaveTextContent("Up to date");
    const latestBrokenRow = screen.getByText("latest-broken").closest("tr")!;
    expect(latestBrokenRow).toHaveTextContent("Latest error: invalid latest manifest");
    expect(within(latestBrokenRow).getByRole("button", { name: "Build latest-broken" })).toBeEnabled();
    const brokenRow = screen.getByText("broken").closest("tr")!;
    expect(brokenRow).toHaveTextContent("Source error: invalid Tariboyfile");
    expect(brokenRow).toHaveTextContent("Latest not built");
    expect(within(brokenRow).getByRole("button", { name: "Build broken" })).toBeDisabled();
    expect(calls).toContainEqual({
      url: "https://store.example/api/stores",
      method: "POST",
      body: JSON.stringify({ name: "design", source: "git@example.com:design.git" }),
      authorization: "Bearer store-token",
    });

    setActiveDaemon({ id: "other", label: "Other", baseURL: "https://other.example", token: "other-token" });
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("refresh failed");
    expect(screen.getByRole("heading", { name: "design" })).toBeInTheDocument();
    expect(calls).toContainEqual(expect.objectContaining({
      url: "https://store.example/api/stores/design/refresh",
      method: "POST",
      authorization: "Bearer store-token",
    }));

    fireEvent.click(screen.getByRole("button", { name: "Build reviewer" }));
    await waitFor(() => expect(calls).toContainEqual({
      url: "https://store.example/api/images/build",
      method: "POST",
      body: JSON.stringify({ source: "design/reviewer" }),
      authorization: "Bearer store-token",
    }));
    expect(screen.getByRole("button", { name: "Building reviewer…" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Refresh" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Remove store" })).toBeDisabled();

    finishBuild(envelope({ name: "reviewer", tag: "1.2.3", digest: "sha256:new", layers: 2 }));
    expect(await screen.findByText("Built reviewer:1.2.3 and reviewer:latest.")).toBeInTheDocument();
    await waitFor(() => expect(detailReads).toBe(2));
    expect(screen.getByText("reviewer").closest("tr")).not.toHaveClass("bg-amber-50");
    expect(screen.getByText("reviewer").closest("tr")).toHaveTextContent("Up to date");
    expect(within(screen.getByText("reviewer").closest("tr")!).getByRole("button", { name: "Build reviewer" })).toBeEnabled();
    expect(imageBuilt).toHaveBeenCalledOnce();

    detailReloadFails = true;
    fireEvent.click(screen.getByRole("button", { name: "Build reviewer" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("reload failed");
    expect(screen.getByText("Built reviewer:1.2.3 and reviewer:latest.")).toBeInTheDocument();
    expect(imageBuilt).toHaveBeenCalledTimes(2);

    fireEvent.click(screen.getByRole("button", { name: "Remove store" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText(/preserves local sources and built images/i)).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove store" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("remove failed");
    expect(within(dialog).getByRole("button", { name: "Remove store" })).toBeEnabled();
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove store" }));
    await waitFor(() => expect(calls).toContainEqual(expect.objectContaining({
      url: "https://store.example/api/stores/design",
      method: "DELETE",
      authorization: "Bearer store-token",
    })));
    expect(await screen.findByRole("heading", { name: "Stores" })).toBeInTheDocument();

    window.removeEventListener("tariboy:image-built", imageBuilt);
  });

  it("discards a detail response after the route selects another Store", async () => {
    const host = await addDaemon({
      label: "Store host",
      baseURL: "https://stale.example",
      token: "stale-token",
    });
    vi.mocked(fetchAllAgents).mockResolvedValue([
      { host: { id: "", label: "This daemon (local)" }, agents: [] },
      { host: { id: host.id, label: host.label }, agents: [] },
    ]);
    let finishOld!: (response: Response) => void;
    const oldResponse = new Promise<Response>((resolve) => { finishOld = resolve; });
    const fetchMock = vi.fn().mockImplementation((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.endsWith("/api/stores/old")) return oldResponse;
      if (url.endsWith("/api/stores/new")) return Promise.resolve(envelope({
        name: "new", source: "/srv/new", path: "/srv/new", images: [],
      }));
      return Promise.resolve(envelope({ agents: [], groups: [], count: 0 }));
    });
    vi.stubGlobal("fetch", fetchMock);
    const hostParam = encodeURIComponent(host.id);
    renderPage(`/servers/${hostParam}/stores/old`, hostParam);

    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith(
      "https://stale.example/api/stores/old",
      expect.any(Object),
    ));
    fireEvent.click(screen.getByRole("button", { name: "Open new" }));
    expect(await screen.findByRole("heading", { name: "new" })).toBeInTheDocument();
    finishOld(envelope({ name: "old", source: "/srv/old", path: "/srv/old", images: [] }));

    await waitFor(() => expect(screen.queryByRole("heading", { name: "old" })).not.toBeInTheDocument());
    expect(screen.getByRole("heading", { name: "new" })).toBeInTheDocument();
  });

  it("isolates late builds by host and supports an explicit target ref", async () => {
    const host = await addDaemon({
      label: "Old Store host",
      baseURL: "https://old-build.example",
      token: "old-build-token",
    });
    const newHost = await addDaemon({
      label: "New Store host",
      baseURL: "https://new-build.example",
      token: "new-build-token",
    });
    vi.mocked(fetchAllAgents).mockResolvedValue([
      { host: { id: "", label: "This daemon (local)" }, agents: [] },
      { host: { id: host.id, label: host.label }, agents: [] },
      { host: { id: newHost.id, label: newHost.label }, agents: [] },
    ]);
    let finishOld!: (response: Response) => void;
    let finishNewFailure!: (response: Response) => void;
    let finishNewSuccess!: (response: Response) => void;
    let finishThirdFailure!: (response: Response) => void;
    let finishNewReload!: (response: Response) => void;
    const oldBuild = new Promise<Response>((resolve) => { finishOld = resolve; });
    const newFailure = new Promise<Response>((resolve) => { finishNewFailure = resolve; });
    const newSuccess = new Promise<Response>((resolve) => { finishNewSuccess = resolve; });
    const thirdFailure = new Promise<Response>((resolve) => { finishThirdFailure = resolve; });
    const newReload = new Promise<Response>((resolve) => { finishNewReload = resolve; });
    let newAttempts = 0;
    let newReads = 0;
    const calls: Call[] = [];
    vi.stubGlobal("fetch", vi.fn().mockImplementation((input: RequestInfo | URL, init: RequestInit = {}) => {
      const url = String(input);
      const method = init.method ?? "GET";
      const body = typeof init.body === "string" ? init.body : undefined;
      calls.push({ url, method, body });
      if (url.startsWith("https://old-build.example") && url.endsWith("/api/stores/old")) return Promise.resolve(envelope({
      name: "old", source: "/srv/old", path: "/srv/old",
      images: [{ name: "old-image", version: "1.0.0", built_version: "", update_needed: false, latest_status: "missing" }],
      }));
      if (url.startsWith("https://new-build.example") && url.endsWith("/api/stores/old")) {
      newReads++;
      if (newReads > 1) return newReload;
      return Promise.resolve(envelope({
        name: "old", source: "/srv/new", path: "/srv/new",
        images: [{ name: "new-image", version: "2.0.0", built_version: "", update_needed: false, latest_status: "missing" }],
      }));
      }
      if (url.endsWith("/api/images/build")) {
      const source = JSON.parse(body ?? "{}").source;
      if (source === "old/old-image") return oldBuild;
      newAttempts++;
      return newAttempts === 1 ? newFailure : newAttempts === 2 ? newSuccess : thirdFailure;
      }
      return Promise.resolve(envelope({ agents: [], groups: [], count: 0 }));
    }));
    const hostParam = encodeURIComponent(host.id);
    const newHostParam = encodeURIComponent(newHost.id);
    renderPage(`/servers/${hostParam}/stores/old`, hostParam, newHostParam);

    fireEvent.click(await screen.findByRole("button", { name: "Build old-image" }));
    expect(screen.getByRole("button", { name: "Building old-image…" })).toBeDisabled();
    fireEvent.click(screen.getByRole("button", { name: "Switch host" }));
    expect(await screen.findByRole("button", { name: "Build new-image" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Open server New Store host" })).toHaveAttribute("aria-current", "page");
    expect(await screen.findByRole("button", { name: "Build new-image" })).toBeEnabled();

    expect(screen.queryByLabelText("Target image name")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Target image tag")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Build new-image" }));
    expect(screen.getByRole("button", { name: "Building new-image…" })).toBeDisabled();
    expect(calls).toContainEqual(expect.objectContaining({
      url: "https://new-build.example/api/images/build",
      method: "POST",
      body: JSON.stringify({ source: "old/new-image" }),
    }));

    finishOld(envelope({ name: "old-image", tag: "1.0.0", digest: "sha256:old", layers: 1 }));
    await waitFor(() => expect(screen.queryByText(/Built old-image/)).not.toBeInTheDocument());
    expect(screen.getByRole("heading", { name: "old" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Building new-image…" })).toBeDisabled();

    finishNewFailure(envelope("immutable target", false));
    expect(await screen.findByRole("alert")).toHaveTextContent("immutable target");
    expect(screen.getByRole("button", { name: "Build new-image" })).toBeEnabled();

    fireEvent.click(screen.getByRole("button", { name: "Build new-image" }));
    finishNewSuccess(envelope({ name: "new-image", tag: "2.0.0", digest: "sha256:new", layers: 1 }));
    expect(await screen.findByText("Built new-image:2.0.0 and new-image:latest.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Build new-image" })).toBeEnabled();
    fireEvent.click(screen.getByRole("button", { name: "Build new-image" }));
    finishNewReload(envelope({ name: "old", source: "/srv/new", path: "/srv/new", images: [] }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Building new-image…" })).toBeDisabled());
    expect(screen.getByText("new-image")).toBeInTheDocument();
    finishThirdFailure(envelope("third build failed", false));
    expect(await screen.findByRole("alert")).toHaveTextContent("third build failed");
    expect(screen.getByRole("button", { name: "Build new-image" })).toBeEnabled();
  });

  it("keeps Refresh ownership when build inventory finishes late", async () => {
    const host = await addDaemon({
      label: "Store host",
      baseURL: "https://refresh-build.example",
      token: "refresh-build-token",
    });
    vi.mocked(fetchAllAgents).mockResolvedValue([
      { host: { id: "", label: "This daemon (local)" }, agents: [] },
      { host: { id: host.id, label: host.label }, agents: [] },
    ]);
    let finishStaleRead!: (response: Response) => void;
    let finishRefresh!: (response: Response) => void;
    const staleRead = new Promise<Response>((resolve) => { finishStaleRead = resolve; });
    const refreshResponse = new Promise<Response>((resolve) => { finishRefresh = resolve; });
    let detailReads = 0;
    vi.stubGlobal("fetch", vi.fn().mockImplementation((input: RequestInfo | URL, init: RequestInit = {}) => {
      const url = String(input);
      const method = init.method ?? "GET";
      if (url.endsWith("/api/stores/team") && method === "GET") {
        detailReads++;
        if (detailReads > 1) return staleRead;
        return Promise.resolve(envelope({
          name: "team", source: "/srv/team", path: "/srv/team",
          images: [{ name: "reviewer", version: "1.0.0", built_version: "", update_needed: false, latest_status: "missing" }],
        }));
      }
      if (url.endsWith("/api/images/build")) {
        return Promise.resolve(envelope({ name: "reviewer", tag: "1.0.0", digest: "sha256:built", layers: 1 }));
      }
      if (url.endsWith("/api/stores/team/refresh")) return refreshResponse;
      return Promise.resolve(envelope({ agents: [], groups: [], count: 0 }));
    }));
    const hostParam = encodeURIComponent(host.id);
    renderPage(`/servers/${hostParam}/stores/team`, hostParam);

    fireEvent.click(await screen.findByRole("button", { name: "Build reviewer" }));
    expect(await screen.findByText("Built reviewer:1.0.0 and reviewer:latest.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Build reviewer" })).toBeEnabled();
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    expect(screen.getByRole("button", { name: "Refreshing…" })).toBeDisabled();

    finishStaleRead(envelope("stale inventory failed", false));
    await waitFor(() => expect(screen.queryByText("stale inventory failed")).not.toBeInTheDocument());
    expect(screen.getByRole("button", { name: "Refreshing…" })).toBeDisabled();

    finishRefresh(envelope({
      name: "team", source: "/srv/team", path: "/srv/team",
      images: [{ name: "fresh", version: "2.0.0", built_version: "2.0.0", update_needed: false, latest_status: "built" }],
    }));
    expect(await screen.findByText("Refreshed team.")).toBeInTheDocument();
    expect(screen.getByText("fresh")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Refresh" })).toBeEnabled();
  });
});

describe("Store automatic builds", () => {
  it("loads the saved policy and posts the edited interval and image selection", async () => {
    vi.mocked(fetchAllAgents).mockResolvedValue([
      { host: { id: "", label: "This daemon (local)" }, agents: [] },
    ]);
    const calls: Call[] = [];
    const images = [
      { name: "reviewer", version: "1.2.3", built_version: "1.0.0", update_needed: true, latest_status: "built" },
      { name: "writer", version: "1.0.0", built_version: "1.0.0", update_needed: false, latest_status: "built" },
    ];
    vi.stubGlobal("fetch", vi.fn().mockImplementation((input: RequestInfo | URL, init: RequestInit = {}) => {
      const url = String(input);
      const method = init.method ?? "GET";
      calls.push({ url, method, body: typeof init.body === "string" ? init.body : undefined });
      if (url.endsWith("/api/stores/old/auto") && method === "POST") {
        return Promise.resolve(envelope({
          name: "old",
          source: "/srv/old",
          path: "/srv/old",
          images,
          auto: { interval_minutes: 30, images: ["reviewer", "writer"] },
        }));
      }
      if (url.endsWith("/api/stores/old") && method === "GET") {
        return Promise.resolve(envelope({
          name: "old",
          source: "/srv/old",
          path: "/srv/old",
          images,
          auto: { interval_minutes: 15, images: ["reviewer"] },
        }));
      }
      return Promise.resolve(envelope({ agents: [], groups: [], count: 0 }));
    }));

    renderPage("/servers/local/stores/old", "local");

    const interval = await screen.findByLabelText("Automatic build interval");
    await waitFor(() => expect(interval).toHaveValue(15));
    expect(screen.getByLabelText("Automatic builds for reviewer")).toBeChecked();
    expect(screen.getByLabelText("Automatic builds for writer")).not.toBeChecked();

    fireEvent.click(screen.getByLabelText("Automatic builds for writer"));
    fireEvent.change(interval, { target: { value: "30" } });
    fireEvent.click(screen.getByRole("button", { name: "Save automatic builds" }));

    await waitFor(() => expect(calls.some((call) => call.url.endsWith("/api/stores/old/auto"))).toBe(true));
    const saved = calls.find((call) => call.url.endsWith("/api/stores/old/auto"));
    expect(saved?.method).toBe("POST");
    expect(JSON.parse(saved?.body ?? "{}")).toEqual({ interval: 30, image: ["reviewer", "writer"] });
    expect(await screen.findByText("Automatic builds run every 30 min for 2 image(s).")).toBeInTheDocument();
  });
});

describe("Store workflow images", () => {
  it("lists workflow sources in their own tab and builds one from the Store", async () => {
    vi.mocked(fetchAllAgents).mockResolvedValue([
      { host: { id: "", label: "This daemon (local)" }, agents: [] },
    ]);
    const calls: Call[] = [];
    let built = false;
    let buildFails = true;
    const workflowBuilt = vi.fn();
    window.addEventListener("tariboy:workflow-image-built", workflowBuilt);
    vi.stubGlobal("fetch", vi.fn().mockImplementation((input: RequestInfo | URL, init: RequestInit = {}) => {
      const url = String(input);
      const method = init.method ?? "GET";
      calls.push({ url, method, body: typeof init.body === "string" ? init.body : undefined });
      if (url.endsWith("/api/stores/old") && method === "GET") {
        return Promise.resolve(envelope({
          name: "old",
          source: "/srv/old",
          path: "/srv/old",
          images: [{ name: "reviewer", version: "1.0.0", built_version: "1.0.0", update_needed: false, latest_status: "built" }],
          workflows: [
            { name: "development", version: "0.3.0" },
            { name: "equal", version: "1.0.0" },
            { name: "fresh", version: "0.1.0" },
            { name: "broken", version: "", error: "invalid Workflowfile.yaml" },
          ],
        }));
      }
      if (url.endsWith("/api/workflow-images") && method === "GET") {
        return Promise.resolve(envelope({
          workflows: [
            { name: "development", tag: "latest", version: built ? "0.3.0" : "0.2.0", digest: "d1", built_at: "" },
            { name: "development", tag: "0.2.0", version: "0.2.0", digest: "d1", built_at: "" },
            { name: "equal", tag: "latest", version: "1.0.0", digest: "d2", built_at: "" },
          ],
          count: 3,
        }));
      }
      if (url.endsWith("/api/workflow-images/build") && method === "POST") {
        if (buildFails) {
          buildFails = false;
          return Promise.resolve(envelope("workflow build failed", false));
        }
        built = true;
        return Promise.resolve(envelope({ name: "development", version: "0.3.0", digest: "d3", tags: ["0.3.0", "latest"], created: true }));
      }
      return Promise.resolve(envelope({ agents: [], groups: [], count: 0 }));
    }));

    renderPage("/servers/local/stores/old", "local");

    expect(await screen.findByRole("button", { name: "Build reviewer" })).toBeInTheDocument();
    fireEvent.mouseDown(screen.getByRole("tab", { name: "Workflow images" }));
    const row = async (workflow: string) => (await screen.findByText(workflow, { selector: "td" })).closest("tr") as HTMLElement;
    expect(screen.queryByRole("button", { name: "Build reviewer" })).not.toBeInTheDocument();
    await waitFor(async () => expect(within(await row("development")).getByText("Update needed")).toBeInTheDocument());
    expect(within(await row("development")).getByText("0.2.0")).toBeInTheDocument();
    expect(within(await row("equal")).getByText("Up to date")).toBeInTheDocument();
    expect(within(await row("fresh")).getByText("Latest not built")).toBeInTheDocument();
    expect(within(await row("broken")).getByText("Source error: invalid Workflowfile.yaml")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Build broken" })).toBeDisabled();

    fireEvent.click(screen.getByRole("button", { name: "Build development" }));
    expect(await screen.findByText("workflow build failed")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Build development" }));
    expect(await screen.findByText("Built development 0.3.0 (tags: 0.3.0, latest).")).toBeInTheDocument();
    const build = calls.filter((call) => call.url.endsWith("/api/workflow-images/build"));
    expect(JSON.parse(build[1]?.body ?? "{}")).toEqual({ source: "old/development" });
    await waitFor(async () => expect(within(await row("development")).getByText("Up to date")).toBeInTheDocument());
    expect(workflowBuilt).toHaveBeenCalledTimes(1);
    window.removeEventListener("tariboy:workflow-image-built", workflowBuilt);
  });

  it("keeps workflow builds available when built images cannot be compared", async () => {
    vi.mocked(fetchAllAgents).mockResolvedValue([
      { host: { id: "", label: "This daemon (local)" }, agents: [] },
    ]);
    vi.stubGlobal("fetch", vi.fn().mockImplementation((input: RequestInfo | URL, init: RequestInit = {}) => {
      const url = String(input);
      const method = init.method ?? "GET";
      if (url.endsWith("/api/stores/old") && method === "GET") {
        return Promise.resolve(envelope({
          name: "old", source: "/srv/old", path: "/srv/old", images: [], workflows: [{ name: "development", version: "0.3.0" }],
        }));
      }
      if (url.endsWith("/api/workflow-images")) return Promise.resolve(envelope("registry down", false));
      return Promise.resolve(envelope({ agents: [], groups: [], count: 0 }));
    }));

    renderPage("/servers/local/stores/old", "local");

    fireEvent.mouseDown(await screen.findByRole("tab", { name: "Workflow images" }));
    expect(await screen.findByText("Comparison unavailable")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Build development" })).toBeEnabled();
  });
});
