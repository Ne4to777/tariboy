import { useEffect, useRef, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { ApiError } from "@/lib/api";
import type { Daemon } from "@/lib/daemons";
import {
  addStore,
  buildStoreImage,
  buildStoreWorkflow,
  getStore,
  listStores,
  refreshStore,
  removeStore,
  setStoreAuto,
  type Store,
  type StoreDetail,
  type StoreImage,
  type StoreWorkflow,
} from "@/lib/stores";
import { listWorkflowImages, type WorkflowImage } from "@/lib/tasks";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from "@/components/ui/alert-dialog";

const message = (error: unknown) => error instanceof ApiError ? error.message : String(error);

const imageStatuses = (image: StoreImage) => {
  const statuses: string[] = [];
  if (image.error) statuses.push(`Source error: ${image.error}`);
  if (image.latest_status === "error") statuses.push(`Latest error: ${image.latest_error || "inspection failed"}`);
  if (image.latest_status === "missing") statuses.push("Latest not built");
  if (image.latest_status === "unversioned" || (image.latest_status === "built" && !image.built_version)) {
    statuses.push("Latest image_version missing; comparison unavailable");
  } else if (image.latest_status === "built" && !image.error) {
    statuses.push(!image.version ? "Source image_version missing; comparison unavailable" : image.update_needed ? "Update needed" : "Up to date");
  } else if (!image.latest_status && !image.error) {
    statuses.push("Comparison unavailable");
  }
  return statuses;
};

// latest is the built latest workflow_version: undefined when not built, null
// when the registry could not be read.
const workflowStatus = (workflow: StoreWorkflow, latest: string | null | undefined) => {
  if (workflow.error) return `Source error: ${workflow.error}`;
  if (latest === null) return "Comparison unavailable";
  if (latest === undefined) return "Latest not built";
  return latest === workflow.version ? "Up to date" : "Update needed";
};

export default function StoresPage({ target, name, basePath }: {
  target: Daemon | null;
  name?: string;
  basePath: string;
}) {
  const navigate = useNavigate();
  const mounted = useRef(true);
  const [stores, setStores] = useState<Store[]>([]);
  const [detail, setDetail] = useState<StoreDetail | null>(null);
  const [storeName, setStoreName] = useState("");
  const [source, setSource] = useState("");
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const [status, setStatus] = useState("");
  const [autoInterval, setAutoInterval] = useState("0");
  const [autoImages, setAutoImages] = useState<string[]>([]);
  const [removeOpen, setRemoveOpen] = useState(false);
  const [removeError, setRemoveError] = useState("");
  const [workflowImages, setWorkflowImages] = useState<WorkflowImage[] | null>(null);
  const buildGeneration = useRef(0);

  useEffect(() => {
    mounted.current = true;
    return () => { mounted.current = false; };
  }, []);

  useEffect(() => {
    let current = true;
    const request = name ? getStore(target, name) : listStores(target);
    void request.then((result) => {
      if (!current) return;
      if (name) {
        const loaded = result as StoreDetail;
        setDetail(loaded);
        setAutoInterval(String(loaded.auto?.interval_minutes ?? 0));
        setAutoImages(loaded.auto?.images ?? []);
      } else setStores(result as Store[]);
    }).catch((cause) => {
      if (current) setError(message(cause));
    }).finally(() => {
      if (current) setLoading(false);
    });
    if (name) {
      void listWorkflowImages(target).then(
        (images) => { if (current) setWorkflowImages(images); },
        () => { if (current) setWorkflowImages(null); },
      );
    }
    return () => { current = false; };
  }, [name, target]);

  const add = async () => {
    const input = { name: storeName.trim(), source: source.trim() };
    if (!input.name || !input.source) return;
    setBusy("add");
    setError("");
    setStatus("");
    try {
      const added = await addStore(target, input);
      if (mounted.current) navigate(`${basePath}/${encodeURIComponent(added.name)}`);
    } catch (cause) {
      if (mounted.current) setError(message(cause));
    } finally {
      if (mounted.current) setBusy("");
    }
  };

  const refresh = async () => {
    if (!name) return;
    buildGeneration.current++;
    setBusy("refresh");
    setError("");
    setStatus("");
    try {
      const refreshed = await refreshStore(target, name);
      if (mounted.current) {
        setDetail(refreshed);
        setStatus(`Refreshed ${name}.`);
      }
    } catch (cause) {
      if (mounted.current) setError(message(cause));
    } finally {
      if (mounted.current) setBusy("");
    }
  };

  const build = async (imageName: string) => {
    if (!name) return;
    const requestedBuild = ++buildGeneration.current;
    const isCurrent = () => mounted.current && buildGeneration.current === requestedBuild;
    setBusy(`build:${imageName}`);
    setError("");
    setStatus("");
    try {
      const built = await buildStoreImage(target, { source: `${name}/${imageName}` });
      if (isCurrent()) {
        setStatus(built.tag === "latest"
          ? `Built ${built.name}:latest.`
          : `Built ${built.name}:${built.tag} and ${built.name}:latest.`);
        setBusy("");
        window.dispatchEvent(new Event("tariboy:image-built"));
      }
      if (!isCurrent()) return;
      try {
        const refreshed = await getStore(target, name);
        if (isCurrent()) setDetail(refreshed);
      } catch (cause) {
        if (isCurrent()) setError(message(cause));
      }
    } catch (cause) {
      if (isCurrent()) setError(message(cause));
    } finally {
      if (isCurrent()) setBusy("");
    }
  };

  const buildWorkflow = async (workflowName: string) => {
    if (!name) return;
    const requestedBuild = ++buildGeneration.current;
    const isCurrent = () => mounted.current && buildGeneration.current === requestedBuild;
    setBusy(`workflow:${workflowName}`);
    setError("");
    setStatus("");
    try {
      const built = await buildStoreWorkflow(target, `${name}/${workflowName}`);
      if (!isCurrent()) return;
      setStatus(`Built ${built.name} ${built.version} (tags: ${built.tags.join(", ")}).`);
      setBusy("");
      window.dispatchEvent(new Event("tariboy:workflow-image-built"));
      const [refreshed, images] = await Promise.all([
        getStore(target, name),
        listWorkflowImages(target).catch(() => null),
      ]);
      if (isCurrent()) {
        setDetail(refreshed);
        setWorkflowImages(images);
      }
    } catch (cause) {
      if (isCurrent()) setError(message(cause));
    } finally {
      if (isCurrent()) setBusy("");
    }
  };

  const latestWorkflow = (workflowName: string) => workflowImages
    ? workflowImages.find((image) => image.name === workflowName && image.tag === "latest")?.version
    : null;

  const saveAuto = async () => {
    if (!name) return;
    const interval = Number(autoInterval);
    if (!Number.isInteger(interval) || interval < 0) {
      setError("Automatic build interval must be a whole number of minutes.");
      return;
    }
    setBusy("auto");
    setError("");
    setStatus("");
    try {
      const saved = await setStoreAuto(target, name, { interval, image: autoImages });
      if (mounted.current) {
        setDetail(saved);
        setAutoInterval(String(saved.auto?.interval_minutes ?? 0));
        setAutoImages(saved.auto?.images ?? []);
        setStatus(interval > 0
          ? `Automatic builds run every ${interval} min for ${autoImages.length} image(s).`
          : "Automatic builds disabled.");
      }
    } catch (cause) {
      if (mounted.current) setError(message(cause));
    } finally {
      if (mounted.current) setBusy("");
    }
  };

  const remove = async () => {
    if (!name) return;
    buildGeneration.current++;
    setBusy("remove");
    setError("");
    setRemoveError("");
    setStatus("");
    try {
      await removeStore(target, name);
      if (mounted.current) navigate(basePath);
    } catch (cause) {
      if (mounted.current) {
        const failure = message(cause);
        setError(failure);
        setRemoveError(failure);
      }
    } finally {
      if (mounted.current) setBusy("");
    }
  };

  return <div className="h-full min-h-0 space-y-4 overflow-y-auto p-6">
    {name ? <>
      <div className="space-y-1">
        <Link className="text-sm text-primary hover:underline" to={basePath}>← Stores</Link>
        <div className="flex items-start justify-between gap-3">
          <div>
            <h1 className="text-lg font-semibold">{name}</h1>
            {detail && <>
              <p className="break-all text-sm text-muted-foreground">{detail.source}</p>
              <p className="break-all font-mono text-xs text-muted-foreground">{detail.path}</p>
            </>}
          </div>
          <div className="flex shrink-0 gap-2">
            <Button variant="outline" disabled={Boolean(busy) || loading} onClick={() => void refresh()}>
              {busy === "refresh" ? "Refreshing…" : "Refresh"}
            </Button>
            <AlertDialog open={removeOpen} onOpenChange={(open) => {
              if (busy === "remove") return;
              if (open) setRemoveError("");
              setRemoveOpen(open);
            }}>
              <AlertDialogTrigger asChild>
                <Button variant="destructive" disabled={Boolean(busy) || loading}>Remove store</Button>
              </AlertDialogTrigger>
              <AlertDialogContent>
                <AlertDialogHeader>
                  <AlertDialogTitle>Remove Store {name}?</AlertDialogTitle>
                  <AlertDialogDescription>
                    Removes this registration and its managed clone. It preserves local sources and built images.
                  </AlertDialogDescription>
                </AlertDialogHeader>
                {removeError && <p role="alert" className="text-sm text-destructive">{removeError}</p>}
                <AlertDialogFooter>
                  <AlertDialogCancel disabled={busy === "remove"}>Cancel</AlertDialogCancel>
                  <AlertDialogAction
                    variant="destructive"
                    disabled={busy === "remove"}
                    onClick={(event) => { event.preventDefault(); void remove(); }}
                  >
                    {busy === "remove" ? "Removing…" : "Remove store"}
                  </AlertDialogAction>
                </AlertDialogFooter>
              </AlertDialogContent>
            </AlertDialog>
          </div>
        </div>
      </div>
      {loading && <p role="status" className="text-sm text-muted-foreground">Loading Store…</p>}
      {detail && <section className="space-y-2">
        <h2 className="font-medium">Images</h2>
        <Tabs defaultValue="agent">
          <TabsList>
              <TabsTrigger value="agent">Agent images</TabsTrigger>
              <TabsTrigger value="workflow">Workflow images</TabsTrigger>
          </TabsList>
          <TabsContent value="agent" className="space-y-2">
            <div className="grid gap-3 md:grid-cols-[14rem_auto]">
              <Input
                aria-label="Automatic build interval"
                type="number"
                min={0}
                step={1}
                placeholder="0"
                value={autoInterval}
                disabled={Boolean(busy)}
                onChange={(event) => setAutoInterval(event.target.value)}
              />
              <Button variant="outline" disabled={Boolean(busy)} onClick={() => void saveAuto()}>
                {busy === "auto" ? "Saving…" : "Save automatic builds"}
              </Button>
            </div>
            <p className="text-xs text-muted-foreground">
              Automatic builds refresh this Store every interval in minutes and rebuild the checked images that need an
              update, publishing image_version and latest. An interval of 0 disables them.
            </p>
            <div className="overflow-x-auto rounded border">
              <table className="w-full text-sm">
                <thead className="bg-muted/50 text-left text-xs text-muted-foreground">
                  <tr><th className="px-3 py-2">Auto</th><th className="px-3 py-2">Image</th><th className="px-3 py-2">Source image_version</th><th className="px-3 py-2">Latest image_version</th><th className="px-3 py-2">Status</th><th className="px-3 py-2" /></tr>
                </thead>
                <tbody>
                  {detail.images.map((image) => <tr key={image.name} className={`border-t ${image.update_needed ? "bg-amber-50 dark:bg-amber-950/30" : ""}`}>
                    <td className="px-3 py-2">
                      <input
                        type="checkbox"
                        aria-label={`Automatic builds for ${image.name}`}
                        checked={autoImages.includes(image.name)}
                        disabled={Boolean(busy) || Boolean(image.error)}
                        onChange={(event) => setAutoImages((current) => event.target.checked
                          ? [...current, image.name]
                          : current.filter((selected) => selected !== image.name))}
                      />
                    </td>
                    <td className="px-3 py-2 font-mono">{image.name}</td>
                    <td className="px-3 py-2 font-mono text-xs">{image.version || "Not specified"}</td>
                    <td className="px-3 py-2 font-mono text-xs">{image.latest_status === "missing" ? "Not built" : image.latest_status === "error" ? "Inspection failed" : image.built_version || "Not specified"}</td>
                    <td className={`px-3 py-2 text-xs ${image.error || image.latest_status === "error" ? "text-destructive" : "text-muted-foreground"}`}>
                      {imageStatuses(image).map((status) => <div key={status}>{status}</div>)}
                    </td>
                    <td className="px-3 py-2 text-right">
                      <Button
                        size="sm"
                        disabled={Boolean(busy) || Boolean(image.error)}
                        onClick={() => void build(image.name)}
                      >
                        {busy === `build:${image.name}` ? `Building ${image.name}…` : `Build ${image.name}`}
                      </Button>
                    </td>
                  </tr>)}
                  {detail.images.length === 0 && <tr><td colSpan={6} className="px-3 py-8 text-center text-muted-foreground">No images in this Store.</td></tr>}
                </tbody>
              </table>
            </div>
          </TabsContent>
          <TabsContent value="workflow">
            <div className="overflow-x-auto rounded border">
              <table className="w-full text-sm">
                <thead className="bg-muted/50 text-left text-xs text-muted-foreground">
                  <tr><th className="px-3 py-2">Workflow</th><th className="px-3 py-2">Source workflow_version</th><th className="px-3 py-2">Latest workflow_version</th><th className="px-3 py-2">Status</th><th className="px-3 py-2" /></tr>
                </thead>
                <tbody>
                  {(detail.workflows ?? []).map((workflow) => {
                    const latest = latestWorkflow(workflow.name);
                    const status = workflowStatus(workflow, latest);
                    return <tr key={workflow.name} className={`border-t ${status === "Update needed" ? "bg-amber-50 dark:bg-amber-950/30" : ""}`}>
                      <td className="px-3 py-2 font-mono">{workflow.name}</td>
                      <td className="px-3 py-2 font-mono text-xs">{workflow.version || "Not specified"}</td>
                      <td className="px-3 py-2 font-mono text-xs">{latest === null ? "Unknown" : latest ?? "Not built"}</td>
                      <td className={`px-3 py-2 text-xs ${workflow.error ? "text-destructive" : "text-muted-foreground"}`}>{status}</td>
                      <td className="px-3 py-2 text-right">
                        <Button
                          size="sm"
                          disabled={Boolean(busy) || Boolean(workflow.error)}
                          onClick={() => void buildWorkflow(workflow.name)}
                        >
                          {busy === `workflow:${workflow.name}` ? `Building ${workflow.name}…` : `Build ${workflow.name}`}
                        </Button>
                      </td>
                    </tr>;
                  })}
                  {!detail.workflows?.length && <tr><td colSpan={5} className="px-3 py-8 text-center text-muted-foreground">No workflow images in this Store.</td></tr>}
                </tbody>
              </table>
            </div>
          </TabsContent>
        </Tabs>
      </section>}
    </> : <>
      <div>
        <h1 className="text-lg font-semibold">Stores</h1>
        <p className="text-sm text-muted-foreground">Register image source repositories and directories on this server.</p>
      </div>
      <section className="space-y-3 rounded border p-4">
        <h2 className="font-medium">Add Store</h2>
        <div className="grid gap-3 md:grid-cols-[14rem_minmax(0,1fr)_auto]">
          <Input aria-label="Store name" required placeholder="name" value={storeName} disabled={Boolean(busy)} onChange={(event) => setStoreName(event.target.value)} />
          <Input aria-label="Store source" required placeholder="Git URL or absolute directory" value={source} disabled={Boolean(busy)} onChange={(event) => setSource(event.target.value)} />
          <Button disabled={Boolean(busy) || !storeName.trim() || !source.trim()} onClick={() => void add()}>
            {busy === "add" ? "Adding…" : "Add store"}
          </Button>
        </div>
      </section>
      {loading && <p role="status" className="text-sm text-muted-foreground">Loading Stores…</p>}
      {!loading && <div className="overflow-x-auto rounded border">
        <table className="w-full text-sm">
          <thead className="bg-muted/50 text-left text-xs text-muted-foreground"><tr><th className="px-3 py-2">Store</th><th className="px-3 py-2">Source</th></tr></thead>
          <tbody>
            {stores.map((store) => <tr key={store.name} className="border-t">
              <td className="px-3 py-2"><Link className="font-medium text-primary hover:underline" to={`${basePath}/${encodeURIComponent(store.name)}`}>{store.name}</Link></td>
              <td className="break-all px-3 py-2 font-mono text-xs text-muted-foreground">{store.source}</td>
            </tr>)}
            {stores.length === 0 && <tr><td colSpan={2} className="px-3 py-8 text-center text-muted-foreground">No Stores registered on this server.</td></tr>}
          </tbody>
        </table>
      </div>}
    </>}
    {error && <p role="alert" className="text-sm text-destructive">{error}</p>}
    {status && <p role="status" className="text-sm text-muted-foreground">{status}</p>}
  </div>;
}
