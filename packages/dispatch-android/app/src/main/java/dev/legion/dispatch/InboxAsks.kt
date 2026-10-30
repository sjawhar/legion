package dev.legion.dispatch

import java.time.Instant
import java.time.OffsetDateTime
import org.json.JSONArray
import org.json.JSONObject

/**
 * The fields of one `GET /api/v1/inbox` row the app reads (`InboxRow` in
 * packages/contracts/src/dispatch-api.ts). Everything else in the row is ignored.
 */
data class InboxAsk(
    val id: String,
    val question: String,
    val urgency: String?,
    val issueKey: String?,
    val issueTitle: String?,
    val documentProject: String?,
    val documentSlug: String?,
    val documentName: String?,
    val waitingOn: String?,
    val snoozedUntil: Instant?,
) {
    /**
     * Whether the ask is one to tell the reader about: the Inbox lists it as waiting on a person
     * (an ask with no `waiting_on` has had no reply, so it waits on whoever it was asked of), and
     * the reader has not snoozed it past [now].
     */
    fun waitsOnReader(now: Instant): Boolean =
        waitingOn != "agent" && (snoozedUntil == null || !snoozedUntil.isAfter(now))

    /** The dashboard path that opens this ask, the same deep links Dispatch itself hands out. */
    fun path(): String = when {
        issueKey != null -> "/issues/${encode(issueKey)}/asks/${encode(id)}"
        documentProject != null && documentSlug != null ->
            "/projects/${encode(documentProject)}/documents/${encode(documentSlug)}?ask=${encode(id)}"
        else -> "/"
    }

    /** The line a notification leads with: the issue or document the ask belongs to. */
    fun subject(): String = when {
        issueKey != null && issueTitle != null -> "$issueKey $issueTitle"
        issueKey != null -> issueKey
        documentName != null -> documentName
        else -> "Dispatch"
    }

    private fun encode(segment: String): String =
        java.net.URLEncoder.encode(segment, "UTF-8").replace("+", "%20")
}

fun parseInbox(body: String): List<InboxAsk> {
    val rows = JSONArray(body)
    return (0 until rows.length()).map { parseAsk(rows.getJSONObject(it)) }
}

private fun parseAsk(row: JSONObject): InboxAsk {
    val issue = row.optJSONObject("issue")
    val document = row.optJSONObject("document")
    return InboxAsk(
        id = row.getString("id"),
        question = row.optString("question"),
        urgency = row.nullableString("urgency"),
        issueKey = row.nullableString("issue_key") ?: issue?.nullableString("key"),
        issueTitle = issue?.nullableString("title"),
        documentProject = document?.nullableString("project"),
        documentSlug = document?.nullableString("slug"),
        documentName = document?.nullableString("name"),
        waitingOn = row.nullableString("waiting_on"),
        snoozedUntil = row.nullableString("snoozed_until")?.let { OffsetDateTime.parse(it).toInstant() },
    )
}

private fun JSONObject.nullableString(name: String): String? =
    if (isNull(name)) null else optString(name).ifEmpty { null }

/** What one Inbox read changes: the asks to notify, the notifications to take back, and the ids
 *  to remember for the next read. */
data class InboxDiff(
    val notify: List<InboxAsk>,
    val withdraw: Set<String>,
    val remember: Set<String>,
)

/**
 * Compares an Inbox read with the ids remembered from the last one. The remembered set is exactly
 * the asks waiting on the reader at that read, so an ask notifies once when it starts waiting on
 * them (opened, handed back to them, or out of its snooze) and its notification is taken back
 * when it stops (answered, resolved, its turn passed to an agent, or snoozed). On the first read
 * ([remembered] null) nothing notifies: the asks already open when the app is installed are the
 * Inbox's to show, not a burst of notifications.
 */
fun diffInbox(remembered: Set<String>?, rows: List<InboxAsk>, now: Instant): InboxDiff {
    val waiting = rows.filter { it.waitsOnReader(now) }
    val ids = waiting.mapTo(LinkedHashSet()) { it.id }
    if (remembered == null) return InboxDiff(emptyList(), emptySet(), ids)
    return InboxDiff(
        notify = waiting.filter { it.id !in remembered },
        withdraw = remembered - ids,
        remember = ids,
    )
}
