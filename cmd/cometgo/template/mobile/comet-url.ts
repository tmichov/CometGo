// Where the app finds the CometGo server.
//
// 1. EXPO_PUBLIC_COMET_URL, when set: release builds (`cometgo build
//    --server`) and `cometgo run --server`.
// 2. In development, the host Expo chose for Metro (the phone already reaches
//    it: localhost through `adb reverse`, or this machine's LAN address) on
//    the server's port, EXPO_PUBLIC_COMET_PORT (set by `cometgo run`).
// 3. Otherwise there is nothing to connect to.
export function cometURL(env: { url?: string; port?: string }, hostUri: string | undefined, dev: boolean, defaultPort: string): string {
  if (env.url) return env.url;
  if (dev && hostUri) {
    const host = hostUri.replace(/^[a-z]+:\/\//, "").split("/")[0]!.replace(/:\d+$/, "");
    return `ws://${host}:${env.port || defaultPort}/_comet`;
  }
  throw new Error("cometgo: no server URL: build with `cometgo build android --server wss://...`, or set EXPO_PUBLIC_COMET_URL");
}
