import { useSyncExternalStore } from "react";

/**
 * Simple shows an ordinary user only the agent's Chat, Tasks, Console and
 * Configuration; Expert shows everything. It belongs to this Desktop WebView,
 * not to a daemon, and anything but an explicit "expert" — including unreadable
 * storage — is Simple. It hides UI only: it is not an access control.
 */
export type UiMode = "simple" | "expert";

export const UI_MODE_KEY = "app:ui-mode:v1";

const listeners = new Set<() => void>();

export function readUiMode(): UiMode {
  try {
    return localStorage.getItem(UI_MODE_KEY) === "expert" ? "expert" : "simple";
  } catch {
    return "simple";
  }
}

/** The tab an agent opens on: Chat in Simple, Console in Expert. */
export function defaultAgentTab(): "chat" | "console" {
  return readUiMode() === "expert" ? "console" : "chat";
}

export function writeUiMode(mode: UiMode): void {
  try {
    localStorage.setItem(UI_MODE_KEY, mode);
  } catch {
    // Unavailable storage leaves the interface in Simple rather than breaking it.
  }
  listeners.forEach((listener) => listener());
}

function subscribe(listener: () => void) {
  listeners.add(listener);
  return () => { listeners.delete(listener); };
}

export function useUiMode(): UiMode {
  return useSyncExternalStore(subscribe, readUiMode);
}
