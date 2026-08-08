# Architecture

Hexagonal: one domain core, and every plane is an adapter over it. That shape is
what makes "gRPC will also work" true by construction rather than by promise —
a second Control Plane adapter implements the same port the HTTP one does.

The Operator Console is deliberately *just another Control API client*. If the
console can do something the API cannot, the API is incomplete.

```
        ┌─────────── Control Plane (2) ───────────┐
        │ controlhttp: REST + SSE + NDJSON        │
        │ controlgrpc: port ready, adapter later  │
        └────────────────┬────────────────────────┘
                         │ core.Fleet (port)
 ┌── Operator Console (3) ──┐    ┌──────────────────┐
 │ console: go:embed assets ├────┤ core             │
 │ (a Control API consumer) │    │ Agent  Session   │
 └──────────────────────────┘    │ Command Event    │
                         │       │ Mirror Artifact  │
        ┌─── Agent Plane (1) ──┐ └────────┬─────────┘
        │ agentws: WS ingress  ├──────────┤ ports
        │ enroll·channel·blobs │          └─ Store(SQLite) · BlobStore(CAS) · EventBus
        └──────────────────────┘
```

## Direction of control

Agents dial **in**. The hub never dials out. This is not a preference — agents
run on laptops and phones behind NAT, and there is no address to dial. Every
consequence of that follows from it: the channel is reverse-connected, the hub
addresses agents by id rather than by host, and an offline agent is a lookup
miss rather than a connection timeout.

## Layout

```
cmd/hubd/main.go               single binary; flags/env; wires adapters
cmd/pack/main.go               Go-only extension bundler → zips (no Node anywhere)
internal/core/                 domain types + ports.go (Fleet, AgentLink, Store, BlobStore, EventBus)
internal/wire/                 envelope codec + hand-rolled RFC 6455
internal/registry/             live agent registry, command router, correlation + deadlines
internal/mirror/               apply snapshot + mutation ops, seq-gap detection, HTML re-serialization
internal/bus/                  in-process pub/sub, fan-out, per-subscriber backpressure and drop policy
internal/store/sqlite/         schema + migrations; WAL, busy_timeout, single writer
internal/store/blob/           content-addressed store: blobs/ab/cd/<sha256>, refcount GC, retention
internal/auth/                 enrollment tokens, agent credentials, API keys + scopes, console sessions
internal/adapters/agentws/     plane 1
internal/adapters/controlhttp/ plane 2
internal/adapters/console/     plane 3 handlers + go:embed of web/console
web/console/                   index.html, style.css, app.js — semantic, no framework, no build step
extension/shared/              agent core
extension/chrome/              MV3 manifest + Chrome host adapter
extension/firefox/             manifest + Firefox host adapter; also the Android build
android/                       WebView host-app scaffolding — placeholder, non-functional
```

## Dependencies

Exactly one direct requirement in `go.mod`: `modernc.org/sqlite`, which is pure
Go, so the binary stays static and CGO stays off. No web framework, no router
(stdlib `http.ServeMux` patterns are enough since Go 1.22), and no WebSocket
library — RFC 6455 is hand-rolled in `internal/wire`.

The extensions have no build step and no `node_modules`. `cmd/pack` zips them
with Go's `archive/zip`.

## Storage split

SQLite holds metadata and indexes. Response bodies never go in the database —
they go to a content-addressed store on disk and are referenced by digest.
Bodies are the part that grows without bound, and a content-addressed store
gives deduplication and a cheap retention sweep for free.

## Why bulk bytes avoid the channel

A captured response body would otherwise head-of-line block every command
sharing the multiplexed channel. Instead the agent `PUT`s it to
`/agent/v1/artifacts/{sha256}` and references the digest in an event. Uploads
become parallel, retryable, and skippable — an agent that already knows the hub
has a digest does not upload it twice.
