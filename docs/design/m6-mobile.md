# M6 design: mobile apps with React Native

Status: agreed design (two review rounds, below). Nothing here is implemented.

**Goal.** A React Native app can use CometGo as a web app does (subscriptions, calls with read-your-writes, auth), it stays correct across what phones do to connections, and building one feels like Meteor's `meteor run android` / `meteor build`: one command to run on a device, one to produce the app.

**Decided with the owner (2026-10-19):**

- React Native through Expo is the mobile stack. Native Swift/Kotlin clients and Flutter are out of M6.
- The mobile app has its own React Native UI, scaffolded, run and built by a `cometgo` CLI that wraps Expo. Wrapping the web UI in a native WebView (Capacitor), which is closer to Meteor's Cordova builds, is a possible later item.
- Reconnect must be solid across backgrounding, network changes and sleep. Offline data and offline writes are out of M6.
- M6 is verified on Android only: development happens on Linux, where the iOS Simulator does not run. The code stays cross-platform, and the iOS checks (the iOS half of the device findings and of the exit criterion) are a separate task for when a Mac is available.

**Minimum versions.** Expo SDK 53 (React Native 0.79), the first where Metro resolves package `exports` by default; `@cometgo/client/react` and the new `/react-native` entry are only reachable through `exports`.

## 1. What breaks today

The TypeScript client was written for browsers. Against React Native (from reading the code; the first task confirms each point on a device):

| Where | What | In React Native |
|---|---|---|
| `Comet` constructor and `close` (client/src/index.ts) | `typeof window !== "undefined"`, then `window.addEventListener("online")`, `document.addEventListener("visibilitychange")` | `window` exists, `window.addEventListener` and `document` do not: the constructor throws |
| `uuid()` | `crypto.getRandomValues` | Hermes has none without a polyfill: the constructor throws (default route key) |
| `routeUrl` | `URL` setters | Expo replaces React Native's minimal `URL` with a standard one; bare React Native's setters are not implemented |
| Lifecycle | reconnect on `online` and on the page becoming visible | the signals are `AppState` and `NetInfo` |
| Dead sockets | liveness closes with `close(4408)` and waits for `onclose` to reconnect | a dead TCP connection can take long to report `onclose` (OkHttp waits for the close frame; SocketRocket can hang), so recovery waits with it. `close()` on an unauthenticated socket already runs the disconnect path at once (index.ts, after `#credential()` fails), which §2.1 generalises |
| Late frames | `onmessage` does not check that its socket is still the current one (`onopen` and `onclose` do) | browsers drop frames once `close()` is called; React Native may not, so a dropped socket's late `tx` would land in the new socket's state |

The rest of the client (WebSocket with a subprotocol, timers, `AbortSignal`, promises, JSON) works in React Native, and `globalThis.location?.href` is safe. The package's ES2022 private fields are transpiled by Metro, which runs dependencies through Expo's Babel preset.

## 2. The client

### 2.1 Wake: a probe, not a reconnect

`ConnectOptions.lifecycle?: (wake: (reason: "foreground" | "network") => void) => () => void`. The client calls it once at construction (a throw is caught and logged, never escapes `connect`) and calls the returned function on `close()`.

`wake` exists for sockets that look open but are dead: after a network switch, or after iOS suspended the app, `readyState` is usually still 1. So `wake` is a probe:

- **No socket:** reconnect now (today's `#reconnectNow`).
- **Opening (`#opening`, set from the start of `#open` until the socket exists):** nothing, so two wakes in a row (foreground and a NetInfo event arrive together) cannot run the token function twice; with rotating refresh tokens two concurrent refreshes can invalidate each other.
- **Connecting (`readyState 0`) on a `network` wake:** drop it; it may be bound to the old network.
- **Open:** probe it: send `ping` (the server answers `pong`) and drop the socket if *nothing* arrives within 5 s. Any message counts, since a `pong` waits behind whatever the server has queued (a replay after a resume can be megabytes on a slow link). Bounded so a slow or flapping network cannot loop: no probe if a message arrived in the last second; one probe in flight at a time; after a probe drops a socket, the next socket is not probed for 30 s (the normal liveness check still runs); and a reconnect started by `wake` keeps the backoff count (`#attempt`) instead of resetting it.

The idle age of a socket is not a signal: the server's heartbeat is a WebSocket-level ping that JavaScript never sees, so a healthy idle socket routinely shows no message for a whole heartbeat.

**Dropping never waits for `onclose`.** Dropping a socket (a failed probe, the liveness check, a `network` wake while connecting) closes it and runs the disconnect path at once, as `close()` on an unauthenticated socket already does. Everything the old socket does afterwards is ignored: `onmessage` gets the same `this.#ws !== ws` check `onopen` and `onclose` have, so a late `tx` part cannot land in the new socket's `#txParts` or move `#seq` after the resume hello went out (which would apply a replay twice); and a dropped socket that was still connecting and opens later is closed in its `onopen`. The server already handles the other side: a resume on a new socket fails the old one, and messages from a replaced socket are dropped.

**On wake, the token is checked too.** React Native pauses JavaScript timers while the app is in the background (measured on Android 15, §9), `#refreshTimer` included, so a token can be due by the time the app returns. `wake` refreshes it if it is due (`#authExp`) before the server expires it.

**Defaults.** In a browser (`window.addEventListener` and `document.addEventListener` exist): wake on `online` (`network`) and on the page becoming visible (`foreground`). Elsewhere: none, as in Node today.

### 2.2 No `URL` setters

`routeUrl` becomes string handling: split off a fragment, then the query; keep the query's bytes, drop any `route` pair, append `route=<key>`; map `http`→`ws` and `https`→`wss`. A relative URL resolves against `location` in a browser; without `location` it must be absolute (checked on its scheme). Needed for bare React Native; harmless elsewhere. Tested with `URL` deleted, not stubbed.

### 2.3 `@cometgo/client/react-native`

```ts
import { connectNative } from "@cometgo/client/react-native";
import type { Schema } from "./schema";

export const comet = connectNative<Schema>({
  url: cometURL(), // see §4.2
  token: () => getFreshToken(),
});
```

- Imports `react-native-get-random-values` first.
- Builds the lifecycle from `AppState` and `@react-native-community/netinfo`:
  - `foreground` when `AppState` becomes `active`;
  - `network` when the connection changes. NetInfo fires once at subscribe with the current state (ignored), and again on signal-strength changes and when reachability is resolved, so the lifecycle compares only `(isConnected, type, details.ipAddress)` with the last seen value and wakes on a change to a connected state. `isInternetReachable` is not used: `null` means unknown, and its check pings Google by default (`reachabilityShouldRun` is set to never).
- `react-native`, `@react-native-community/netinfo` and `react-native-get-random-values` are optional peer dependencies; the browser entry never imports them.
- The lifecycle is a pure function over `AppState`- and `NetInfo`-shaped objects, tested in Node with fakes for each case above.

The React hooks (`@cometgo/client/react`) need no change.

### 2.4 Auth on mobile

Bearer tokens only (`Authenticate`); no cookies. The token source reads the token from where the app keeps secrets (`expo-secure-store`) and must refresh it itself: returning an expired token from storage logs the app out on the next connect. Docs.

### 2.5 Background

React Native pauses JavaScript timers while the app is in the background, so nothing in the client runs there; on Android the native WebSocket stays open and keeps answering the server's heartbeat until Doze or the system cuts the network (§9), and iOS suspends the app. An opt-in `closeInBackground` closes the socket as soon as the app goes to the background (the `background` lifecycle event still runs JavaScript), with a 4xxx code (1000 and 1001 end the session for good): the session detaches at once and resumes within the grace when the app returns, and a long background costs neither a server connection nor battery. A delayed close ("after N seconds in the background") cannot work, since no timer fires there. Off by default.

## 3. Sleep and resume

**When a session can resume.** The server notices a suspended iOS app only when its WebSocket ping goes unanswered: up to about three heartbeats (75 s with the default `Heartbeat` of 25 s), and only then starts `ResumeGrace` (60 s). So a phone asleep for up to about `3 × Heartbeat + ResumeGrace` resumes; longer resyncs, which is correct but slower. A mobile-heavy app can raise `ResumeGrace`, at the memory cost the server docs give. Documented in `docs/mobile.md`.

**Resume after expiry: changed, for a session the server knows is detached.** Today, when a detached session's identity reaches `ExpiresAt`, `expire` downgrades it to anonymous, rebinds every subscription and fills the replay buffer with resets; the phone then wakes with a fresh token for the same user, `resumeSession` refuses it (`samePrincipal`, anonymous against the user), and the work was wasted. The change:

- while the session has no socket, `expire` leaves it alone: it keeps its identity and subscriptions (no mark is needed, since `resumeSession` compares the kept identity);
- `resumeSession` accepts a hello whose freshly authenticated identity is the same principal as the stored one, installs it (as it does for a refresh today);
- a hello without a valid token for that principal resyncs, as today; with no hello within the grace, the session ends, as today.

**What it does not cover.** The server detaches a session only once it notices the socket is gone: up to `3 × Heartbeat` after iOS suspends the app, and on Android not until Doze or a network cut, since the native socket keeps answering pings (§9). An identity that expires before that is downgraded as today, and the resume resyncs. Apps with short-lived tokens should set `closeInBackground` (§2.5), which detaches the session at once. The check that the session has no socket runs under `authMu` and reads `c.sock` under `c.mu`; sockets attach to an existing session only inside `resumeSession`, under `authMu` too, so none can attach between the timer and the mark.

**Security.** Replay data reaches only a client presenting a valid token for the same principal and scope, the bar any resume already has. `ExpiresAt` is not revocation: a user revoked while their phone sleeps keeps live subscriptions on the server, sending nothing, until the grace ends or a resume fails, and `Revalidate` still reaches detached sessions. The docs say so.

`resumeSession` also refuses an identity whose `ExpiresAt` has already passed (`Authenticate` may return one; today it would resume and be downgraded at once), so it resyncs instead.

**Tests:** a detached session past `ExpiresAt` resumes with a valid same-principal token; resyncs when `Authenticate` rejects the token, and when it returns an already-expired identity; resyncs with another principal; a connected session still downgrades at `ExpiresAt` as today.

## 4. The `cometgo` CLI

A new command, `cmd/cometgo` (`go install github.com/tmichov/cometgo/cmd/cometgo@latest`), also the future home of the auth-setup command. It wraps Expo's CLI and the platform tools; it never reimplements a build. Each command checks its prerequisites first (Node, the Android SDK, `adb`, Xcode) and says what to install.

### 4.1 Commands

| Command | Does |
|---|---|
| `cometgo mobile init [dir]` | Scaffolds an Expo SDK 53+ app (default `mobile/`) from a template embedded in the binary: `@cometgo/client`, `expo-dev-client`, NetInfo and get-random-values, a `comet.ts` with `connectNative`, a root `CometProvider`, an `app.config.js` (so `COMET_INSECURE` can switch cleartext per build) with the name and bundle id from flags (letters and digits only: iOS refuses `_`, Android refuses `-` and Java keywords) and an iOS `NSLocalNetworkUsageDescription` (a physical iPhone reaching a LAN address prompts for local network access). Refuses a non-empty directory. |
| `cometgo run android\|ios [--port N] [--server URL] [--expo-go]` | Runs the app against the dev server as a development build (`npx expo run:android\|ios`), with `EXPO_PUBLIC_COMET_PORT` set to the server's port (default 8080) and, only with `--server`, `EXPO_PUBLIC_COMET_URL`. Development builds, not Expo Go, by default: Expo Go supports only the latest SDK, so a pinned template would break at every SDK release; `--expo-go` uses it anyway. Warns when the app's `.env` already sets `EXPO_PUBLIC_COMET_URL`, which would silently win. |
| `cometgo build android --server URL [--insecure]` | A release APK/AAB pointed at a production server: `npx expo prebuild`, then Gradle with `EXPO_PUBLIC_COMET_URL` in its environment (the value is inlined when Gradle bundles the JavaScript). Refuses a `ws://` server, since release builds block cleartext traffic and the app would never connect; `--insecure` allows it for that build only: the template's `app.config.js` enables cleartext (`expo-build-properties`) when `COMET_INSECURE` is set, and `prebuild --clean` runs every time, so a later secure build carries nothing over. Warns when the release is signed with the template's debug keystore. |

**iOS builds are out of M6.** `xcodebuild archive` produces an archive, not an installable app; exporting one needs a team, a provisioning profile and an `ExportOptions.plist`. `docs/mobile.md` explains building iOS with Xcode or EAS; `cometgo build ios` is a later item.

### 4.2 Reaching the dev server

No LAN-address detection: Docker bridges, VPNs and several interfaces make it guess wrong.

The app connects to the host Expo already chose for Metro (`Constants.expoConfig.hostUri`), on the server's port:

- **Android emulator and USB device:** `cometgo run android` also runs `adb reverse tcp:<port> tcp:<port>`, so the port is reachable whether Expo chose `localhost` or the LAN address.
- **iOS simulator:** `localhost` works as it is.
- **Physical device over Wi-Fi:** the LAN address Expo chose. The docs say the dev server must then listen on all interfaces (`:8080`, not `localhost:8080`).
- **`--server`** overrides all of it.

The template's `cometURL()`: `EXPO_PUBLIC_COMET_URL` if set (`build`, `run --server`), else `ws://<hostUri host>:<EXPO_PUBLIC_COMET_PORT>/_comet` in development, else an error.

### 4.3 Schema and updates

- **Schema.** The typed schema comes from the app's own binary (`Describe`), which the CLI cannot run. The docs show writing it to both `web/src/schema.ts` and `mobile/schema.ts` from the app's `-gen` flag, as the todo example will.
- **Hot code push.** Expo's equivalent of Meteor's is `expo-updates`, which needs an update server; serving it from the CometGo server is a later item. The docs show using the `build` handshake (`ConnectOptions.build`, `serverBuild`) as the signal to check for an update.

## 5. Server

- **Resume after expiry** (§3): the one server change.
- **Origin:** React Native's WebSocket sends an `Origin` derived from the URL, which the server's same-origin check (host:port against `Host`) accepts, and a missing `Origin` is allowed. To confirm on both platforms in the first task.

## 6. Example, docs and CI

- `examples/todo/mobile`: the todo app in React Native, scaffolded with `cometgo mobile init`, sharing the generated schema.
- `docs/mobile.md`: setup and minimum versions, `connectNative`, tokens, the lifecycle and background behaviour, the resume window and `ResumeGrace`, the CLI, reaching the dev server, cleartext and release signing, iOS builds, updates.
- **CI:**
  - The client's tests in a React Native-like environment: no `window.addEventListener`, no `document`, no `crypto`, `URL` deleted; the lifecycle and probe logic with fake `AppState`, `NetInfo` and sockets: the probe timeout, a probe during a large replay not dropping the socket, the probe limits, dropping without `onclose`, late frames and a late `onopen` from a dropped socket ignored, `#opening`, the first NetInfo event, signal-strength events.
  - `npx expo export --platform android` and `--platform ios` on the example: proves module resolution (the `exports` map, peer dependencies) and that the bundle compiles; it executes nothing, so runtime behaviour comes from the tests above and the exit criterion below.
  - No emulator in CI.

## 7. Exit criterion

The resumed/resynced outcome is observable: the client exposes whether its last connect resumed (`comet.resumed`), and the example shows it on screen.

On a physical Android device, with the todo example run by `cometgo run` (the Android emulator crashes under KVM on the development machine, §9; the same checks on an iOS simulator are a separate task, for a Mac):

- subscribes and shows rows; a call's result arrives after its data (read-your-writes);
- backgrounded 30 s, then foregrounded: the data is current, with no resync and no error (the socket stays open on Android);
- with `closeInBackground`, backgrounded 30 s: `resumed` is true;
- with `closeInBackground`, backgrounded past `ResumeGrace` (run with `Heartbeat` 5 s and `ResumeGrace` 20 s): resyncs to the right state, no error shown;
- with `closeInBackground`, a token expiring while backgrounded, then foregrounded with a refreshable token: `resumed` is true (§3);
- Wi-Fi off for 30 s and back on (on Samsung phones airplane mode leaves Wi-Fi on, so Wi-Fi is switched off directly): reconnects within 5 s of the network returning;
- `cometgo build android --server wss://…` produces a release APK that installs and passes the checks above against that server, and also:
  - Wi-Fi to cellular: reconnects within 5 s (against the public server; a dev server is reachable over only one of them),
  - forced Doze (`adb shell dumpsys battery unplug`, screen off, `adb shell dumpsys deviceidle force-idle`), then wake: reconnects within 5 s, still logged in.

Not exercised: `NSLocalNetworkUsageDescription`, which only a physical iPhone on a LAN address triggers.

CI bundles the example for both platforms and runs the client's React Native environment tests.

## 8. Out of M6

Native Swift/Kotlin/Flutter clients, the Capacitor WebView build, `cometgo build ios`, hot code push served by CometGo, offline data and writes, push notifications, an emulator in CI.

## 9. Device findings (CMTG-111)

Measured on 2026-10-19 on a Samsung Galaxy S22 (Android 15, API 35) over USB with `adb reverse`, in an Expo SDK 58 / React Native 0.88 app (Hermes), development and release builds, against a test server with `Heartbeat` 5 s and `ResumeGrace` 20 s. The Android emulator (37.2.12) segfaults under KVM on the development machine's kernel (7.1.13), with or without a window and whatever the GPU mode; without KVM it runs but is too slow to use. iOS was not tested (CMTG-121).

| Check | Result | Consequence |
|---|---|---|
| Globals | `window` exists without `addEventListener`; no `document`, `crypto` or `location`; `URL` exists | §1 confirmed |
| The current client's constructor | without a polyfill: `ReferenceError: Property 'crypto' doesn't exist`; with `react-native-get-random-values`: `TypeError: undefined is not a function` (`window.addEventListener`) | §1 confirmed; both fixed by CMTG-112/113 |
| `URL` setters (`protocol`, `search`) | work, and `searchParams` exists: Expo's `URL` is standard | §2.2 is needed for bare React Native only |
| `onmessage` after `close()` | 6 messages delivered after `close()` on a socket receiving 1 per ms | the `onmessage` guard of §2.1 is required |
| `close()` on a connecting socket | `readyState` becomes 2, but the socket then **opens** 3 s later and delivers a message; no `onclose` within 8 s | a dropped connecting socket must be closed in its `onopen` (§2.1); without it, it leaks |
| `onclose` after `close()` on an unresponsive peer | 60 s, code 1006 (OkHttp waits for the close frame) | dropping must not wait for `onclose` (§2.1) |
| `Origin` on the upgrade | `http://localhost:8080` for `ws://localhost:8080`; accepted by the same-origin check | §5: no server change |
| NetInfo at subscribe | **three** identical events, not one | the lifecycle compares `(isConnected, type, ipAddress)` with the last seen value rather than skipping one event |
| NetInfo on Wi-Fi off | `none` and a Wi-Fi event with `isInternetReachable: false`, then `cellular` (reachability `false`, then `true`, then again with `cellularGeneration` set); cellular has no `ipAddress` | the tuple comparison wakes once on the change to cellular; reachability flips are ignored as designed |
| Airplane mode (`cmd connectivity airplane-mode enable`) | no NetInfo event: Samsung keeps Wi-Fi on in airplane mode when it was on | the exit criterion switches Wi-Fi off directly |
| 40 s in the background | JavaScript timers paused (no ticks, no pings); the socket stayed open, the native WebSocket kept answering the server's heartbeat; on return everything resumed | §2.5 rewritten: no delayed close is possible; `closeInBackground` closes at once. Once, after Wi-Fi and airplane toggles, the socket was reset during a background spell and `onclose` (1006, "Connection reset") arrived on return; not reproduced |
| Forced Doze for 60 s | the server dropped the socket (no pong); on wake `onclose` arrived at once (1006) | wake then finds no socket and reconnects (§2.1) |
| Release build, `ws://localhost` | blocked: the WebSocket fails at once (1006) and `fetch` to `http://localhost` reaches nothing | §4.1's refusal of `ws://` for release builds is right |
| Toolchain | Gradle 9.4.1 builds with JDK 25 (the system JDK; Fedora 44 no longer packages JDK 17); a clean debug build takes about 8 minutes; the release build is signed with the debug keystore | the CLI's prerequisite check accepts JDK 17 or later; `build android`'s debug-keystore warning is needed |

## Review

A sparring agent reviewed the design from the code over two rounds and ended at AGREE WITH CHANGES; the changes are applied above. Two Expo facts were checked against current documentation (Expo's standard `URL`; Metro resolving `exports` by default from SDK 53); the React Native socket behaviours it relies on (late frames after `close()`, OkHttp and SocketRocket on dead connections, `Origin`) are from memory and the first task checks them on devices. What the review forced:

- `wake` as a bounded probe instead of a reconnect, since a dead socket usually looks open and an idle healthy one sends JavaScript nothing (§2.1),
- dropping a socket without waiting for `onclose`, and ignoring everything a dropped socket does afterwards (§2.1),
- one `#open` at a time, so two wakes cannot run the token function twice (§2.1),
- the token refresh on wake, for Doze (§2.1), and opt-in background disconnect (§2.5),
- resume after expiry for detached sessions, with what it does not cover (§3),
- refusing cleartext release builds, per-build `--insecure`, and no LAN-address guessing (§4),
- development builds by default, and `build ios` out of M6 (§4),
- an exit criterion that can be observed and that runs on a physical device and a release build (§7).

## Tasks

Filed in Backlog (module `mobile`) with these dependencies:

| Task | What | Depends on |
|---|---|---|
| CMTG-111 | Verify the React Native behaviours this design takes from memory, on devices; append the findings here | |
| CMTG-112 | Client: lifecycle hook, wake probe and its limits, dropping without `onclose`, stale-socket guards, token refresh on wake, `closeInBackground`, `resumed`, no `URL` setters (§2.1, §2.2, §2.5) | |
| CMTG-113 | `@cometgo/client/react-native`: `connectNative`, the `AppState`/`NetInfo` lifecycle, peer dependencies (§2.3) | CMTG-112 |
| CMTG-114 | Server: resume a detached session whose identity expired, for the same principal (§3) | |
| CMTG-115 | `cometgo` CLI and `mobile init` with the Expo template and `cometURL()` (§4.1–§4.3) | |
| CMTG-116 | `cometgo run android\|ios` (§4.1, §4.2) | CMTG-115 |
| CMTG-117 | `cometgo build android` (§4.1) | CMTG-115 |
| CMTG-118 | The todo example as a mobile app; CI bundling (§6) | CMTG-113, CMTG-115, CMTG-116 |
| CMTG-119 | `docs/mobile.md` and the client and server docs (§6) | CMTG-112, CMTG-113, CMTG-114, CMTG-116, CMTG-117 |
| CMTG-120 | Run the exit criterion (§7) | CMTG-111, CMTG-112, CMTG-113, CMTG-114, CMTG-116, CMTG-117, CMTG-118 |
