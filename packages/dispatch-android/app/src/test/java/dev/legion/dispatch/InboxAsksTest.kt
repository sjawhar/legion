package dev.legion.dispatch

import java.time.Instant
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class InboxAsksTest {
    private val now = Instant.parse("2026-10-30T12:00:00Z")

    private fun ask(
        id: String,
        waitingOn: String? = "human",
        snoozedUntil: Instant? = null,
        issueKey: String? = "LEGION-1",
    ) = InboxAsk(
        id = id,
        question = "Ship it?",
        urgency = "normal",
        issueKey = issueKey,
        issueTitle = "A title",
        documentProject = null,
        documentSlug = null,
        documentName = null,
        waitingOn = waitingOn,
        snoozedUntil = snoozedUntil,
    )

    @Test
    fun parsesAnInboxRow() {
        val body = """
            [{"id":"a1","issue_key":"LEGION-7","question":"Merge?","urgency":"high",
              "issue":{"key":"LEGION-7","title":"Fix it","assignee":"me@example.com"},
              "waiting_on":"human","snoozed_until":"2026-10-30T13:00:00.123456+00:00",
              "options":[],"state":"open"},
             {"id":"d1","issue_key":null,"question":"Approve?","urgency":"normal",
              "document":{"project":"LEGION","slug":"arch-md","name":"arch.md"},
              "snoozed_until":null}]
        """.trimIndent()
        val rows = parseInbox(body)
        assertEquals(2, rows.size)
        assertEquals("LEGION-7", rows[0].issueKey)
        assertEquals("LEGION-7 Fix it", rows[0].subject())
        assertEquals("/issues/LEGION-7/asks/a1", rows[0].path())
        assertEquals(Instant.parse("2026-10-30T13:00:00.123456Z"), rows[0].snoozedUntil)
        assertNull(rows[1].issueKey)
        assertNull(rows[1].waitingOn)
        assertEquals("arch.md", rows[1].subject())
        assertEquals("/projects/LEGION/documents/arch-md?ask=d1", rows[1].path())
    }

    @Test
    fun waitsOnReaderUnlessAnAgentHasTheTurnOrItIsSnoozed() {
        assertTrue(ask("a", waitingOn = "human").waitsOnReader(now))
        assertTrue(ask("a", waitingOn = null).waitsOnReader(now))
        assertFalse(ask("a", waitingOn = "agent").waitsOnReader(now))
        assertFalse(ask("a", snoozedUntil = now.plusSeconds(60)).waitsOnReader(now))
        assertTrue(ask("a", snoozedUntil = now.minusSeconds(60)).waitsOnReader(now))
    }

    @Test
    fun firstReadRemembersWithoutNotifying() {
        val diff = diffInbox(null, listOf(ask("a"), ask("b")), now)
        assertTrue(diff.notify.isEmpty())
        assertTrue(diff.withdraw.isEmpty())
        assertEquals(setOf("a", "b"), diff.remember)
    }

    @Test
    fun aNewAskNotifiesOnceAndAnAnsweredOneIsWithdrawn() {
        val first = diffInbox(setOf("a", "b"), listOf(ask("b"), ask("c")), now)
        assertEquals(listOf("c"), first.notify.map { it.id })
        assertEquals(setOf("a"), first.withdraw)
        assertEquals(setOf("b", "c"), first.remember)

        val again = diffInbox(first.remember, listOf(ask("b"), ask("c")), now)
        assertTrue(again.notify.isEmpty())
        assertTrue(again.withdraw.isEmpty())
    }

    @Test
    fun anAskHandedBackToTheReaderNotifiesAgain() {
        val agentsTurn = diffInbox(setOf("a"), listOf(ask("a", waitingOn = "agent")), now)
        assertEquals(setOf("a"), agentsTurn.withdraw)
        assertTrue(agentsTurn.remember.isEmpty())

        val handedBack = diffInbox(agentsTurn.remember, listOf(ask("a", waitingOn = "human")), now)
        assertEquals(listOf("a"), handedBack.notify.map { it.id })
    }

    @Test
    fun pathSegmentsAreEncoded() {
        assertEquals("/issues/A%20B/asks/x%2Fy", ask("x/y", issueKey = "A B").path())
    }
}
