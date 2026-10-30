package dev.legion.dispatch

import android.content.Context
import androidx.work.Constraints
import androidx.work.ExistingPeriodicWorkPolicy
import androidx.work.NetworkType
import androidx.work.PeriodicWorkRequestBuilder
import androidx.work.WorkManager
import androidx.work.Worker
import androidx.work.WorkerParameters
import java.util.concurrent.TimeUnit

/**
 * The backstop for when Android stops [InboxWatchService] (battery saver, a force stop, a phone
 * that kills foreground services): one Inbox read every 15 minutes, WorkManager's shortest period.
 * While the service runs it finds nothing new to say, since the service already said it.
 */
class InboxPollWorker(context: Context, params: WorkerParameters) : Worker(context, params) {
    override fun doWork(): Result {
        InboxNotifier.refresh(applicationContext)
        return Result.success()
    }

    companion object {
        private const val NAME = "inbox-poll"

        fun schedule(context: Context) {
            val request = PeriodicWorkRequestBuilder<InboxPollWorker>(15, TimeUnit.MINUTES)
                .setConstraints(
                    Constraints.Builder().setRequiredNetworkType(NetworkType.CONNECTED).build()
                )
                .build()
            WorkManager.getInstance(context)
                .enqueueUniquePeriodicWork(NAME, ExistingPeriodicWorkPolicy.KEEP, request)
        }
    }
}
