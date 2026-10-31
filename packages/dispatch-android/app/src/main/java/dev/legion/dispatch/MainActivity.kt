package dev.legion.dispatch

import android.Manifest
import android.annotation.SuppressLint
import android.app.AlertDialog
import android.content.ActivityNotFoundException
import android.content.Intent
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.text.InputType
import android.view.ViewGroup
import android.webkit.CookieManager
import android.webkit.ValueCallback
import android.webkit.WebChromeClient
import android.webkit.WebResourceRequest
import android.webkit.WebView
import android.webkit.WebViewClient
import android.widget.EditText
import android.widget.FrameLayout
import android.widget.Toast
import androidx.activity.ComponentActivity
import androidx.activity.OnBackPressedCallback
import androidx.activity.result.contract.ActivityResultContracts
import androidx.browser.customtabs.CustomTabsIntent

/**
 * The dashboard in a WebView. The app adds nothing to the page: it renders and navigates exactly
 * as a phone browser would, and links that leave Dispatch open in the browser. Sign-in runs in a
 * Custom Tab (startSignIn), since Google refuses it inside a WebView.
 */
class MainActivity : ComponentActivity() {
    private lateinit var webView: WebView
    private var fileCallback: ValueCallback<Array<Uri>>? = null

    private val pickFiles =
        registerForActivityResult(ActivityResultContracts.GetMultipleContents()) { uris ->
            fileCallback?.onReceiveValue(uris.toTypedArray())
            fileCallback = null
        }

    private val askNotifications =
        registerForActivityResult(ActivityResultContracts.RequestPermission()) { startWatching() }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        InboxNotifier.createChannels(this)
        webView = buildWebView()
        setContentView(webView)
        onBackPressedDispatcher.addCallback(this, object : OnBackPressedCallback(true) {
            override fun handleOnBackPressed() {
                if (webView.canGoBack()) {
                    webView.goBack()
                } else {
                    isEnabled = false
                    onBackPressedDispatcher.onBackPressed()
                }
            }
        })
        if (savedInstanceState != null) {
            webView.restoreState(savedInstanceState)
        } else {
            open(intent)
        }
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        open(intent)
    }

    override fun onSaveInstanceState(outState: Bundle) {
        super.onSaveInstanceState(outState)
        webView.saveState(outState)
    }

    override fun onPause() {
        super.onPause()
        // The watch service reads the session cookie from disk; write it there now.
        CookieManager.getInstance().flush()
    }

    private fun open(intent: Intent?) {
        if (intent?.action == ACTION_CHANGE_SERVER) {
            promptForServer()
            return
        }
        val base = DispatchServer.baseUrl(this)
        if (base == null) {
            promptForServer()
            return
        }
        val data = intent?.data
        if (data?.scheme == SIGN_IN_SCHEME) {
            // The browser's sign-in handed the session back: redeem it here, in the WebView's own
            // cookie store. A handoff with no sign-in in flight is not ours to redeem.
            val handoff = data.getQueryParameter("handoff")
            val verifier = DispatchServer.takeSignInVerifier(this)
            if (handoff != null && verifier != null) {
                webView.loadUrl("$base/auth/app?handoff=${Uri.encode(handoff)}&verifier=${Uri.encode(verifier)}")
                return
            }
        }
        val path = intent?.getStringExtra(InboxNotifier.EXTRA_PATH) ?: "/"
        webView.loadUrl(base + path)
    }

    private fun promptForServer() {
        val input = EditText(this).apply {
            inputType = InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_VARIATION_URI
            hint = getString(R.string.server_hint)
            setText(DispatchServer.baseUrl(this@MainActivity) ?: "https://")
        }
        val frame = FrameLayout(this).apply {
            val pad = (20 * resources.displayMetrics.density).toInt()
            setPadding(pad, pad / 2, pad, 0)
            addView(input)
        }
        AlertDialog.Builder(this)
            .setTitle(R.string.server_title)
            .setView(frame)
            .setCancelable(DispatchServer.baseUrl(this) != null)
            .setPositiveButton(R.string.server_save, null)
            .create()
            .apply {
                setOnShowListener {
                    getButton(AlertDialog.BUTTON_POSITIVE).setOnClickListener {
                        val url = input.text.toString().trim()
                        if (!DispatchServer.isUsableUrl(url)) {
                            input.error = getString(R.string.server_invalid)
                            return@setOnClickListener
                        }
                        DispatchServer.setBaseUrl(this@MainActivity, url)
                        dismiss()
                        webView.loadUrl(DispatchServer.baseUrl(this@MainActivity) + "/")
                    }
                }
            }
            .show()
    }

    @SuppressLint("SetJavaScriptEnabled")
    private fun buildWebView(): WebView {
        val view = WebView(this)
        view.layoutParams = ViewGroup.LayoutParams(
            ViewGroup.LayoutParams.MATCH_PARENT,
            ViewGroup.LayoutParams.MATCH_PARENT,
        )
        with(view.settings) {
            javaScriptEnabled = true
            domStorageEnabled = true
        }
        CookieManager.getInstance().setAcceptCookie(true)
        CookieManager.getInstance().setAcceptThirdPartyCookies(view, true)

        view.webViewClient = object : WebViewClient() {
            override fun shouldOverrideUrlLoading(view: WebView, request: WebResourceRequest): Boolean {
                val url = request.url
                if (url.scheme != "http" && url.scheme != "https") return openOutside(url)
                if (url.host == dispatchHost() && url.path == "/auth/start") return startSignIn(url)
                // A redirect, or a page that has already left Dispatch, stays in the app.
                if (request.isRedirect || Uri.parse(view.url ?: "").host != dispatchHost()) {
                    return false
                }
                if (url.host == dispatchHost()) return false
                // A link a person followed out of Dispatch: a pull request, a document elsewhere.
                return if (request.hasGesture()) openOutside(url) else false
            }

            override fun onPageFinished(view: WebView, url: String) {
                if (Uri.parse(url).host == dispatchHost() && DispatchServer.signedIn(this@MainActivity)) {
                    CookieManager.getInstance().flush()
                    requestNotificationsThenWatch()
                }
            }
        }
        view.webChromeClient = object : WebChromeClient() {
            override fun onShowFileChooser(
                webView: WebView,
                filePathCallback: ValueCallback<Array<Uri>>,
                fileChooserParams: FileChooserParams,
            ): Boolean {
                fileCallback?.onReceiveValue(null)
                fileCallback = filePathCallback
                val type = fileChooserParams.acceptTypes.firstOrNull { it.isNotBlank() } ?: "*/*"
                pickFiles.launch(type)
                return true
            }
        }
        return view
    }

    private var askedForNotifications = false

    private fun requestNotificationsThenWatch() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU && !askedForNotifications &&
            checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) !=
            android.content.pm.PackageManager.PERMISSION_GRANTED
        ) {
            askedForNotifications = true
            askNotifications.launch(Manifest.permission.POST_NOTIFICATIONS)
            return
        }
        startWatching()
    }

    private fun startWatching() {
        InboxPollWorker.schedule(this)
        InboxWatchService.start(this)
    }

    private fun dispatchHost(): String? = DispatchServer.host(this)

    /**
     * Signs in through a Custom Tab, the browser Google accepts. The session it ends with is the
     * browser's, so `/auth/start?app=` has Dispatch hand it back to the app as a handoff only this
     * install's verifier redeems (open).
     */
    private fun startSignIn(url: Uri): Boolean {
        val challenge = DispatchServer.newSignInChallenge(this)
        CustomTabsIntent.Builder().build()
            .launchUrl(this, url.buildUpon().appendQueryParameter("app", challenge).build())
        return true
    }

    private fun openOutside(url: Uri): Boolean {
        try {
            startActivity(Intent(Intent.ACTION_VIEW, url))
        } catch (e: ActivityNotFoundException) {
            Toast.makeText(this, url.toString(), Toast.LENGTH_SHORT).show()
        }
        return true
    }

    companion object {
        const val ACTION_CHANGE_SERVER = "dev.legion.dispatch.CHANGE_SERVER"
        const val SIGN_IN_SCHEME = "dev.legion.dispatch"
    }
}
