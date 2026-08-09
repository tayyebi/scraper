// Android WebView host adapter — CONTRACT ONLY. This does not compile into
// anything, because there is no Gradle project here on purpose: scaffolding
// that fails to build is worse than scaffolding that admits it is a sketch.
//
// See README.md in this directory for what would have to be solved, and why the
// Firefox add-on is the Android deliverable that actually ships.
//
// The shape below is deliberately the same as the object Hub.Agent takes in
// extension/shared/agent.js. Everything above this seam -- protocol, channel,
// DOM snapshot, mutation mirror, the command vocabulary, capability reporting --
// is shared JavaScript that would run unchanged inside the WebView. A new
// platform should be a new adapter, not a fork.

package io.github.tayyebi.fleetagent

/**
 * Mirrors core.Capabilities in Go and Hub.capabilities in JavaScript.
 *
 * The rule that matters: never claim a capability that is not real. A capture
 * mode that overstates itself produces a request log which looks complete and
 * is not, and nobody goes looking for what is missing.
 */
data class Capabilities(
    /**
     * On Android this would be "intercept": shouldInterceptRequest sees the
     * request but cannot observe the response the network stack produced -- it
     * can only replace it. Honest capture means proxying through the app's own
     * HTTP client, which changes the TLS fingerprint, which is a direct cost to
     * the thing BYOB exists for. That trade is the open question here.
     */
    val capture: String = "none",
    val eval: Boolean = false,
    val screenshot: Boolean = false,
    val cookies: Boolean = false,
    val attach: Boolean = false,
    val openTabs: Boolean = false,
    val mirror: Boolean = true,
    val ops: List<String> = emptyList(),
)

data class TabInfo(val tabId: Long, val url: String, val title: String)

/**
 * HostAdapter is everything the shared agent core cannot do for itself.
 *
 * Implementations must be safe to call from the channel's thread; the core
 * makes no assumptions about which one that is.
 */
interface HostAdapter {

    // ---- persistence -------------------------------------------------------
    // The agent credential lives here. It must never be readable by page
    // JavaScript, which on Android means it stays out of the WebView entirely
    // and out of anything reachable from the JS bridge.

    suspend fun storageGet(keys: List<String>): Map<String, Any?>
    suspend fun storageSet(values: Map<String, Any?>)

    // ---- tabs --------------------------------------------------------------
    // "Tab" here means a WebView instance the app owns. There is no equivalent
    // of attaching a tab a person was already using -- that is exactly the
    // capability the Firefox add-on has and this does not, and `attach` in
    // Capabilities must report false accordingly.

    suspend fun openTab(url: String, active: Boolean): TabInfo
    suspend fun closeTab(tabId: Long, origin: String)

    /**
     * Sends a command into a WebView and awaits its result.
     *
     * The bridge back from the page must be narrower than
     * addJavascriptInterface's default: that API has a long history of being
     * the way an injected script escapes the WebView.
     */
    suspend fun callTab(tabId: Long, message: Map<String, Any?>): Map<String, Any?>

    // ---- privileged operations ---------------------------------------------

    suspend fun screenshot(tabId: Long): Map<String, Any?>
    suspend fun cookies(url: String?): Map<String, Any?>

    // ---- reporting ---------------------------------------------------------

    fun capabilities(): Capabilities

    /**
     * describe() reports browser, browserVersion, platform and userAgent, which
     * the hub shows to operators picking an agent to steer.
     */
    fun describe(): Map<String, String>

    /**
     * onStatus is called as the channel changes state: connecting, online,
     * offline, rejected, error.
     *
     * On Android this must also drive the foreground-service notification.
     * Holding a WebSocket open without one gets the process killed, and a
     * persistent notification is also the honest version of the consent story:
     * the person holding the phone can always see that the agent is running.
     */
    fun onStatus(status: String, detail: String?)
}
