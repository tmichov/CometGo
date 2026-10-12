# Stability policy

CometGo is pre-1.0. This page says what can change between releases, and how you will know.

## Versions

The Go module (`github.com/tmichov/cometgo`) and the npm package (`@cometgo/client`) follow [semantic versioning](https://semver.org). They are versioned separately, so their numbers do not have to match.

While the major version is 0:

- **Minor releases (0.1 → 0.2) may break the API**: Go functions, types and `Config` fields, the TypeScript client's exports, defaults, and documented behaviour. Each breaking change is listed in the release notes, together with what to change in your code.
- **Patch releases (0.1.0 → 0.1.1) do not break anything.** They fix bugs and may add things.
- Anything not in these docs (unexported Go identifiers, files under `client/src` other than the package exports, log messages, metric names not listed in [Metrics](server.md#metrics)) can change in any release.

After 1.0, breaking changes need a new major version: `github.com/tmichov/cometgo/v2` for the Go module.

## The wire protocol

The protocol is versioned separately from both packages, by its WebSocket subprotocol. The current version is `cometgo.1.json` ([protocol §1](protocol.md#1-transport)).

- Within a protocol version, changes are additive only. New messages, fields and features are negotiated through `caps`, and both sides ignore what they don't know. So any server and client that both speak `cometgo.1.json` can talk to each other, whatever their package versions.
- A change that is not additive gets a new subprotocol name (`cometgo.2.json`).

## The database side

The trigger functions and notification payloads that `InstallTriggers` creates are internal ([protocol §11](protocol.md#11-postgres-change-feed-server-internal-v1)). If a release changes them, its release notes say so, and you re-run `InstallTriggers` (it is idempotent) before starting the new version. The same goes for the table `InstallIdempotency` creates.
