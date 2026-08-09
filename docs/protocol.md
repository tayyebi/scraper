# The command channel

The agent dials the hub; the hub issues the requests. That direction is not a
preference — agents run on laptops and phones behind NAT, and there is no
address for a hub to dial. Everything else in this document follows from it.

Implemented twice, and the two must agree exactly:

- Go: [internal/wire](../internal/wire) — `envelope.go`, plus a hand-rolled
  RFC 6455 in `frame.go`, `conn.go`, `upgrade.go`.
- JavaScript: [extension/shared/protocol.js](../extension/shared/protocol.js)
  and `channel.js`.

Go's `TestEnvelopeJSONFieldNames` pins the field names, so renaming one there
fails a test rather than silently breaking every agent in the field.

## Handshake

```
POST /agent/v1/enroll             spend a one-time token, receive a credential
GET  /agent/v1/channel            WebSocket upgrade, authenticated
PUT  /agent/v1/artifacts/{sha256} upload captured bytes
HEAD /agent/v1/artifacts/{sha256} ask whether the hub already has them
```

### Enrollment

```http
POST /agent/v1/enroll
Content-Type: application/json

{
  "token": "e_01J...",
  "name": "my laptop",
  "browser": "chrome",
  "browserVersion": "131.0",
  "platform": "macOS",
  "userAgent": "Mozilla/5.0 ...",
  "agentVersion": "0.1.0",
  "capabilities": { "capture": "fetch-patch", "eval": false, "ops": ["navigate", "..."] }
}
```

```json
{
  "agentId": "a_01J...",
  "credential": "ac_...",
  "protocolVersion": 1,
  "pingIntervalSeconds": 20,
  "maxArtifactBytes": 33554432
}
```

The token is spent by this call and is worthless afterwards. The credential is
this agent's own, is never shown again, and is what gets revoked when an
operator revokes *this device* rather than all of them. That split is the whole
reason there are two secrets — see [GLOSSARY.md](../GLOSSARY.md).

Labels on the token override labels the agent proposes. An operator minting the
token decides what a device is; an agent that could relabel itself could move
into another team's fleet.

### Opening the channel

```
GET /agent/v1/channel
Upgrade: websocket
Sec-WebSocket-Version: 13
Sec-WebSocket-Protocol: hub.v1, hub.credential.ac_...
```

The credential may travel either as `Authorization: Bearer ac_...` or in
`Sec-WebSocket-Protocol`. Browsers get the second form because **the WebSocket
API in a browser cannot set request headers** — that is not a shortcut, it is
the only header-shaped channel the platform gives a client.

A rejected credential closes with **1008**, and agents treat that as terminal:
retrying the same rejected secret forever is how a revoked device turns into a
denial-of-service against its own hub.

## Envelope

One JSON text frame shape, in both directions:

```json
{"v":1,"id":"c_01J...","t":"cmd","ts":1712345678901,"sid":"s_01J...","op":"navigate","body":{}}
```

| Field | Meaning |
| --- | --- |
| `v` | protocol version, checked on every frame, not just at handshake |
| `id` | correlation id; a `res`/`err` carries the `id` of the `cmd` it answers |
| `t` | `cmd` (hub→agent), `res`, `evt` (agent→hub), `err` |
| `ts` | milliseconds since the epoch |
| `sid` | session id; absent for agent-scoped commands like `openTab` |
| `op` | the command, or the event name |
| `seq` | mutation sequence number, on mirror events |
| `body` | payload, opaque until dispatch |
| `err` | `{code, message}` on `t:"err"` |

One shape rather than several because many sessions share one socket: every
frame has to be routable before anything knows what it is, so `id`, `sid` and
`t` sit at a fixed place and `body` stays opaque.

Binary frames are a protocol violation on this channel.

### Error codes

`unsupported`, `no_such_tab`, `timeout`, `navigation_failed`, `not_found`,
`bad_params`, `denied`, `internal`, `detached`, `mirror_gap`.

## Bulk bytes never cross the channel

A captured response body would head-of-line block every command multiplexed onto
the same socket. So the agent uploads it and references the digest:

```http
PUT /agent/v1/artifacts/9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08
Authorization: Bearer ac_...
```

The hub verifies the bytes hash to the name in the path and refuses otherwise —
a blob that does not match its own name must never exist, because every later
read would return the wrong content under a trusted one.

`HEAD` first. The store is content-addressed, so if the hub already holds these
bytes the upload is pure waste, and on a page that re-fetches the same JSON on
every scroll that is most uploads.

## Keepalive

The agent sends `{"t":"evt","op":"ping"}` roughly every 20 seconds.

This is liveness *and* life support. Chrome evicts an MV3 service worker after
about 30 seconds idle, and since Chrome 116 WebSocket traffic resets that timer
— so the ping is what keeps the agent running at all. When the socket is already
down there is no traffic to keep the worker alive, so `chrome.alarms` wakes it to
reconnect. Firefox's background page is persistent and needs neither.

The hub also sends RFC 6455 ping frames; `internal/wire` answers them from
inside the read loop without the application seeing them.

## The mutation stream

A snapshot is a photograph. A mirror is a video feed with a photograph at the
start. They are named apart because conflating them is how you end up serving a
document that is confidently wrong.

```jsonc
// seq 1: the photograph
{"t":"evt","op":"mirror.snapshot","sid":"s_...","seq":1,
 "body":{"frameId":"main","url":"...","title":"...","root":{...}}}

// seq 2, 3, ...: deltas
{"t":"evt","op":"mirror.mutation","sid":"s_...","seq":2,
 "body":{"ops":[
   {"op":"attr","id":12,"n":"class","v":"active"},
   {"op":"text","id":13,"v":"new text"},
   {"op":"insert","parent":12,"ref":14,"node":{...}},
   {"op":"remove","id":15}
 ]}}
```

Node shapes are short because a busy page's stream is almost entirely this:
`id`, `t` (nodeType), `n` (name), `v` (value), `a` (attributes), `c` (children),
`sr` (shadow root), `f` (frame id).

**A gap forces a re-snapshot.** Batch *n* is applied only after *n−1*. A hole
means the deltas that would close it are gone, so the hub stops serving the
mirror and asks the agent for a fresh snapshot. Nothing about this is
best-effort: without it, `GET /v1/sessions/{id}/dom` could answer from a
document that quietly diverged, and a caller would have no way to tell.

Agent-side, the stream is coalesced on a ~100 ms flush with a cap of 2000 ops
per batch. Past the cap the agent sends a snapshot instead — bounded work, where
an unbounded delta queue on an animating page is not.

`v: null` on an `attr` op means *remove the attribute*, which is distinct from
setting it to the empty string.

### Frames and shadow roots

Frames form a forest keyed by `frameId`; each frame snapshots itself. Open
shadow roots serialize as a marked child (`sr`). **Closed shadow roots are
inaccessible** — that is what closed means — and this is documented rather than
worked around, because working around it would mean defeating a boundary the
page deliberately set.

## Events

| `op` | Meaning |
| --- | --- |
| `session.attached` | a human handed over a tab they already had open |
| `session.updated` | tab url or title changed |
| `session.closed` | the tab is gone |
| `navigated` | the document was replaced — invalidates the mirror |
| `mirror.snapshot` / `mirror.mutation` | see above |
| `exchange` | one request/response for the request log |
| `ping` | keepalive |

Everything an agent sends is treated as untrusted. An authenticated channel says
*who* sent a message, not that its contents are sane — an agent is a browser
extension living alongside pages that would like to influence it. So an agent
cannot claim a tab is `managed` when a human handed it over, cannot close
another agent's session, and a malformed body digest is dropped rather than
recorded as a reference that could never resolve.

## Deliberate limits

- **HTTP/1.1 only.** The upgrade needs `http.Hijacker`, which does not exist
  under HTTP/2, and Go negotiates h2 automatically on a TLS listener. `hubd`
  sets an empty `TLSNextProto` to pin HTTP/1.1. Terminating TLS at a proxy works
  and is the recommended arrangement.
- **No permessage-deflate.** No extension is negotiated, so any reserved bit set
  fails the connection.
- **8 MiB per channel message.** Anything near it is a bug or an attack; real
  bulk goes to the artifact endpoint.
