import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { Link } from "react-router-dom";
import {
  desktopUpdateDownload,
  desktopUpdateInstall,
  desktopUpdateState,
  isDesktop,
  onDesktopUpdateState,
  type DesktopUpdateSnapshot,
} from "@/lib/desktop";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Segmented } from "@/components/ui/segmented";
import { useUiMode, writeUiMode, type UiMode } from "@/lib/uiMode";

const AUTO_DOWNLOAD_KEY = "desktop:updates:auto-download:v1";
const SIX_HOURS = 6 * 60 * 60 * 1000;
const STORAGE_ERROR = "Could not save the automatic download setting.";

type Pending = "download" | "install" | null;

interface DesktopUpdatesContextValue {
  desktop: boolean;
  snapshot: DesktopUpdateSnapshot | null;
  automatic: boolean;
  pending: Pending;
  storageError: string;
  requestError: string;
  setAutomatic: (enabled: boolean) => void;
  request: (kind: Exclude<Pending, null>) => Promise<void>;
}

const DesktopUpdatesContext = createContext<DesktopUpdatesContextValue | null>(null);

function readPreference(): { automatic: boolean; error: string } {
  try {
    const stored = localStorage.getItem(AUTO_DOWNLOAD_KEY);
    return { automatic: stored === null ? true : stored === "true", error: "" };
  } catch {
    return { automatic: true, error: STORAGE_ERROR };
  }
}

export function DesktopUpdatesProvider({ children }: { children: ReactNode }) {
  const desktop = isDesktop();
  const [initial] = useState(readPreference);
  const [automatic, setAutomaticState] = useState(initial.automatic);
  const [storageError, setStorageError] = useState(initial.error);
  const [snapshot, setSnapshot] = useState<DesktopUpdateSnapshot | null>(null);
  const [pending, setPending] = useState<Pending>(null);
  const [requestError, setRequestError] = useState("");
  const [bridgeReady, setBridgeReady] = useState(false);
  const snapshotRef = useRef<DesktopUpdateSnapshot | null>(null);
  const inFlight = useRef(false);

  const accept = useCallback((next: DesktopUpdateSnapshot | null) => {
    if (!next || (snapshotRef.current && next.revision < snapshotRef.current.revision)) return;
    snapshotRef.current = next;
    setSnapshot(next);
  }, []);

  useEffect(() => {
    if (!desktop) return;
    let active = true;
    const failed = () => {
      if (!active) return;
      setRequestError("Could not connect to the Desktop update service.");
      setBridgeReady(true);
    };
    const unsubscribe = onDesktopUpdateState(
      (next) => { if (active) accept(next); },
      () => {
        void desktopUpdateState()
          .then((next) => { if (active) accept(next); })
          .catch(failed)
          .finally(() => { if (active) setBridgeReady(true); });
      },
      failed,
    );
    return () => {
      active = false;
      unsubscribe();
    };
  }, [accept, desktop]);

  const request = useCallback(async (kind: Exclude<Pending, null>) => {
    if (!desktop || inFlight.current) return;
    const phase = snapshotRef.current?.phase;
    if (kind === "download" && phase && !["idle", "up-to-date", "error"].includes(phase)) return;
    if (kind === "install" && phase !== "ready") return;
    inFlight.current = true;
    setPending(kind);
    setRequestError("");
    try {
      accept(await (kind === "download" ? desktopUpdateDownload() : desktopUpdateInstall()));
    } catch {
      setRequestError(kind === "download"
        ? "Could not check for updates. Try again."
        : "Could not install the update. Try again.");
    } finally {
      inFlight.current = false;
      setPending(null);
    }
  }, [accept, desktop]);

  useEffect(() => {
    if (!desktop || !bridgeReady || !automatic) return;
    let cancelled = false;
    let timer = 0;
    const check = async () => {
      await request("download");
      if (!cancelled) timer = window.setTimeout(check, SIX_HOURS);
    };
    void check();
    return () => {
      cancelled = true;
      window.clearTimeout(timer);
    };
  }, [automatic, bridgeReady, desktop, request]);

  const setAutomatic = (enabled: boolean) => {
    setAutomaticState(enabled);
    try {
      localStorage.setItem(AUTO_DOWNLOAD_KEY, String(enabled));
      setStorageError("");
    } catch {
      setStorageError(STORAGE_ERROR);
    }
  };

  return (
    <DesktopUpdatesContext.Provider value={{
      desktop,
      snapshot,
      automatic,
      pending,
      storageError,
      requestError,
      setAutomatic,
      request,
    }}>
      {children}
    </DesktopUpdatesContext.Provider>
  );
}

function useDesktopUpdates() {
  const context = useContext(DesktopUpdatesContext);
  if (!context) throw new Error("Desktop updates must be used within DesktopUpdatesProvider");
  return context;
}

function DownloadStatus({ snapshot }: { snapshot: DesktopUpdateSnapshot }) {
  if (snapshot.phase === "checking") return <p role="status">Checking for updates…</p>;
  if (snapshot.phase === "installing") return <p role="status">Installing update…</p>;
  if (snapshot.phase === "up-to-date") return <p role="status">You have the latest version</p>;
  if (snapshot.phase !== "downloading") return null;
  const knownTotal = snapshot.total_bytes !== null && snapshot.total_bytes > 0;
  const percent = knownTotal
    ? Math.min(100, Math.round(snapshot.downloaded_bytes / snapshot.total_bytes! * 100))
    : null;
  return (
    <div className="space-y-2" role="status">
      <progress
        aria-label="Downloading update"
        className="w-full"
        {...(knownTotal ? { max: snapshot.total_bytes!, value: snapshot.downloaded_bytes } : {})}
      />
      <p>{percent === null
        ? `Downloaded ${snapshot.downloaded_bytes} bytes`
        : `Downloaded ${percent}%`}</p>
    </div>
  );
}

export function AppSettings() {
  const updates = useDesktopUpdates();
  const mode = useUiMode();
  const canCheck = !updates.snapshot
    || ["idle", "up-to-date", "error"].includes(updates.snapshot.phase);
  const error = updates.snapshot?.phase === "ready"
    ? ""
    : updates.requestError || updates.snapshot?.error;
  return (
    <section className="h-full overflow-auto p-6">
      <div className="mx-auto max-w-2xl space-y-6">
        <Button asChild variant="ghost"><Link to="/">Back to workspace</Link></Button>
        <h1 className="text-2xl font-semibold">Application settings</h1>
        <Card>
          <CardHeader><CardTitle>Interface mode</CardTitle></CardHeader>
          <CardContent className="space-y-3">
            <Segmented<UiMode>
              label="Interface mode"
              className="w-fit"
              value={mode}
              onChange={writeUiMode}
              options={[{ value: "simple", label: "Simple" }, { value: "expert", label: "Expert" }]}
            />
            <p className="text-muted-foreground">
              Simple shows an agent&apos;s Chat, Tasks, Console and Configuration. Expert shows every tab and chat.
            </p>
          </CardContent>
        </Card>
        <Card>
          <CardHeader><CardTitle>Desktop updates</CardTitle></CardHeader>
          <CardContent className="space-y-5">
            <p>Current Desktop version: <strong>{updates.snapshot?.current_version || "—"}</strong></p>
            <div className="flex items-center justify-between gap-4">
              <Label htmlFor="desktop-auto-updates">Download updates automatically</Label>
              <Switch
                id="desktop-auto-updates"
                checked={updates.automatic}
                disabled={!updates.desktop}
                onCheckedChange={updates.setAutomatic}
              />
            </div>
            {updates.storageError && <p role="alert" className="text-destructive">{updates.storageError}</p>}
            {!updates.desktop ? (
              <p role="status" className="text-muted-foreground">
                Updates are only available in the Desktop app.
              </p>
            ) : (
              <>
                <Button
                  type="button"
                  disabled={updates.pending !== null || !canCheck}
                  onClick={() => void updates.request("download")}
                >
                  Check for and download update
                </Button>
                {updates.snapshot && <DownloadStatus snapshot={updates.snapshot} />}
                {error && <p role="alert" className="text-destructive">{error}</p>}
              </>
            )}
          </CardContent>
        </Card>
      </div>
    </section>
  );
}

export function UpdateBanner() {
  const updates = useDesktopUpdates();
  if (!updates.desktop) return null;
  if (updates.snapshot?.phase === "installing" || updates.pending === "install") {
    return <div role="status" className="border-b bg-muted px-4 py-2">Installing update…</div>;
  }
  if (updates.snapshot?.phase !== "ready") return null;
  const error = updates.requestError || updates.snapshot.error;
  return (
    <div role="status" className="flex flex-wrap items-center justify-center gap-3 border-b bg-muted px-4 py-2">
      <span>Version {updates.snapshot.version} downloaded</span>
      <Button
        type="button"
        size="sm"
        disabled={updates.pending !== null}
        onClick={() => void updates.request("install")}
      >
        Update
      </Button>
      {error && <span role="alert" className="text-destructive">{error}</span>}
    </div>
  );
}
