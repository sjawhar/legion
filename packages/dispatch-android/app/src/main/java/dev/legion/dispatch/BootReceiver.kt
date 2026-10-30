package dev.legion.dispatch

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent

/** Resumes watching after a reboot or an app update, without waiting for the app to be opened. */
class BootReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        when (intent.action) {
            Intent.ACTION_BOOT_COMPLETED, Intent.ACTION_MY_PACKAGE_REPLACED -> {
                InboxNotifier.createChannels(context)
                InboxPollWorker.schedule(context)
                InboxWatchService.start(context)
            }
        }
    }
}
