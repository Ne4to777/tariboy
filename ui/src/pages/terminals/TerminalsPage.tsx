import { useCallback, useContext, useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { useNavigate, useParams, useSearchParams } from "react-router-dom";
import { usePolling } from "@/hooks/usePolling";
import { fetchAllAgents, type HostAgents } from "@/lib/aggregate";
import { paramToHost, hostToParam, serverPath, targetFor } from "@/lib/terminalsHost";
import { AgentSidebar } from "./sidebar/AgentSidebar";
import { useSharedSidebarState } from "./sidebarStateContext";
import { CreateAgentDialog, type CloneAgentSource } from "./CreateAgentDialog";
import { ServerDialog } from "./ServerDialog";
import { useDaemons } from "@/components/DaemonProvider";
import { listDaemons, removeDaemon, resolveDaemon, type DaemonMeta } from "@/lib/daemons";
import { apiOn } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { hostConnect, hostUpdate } from "@/lib/desktop";
import AgentWorkspace from "@/pages/agents/AgentWorkspace";
import { ServerContextBar } from "./ServerContextBar";
import { RouteHostBoundary } from "./RouteHostBoundary";
import TasksWorkspace from "@/pages/tasks/TasksWorkspace";
import AllTasksWorkspace from "@/pages/tasks/AllTasksWorkspace";
import ImagesPage from "@/pages/ImagesPage";
import ImageTags from "@/pages/images/ImageTags";
import StoresPage from "@/pages/StoresPage";
import { ImageLayout } from "@/components/ImageLayout";
import SettingsPage from "@/pages/settings/SettingsPage";
import { useCustomerQuestionNotifications } from "@/components/customerQuestionNotificationsContext";
import { MessagesLiveRefresh } from "./MessagesLiveRefresh";
import { TitlebarSlotContext } from "./titlebarSlotContext";
import { WorkspaceManager, WorkspaceSwitcher, type WorkspaceHost } from "./HostWorkspaces";
import { useWorkspaces, workspaceOf } from "@/lib/workspaces";
import { defaultAgentTab } from "@/lib/uiMode";

type ServerDialogState = { mode: "add" } | { mode: "edit"; server: DaemonMeta };
type CreateDialogState = {
  hostId: string;
  imageRef?: string;
  cloneSource?: CloneAgentSource;
};
export type ServerView = "tasks" | "images" | "image-tags" | "image-detail" | "stores" | "store-detail" | "settings";

export default function TerminalsPage({ serverView }: { serverView?: ServerView }) {
  const { hostId: hostParam, agent: agentName, tab: agentTab, team: teamName, name: storeName } = useParams();
  const navigate = useNavigate();
  const [searchParams] = useSearchParams();
  const allTasks = searchParams.get("view") === "all";
  const [lastSuccessfulHostsById, setLastSuccessfulHostsById] = useState<Map<string, HostAgents>>(
    () => new Map(),
  );
  const fetchHosts = useCallback(async () => {
    const data = await fetchAllAgents();
    setLastSuccessfulHostsById((previous) => {
      const next = new Map(previous);
      for (const entry of data) {
        if (!entry.error) next.set(entry.host.id, entry);
      }
      return next;
    });
    return data;
  }, []);
  const { data, refresh } = usePolling(fetchHosts, 3000);
  const hosts = useMemo(() => data ?? [], [data]);
  const hostId = hostParam !== undefined ? paramToHost(hostParam) : undefined;
  const [createFor, setCreateFor] = useState<CreateDialogState | null>(() => {
    const imageRef = searchParams.get("image");
    if (!imageRef && searchParams.get("new") !== "1") return null;
    return {
      hostId: searchParams.get("host") ?? "",
      imageRef: imageRef ?? undefined,
    };
  });
  const [serverDialog, setServerDialog] = useState<ServerDialogState | null>(null);
  // DaemonProvider caches the registry in React state and is the only thing
  // that pushes the active daemon into the api client, so registry edits made
  // here have to notify it too — otherwise the rest of the SPA keeps using a
  // stale (or removed) host as its target.
  const { daemons, appVersion, select: selectDaemon, refresh: refreshDaemons } = useDaemons();
  const { attention, refreshHost } = useCustomerQuestionNotifications();
  const sidebar = useSharedSidebarState();
  const workspaces = useWorkspaces();
  const titlebarSlot = useContext(TitlebarSlotContext);
  const [workspaceManager, setWorkspaceManager] = useState<{ id: string; focusNew: boolean } | null>(null);
  const [hostError, setHostError] = useState("");
  const [serverOrder, setServerOrder] = useState<string[]>(() => {
    try {
      const parsed = JSON.parse(localStorage.getItem("terminals:server-order:v1") ?? "null") as {
        version?: number;
        ids?: unknown;
      } | null;
      return parsed?.version === 1 && Array.isArray(parsed.ids)
        ? parsed.ids.filter((id): id is string => typeof id === "string")
        : [];
    } catch {
      return [];
    }
  });
  const [sidebarOrderOverrides, setSidebarOrderOverrides] = useState(
    () => new Map<string, NonNullable<HostAgents["sidebarOrder"]>>(),
  );
  const sidebarSaveQueues = useRef(new Map<string, Promise<unknown>>());
  const sidebarSaveVersions = useRef(new Map<string, number>());
  const sidebarAcknowledgedOrders = useRef(
    new Map<string, NonNullable<HostAgents["sidebarOrder"]>>(),
  );
  const createHosts = useMemo(() => {
    const aggregateById = new Map(hosts.map((entry) => [entry.host.id, entry]));
    const metadataById = new Map(daemons.map((entry) => [entry.id, entry]));
    const ids = new Set([
      "",
      ...hosts.map((entry) => entry.host.id),
      ...daemons.map((entry) => entry.id),
    ]);
    return [...ids].map((id) => {
      const aggregate = aggregateById.get(id);
      const metadata = metadataById.get(id);
      return {
        id,
        label:
          aggregate?.host.label
          ?? metadata?.label
          ?? (id === "" ? "This daemon (local)" : id),
        revision: [
          aggregate ? aggregate.error ?? "reachable" : "pending",
          metadata?.state ?? "",
          metadata?.baseURL ?? "",
        ].join("\u0000"),
      };
    });
  }, [daemons, hosts]);

  const workspaceHosts = useMemo<WorkspaceHost[]>(() => createHosts.map((host) => {
    const aggregate = hosts.find((entry) => entry.host.id === host.id);
    const state = daemons.find((entry) => entry.id === host.id)?.state ?? "ready";
    return { id: host.id, label: host.label, ready: Boolean(aggregate && !aggregate.error) && state === "ready" };
  }), [createHosts, daemons, hosts]);

  // Re-read after any registry mutation: refresh() runs one immediate
  // fetchAllAgents() tick, which resolves every daemon straight from storage,
  // so labels/reachability update without waiting up to 3s for the next poll.
  const registryChanged = async () => {
    refresh();
    await refreshDaemons();
  };

  const connectHost = async (id: string) => {
    setHostError("");
    try {
      await hostConnect(id);
      await refreshDaemons();
    } catch (cause) {
      const label = daemons.find((host) => host.id === id)?.label ?? id;
      setHostError(`Could not connect to ${label}: ${String(cause)}`);
    }
  };

  const editServer = async (id: string) => {
    const server = (await listDaemons()).find((m) => m.id === id);
    if (server) setServerDialog({ mode: "edit", server });
  };

  // Every host behind the app version, updated in turn by the same native
  // command the server dialog's Update button runs. One failure is reported
  // and the remaining hosts still go.
  const updateAllServers = async (ids: string[]) => {
    const labelOf = (id: string) => daemons.find((host) => host.id === id)?.label ?? id;
    if (!window.confirm(
      `Update ${ids.length} server(s) to ${appVersion}? ${ids.map(labelOf).join(", ")}`,
    )) return;
    setHostError("");
    const failures: string[] = [];
    for (const id of ids) {
      try {
        await hostUpdate(id);
      } catch (cause) {
        failures.push(`${labelOf(id)}: ${String(cause)}`);
      }
    }
    if (failures.length > 0) setHostError(`Could not update ${failures.join("; ")}`);
    await registryChanged();
  };

  const removeServer = async (id: string) => {
    const server = (await listDaemons()).find((m) => m.id === id);
    if (!window.confirm(
      `Remove host ${server?.label ?? id} from this app? The remote daemon and data remain on the server.`,
    )) return;
    await removeDaemon(id);
    // Leave a route whose host no longer exists. targetFor() also fails closed,
    // but navigating away gives the user a useful screen instead of a dead pane.
    if (hostId === id) navigate("/");
    await registryChanged();
  };

  const liveSelectedHost = useMemo(() => {
    if (hostId === undefined) return undefined;
    return hosts.find((h) => h.host.id === hostId);
  }, [hosts, hostId]);
  const selectedHost = useMemo(() => {
    if (hostId === undefined) return undefined;
    if (liveSelectedHost && !liveSelectedHost.error) return liveSelectedHost;
    return lastSuccessfulHostsById.get(hostId) ?? liveSelectedHost;
  }, [hostId, lastSuccessfulHostsById, liveSelectedHost]);
  const sidebarHosts = useMemo(() => hosts.map((entry) => {
    if (!entry.error) return entry;
    const snapshot = lastSuccessfulHostsById.get(entry.host.id);
    return snapshot ? { ...snapshot, error: entry.error } : entry;
  }), [hosts, lastSuccessfulHostsById]);
  // Every host in server order; the sidebar shows the current workspace's.
  const rankedHosts = useMemo(() => {
    const rank = new Map(serverOrder.map((id, index) => [id, index]));
    return sidebarHosts
      .map((entry, index) => ({ entry, index, rank: rank.get(entry.host.id) }))
      .sort((left, right) => (left.rank ?? serverOrder.length + left.index) - (right.rank ?? serverOrder.length + right.index))
      .map(({ entry }) => ({
        ...entry,
        sidebarOrder: sidebarOrderOverrides.get(entry.host.id) ?? entry.sidebarOrder,
      }));
  }, [serverOrder, sidebarHosts, sidebarOrderOverrides]);
  const orderedSidebarHosts = useMemo(
    () => rankedHosts.filter((entry) => workspaceOf(workspaces, entry.host.id) === workspaces.active),
    [rankedHosts, workspaces],
  );
  const activeWorkspaceName = workspaces.workspaces.find((w) => w.id === workspaces.active)?.name ?? "";
  const workspaceEmpty = orderedSidebarHosts.every((entry) => entry.agents.length === 0);

  // A route on a host outside the chosen workspace moves to that workspace's
  // first agent, in the same tab, or to the empty console when it has none.
  const followWorkspace = (id: string) => {
    if (hostId === undefined || workspaceOf(workspaces, hostId) === id) return;
    const first = rankedHosts.find((entry) =>
      workspaceOf(workspaces, entry.host.id) === id && entry.agents.length > 0);
    navigate(first
      ? `/agents/${hostToParam(first.host.id)}/${encodeURIComponent(first.agents[0].name)}/${agentName && agentTab ? agentTab : defaultAgentTab()}${allTasks ? "?view=all" : ""}`
      : `/${allTasks ? "?view=all" : ""}`);
  };
  const selectedAgent = useMemo(() => {
    if (!agentName) return undefined;
    return selectedHost?.agents.find((a) => a.name === agentName);
  }, [selectedHost, agentName]);
  const serverBasePath = hostId === undefined
    ? ""
    : `/servers/${encodeURIComponent(hostToParam(hostId))}`;
  const selectedHostLabel = selectedHost?.host.label
    ?? daemons.find((daemon) => daemon.id === hostId)?.label
    ?? (hostId === "" ? "This daemon (local)" : hostId);
  const routeUnavailable = Boolean(liveSelectedHost?.error);

  const reorderSidebar = async (kind: "servers" | "groups" | "agents", id: string, ids: string[]) => {
    setHostError("");
    if (kind === "servers") {
      // The sidebar reorders only the visible workspace's hosts: they take
      // back the slots they held, so hidden hosts keep their places.
      const visible = new Set(ids);
      let next = 0;
      ids = rankedHosts.map((entry) => entry.host.id)
        .map((id) => visible.has(id) ? ids[next++] : id);
      const previous = serverOrder;
      setServerOrder(ids);
      try {
        localStorage.setItem("terminals:server-order:v1", JSON.stringify({ version: 1, ids }));
      } catch (cause) {
        setServerOrder(previous);
        setHostError(`Could not save server order: ${String(cause)}`);
      }
      return;
    }
    const host = orderedSidebarHosts.find((entry) => entry.host.id === id);
    if (!host) return;
    const previous = host.sidebarOrder ?? { version: 1, groups: [], agents: [] };
    const next = { ...previous, [kind]: ids };
    if (!sidebarSaveQueues.current.has(id)) sidebarAcknowledgedOrders.current.set(id, previous);
    const version = (sidebarSaveVersions.current.get(id) ?? 0) + 1;
    sidebarSaveVersions.current.set(id, version);
    setSidebarOrderOverrides((current) => new Map(current).set(id, next));
    const save = (sidebarSaveQueues.current.get(id) ?? Promise.resolve())
      .catch(() => undefined)
      .then(async () => apiOn(await resolveDaemon(id), "POST", "/api/daemon/config", {
        key: "sidebar_order_v1",
        value: JSON.stringify(next),
      }));
    sidebarSaveQueues.current.set(id, save);
    try {
      await save;
      sidebarAcknowledgedOrders.current.set(id, next);
      if (sidebarSaveVersions.current.get(id) === version) {
        await refresh();
        setSidebarOrderOverrides((current) => {
          const copy = new Map(current);
          copy.delete(id);
          return copy;
        });
      }
    } catch (cause) {
      if (sidebarSaveVersions.current.get(id) === version) {
        setSidebarOrderOverrides((current) => {
          const copy = new Map(current);
          copy.set(id, sidebarAcknowledgedOrders.current.get(id) ?? previous);
          return copy;
        });
        setHostError(`Could not save ${kind} order: ${String(cause)}`);
      }
    } finally {
      if (sidebarSaveQueues.current.get(id) === save) sidebarSaveQueues.current.delete(id);
    }
  };

  return (
    // Console body: the chrome shows through as an 8px gutter around the
    // content island, so neither the sidebar nor the island needs a border.
    <div className="flex h-full px-2 pb-2">
      {/* Chat order is live: a published message refreshes the authoritative
          snapshot at once rather than waiting for the next poll. */}
      <MessagesLiveRefresh hostIds={hosts.map((entry) => entry.host.id)} onChange={refresh} />
      {/* All tasks spans every server, so no agent is the subject there. */}
      {!sidebar.hidden && !allTasks && <AgentSidebar
        hosts={orderedSidebarHosts}
        selectedHostId={hostId}
        selected={hostId !== undefined && agentName ? { hostId, agent: agentName } : undefined}
        onSelectHost={(id) => navigate(serverPath(id, "tasks"))}
        onSelect={(h, a) => {
          // Switching agents keeps the tab the operator is reading, so the same
          // view answers the same question about the next agent. Any query
          // (a selected task) belongs to the agent being left, so it is dropped.
          navigate(`/agents/${hostToParam(h)}/${encodeURIComponent(a)}/${agentName && agentTab ? agentTab : defaultAgentTab()}${allTasks ? "?view=all" : ""}`);
        }}
        onSelectTeam={(h, team) => {
          navigate(`/agents/${hostToParam(h)}/teams/${encodeURIComponent(team)}`);
        }}
        onReorder={(kind, id, ids) => void reorderSidebar(kind, id, ids)}
        onClone={(cloneHostId, cloneAgentName) => {
          const hostLabel = sidebarHosts.find((entry) => entry.host.id === cloneHostId)?.host.label
            ?? (cloneHostId === "" ? "This daemon (local)" : cloneHostId);
          setCreateFor({
            hostId: cloneHostId,
            cloneSource: { hostId: cloneHostId, agentName: cloneAgentName, hostLabel },
          });
        }}
        onCreate={(hostId) => setCreateFor({ hostId })}
        onAddServer={() => setServerDialog({ mode: "add" })}
        onEditServer={(id) => void editServer(id)}
        onUpdateAll={(ids) => void updateAllServers(ids)}
        onRemoveServer={(id) => void removeServer(id)}
        daemonViews={daemons}
        appVersion={appVersion}
        onConnectHost={(id) => void connectHost(id)}
        attention={attention}
        width={sidebar.width}
        onResize={sidebar.setWidth}
      />}
      {/* The content island: --card on the --background chrome, one radius and
          one of the two allowed shadows. Everything the operator works with
          lives inside it. */}
      <main className="flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden rounded-[var(--panel-radius)] bg-card shadow-[var(--lift)]">
        {!allTasks && hostId !== undefined && (
          <ServerContextBar hostId={hostId} label={selectedHostLabel ?? "Unknown server"} />
        )}
        <div className={`flex min-h-0 flex-1 flex-col${serverView || allTasks ? "" : " p-3"}`}>
        {hostError && (
          <p role="alert" className="mb-2 text-sm text-destructive">{hostError}</p>
        )}
        <section className="min-h-0 flex-1">
        {allTasks ? (
          <AllTasksWorkspace hosts={hosts} />
        ) : serverView === "tasks" && hostId !== undefined ? (
          <RouteHostBoundary hostId={hostId} unavailable={routeUnavailable}>
            <TasksWorkspace
              target={targetFor(hostId)}
              initialTaskKey={searchParams.get("task") ?? undefined}
              onNotificationsChanged={() => void refreshHost(hostId)}
            />
          </RouteHostBoundary>
        ) : serverView === "images" && hostId !== undefined ? (
          <RouteHostBoundary hostId={hostId} unavailable={routeUnavailable}>
            <ImagesPage hostId={hostId} basePath={`${serverBasePath}/images`} />
          </RouteHostBoundary>
        ) : serverView === "image-tags" && hostId !== undefined ? (
          <RouteHostBoundary hostId={hostId} unavailable={routeUnavailable}>
            <ImageTags hostId={hostId} basePath={`${serverBasePath}/images`} />
          </RouteHostBoundary>
        ) : serverView === "image-detail" && hostId !== undefined ? (
          <RouteHostBoundary hostId={hostId} unavailable={routeUnavailable}>
            <ImageLayout hostId={hostId} basePath={`${serverBasePath}/images`} />
          </RouteHostBoundary>
        ) : (serverView === "stores" || serverView === "store-detail") && hostId !== undefined ? (
          <RouteHostBoundary hostId={hostId} unavailable={routeUnavailable}>
            {(target) => <StoresPage
              key={`${hostId}\u0000${storeName ?? ""}\u0000${target?.baseURL ?? "local"}`}
              target={target}
              name={storeName}
              basePath={`${serverBasePath}/stores`}
            />}
          </RouteHostBoundary>
        ) : serverView === "settings" && hostId !== undefined ? (
          <RouteHostBoundary hostId={hostId} unavailable={routeUnavailable}>
            {(target) => <SettingsPage basePath={`${serverBasePath}/settings`} target={target} />}
          </RouteHostBoundary>
        ) : teamName && selectedHost && hostId !== undefined ? (
          <RouteHostBoundary hostId={hostId} unavailable={routeUnavailable}>
            <TeamWorkspace
              name={teamName}
              hostLabel={selectedHost.host.label}
              members={selectedHost.agents.filter((agent) => agent.group === teamName)}
              onOpen={(agent) => navigate(`/agents/${hostToParam(hostId)}/${encodeURIComponent(agent)}/${defaultAgentTab()}`)}
              onManage={() => void selectDaemon(hostId).then((selected) => { if (selected) navigate(`/settings/advanced/groups?team=${encodeURIComponent(teamName)}`); })}
            />
          </RouteHostBoundary>
        ) : selectedAgent && hostId !== undefined ? (
          <AgentWorkspace
            key={`${hostId}\u0000${selectedAgent.name}`}
            hostId={hostId}
            hostLabel={selectedHost?.host.label}
            agent={selectedAgent}
            refresh={refresh}
            unavailable={routeUnavailable}
          />
        ) : hostId !== undefined && agentName ? (
          <div className="flex h-full flex-col items-center justify-center gap-2 text-muted-foreground">
            <p>Host or agent unavailable.</p>
            <p className="text-xs">The route was kept unchanged so it cannot silently target the local daemon.</p>
          </div>
        ) : workspaceEmpty && workspaces.workspaces.length > 1 ? (
          <div className="flex h-full flex-col items-center justify-center gap-3 text-muted-foreground">
            <p>{activeWorkspaceName} has no agents yet</p>
            <Button onClick={() => setWorkspaceManager({ id: workspaces.active, focusNew: false })}>
              Manage workspaces
            </Button>
          </div>
        ) : (
          <div className="flex h-full flex-col items-center justify-center gap-3 text-muted-foreground">
            <p>Pick an agent on the left, or create one.</p>
            <Button onClick={() => setCreateFor({ hostId: "" })}>New agent</Button>
          </div>
        )}
        </section>
        </div>
      </main>
      <CreateAgentDialog
        open={createFor !== null}
        hostId={createFor?.hostId ?? ""}
        imageRef={createFor?.imageRef}
        cloneSource={createFor?.cloneSource}
        hosts={createHosts}
        onOpenChange={(o) => { if (!o) setCreateFor(null); }}
        onCreated={(h, name) => {
          // usePolling exposes `refresh` (its tick fn) — call it so the new
          // agent shows up immediately instead of waiting up to 3s for the
          // next poll tick.
          refresh();
          navigate(`/agents/${hostToParam(h)}/${encodeURIComponent(name)}/${defaultAgentTab()}`);
        }}
      />
      {titlebarSlot && createPortal(
        <WorkspaceSwitcher
          hosts={workspaceHosts}
          onSelect={followWorkspace}
          onManage={(focusNew) => setWorkspaceManager({ id: workspaces.active, focusNew })}
        />,
        titlebarSlot,
      )}
      <WorkspaceManager
        hosts={workspaceHosts}
        open={workspaceManager !== null}
        initialId={workspaceManager?.id ?? workspaces.active}
        focusNew={workspaceManager?.focusNew ?? false}
        onOpenChange={(open) => { if (!open) setWorkspaceManager(null); }}
      />
      <ServerDialog
        open={serverDialog !== null}
        server={serverDialog?.mode === "edit" ? serverDialog.server : undefined}
        onOpenChange={(o) => { if (!o) setServerDialog(null); }}
        onSaved={registryChanged}
      />
    </div>
  );
}

function TeamWorkspace({ name, hostLabel, members, onOpen, onManage }: {
  name: string;
  hostLabel: string;
  members: Array<{ name: string; image: string; state: string; interactive?: boolean }>;
  onOpen: (agent: string) => void;
  onManage: () => void;
}) {
  return <div className="space-y-4 p-3">
    <div className="flex items-center justify-between">
      <div><h1 className="text-xl font-semibold">{name}</h1><p className="text-sm text-muted-foreground">Team on {hostLabel}</p></div>
      <Button variant="outline" onClick={onManage}>Manage team</Button>
    </div>
    <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
      {members.map((member) => <button key={member.name} type="button" aria-label={`Open team member ${member.name}`}
        onClick={() => onOpen(member.name)} className="rounded-lg border p-3 text-left hover:bg-accent">
        <div className="font-medium">{member.name}</div>
        <div className="text-xs text-muted-foreground">{member.image} · {member.state}{member.interactive === false ? " · non-tty" : ""}</div>
      </button>)}
    </div>
    {members.length === 0 && <p className="text-sm text-muted-foreground">No members are currently assigned.</p>}
  </div>;
}
