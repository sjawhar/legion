package dev.legion.dispatch

import android.Manifest
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Build
import android.util.Log
import androidx.core.app.NotificationCompat
import androidx.core.app.NotificationManagerCompat
import androidx.core.content.ContextCompat
import java.io.IOException
import java.time.Instant

/**
 * Reads the Inbox and turns what changed since the last read into notifications. The live socket
 * ([InboxWatchService]) and the periodic backstop ([InboxPollWorker]) both call [refresh]; it is
 * serialized, so two triggers at once cannot notify one ask twice.
 */
object InboxNotifier {
    const val CHANNEL_ASKS = "asks"
    const val CHANNEL_WATCH = "watch"
    const val EXTRA_PATH = "dev.legion.dispatch.PATH"
    private const val TAG = "DispatchInbox"
    private const val PREFS = "inbox"
    private const val KEY_REMEMBERED = "remembered"
    private const val KEY_SIGNED_OUT_SHOWN = "signed_out_shown"
    private const val SIGNED_OUT_ID = 1
    private const val TAG_ASK = "ask"

    fun createChannels(context: Context) {
        val manager = context.getSystemService(NotificationManager::class.java)
        manager.createNotificationChannel(
            NotificationChannel(
                CHANNEL_ASKS,
                context.getString(R.string.channel_asks),
                NotificationManager.IMPORTANCE_HIGH,
            ).apply { description = context.getString(R.string.channel_asks_description) }
        )
        manager.createNotificationChannel(
            NotificationChannel(
                CHANNEL_WATCH,
                context.getString(R.string.channel_watch),
                NotificationManager.IMPORTANCE_MIN,
            ).apply {
                description = context.getString(R.string.channel_watch_description)
                setShowBadge(false)
            }
        )
    }

    /**
     * Reads the Inbox once and notifies each ask that started waiting on the reader since the last
     * read. Returns false when the session is gone, so the caller stops watching.
     */
    @Synchronized
    fun refresh(context: Context): Boolean {
        val rows = try {
            DispatchServer.readInbox(context)
        } catch (e: SignedOutException) {
            // Said once per lost session, not at every backstop read while it stays lost.
            val prefs = context.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
            if (!prefs.getBoolean(KEY_SIGNED_OUT_SHOWN, false)) {
                notifySignedOut(context)
                prefs.edit().putBoolean(KEY_SIGNED_OUT_SHOWN, true).apply()
            }
            return false
        } catch (e: IOException) {
            Log.w(TAG, "Inbox read failed: ${e.message}")
            return true
        }
        val prefs = context.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
        val remembered = prefs.getStringSet(KEY_REMEMBERED, null)
        val diff = diffInbox(remembered, rows, Instant.now())
        val manager = NotificationManagerCompat.from(context)
        manager.cancel(SIGNED_OUT_ID)
        for (id in diff.withdraw) manager.cancel(TAG_ASK, id.hashCode())
        if (canNotify(context)) {
            for (ask in diff.notify) post(context, manager, ask)
        }
        prefs.edit()
            .putStringSet(KEY_REMEMBERED, diff.remember)
            .putBoolean(KEY_SIGNED_OUT_SHOWN, false)
            .apply()
        return true
    }

    fun openIntent(context: Context, path: String?, requestCode: Int): PendingIntent {
        val intent = Intent(context, MainActivity::class.java)
            .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_CLEAR_TOP)
        if (path != null) intent.putExtra(EXTRA_PATH, path)
        return PendingIntent.getActivity(
            context,
            requestCode,
            intent,
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
    }

    private fun post(context: Context, manager: NotificationManagerCompat, ask: InboxAsk) {
        val id = ask.id.hashCode()
        val notification = NotificationCompat.Builder(context, CHANNEL_ASKS)
            .setSmallIcon(R.drawable.ic_notification)
            .setContentTitle(ask.subject())
            .setContentText(ask.question)
            .setStyle(NotificationCompat.BigTextStyle().bigText(ask.question))
            .setCategory(NotificationCompat.CATEGORY_MESSAGE)
            .setPriority(
                if (ask.urgency == "low") NotificationCompat.PRIORITY_DEFAULT
                else NotificationCompat.PRIORITY_HIGH
            )
            .setAutoCancel(true)
            .setContentIntent(openIntent(context, ask.path(), id))
            .build()
        try {
            manager.notify(TAG_ASK, id, notification)
        } catch (e: SecurityException) {
            Log.w(TAG, "Notification permission withdrawn: ${e.message}")
        }
    }

    private fun notifySignedOut(context: Context) {
        if (!canNotify(context)) return
        val notification = NotificationCompat.Builder(context, CHANNEL_ASKS)
            .setSmallIcon(R.drawable.ic_notification)
            .setContentTitle(context.getString(R.string.app_name))
            .setContentText(context.getString(R.string.signed_out))
            .setAutoCancel(true)
            .setContentIntent(openIntent(context, null, SIGNED_OUT_ID))
            .build()
        try {
            NotificationManagerCompat.from(context).notify(SIGNED_OUT_ID, notification)
        } catch (e: SecurityException) {
            Log.w(TAG, "Notification permission withdrawn: ${e.message}")
        }
    }

    private fun canNotify(context: Context): Boolean =
        Build.VERSION.SDK_INT < Build.VERSION_CODES.TIRAMISU ||
            ContextCompat.checkSelfPermission(context, Manifest.permission.POST_NOTIFICATIONS) ==
            PackageManager.PERMISSION_GRANTED
}
