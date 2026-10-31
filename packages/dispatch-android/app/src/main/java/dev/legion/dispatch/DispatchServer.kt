package dev.legion.dispatch

import android.content.Context
import android.net.Uri
import android.util.Base64
import android.webkit.CookieManager
import java.io.IOException
import java.security.MessageDigest
import java.security.SecureRandom
import java.util.concurrent.TimeUnit
import okhttp3.OkHttpClient
import okhttp3.Request

/** Raised when Dispatch no longer accepts the WebView's session: the person must sign in again. */
class SignedOutException : IOException("Dispatch answered 401: the session cookie is not accepted")

/**
 * The one Dispatch server the app talks to, and the session the WebView signed in with. Native
 * reads send the WebView's own cookies, so there is no second sign-in and no token to keep: the
 * `dsession` cookie is what the dashboard itself sends, and `GET /api/v1/inbox` is a human-only
 * route that refuses a bearer.
 */
object DispatchServer {
    private const val PREFS = "dispatch"
    private const val KEY_URL = "server_url"
    private const val KEY_SIGN_IN_VERIFIER = "sign_in_verifier"
    private const val SESSION_COOKIE = "dsession="

    val http: OkHttpClient by lazy {
        OkHttpClient.Builder()
            .connectTimeout(15, TimeUnit.SECONDS)
            .readTimeout(30, TimeUnit.SECONDS)
            .build()
    }

    /** The server's origin, without a trailing slash, or null before one is set. */
    fun baseUrl(context: Context): String? =
        prefs(context).getString(KEY_URL, null) ?: BuildConfig.DISPATCH_URL.ifEmpty { null }

    fun setBaseUrl(context: Context, url: String) {
        prefs(context).edit().putString(KEY_URL, url.trimEnd('/')).apply()
    }

    /** Whether [url] is an https origin the app can use. */
    fun isUsableUrl(url: String): Boolean {
        val uri = Uri.parse(url.trim())
        return uri.scheme == "https" && !uri.host.isNullOrEmpty()
    }

    fun host(context: Context): String? = baseUrl(context)?.let { Uri.parse(it).host }

    fun cookieHeader(context: Context): String? {
        val base = baseUrl(context) ?: return null
        return CookieManager.getInstance().getCookie(base)
    }

    fun signedIn(context: Context): Boolean =
        cookieHeader(context)?.split(';')?.any { it.trim().startsWith(SESSION_COOKIE) } == true

    /** Reads the asks on issues assigned to the reader: the Inbox's default view. */
    @Throws(IOException::class)
    fun readInbox(context: Context): List<InboxAsk> {
        val base = baseUrl(context) ?: throw IOException("no Dispatch server is set")
        val cookie = cookieHeader(context) ?: throw SignedOutException()
        val request = Request.Builder()
            .url("$base/api/v1/inbox?assignee=me")
            .header("Cookie", cookie)
            .header("Accept", "application/json")
            .build()
        http.newCall(request).execute().use { response ->
            if (response.code == 401) throw SignedOutException()
            if (!response.isSuccessful) throw IOException("GET /api/v1/inbox answered ${response.code}")
            return parseInbox(response.body?.string() ?: "[]")
        }
    }

    /**
     * Starts a sign-in in the browser: a fresh verifier is kept for [takeSignInVerifier], and the
     * challenge made from it (RFC 7636's S256) is returned for `/auth/start?app=`.
     */
    fun newSignInChallenge(context: Context): String {
        val verifier = base64Url(ByteArray(32).also { SecureRandom().nextBytes(it) })
        prefs(context).edit().putString(KEY_SIGN_IN_VERIFIER, verifier).apply()
        return base64Url(MessageDigest.getInstance("SHA-256").digest(verifier.toByteArray()))
    }

    /** The verifier of the sign-in in flight, which only one handoff may redeem, or null. */
    fun takeSignInVerifier(context: Context): String? {
        val verifier = prefs(context).getString(KEY_SIGN_IN_VERIFIER, null)
        prefs(context).edit().remove(KEY_SIGN_IN_VERIFIER).apply()
        return verifier
    }

    private fun base64Url(bytes: ByteArray): String =
        Base64.encodeToString(bytes, Base64.URL_SAFE or Base64.NO_PADDING or Base64.NO_WRAP)

    private fun prefs(context: Context) = context.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
}
