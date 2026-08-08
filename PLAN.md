# Browser Fleet Controller — implementation plan

## Context

Greenfield. `/Users/tayyebi/Projects/scraper` is empty; `git@github.com:tayyebi/scraper.git` exists, is public,
default branch `main`, **zero commits**. SSH push works. There is **no Go and no Node toolchain on this machine and
none will be installed** — every build happens in GitHub Actions on pushes to `main`.

The goal is a service that turns **real, logged-in browsers** into a programmable endpoint that scrapers and
automations can drive over an API. Unlike a headless-browser service, the browser is the user's own: real profile,
real cookies, real TLS fingerprint, real session state — so it can reach pages that require a human to have logged
in. Three planes: where browsers attach, where automations drive them, where an operator watches.

---

## Terminology (requested)

"Three-headed service" is better said as **three planes**. The system is a **browser fleet controller** for
**BYOB (bring-your-own-browser) automation**. The binary is `hubd`; a deployment is "the hub".

| You said | Use instead | Why |
| --- | --- | --- |
| three heads | three **planes** | standard distributed-systems vocabulary |
| head 1, "connects to clients" | **Agent Plane** (ingress: *agent gateway*) | agents dial **in**; the hub never dials out — they sit behind NAT |
| head 2, "rest api which controls" | **Control Plane** (the *northbound* Control API) | northbound = toward operators and automation |
| head 3, "web ui administration area" | **Operator Console** | it is a console, not an "area" |
| client / extension / app | **Agent** | one noun covering all form factors |
| the extension's code | **agent core** + per-browser **host adapter** | shared JS core, thin platform shims |
| server address | **hub URL** | says what it is |
| pair token | **enrollment token** (one-time) → exchanged for an **agent credential** | a long-lived reusable pairing secret is a vulnerability; split the two |
| pairing | **enrollment** | a one-time act; the ongoing link is the *channel* |
| the persistent connection | **command channel** (reverse-connected, multiplexed) | states direction and purpose |
| "browse remotely" | **session steering** via **commands** | |
| a controlled tab | **browsing session** (a tab plus its frame tree) | |
| one action | **command** (correlation id, deadline, result) | |
| `capture()` the DOM | **DOM snapshot** — a point-in-time serialization | |
| "DOM live" | **DOM mirror**, fed by a **mutation stream** | two distinct things; naming them apart is the whole design |
| "intercept the resources" | **network capture**; each record is an **exchange** in the **request log** | |
| the captured bytes | **artifact**, in a content-addressed **blob store** | dedupes; referenced by digest |
| "observe the traffic" | **event stream** (telemetry) + request log | |
| adapters | **transport adapters** over **ports** (hexagonal) | formal name for what you described |
| "scraper" | this service is the **substrate**; a scraper is its *consumer* | keeps the boundary honest |

---

## Decisions

- **Capture — hybrid.** MAIN-world `fetch`/`XHR` patching by default (silent, covers the JSON/XHR traffic most
  scraping targets); per-agent opt-in `chrome.debugger`/CDP for full fidelity. Firefox is always full fidelity via
  `webRequest.filterResponseData`. Agents advertise **capabilities** at enrollment; the Control API reports what a
  given agent can actually capture, so a caller is never silently shortchanged.
- **Android — two separate deliverables.** The **Firefox-for-Android add-on ships this phase** (it is the Firefox
  extension, packaged for a Fenix collection). The **WebView host app does not** — this phase lands its host-adapter
  seam and placeholder scaffolding under `android/`, explicitly marked non-functional rather than faked.
- **Dependencies — no framework, no router, no WebSocket library.** RFC 6455 is hand-rolled. Exactly **one** direct
  `go.mod` require: `modernc.org/sqlite`. Still one static binary.
- **Storage — SQLite** for metadata and indexes; **content-addressed blobs on disk** for bodies (never in the DB),
  with age + total-size retention and a per-body cap.
- **Sessions — both.** The hub opens managed tabs, *and* the user can attach an already-open tab from the popup.
  Attaching is the point of BYOB: it's how you drive a session a human had to log into. Attached tabs are listed
  and detachable.
- **Consent — visible always, auto-accept.** Enrollment is the one consent gesture; commands then run unattended.
  But a controlled tab always carries a visible indicator, and the popup shows a live command log and a kill switch.
- **DOM — mirror + mutation stream.** Full snapshot, then sequence-numbered `MutationObserver` deltas; the hub keeps
  a materialized document so `capture()` answers with no round trip, and any `seq` gap forces an automatic
  re-snapshot so the mirror cannot silently drift.
- **Auth — scoped API keys + console login.** Bearer keys with `read` / `steer` / `admin` scopes, hashed at rest,
  revocable, shown once; separate cookie session for the console.

---

## Architecture

Hexagonal: one domain core, every plane an adapter over it. This is what makes "gRPC will also work" true by
construction. The console is deliberately *just another Control API client* — if the console can't do it, the API
is incomplete.

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

### Layout

```
cmd/hubd/main.go               single binary; flags/env; wires adapters
cmd/pack/main.go               Go-only extension bundler → zips (no Node anywhere)
internal/core/                 domain types + ports.go (Fleet, AgentLink, Store, BlobStore, EventBus)
internal/wire/                 envelope codec + hand-rolled RFC 6455 (handshake, frames, masking, ping/pong, close)
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
extension/shared/              agent core: channel, protocol, commands, domsnap, dommirror, netlog, capabilities
extension/chrome/              MV3 manifest + Chrome host adapter (MAIN-world patch; optional CDP)
extension/firefox/             manifest + Firefox host adapter (filterResponseData); also the Android build
android/                       WebView host-app scaffolding — placeholder, marked non-functional
.github/workflows/ci.yml       the only place anything is built
docs/protocol.md docs/api.md GLOSSARY.md ARCHITECTURE.md
```

### Wire protocol

JSON text frames, one envelope:

```json
{"v":1,"id":"c_01J...","t":"cmd|res|evt|err","ts":1712345678901,"sid":"s_...","op":"navigate","body":{}}
```

`cmd` hub→agent, `res` agent→hub correlated by `id`, `evt` unsolicited. **Bulk bytes never cross the channel** — the
agent uploads via `PUT /agent/v1/artifacts/{sha256}` and references the digest in an event, keeping the control
channel small and uploads parallel and retryable.

Mirror ops carry `seq`; a gap triggers a re-snapshot demand. Frames form a forest keyed by `frameId`; open shadow
roots serialize as a marked child. Closed shadow roots are inaccessible — documented, not worked around.

### Control API (v1)

```
POST   /v1/enrollments                mint an enrollment token
GET    /v1/agents                     list: status, labels, capabilities
DELETE /v1/agents/{id}                revoke credential
POST   /v1/agents/{id}/sessions       open a managed tab
GET    /v1/sessions                   list (managed + attached)
POST   /v1/sessions/{id}/commands     ?wait=30s → sync result; otherwise 202 + command id
GET    /v1/commands/{id}              async result
GET    /v1/sessions/{id}/dom          materialized mirror; ?fresh=1 forces a round trip; ?format=html|json
GET    /v1/sessions/{id}/events       SSE or NDJSON by Accept
GET    /v1/sessions/{id}/requests     request log
GET    /v1/sessions/{id}/har          HAR 1.2 export
GET    /v1/artifacts/{digest}         raw captured bytes
```

Agent-facing: `POST /agent/v1/enroll`, `GET /agent/v1/channel` (WS upgrade), `PUT /agent/v1/artifacts/{digest}`.

Commands v1: `navigate, waitFor, click, type, scroll, select, eval, snapshotDom, extract, screenshot, cookies,
back, forward, reload, close`.

---

## Known hazards, handled deliberately

These are the things that will otherwise be discovered at runtime, which is expensive when nothing runs locally.

1. **`go.sum` cannot be hand-written.** Without running Go I cannot produce valid module hashes. Fix: `ci.yml`'s
   first job runs `go mod tidy` and commits `go.mod`/`go.sum` back to `main` with `[skip ci]` in the message (which
   GitHub honors) to avoid a trigger loop, guarded on actor so it self-limits. Until then the build job runs with
   `GOFLAGS=-mod=mod` so a missing `go.sum` never blocks it.
2. **HTTP/2 breaks WebSocket.** `http.Hijacker` is unavailable under h2, so the hand-rolled upgrade would fail on a
   TLS listener with automatic h2. Fix: set `TLSNextProto` to an empty map, forcing HTTP/1.1, and document
   terminating TLS at a proxy as the alternative.
3. **MV3 service workers die after 30s idle.** Chrome keeps them alive on WebSocket traffic (116+), so the agent
   pings every ~20s, with a `chrome.alarms` revival as a backstop.
4. **`eval` is remote code execution** as far as extension-store policy is concerned. It is gated behind the same
   opt-in as CDP and documented as store-incompatible; everything else stays store-legal.
5. **Mirror volume.** Mutation streams on a busy page are enormous. Agent-side coalescing with a flush interval, an
   ops-per-flush cap, and overflow → force re-snapshot instead of unbounded queueing.
6. **SSE buffering.** Requires `http.Flusher` plus `X-Accel-Buffering: no` to survive reverse proxies.

---

## Delivery sequence

One pushed commit per step — CI is the compiler, so the loop must be established before anything depends on it.

1. Docs, `GLOSSARY.md`, `.gitignore`, `go.mod`, a trivially-buildable `cmd/hubd`, and `ci.yml`. **Green CI from
   commit one**, which is what makes the remaining steps verifiable at all.
2. `internal/core` — domain types and ports; no I/O, fully unit-testable.
3. `internal/wire` — envelope codec and hand-rolled RFC 6455, with table tests over the RFC's frame vectors. This
   is the riskiest hand-written component, so it gets the heaviest tests.
4. `internal/store/sqlite` + `internal/store/blob` — schema, migrations, CAS, retention/GC.
5. `internal/registry` + `internal/bus` — routing, correlation, deadlines, backpressure.
6. `internal/mirror` — snapshot/delta application, gap detection, HTML re-serialization.
7. `internal/auth` — enrollment tokens, agent credentials, scoped API keys, console sessions.
8. `agentws` (plane 1) — enroll, channel, artifact upload.
9. `controlhttp` (plane 2) — REST, SSE, NDJSON.
10. `console` (plane 3) — semantic HTML/CSS/JS, `go:embed`.
11. `extension/shared` — agent core.
12. `extension/chrome` — MV3 host adapter, MAIN-world capture, optional CDP.
13. `extension/firefox` — `filterResponseData` host adapter + the Android/Fenix packaging.
14. `cmd/pack` + CI zip artifacts.
15. `android/` WebView placeholder scaffolding.
16. Quickstart and end-to-end docs.

### Console UI conventions

Semantic elements carry the meaning and the styling: `<table>` for agents and the request log, `<template>` for
rows, `<details>` for exchange bodies, `<dialog>` for the DOM viewer, `<output>` for command results, `EventSource`
for the live feed. Styling hangs off element and **state attributes** — `td[data-status=online]`,
`[aria-current=page]`, `details[open]` — so the same attribute drives both appearance and accessibility. Effectively
no classes, no ids, no build step, three files.

---

## Verification

Nothing is built on this machine. The loop is: push to `main` → Actions builds → read the result over the public
API. Raw Actions logs require auth, but **check-run annotations on a public repo do not** — so `ci.yml` pipes
`gofmt`, `go vet`, `go build` and `go test` output through `::error file=…,line=…::` workflow commands, making every
failure readable at `GET /repos/tayyebi/scraper/commits/{sha}/check-runs` →
`GET /repos/tayyebi/scraper/check-runs/{id}/annotations`. CI also builds the extension zips via `go run ./cmd/pack`
and uploads them as artifacts.

Because I cannot execute the result, the honest statement of what CI proves is: it compiles, it is vetted and
formatted, and its unit tests pass. It does **not** prove the extension talks to the hub.

That last mile is yours, and the quickstart will cover exactly it: run `hubd`, load the unpacked extension, paste
the hub URL and enrollment token, watch the agent appear in the console, `POST` a `navigate` command, then
`GET /v1/sessions/{id}/dom` and confirm the mirror matches the page.
