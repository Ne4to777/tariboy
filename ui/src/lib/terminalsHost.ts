import { cachedDaemon, type Daemon } from "@/lib/daemons";

// URL param mapping for /terminals/:hostId/:agent. The same-origin local
// daemon has registry id "" which cannot appear in a path segment, so it is
// spelled "local" in URLs.
export const LOCAL_PARAM = "local";
export const hostToParam = (id: string): string => (id === "" ? LOCAL_PARAM : id);
export const paramToHost = (p: string): string => (p === LOCAL_PARAM ? "" : p);
export type ServerSection = "tasks" | "images" | "workflows" | "stores" | "settings";
export const serverPath = (hostId: string, section: ServerSection): string =>
  `/servers/${encodeURIComponent(hostToParam(hostId))}/${section}`;

// targetFor resolves a host id to an explicit api target: null = same-origin
// local (NOT the active daemon), a Daemon = that registered host.
export function targetFor(hostId: string): Daemon | null {
  if (hostId === "") return null;
  return cachedDaemon(hostId) ?? {
    id: hostId,
    label: hostId,
    baseURL: "",
    token: "",
  };
}
