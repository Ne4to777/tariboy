import { useState } from "react";
import { Link, Navigate, Route, Routes, useLocation, useOutletContext, useParams, useSearchParams } from "react-router-dom";
import { Segmented } from "@/components/ui/segmented";
import { Toaster } from "@/components/ui/sonner";
import { ThemeToggle } from "@/components/ThemeToggle";
import { DaemonBanner } from "@/components/DaemonBanner";
import { DaemonProvider, useDaemons } from "@/components/DaemonProvider";
import { Button } from "@/components/ui/button";
import { PanelLeftClose, PanelLeftOpen, Settings } from "lucide-react";
import ImageOverview from "@/pages/ImageOverview";
import ImageTemplate from "@/pages/images/ImageTemplate";
import ImageSkills from "@/pages/images/ImageSkills";
import ImageFiles from "@/pages/ImageFiles";
import TerminalsPage from "@/pages/terminals/TerminalsPage";
import {
  AdvancedSettingsIndex,
  AppearanceSettings,
  CliSettings,
  GeneralSettings,
} from "@/pages/settings/SettingsPage";
import PluginSettings from "@/pages/settings/PluginSettings";
import UsagePage from "@/pages/UsagePage";
import GroupsPage from "@/pages/GroupsPage";
import BudgetsPage from "@/pages/BudgetsPage";
import PluginsPage from "@/pages/PluginsPage";
import OpsPage from "@/pages/OpsPage";
import ChannelsPage from "@/pages/ChannelsPage";
import DaemonsPage from "@/pages/DaemonsPage";
import { SidebarStateProvider } from "@/pages/terminals/SidebarStateProvider";
import { useSharedSidebarState } from "@/pages/terminals/sidebarStateContext";
import { TitlebarSlotContext } from "@/pages/terminals/titlebarSlotContext";
import { hostToParam } from "@/lib/terminalsHost";
import { defaultAgentTab } from "@/lib/uiMode";
import { CustomerQuestionNotifications } from "@/components/CustomerQuestionNotifications";
import type { ApiTarget } from "@/lib/api";
import {
  AppSettings,
  DesktopUpdatesProvider,
  UpdateBanner,
} from "@/components/DesktopUpdates";

export default function App() {
  return (
    <DaemonProvider>
      <CustomerQuestionNotifications>
        <SidebarStateProvider>
          <DesktopUpdatesProvider>
            <div className="flex h-screen flex-col">
              <MainApp />
            </div>
          </DesktopUpdatesProvider>
        </SidebarStateProvider>
      </CustomerQuestionNotifications>
      <Toaster richColors />
    </DaemonProvider>
  );
}

const VIEW_OPTIONS = [
  { value: "agents", label: "Agents" },
  { value: "all", label: "All tasks" },
] as const;

function MainApp() {
  const location = useLocation();
  const [searchParams, setSearchParams] = useSearchParams();
  const sidebar = useSharedSidebarState();
  const [titlebarSlot, setTitlebarSlot] = useState<HTMLElement | null>(null);
  const sidebarRoute =
    location.pathname === "/"
    || location.pathname.startsWith("/agents/")
    || location.pathname.startsWith("/servers/");
  return (
    <TitlebarSlotContext.Provider value={titlebarSlot}>
      {/* Topbar: 42px, no bottom border — the separation between chrome and the
          content island is made by the background, not by a line. */}
      <header
        className="flex h-[42px] shrink-0 items-center gap-2.5 px-3"
        data-tauri-drag-region="deep"
        data-testid="app-titlebar"
      >
        <div className="flex min-w-0 items-center">
          <div
            aria-hidden="true"
            className="h-full w-[72px] shrink-0"
          />
          {sidebarRoute && (
            <Button
              type="button"
              variant="ghost"
              size="icon-sm"
              className="size-[26px] shrink-0 rounded-[7px] text-muted-foreground hover:bg-accent hover:text-foreground"
              aria-label={sidebar.hidden ? "Show agents" : "Hide agents"}
              title={sidebar.hidden ? "Show agents" : "Hide agents"}
              onClick={() => sidebar.setHidden(!sidebar.hidden)}
            >
              {sidebar.hidden
                ? <PanelLeftOpen className="size-4" />
                : <PanelLeftClose className="size-4" />}
            </Button>
          )}
          {/* The workspace switcher of the console pages portals in here. */}
          {sidebarRoute && <div ref={setTitlebarSlot} className="ml-1 flex min-w-0" />}
          {/* The view lives in the query, not the path: switching keeps the
              selected agent's route, survives a reload and is one Back away. */}
          {/* Only where an agent is the subject: server pages are neither
              view. */}
          {(location.pathname === "/" || location.pathname.startsWith("/agents/")) && (
            <Segmented
              label="View"
              className="ml-2"
              options={VIEW_OPTIONS}
              value={searchParams.get("view") === "all" ? "all" : "agents"}
              onChange={(value) => setSearchParams((current) => {
                const next = new URLSearchParams(current);
                if (value === "all") next.set("view", "all");
                else next.delete("view");
                return next;
              })}
            />
          )}
        </div>
        <div
          aria-hidden="true"
          className="h-full min-w-4 flex-1"
        />
        <div className="flex items-center gap-0.5">
          <Button
            asChild
            variant="ghost"
            size="icon-sm"
            className="size-[26px] rounded-[7px] text-muted-foreground hover:bg-accent hover:text-foreground"
          >
            <Link to="/app-settings" aria-label="Application settings" title="Application settings">
              <Settings className="size-[15px]" />
            </Link>
          </Button>
          <ThemeToggle
            variant="ghost"
            className="size-[26px] rounded-[7px] text-muted-foreground hover:bg-accent hover:text-foreground"
          />
        </div>
      </header>
      <UpdateBanner />
      <DaemonBanner />
      <main className="min-h-0 flex-1 overflow-hidden">
        <Routes>
          <Route path="/" element={<TerminalsPage />} />
          <Route path="/app-settings" element={<AppSettings />} />
          <Route path="/agents/new" element={<CanonicalCreateRedirect />} />
          <Route path="/agents/:hostId/teams/:team" element={<TerminalsPage />} />
          <Route path="/agents/:hostId/:agent/:tab/*" element={<TerminalsPage />} />
          <Route path="/servers/:hostId/tasks" element={<TerminalsPage serverView="tasks" />} />

          <Route path="/servers/:hostId/images" element={<TerminalsPage serverView="images" />} />
          <Route path="/servers/:hostId/images/:name" element={<TerminalsPage serverView="image-tags" />} />
          <Route path="/servers/:hostId/images/:name/:tag" element={<TerminalsPage serverView="image-detail" />}>
            <Route index element={<ImageOverview />} />
            <Route path="template" element={<ImageTemplate />} />
            <Route path="skills" element={<ImageSkills />} />
            <Route path="files" element={<ImageFiles />} />
          </Route>

          <Route path="/servers/:hostId/stores" element={<TerminalsPage serverView="stores" />} />
          <Route path="/servers/:hostId/stores/:name" element={<TerminalsPage serverView="store-detail" />} />

          <Route path="/servers/:hostId/settings" element={<TerminalsPage serverView="settings" />}>
            <Route index element={<GeneralSettings />} />
            <Route path="hosts" element={<DaemonsPage />} />
            <Route path="cli" element={<CliSettings />} />
            <Route path="appearance" element={<AppearanceSettings />} />
            <Route path="integrations/:plugin" element={<PluginSettingsRoute />} />
            <Route path="advanced" element={<AdvancedSettingsIndex />} />
            <Route path="advanced/usage" element={<UsagePage />} />
            <Route path="advanced/budgets" element={<BudgetsPage />} />
            <Route path="advanced/plugins" element={<PluginsPage />} />
            <Route path="advanced/ops" element={<OpsPage />} />
            <Route path="advanced/daemons" element={<DaemonsPage />} />
            <Route path="advanced/groups" element={<GroupsPage />} />
            <Route path="advanced/channels" element={<ChannelsPage />} />
          </Route>

          <Route path="/tasks" element={<LegacyServerSurfaceRedirect section="tasks" />} />
          <Route path="/images/*" element={<LegacyServerSurfaceRedirect section="images" />} />
          <Route path="/settings/*" element={<LegacyServerSurfaceRedirect section="settings" />} />

          <Route path="/terminals" element={<CanonicalTerminalsRedirect />} />
          <Route path="/terminals/:hostId/:agent" element={<LegacyTerminalRedirect />} />
          <Route path="/agent/:name/*" element={<LegacyAgentRedirect />} />
          <Route path="/hosts" element={<Navigate to="/settings/hosts" replace />} />
          <Route path="/usage" element={<Navigate to="/settings/advanced/usage" replace />} />
          <Route path="/budgets" element={<Navigate to="/settings/advanced/budgets" replace />} />
          <Route path="/plugins" element={<Navigate to="/settings/advanced/plugins" replace />} />
          <Route path="/ops" element={<Navigate to="/settings/advanced/ops" replace />} />
          <Route path="/daemons" element={<Navigate to="/settings/advanced/daemons" replace />} />
          <Route path="/groups" element={<Navigate to="/settings/advanced/groups" replace />} />
          <Route path="/channels" element={<Navigate to="/settings/advanced/channels" replace />} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
      </main>
    </TitlebarSlotContext.Provider>
  );
}

function PluginSettingsRoute() {
  const target = useOutletContext<ApiTarget>();
  const { plugin = "" } = useParams();
  return <PluginSettings name={plugin} target={target} />;
}

function LegacyTerminalRedirect() {
  const { hostId = "local", agent = "" } = useParams();
  return <Navigate to={`/agents/${hostId}/${encodeURIComponent(agent)}/${defaultAgentTab()}`} replace />;
}

function CanonicalCreateRedirect() {
  const location = useLocation();
  const params = new URLSearchParams(location.search);
  params.set("new", "1");
  return <Navigate to={`/?${params.toString()}`} replace />;
}

function CanonicalTerminalsRedirect() {
  const location = useLocation();
  return <Navigate to={`/${location.search}`} replace />;
}

function LegacyAgentRedirect() {
  const { name = "", "*": tail = "" } = useParams();
  const location = useLocation();
  const { activeId } = useDaemons();
  const host = activeId || "local";
  const first = tail.split("/")[0];
  const tab =
    first === "logs" || first === "usage" ? "activity"
    : first === "settings" ? "configuration"
    : first === "" ? defaultAgentTab()
    : "advanced";
  const query = tab === "advanced" && first
    ? `?view=${encodeURIComponent(first)}`
    : location.search;
  return (
    <Navigate
      to={`/agents/${encodeURIComponent(host)}/${encodeURIComponent(name)}/${tab}${query}`}
      replace
    />
  );
}

function LegacyServerSurfaceRedirect({ section }: {
  section: "tasks" | "images" | "settings";
}) {
  const location = useLocation();
  const { activeId } = useDaemons();
  const legacyBase = `/${section}`;
  const tail = location.pathname.slice(legacyBase.length);
  return (
    <Navigate
      to={`/servers/${encodeURIComponent(hostToParam(activeId))}/${section}${tail}${location.search}`}
      replace
    />
  );
}
