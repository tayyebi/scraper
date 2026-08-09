# Android WebView host — scaffolding only

**This does not work yet. Nothing here builds, and nothing here connects to a
hub.** It is the host-adapter seam and a written-down contract, landed so the
shape is fixed before anyone writes the Kotlin.

If you want a browser fleet agent on Android today, use the **Firefox add-on**,
which does ship: see [extension/firefox](../extension/firefox) and the
[quickstart](../docs/quickstart.md#android). It installs on Firefox for Android
through a collection, and it is full-fidelity capture, not a reduced build.

## Why this is a separate deliverable

The Firefox add-on and a WebView host app solve different problems:

| | Firefox add-on | WebView host app |
| --- | --- | --- |
| The browser | the user's real Firefox, with their real profile | a WebView the app owns |
| Sessions | whatever the person is already logged into | only what the app itself signs into |
| Install | add-on, from a collection | an APK, sideloaded or from a store |
| Capture | `filterResponseData`, full fidelity | `shouldInterceptRequest`, and it has real gaps |

The add-on is BYOB in the sense this project means it: *the user's own browser*.
A WebView host is a different thing — a controllable browser that happens to run
on a phone. Both are worth having. Only one of them is finished.

## What the WebView host would have to solve

These are the reasons it is not done rather than an oversight:

1. **Capture is genuinely worse.** `WebViewClient.shouldInterceptRequest` sees
   requests but gives no access to the response the network stack produces —
   you can *replace* a response, not *observe* one. Honest capture means
   proxying every request through the app's own HTTP client, which changes the
   TLS fingerprint. That is a direct cost to the thing this project exists for.
2. **Background execution.** Android will kill a process holding a WebSocket
   open. It needs a foreground service with a persistent notification, which is
   its own consent conversation and its own Play policy question.
3. **The JavaScript bridge.** The shared agent core would run inside the
   WebView via `addJavascriptInterface`, which is an attack surface with a long
   history. It needs a narrower bridge than the extension messaging it replaces.

## The seam

[`HostAdapter.kt`](HostAdapter.kt) is the contract, and it is deliberately the
same shape as the object `Hub.Agent` takes in
[extension/shared/agent.js](../extension/shared/agent.js). That is the whole
point of the agent core being separate from the host adapter: a new platform is
a new adapter, not a fork.

Everything above the adapter — protocol, channel, snapshot, mirror, commands,
capability reporting — is shared JavaScript that would run unchanged inside the
WebView.

## Status

| Piece | State |
| --- | --- |
| Host adapter contract | written down, in `HostAdapter.kt` |
| Kotlin implementation | **not started** |
| Gradle project | **not present** — an empty one that fails to build would be worse than none |
| Foreground service | **not started** |
| Capture | **not started**, and see the fidelity problem above |
