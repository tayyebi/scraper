# Control API v1

The northbound API. Everything an operator or an automation can do, and the only
thing the Operator Console talks to — if the console could do something this API
cannot, the API would be incomplete.

Base: `/v1`. All requests need a credential.

## Authentication

```http
Authorization: Bearer k_01J...
```

Or the console's session cookie, which the browser sends automatically. Two
shapes because they have genuinely different theft models; treating a cookie as
a bearer token is how CSRF happens.

### Scopes

| Scope | Can |
| --- | --- |
| `read` | list and observe: agents, sessions, DOM, events, request log, artifacts |
| `steer` | also open sessions and dispatch commands |
| `admin` | also mint enrollment tokens, revoke agents, manage keys |

They nest: `admin` implies `steer` implies `read`. Three levels a human can
reason about beat twenty a human will get wrong.

A first admin key is printed once, to stderr, the first time `hubd` starts on a
database with no keys. Without it a fresh hub is unusable: every endpoint needs
a key and the only way to mint one is an endpoint.

## Errors

```json
{"error": "agent a_01J... cannot eval: the agent did not advertise this op at enrollment"}
```

| Status | Meaning |
| --- | --- |
| 400 | malformed request, or an op outside the v1 vocabulary |
| 401 | no credential, or a revoked one |
| 403 | authenticated, but the scope is insufficient |
| 404 | no such agent, session, command, or artifact |
| 409 | the session is closed, or a one-time token was already spent |
| 413 | body over the cap |
| **422** | well-formed, but **this agent cannot do it** |
| **503** | the agent holds no channel right now — with `Retry-After` |
| 504 | `?wait` expired; the command is still live, poll its id |

422 versus 400 is the distinction worth knowing: 400 means you sent something
wrong, 422 means you sent something fine to an agent that cannot carry it out.
503 rather than 404 for an offline agent, because it is expected back.

## Agents

```http
POST   /v1/enrollments                mint a one-time enrollment token   (admin)
GET    /v1/agents                     list                               (read)
GET    /v1/agents/{id}                one                                (read)
DELETE /v1/agents/{id}                revoke the credential              (admin)
POST   /v1/agents/{id}/sessions       open a managed tab                 (steer)
```

`POST /v1/enrollments`:

```json
{"labels": {"team": "growth"}, "ttlSeconds": 3600}
```

```json
{
  "id": "e_01J...",
  "token": "e_...",
  "expiresAt": "2026-08-09T12:00:00Z",
  "note": "This token is shown once, is good for a single agent, and is spent on first use."
}
```

An agent carries what its capture mode **cannot** see:

```json
{
  "id": "a_01J...",
  "status": "online",
  "capabilities": {"capture": "fetch-patch", "eval": false, "ops": ["navigate", "click", "..."]},
  "fullFidelityCapture": false,
  "captureGaps": [
    "document navigations are not recorded",
    "subresources (img, script, css, media) are not recorded",
    "requests issued by service workers are not recorded",
    "WebSocket and EventSource traffic is not recorded",
    "a page that captures a reference to fetch before the patch installs is not recorded"
  ]
}
```

This is the point of capability reporting. A `fetch-patch` agent's request log
genuinely omits navigations, and a caller who does not know that will read an
incomplete log as a complete one. `status` is corrected from the live channel
map on every read, so a hub that was killed does not report ghosts as online.

## Sessions

```http
GET    /v1/sessions                   list (managed + attached)          (read)
GET    /v1/sessions/{id}                                                 (read)
DELETE /v1/sessions/{id}              close                              (steer)
```

Filters: `?agent=`, `?state=open|closed`, `?origin=managed|attached`, `?limit=`.

`origin` matters. A **managed** tab is one the hub opened. An **attached** tab
was handed over by a person who was using it — that is the point of BYOB, and it
is why closing one asks the agent to detach rather than closing the window out
from under them.

## Commands

```http
POST /v1/sessions/{id}/commands       ?wait=30s → 200; without → 202     (steer)
GET  /v1/commands/{id}                                                   (read)
```

```json
{"op": "navigate", "params": {"url": "https://example.com"}}
```

Both forms write the same row, which is what lets a caller whose `?wait` expired
poll `Location` rather than lose the result. `?wait` accepts `30s`, `1m`, or a
bare `30` (read as seconds), and is clamped to 5 minutes — an hour-long hold
gets dropped by every proxy in between anyway.

### Vocabulary

`navigate`, `waitFor`, `click`, `type`, `scroll`, `select`, `eval`,
`snapshotDom`, `extract`, `screenshot`, `cookies`, `back`, `forward`, `reload`,
`close`. `GET /v1/meta` returns the list this build supports.

An op the agent did not advertise is refused with 422 *before* being sent, which
turns a full deadline burn into an immediate reason.

`eval` is remote code execution as far as extension-store policy is concerned.
It is off unless an operator turns it on per agent, and it is documented as
store-incompatible; everything else stays store-legal. `extract` covers the
common scrape declaratively and is why most callers never need `eval`:

```json
{
  "op": "extract",
  "params": {
    "selector": "article.product",
    "fields": {
      "title": "h2",
      "price": ".price",
      "link": {"selector": "a", "attribute": "href"}
    }
  }
}
```

## DOM

```http
GET /v1/sessions/{id}/dom             ?fresh=1  ?format=json|html|text   (read)
```

Answers from the hub's materialized mirror with **no round trip to the browser**
— that is what the mirror is for. `?fresh=1` forces a re-snapshot. The
`X-Hub-Mirror-Seq` header carries the mutation sequence the document reflects,
so two reads can be told apart without parsing them.

If the mirror has drifted, the hub does not serve it. It demands a fresh
snapshot instead; see [protocol.md](protocol.md#the-mutation-stream).

## Events

```http
GET /v1/sessions/{id}/events                                             (read)
```

`Accept: text/event-stream` (default) for SSE, `application/x-ndjson` for
newline JSON. Two formats because `EventSource` in a browser speaks SSE and
nothing else, while a scraper piping to `jq` finds SSE's framing to be noise.

SSE carries `id:`, and a reconnecting client resumes with `Last-Event-ID`
(or `?after=`). Cursors are row ids, not timestamps: two events can share a
millisecond.

Delivery is bounded and lossy by design — a subscriber that stops reading must
never be able to stall the browser producing events. When a subscriber falls
behind it receives an `error` event carrying a `dropped` count, so a hole is
visible rather than silent.

## Request log

```http
GET /v1/sessions/{id}/requests                                           (read)
GET /v1/sessions/{id}/har             ?bodies=1                          (read)
GET /v1/artifacts/{digest}                                               (read)
```

The log carries `captureGaps` alongside the exchanges, for the same reason the
agent listing does. The HAR export carries the same warning in `log.comment`,
because that is the only place it survives the file being downloaded and mailed
around.

Bodies are digests, not bytes. `GET /v1/artifacts/{digest}` serves them as an
**attachment** under `Content-Security-Policy: sandbox` and `nosniff`: captured
bytes are somebody else's HTML and JavaScript, and serving them inline from the
hub's origin would let a captured page script the console. They are
`immutable`-cacheable, which content addressing makes trivially true.

## Keys and metadata

```http
GET    /v1/apikeys                                                       (admin)
POST   /v1/apikeys      {"name": "ci scraper", "scope": "steer"}         (admin)
DELETE /v1/apikeys/{id}                                                  (admin)
GET    /v1/meta                                                          (read)
```

A minted key is returned once. Revoked keys stay listed — an operator needs to
see what was revoked as much as what is live.

## A whole scrape

```sh
HUB=http://localhost:8080
KEY=k_...

TOKEN=$(curl -sX POST $HUB/v1/enrollments -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' -d '{}' | jq -r .token)
# paste $TOKEN into the agent's popup, then:

AGENT=$(curl -s $HUB/v1/agents -H "Authorization: Bearer $KEY" | jq -r '.agents[0].id')

SESSION=$(curl -sX POST $HUB/v1/agents/$AGENT/sessions \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com","active":true}' | jq -r .id)

curl -sX POST "$HUB/v1/sessions/$SESSION/commands?wait=30s" \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"op":"waitFor","params":{"selector":"h1"}}'

curl -s "$HUB/v1/sessions/$SESSION/dom?format=html" -H "Authorization: Bearer $KEY"
curl -s "$HUB/v1/sessions/$SESSION/requests" -H "Authorization: Bearer $KEY" | jq .captureGaps
```
