// React Native bindings for the CometGo client: @cometgo/client/react-native.
// Needs Expo SDK 53 (React Native 0.79) or later, where Metro resolves
// package exports.

// First: the client's ids need crypto.getRandomValues, which Hermes lacks.
import "react-native-get-random-values";
import { AppState } from "react-native";
import NetInfo from "@react-native-community/netinfo";
import { type Comet, connect, type ConnectOptions, type Schema, type TypedComet } from "./index.ts";
import { nativeLifecycle } from "./native-lifecycle.ts";

export { nativeLifecycle } from "./native-lifecycle.ts";
export type { AppStateLike, NetInfoLike, NetInfoStateLike } from "./native-lifecycle.ts";

// NetInfo's reachability check pings an external server; connected and the
// network type are enough to know when to check the connection.
NetInfo.configure({ reachabilityShouldRun: () => false });

/**
 * connect, with the lifecycle wired to AppState and NetInfo, so the client
 * checks its connection when the app returns to the foreground or the
 * network changes. `url` is required: there is no page to default to.
 */
export function connectNative(opts: ConnectOptions & { url: string }): Comet;
export function connectNative<S extends Schema>(opts: ConnectOptions & { url: string }): TypedComet<S>;
export function connectNative(opts: ConnectOptions & { url: string }): Comet {
  return connect({ lifecycle: nativeLifecycle(AppState, NetInfo), ...opts });
}
