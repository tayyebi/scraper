# Quickstart

Run a hub, attach a real browser to it, drive that browser over an API, and read
the page back out of the hub's mirror.

This is deliberately the last mile that CI cannot prove. CI compiles the hub,
vets it, runs its tests, and packages the extensions — what it does **not** do is
run a browser. So the steps below are the ones that actually demonstrate the
system works, and they end with a check you can see with your own eyes.

## 1. Get a hub

Grab `hubd` from the latest CI run's artifacts, or build it:

```sh
go build -o hubd ./cmd/hubd
```

Start it:

```sh
./hubd -addr :8080 -data ./data
```

On a database with no keys it prints one, once:

```
────────────────────────────────────────────────────────────────────────
This hub had no API keys, so one was created for you:

    k_01JABCDEFGHJKMNPQRSTVWXYZ...

It has the admin scope. It is shown once and is not recoverable -- store
it now. Revoke it from the console once you have minted your own.
────────────────────────────────────────────────────────────────────────
```

Keep it:

```sh
export HUB=http://localhost:8080
export KEY=k_01JABC...
```

To use the Operator Console, give it a password too:

```sh
./hubd hash-password          # prints a hash on stdout; type the password at the prompt
./hubd -console-user you -console-password-hash 'pbkdf2-sha256$210000$...'
```

Then open <http://localhost:8080/console/>.

> The password is read from stdin rather than a flag on purpose: a password on a
> command line lands in shell history and in the process table.

## 2. Build the agent

```sh
go run ./cmd/pack
```

That writes `dist/chrome/` and `dist/firefox/` (unpacked, for loading during
development) plus `dist/chrome.zip` and `dist/firefox.xpi`.

There is no `npm install` and no `node_modules`. `pack` copies the shared agent
core in beside each host adapter and verifies every file the manifests reference
actually exists.

**Chrome:** `chrome://extensions` → enable Developer mode → *Load unpacked* →
select `dist/chrome`.

**Firefox:** `about:debugging#/runtime/this-firefox` → *Load Temporary Add-on* →
select `dist/firefox/manifest.json`.

## 3. Mint an enrollment token

```sh
curl -sX POST $HUB/v1/enrollments \
  -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' \
  -d '{"labels":{"env":"dev"}}' | jq -r .token
```

Or click **Mint enrollment token** in the console.

The token is one-time and short-lived. It buys the agent its own credential and
is worthless afterwards — which is why it is safe to paste and why a leaked one
grants nothing.

## 4. Enroll the browser

Click the extension's toolbar button. Paste:

- **Hub URL** — `http://localhost:8080`
- **Enrollment token** — the `e_...` from the previous step
- **Name** — whatever you want to see in the console

Press **Connect**. The popup should turn `online`, and the agent appears in the
console's Agents table with a green status.

```sh
curl -s $HUB/v1/agents -H "Authorization: Bearer $KEY" | jq '.agents[] | {id, name, status, captureGaps}'
```

Note the `captureGaps`. A Chrome agent in its default mode says, in writing,
that it cannot see navigations or subresources. A Firefox agent reports none,
because `filterResponseData` genuinely sees everything.

## 5. Open a session and steer it

```sh
AGENT=$(curl -s $HUB/v1/agents -H "Authorization: Bearer $KEY" | jq -r '.agents[0].id')

SESSION=$(curl -sX POST $HUB/v1/agents/$AGENT/sessions \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"url":"https://example.com","active":true}' | jq -r .id)

echo $SESSION
```

A tab opens in your browser, with an orange bar across the top reading *This tab
is under automation control*. That bar is not decoration and cannot be dismissed
by the page: enrollment is the one consent gesture, commands run unattended
afterwards, and the bar is what keeps that grant honest. Its **Stop** button
ends control immediately.

Drive it:

```sh
curl -sX POST "$HUB/v1/sessions/$SESSION/commands?wait=30s" \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"op":"waitFor","params":{"selector":"h1"}}' | jq .state

curl -sX POST "$HUB/v1/sessions/$SESSION/commands?wait=30s" \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"op":"extract","params":{"fields":{"heading":"h1","blurb":"p"}}}' | jq .result
```

## 6. The check that matters

Read the DOM back out of the hub:

```sh
curl -s "$HUB/v1/sessions/$SESSION/dom?format=html" -H "Authorization: Bearer $KEY"
```

**Confirm the mirror matches the page.** The heading and paragraph you see in
your browser should be in that HTML.

This request never touched the browser. The hub answered from its own
materialized copy, kept current by a sequence-numbered mutation stream. To see
that live, scroll or interact with the page and read it again — the content
changes and `X-Hub-Mirror-Seq` advances:

```sh
curl -sD- -o/dev/null "$HUB/v1/sessions/$SESSION/dom" -H "Authorization: Bearer $KEY" | grep -i mirror-seq
```

## 7. The thing headless cannot do

Now the actual point of the project.

1. In the same browser, open a site you are **logged into**. Your real profile,
   your real cookies, your real session.
2. Click the extension button → **Attach the current tab**.
3. `GET $HUB/v1/sessions` — it appears with `"origin": "attached"`.
4. Read its DOM through the hub.

You are reading a logged-in page that no headless browser could have reached,
because reaching it required a human to sign in. That is what BYOB means here.

Closing an attached session asks the agent to *detach* rather than closing the
window — the tab was somebody's before it was the hub's.

## 8. Watch the traffic

```sh
curl -s "$HUB/v1/sessions/$SESSION/events" -H "Authorization: Bearer $KEY" \
     -H 'Accept: application/x-ndjson' | jq -c '{type, seq}'
```

```sh
curl -s "$HUB/v1/sessions/$SESSION/requests" -H "Authorization: Bearer $KEY" \
  | jq '{gaps: .captureGaps, urls: [.exchanges[].url]}'

curl -s "$HUB/v1/sessions/$SESSION/har?bodies=1" -H "Authorization: Bearer $KEY" > session.har
```

`session.har` opens in browser devtools, Charles, or any HAR viewer. If the
capture was partial, the file says so in `log.comment` — the warning has to
survive being downloaded and mailed around.

## Android

The **Firefox add-on is the Android deliverable** and it ships today. Install
`dist/firefox.xpi` on Firefox for Android through an add-on collection
([Mozilla's instructions](https://extensionworkshop.com/documentation/publish/distribute-sideloading/)),
open the add-on's popup, and enroll exactly as above. It is full-fidelity
capture, not a reduced build.

The **WebView host app does not ship.** `android/` holds its host-adapter seam
and a written-down account of what it would have to solve, marked non-functional
rather than faked. See [android/README.md](../android/README.md).

## Production notes

- **Terminate TLS at a proxy.** The command channel needs HTTP/1.1, because a
  WebSocket upgrade needs `http.Hijacker` and that does not exist under HTTP/2.
  `hubd` pins HTTP/1.1 on its own TLS listener; behind nginx or Caddy, make sure
  the proxy speaks HTTP/1.1 upstream and forwards `Upgrade`/`Connection`.
- **SSE through nginx** needs `proxy_buffering off`. The hub sets
  `X-Accel-Buffering: no`, which nginx honours; other proxies may need their own
  setting, or a live stream arrives as one long silence followed by a burst.
- **Retention** defaults to 14 days and an 8 GiB artifact cap
  (`-retention`, `-max-blob-bytes`). Bodies are the part that grows without
  bound.
- **Mint scoped keys.** Give a scraper `steer`, not `admin`, and revoke the
  bootstrap key once you have your own.

## Troubleshooting

| Symptom | Cause |
| --- | --- |
| Popup stuck on `connecting` | wrong hub URL, or the hub is not reachable from the browser |
| Popup says `rejected` | the credential was revoked; mint a new token and re-enroll |
| Enrollment returns 409 | that token was already spent — they are one-time |
| Agent goes offline every ~30s (Chrome) | the service worker is being evicted; check the ping is reaching the hub |
| `503` with `Retry-After` | the agent holds no channel right now |
| `422` on a command | the agent never advertised that op — check `captureGaps`/`ops` |
| DOM is empty or stale | the mirror hit a gap and is waiting for a re-snapshot; try `?fresh=1` |
| Console login does nothing | no `-console-user`/`-console-password-hash`; the page says so |
