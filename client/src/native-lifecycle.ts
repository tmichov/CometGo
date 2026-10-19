// The React Native lifecycle for @cometgo/client/react-native, over
// AppState- and NetInfo-shaped objects so it is tested without React Native.

import type { Lifecycle } from "./index.ts";

/** The part of React Native's AppState the lifecycle uses. */
export interface AppStateLike {
  currentState: string | null;
  addEventListener(type: "change", fn: (state: string) => void): { remove(): void };
}

/** A NetInfo state, as @react-native-community/netinfo reports it. */
export interface NetInfoStateLike {
  type: string;
  isConnected: boolean | null;
  details: { ipAddress?: string | null } | null;
}

/** The part of NetInfo the lifecycle uses. */
export interface NetInfoLike {
  addEventListener(fn: (state: NetInfoStateLike) => void): () => void;
}

/**
 * foreground when the app becomes active, background when it goes to the
 * background, and network when it gets a connection after none or on
 * another network (a socket bound to the old one is dead). NetInfo fires
 * several identical events at subscribe and more on signal-strength and
 * reachability changes, so only (connected, type, address) is compared.
 */
export function nativeLifecycle(appState: AppStateLike, netInfo: NetInfoLike): Lifecycle {
  return (on) => {
    let active = appState.currentState === "active";
    if (appState.currentState === "background") on("background");
    const app = appState.addEventListener("change", (s) => {
      if (s === "active" && !active) on("foreground");
      else if (s === "background") on("background");
      active = s === "active"; // "inactive" (iOS, between the two) counts as away
    });
    let last: string | undefined;
    const unNet = netInfo.addEventListener((s) => {
      const key = `${s.isConnected}|${s.type}|${s.details?.ipAddress ?? ""}`;
      const changed = last !== undefined && key !== last;
      last = key;
      if (changed && s.isConnected === true) on("network");
    });
    return () => {
      app.remove();
      unNet();
    };
  };
}
