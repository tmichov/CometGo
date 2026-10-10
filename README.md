# CometGo

A realtime web framework for Go, inspired by MeteorJS.

- **Go server** that serves your UI and pushes live data over WebSocket.
- **Postgres live queries**: subscribe to a query, get an initial snapshot, then atomic diffs as the database changes.
- **Server-authoritative methods** with read-your-writes: when a call resolves, its effects are already on screen.
- **TypeScript client** with a reactive local store.

Status: early design. See [PROTOCOL.md](PROTOCOL.md) for the wire protocol.

## License

MIT
