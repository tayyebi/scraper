# Glossary

The vocabulary is load-bearing. Two things in this system are easy to conflate —
a *snapshot* and a *mirror*, an *enrollment token* and an *agent credential* —
and in both cases naming them apart is the design, not decoration.

## The system

**Browser fleet controller** — this service. It turns real, logged-in browsers
into a programmable endpoint. **BYOB (bring-your-own-browser) automation** is the
category: unlike a headless-browser service, the browser is the user's own —
real profile, real cookies, real TLS fingerprint, real session state — so it can
reach pages that require a human to have logged in.

**hubd** — the binary. **The hub** — a deployment of it.

**Plane** — one of the three faces of the hub. Not "head": *plane* is the
standard distributed-systems term for a slice of a system grouped by who talks
to it.

| Plane | Name | Who talks to it |
| --- | --- | --- |
| 1 | **Agent Plane** (ingress: the *agent gateway*) | browsers, dialing **in** |
| 2 | **Control Plane** (the *northbound* Control API) | automations and operators |
| 3 | **Operator Console** | a human, in a browser |

*Northbound* means toward the operator. Agents dial in and the hub never dials
out, because agents sit behind NAT on laptops and phones.

## The agent side

**Agent** — the software inside a browser that attaches it to a hub. One noun
for every form factor: Chrome extension, Firefox add-on, Android WebView host.

**Agent core** — the shared JavaScript that implements the protocol. **Host
adapter** — the thin per-browser shim underneath it. The core is where the
behaviour lives; the adapter only knows how *this* browser exposes tabs,
network interception, and storage.

**Enrollment** — the one-time act of attaching an agent to a hub. The ongoing
link is the *channel*, not "the pairing".

**Enrollment token** — a short-lived, one-time secret an operator mints and
pastes into an agent. It is spent immediately and exchanged for an agent
credential. **Agent credential** — the long-lived per-agent secret used to
authenticate the channel thereafter.

> These are split on purpose. A single long-lived reusable pairing secret is a
> vulnerability: it is copied into every agent, cannot be revoked per-device,
> and leaks by being pasted around. A one-time token that mints a per-agent
> credential is revocable per device and worthless once spent.

**Command channel** — the persistent, reverse-connected, multiplexed WebSocket
from agent to hub. *Reverse-connected* because the client dials the server but
the server issues the requests. Multiplexed because many sessions share it.

**Capabilities** — what a given agent can actually do, advertised at enrollment.
The Control API reports them so a caller is never silently shortchanged: an
agent capturing via MAIN-world `fetch` patching cannot see response bodies for
document navigations, and the API says so rather than returning a partial log
that looks complete.

## Driving a browser

**Session steering** — driving a browser through commands. Not "browsing
remotely": nothing is streamed to the operator.

**Browsing session** — a tab plus its frame tree. *Managed* if the hub opened
it; *attached* if a human handed over an already-open tab. Attaching is the
point of BYOB — it is how you drive a session a human had to log into.

**Command** — one action, with a correlation id, a deadline, and a result.

**DOM snapshot** — a point-in-time serialization of a document.

**DOM mirror** — a materialized copy of the live document, kept current by a
**mutation stream** of sequence-numbered deltas. A snapshot is a photograph; a
mirror is a video feed with a photograph at the start. A gap in `seq` forces an
automatic re-snapshot, so the mirror cannot silently drift.

## Capturing traffic

**Network capture** — recording a session's HTTP traffic. Each record is an
**exchange** (request plus response), and the collection is the **request log**.

**Artifact** — captured bytes. Artifacts live in a content-addressed **blob
store** and are referenced by digest, so identical bodies stored twice cost one
copy. Bulk bytes never cross the command channel.

**Event stream** — the live telemetry feed (lifecycle, navigation, mutation,
capture events), distinct from the request log.

## Internals

**Port** and **transport adapter** — the hexagonal-architecture terms for the
structure described in ARCHITECTURE.md. A port is an interface the domain core
defines; an adapter is an implementation that speaks some wire protocol.

**Substrate** — what this service is. A scraper is its *consumer*. The hub
exposes browsers; it does not know what anyone extracts from them.
