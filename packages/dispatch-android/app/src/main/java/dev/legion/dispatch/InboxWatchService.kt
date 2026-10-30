package dev.legion.dispatch

import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.os.Build
import android.os.Handler
import android.os.HandlerThread
import android.os.IBinder
import android.os.SystemClock
import android.util.Log
import androidx.core.app.NotificationCompat
import androidx.core.app.ServiceCompat
import androidx.core.content.ContextCompat
import kotlin.math.min
import kotlin.random.Random
import okhttp3.Request
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import org.json.JSONObject

/**
 * Keeps one `GET /ws/live` socket open with an `events` subscription, the same feed the dashboard
 * reads (packages/dispatch/web/src/api/live-socket.ts), and reads the Inbox again whenever an ask
 * or issue event arrives. Nothing here reads on a timer: a quiet Inbox costs the 15-second
 * heartbeat frames and nothing else.
 *
 * It runs as a foreground service so Android does not stop it, which is why a silent "Watching your
 * Inbox" notification stays in the shade while it runs.
 */
class InboxWatchService : Service() {
    private lateinit var thread: HandlerThread
    private lateinit var handler: Handler
    private var socket: WebSocket? = null
    private var generation = 0
    private var attempt = 0
    private var lastFrameAt = 0L
    private var stopped = false

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onCreate() {
        super.onCreate()
        InboxNotifier.createChannels(this)
        thread = HandlerThread("dispatch-inbox-watch").apply { start() }
        handler = Handler(thread.looper)
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        val notification = NotificationCompat.Builder(this, InboxNotifier.CHANNEL_WATCH)
            .setSmallIcon(R.drawable.ic_notification)
            .setContentTitle(getString(R.string.watch_notification))
            .setPriority(NotificationCompat.PRIORITY_MIN)
            .setOngoing(true)
            .setContentIntent(InboxNotifier.openIntent(this, null, WATCH_NOTIFICATION_ID))
            .build()
        ServiceCompat.startForeground(
            this,
            WATCH_NOTIFICATION_ID,
            notification,
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) {
                ServiceInfo.FOREGROUND_SERVICE_TYPE_REMOTE_MESSAGING
            } else {
                0
            },
        )
        handler.post {
            if (socket == null) {
                attempt = 0
                connect()
            }
        }
        return START_STICKY
    }

    override fun onDestroy() {
        stopped = true
        handler.removeCallbacksAndMessages(null)
        socket?.close(NORMAL_CLOSURE, null)
        socket = null
        thread.quitSafely()
        super.onDestroy()
    }

    private fun connect() {
        if (stopped) return
        val base = DispatchServer.baseUrl(this)
        val cookie = DispatchServer.cookieHeader(this)
        if (base == null || cookie == null) {
            stopSelf()
            return
        }
        val url = base.replaceFirst("https://", "wss://") + "/ws/live"
        val request = Request.Builder()
            .url(url)
            .header("Cookie", cookie)
            .header("Origin", base)
            .build()
        val mine = ++generation
        lastFrameAt = SystemClock.elapsedRealtime()
        socket = DispatchServer.http.newWebSocket(request, Listener(mine))
        handler.postDelayed({ checkSilence(mine) }, SILENCE_CHECK_MS)
    }

    /** A socket that has carried no frame for three heartbeats is replaced, as the page does. */
    private fun checkSilence(mine: Int) {
        if (mine != generation || stopped) return
        if (SystemClock.elapsedRealtime() - lastFrameAt > SILENT_MS) {
            Log.i(TAG, "Live socket silent; replacing it")
            socket?.cancel()
            reconnect(mine, immediately = true)
            return
        }
        handler.postDelayed({ checkSilence(mine) }, SILENCE_CHECK_MS)
    }

    private fun reconnect(mine: Int, immediately: Boolean) {
        if (mine != generation || stopped) return
        generation++
        socket = null
        val delay = if (immediately) Random.nextLong(RECONNECT_SPREAD_MS) else backoff(attempt++)
        handler.postDelayed({ if (socket == null) connect() }, delay)
    }

    /** Equal-jitter exponential backoff, capped at 30 s (`reconnectDelayMs` in live-socket.ts). */
    private fun backoff(n: Int): Long {
        val ceiling = min(1_000L shl min(n, 5), 30_000L)
        return ceiling / 2 + Random.nextLong(ceiling / 2 + 1)
    }

    private var refreshQueued = false

    /** Coalesces a burst of events (an issue close withdraws every ask at once) into one read. */
    private fun scheduleRefresh() {
        if (refreshQueued) return
        refreshQueued = true
        handler.postDelayed({
            refreshQueued = false
            if (!InboxNotifier.refresh(this)) stopSelf()
        }, REFRESH_DEBOUNCE_MS)
    }

    private inner class Listener(private val mine: Int) : WebSocketListener() {
        override fun onOpen(webSocket: WebSocket, response: Response) {
            handler.post {
                if (mine != generation) return@post
                attempt = 0
                webSocket.send(
                    JSONObject()
                        .put("type", "subscribe")
                        .put("sub", SUBSCRIPTION)
                        .put("view", JSONObject().put("kind", "events"))
                        .toString()
                )
                // Whatever changed while no socket was open is caught up by one read.
                scheduleRefresh()
            }
        }

        override fun onMessage(webSocket: WebSocket, text: String) {
            handler.post {
                if (mine != generation) return@post
                lastFrameAt = SystemClock.elapsedRealtime()
                val frame = try {
                    JSONObject(text)
                } catch (e: org.json.JSONException) {
                    return@post
                }
                when (frame.optString("type")) {
                    "event" -> {
                        val type = frame.optJSONObject("event")?.optString("type").orEmpty()
                        if (type.startsWith("ask.") || type.startsWith("issue.")) scheduleRefresh()
                    }
                    // A lost or refused subscription: the socket is no use without it.
                    "ended", "error" -> {
                        webSocket.cancel()
                        reconnect(mine, immediately = false)
                    }
                }
            }
        }

        override fun onClosing(webSocket: WebSocket, code: Int, reason: String) {
            webSocket.close(NORMAL_CLOSURE, null)
        }

        override fun onClosed(webSocket: WebSocket, code: Int, reason: String) {
            handler.post { closed(code, null) }
        }

        override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) {
            handler.post { closed(null, response?.code) }
        }

        private fun closed(code: Int?, status: Int?) {
            if (mine != generation || stopped) return
            Log.i(TAG, "Live socket closed: code=$code status=$status")
            when {
                // Signed out: the caller no longer resolves. The Inbox read says so to the person.
                code == CALLER_GONE || status == 401 -> {
                    InboxNotifier.refresh(this@InboxWatchService)
                    stopSelf()
                }
                // A restart or the socket's age: reopen at once, spread like the page's.
                code == SERVICE_RESTART || code == MAX_AGE -> reconnect(mine, immediately = true)
                else -> reconnect(mine, immediately = false)
            }
        }
    }

    companion object {
        private const val TAG = "DispatchWatch"
        private const val WATCH_NOTIFICATION_ID = 2
        private const val SUBSCRIPTION = "inbox"
        private const val NORMAL_CLOSURE = 1000
        private const val CALLER_GONE = 1008
        private const val SERVICE_RESTART = 1012
        private const val MAX_AGE = 4001
        private const val SILENT_MS = 45_000L
        private const val SILENCE_CHECK_MS = 15_000L
        private const val RECONNECT_SPREAD_MS = 2_000L
        private const val REFRESH_DEBOUNCE_MS = 1_500L

        /** Starts watching if a session exists. Call only from the foreground (an activity, or
         *  the boot broadcast, which Android lets start this service type). */
        fun start(context: Context) {
            if (!DispatchServer.signedIn(context)) return
            ContextCompat.startForegroundService(
                context,
                Intent(context, InboxWatchService::class.java),
            )
        }
    }
}
