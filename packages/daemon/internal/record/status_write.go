package record

// A Dispatch status write is the one outbox payload whose retry is judged differently for each
// thing a write can be for, so what it is for is a value of its own (StatusRowKind), stored in
// the row and set by one constructor per kind. They live here rather than beside the other
// payloads because they are a vocabulary in their own right: five kinds, five constructors and
// the rule that every reader of the list is held to the same five.

// StatusRowKind is what a status row is for, and the one thing that decides the rule the outbox
// judges its retry by (the daemon's stillWanted). It is stored in the payload rather than worked
// out from the row's other fields: while the kind was derived from a set of marks, a kind nobody
// had given a mark read as the ordinary one and was judged by the ordinary rule in silence, so a
// row queued at a status out of the workflow — a re-assert of the daemon's own status over a
// session's close, a person's handover, the daemon's own close — was dropped for standing at the
// very status it exists to undo. Every kind is named once, in StatusRowKinds, which the daemon's
// rule switch and the tests of both packages read, so a kind added to that list and nowhere else
// fails its row and reddens the package rather than borrowing another kind's rule.
type StatusRowKind string

const (
	// OrdinaryStatusRow is the workflow moving an issue along inside the workflow: the phase
	// statuses. The done a merge or an architect's close writes is a CloseStatusRow.
	OrdinaryStatusRow StatusRowKind = "ordinary"
	// PersonHandoverStatusRow is a person's handover: a person's legion label on an unrecorded
	// root in triage, or a person's approval of a parked labelled root's primary document. It is
	// queued at the triage, backlog or icebox that act found, to bring the issue into the
	// workflow. The pending-row rule (Store.PersonHandoverPending) reads the kind to tell such a
	// row from the workflow's other status rows, which suppress nothing: while one is pending for
	// an issue, no second one is queued for it. The outbox's status executor also drops such a row
	// at delivery when the re-read issue no longer carries the label: a person who took it off
	// withdrew the handover. A HandOverStatusRow, also a todo, is not one.
	PersonHandoverStatusRow StatusRowKind = "person-handover"
	// ReassertStatusRow is the daemon setting its own status back over one a session wrote
	// (workflow's sessionStatusWrite): the session's close or park stands in Dispatch and the
	// record kept the daemon's status, so this row is queued observing a status the issue is not
	// meant to be at — done, backlog or icebox — exactly as a person's handover is. Asking the
	// ordinary rule of it, whether the issue is still in the workflow, would drop the row for
	// being at the status it exists to undo.
	ReassertStatusRow StatusRowKind = "reassert"
	// HandOverStatusRow hands a child of a closed tree to Legion: the write also puts
	// dispatch.LegionLabel on the issue, in the same Dispatch write, and the runner makes it while
	// the label is missing even where Dispatch already shows Status.
	HandOverStatusRow StatusRowKind = "hand-over"
	// CloseStatusRow is the daemon's own close of an issue: the done a merge or an architect's
	// close_root writes, queued in the transaction that takes the record out of the workflow. The
	// ordinary rule cannot judge it, for the reason the re-assert's cannot: one refusal — an
	// agent's comment, a handoff record, or the done reason this row itself just posted — sends
	// it to its retry, which would find the record already out of the workflow and drop the write
	// that puts Dispatch there, leaving the issue open in Dispatch for good while the tree
	// lingers and frees its slot. Its own rule asks the question that matters instead: the close
	// still stands in the record at the generation the row names.
	CloseStatusRow StatusRowKind = "close"
)

// StatusRowKinds is every kind of status row, listed once. The daemon's rule switch, the
// migration that gave the stored rows their kind and the tests of both packages read this list,
// so a kind is added in one place and every reader of it is held to the same five.
var StatusRowKinds = []StatusRowKind{OrdinaryStatusRow, PersonHandoverStatusRow, ReassertStatusRow, HandOverStatusRow, CloseStatusRow}

// StatusWrite is a Dispatch status the daemon writes. ObservedStatus is the status Dispatch shows
// until the write runs: the runner writes Status only while Dispatch still shows ObservedStatus, so a
// move someone else made since the write was queued stands. Kind is what the write is for, set by
// the constructor its queueing site goes through — OrdinaryStatusWrite, PersonHandoverStatusWrite,
// ReassertStatusWrite, HandOverStatusWrite or CloseStatusWrite — which are the only places one is
// built, so no row reaches the outbox without a kind (validateOutboxPayload refuses one that does).
type StatusWrite struct {
	Kind           StatusRowKind `json:"kind"`
	Status         string        `json:"status"`
	ObservedStatus string        `json:"observedStatus"`
	// Reason is a done write's reason: the runner posts it as a message on the issue before it
	// writes the status, since Dispatch refuses a message on a closed issue. Empty for a write
	// with no reason to record.
	Reason string `json:"reason,omitempty"`
	// HandOverTree and HandOverLinger fence a hand-over (HandOverStatusRow): the runner writes it
	// only while the issue is still that tree's and the tree's root still lingers at that
	// generation (Issue.LingersAt), so a root re-admitted since keeps the issue as its child.
	HandOverTree   string `json:"handOverTree,omitempty"`
	HandOverLinger uint64 `json:"handOverLinger,omitempty"`
	// CloseGeneration fences the daemon's own close (CloseStatusRow) on the run it closed: every
	// attempt of the row writes done only while the record still holds that generation, so a
	// person's reopen, which re-admits the issue in a generation of its own, drops a close that
	// has not landed rather than closing the issue the person just took back. Zero in a row no generation is
	// known for — one migration 20261102003600 gave its kind, queued by a release that fenced
	// such a write on nothing — which writes as that release wrote it.
	CloseGeneration uint64 `json:"closeGeneration,omitempty"`
}

func (StatusWrite) OutboxKind() OutboxKind { return OutboxKindDispatchStatus }

// OrdinaryStatusWrite is the workflow's own move of an issue inside the workflow, to status from
// the observed status Dispatch shows until the write runs. reason is a done write's, posted as a
// message before the status lands (StatusWrite.Reason); every other write has none.
func OrdinaryStatusWrite(status, observed, reason string) StatusWrite {
	return StatusWrite{Kind: OrdinaryStatusRow, Status: status, ObservedStatus: observed, Reason: reason}
}

// PersonHandoverStatusWrite is the write a person's handover queues, observed at the status that
// person's act found (PersonHandoverStatusRow).
func PersonHandoverStatusWrite(status, observed string) StatusWrite {
	return StatusWrite{Kind: PersonHandoverStatusRow, Status: status, ObservedStatus: observed}
}

// ReassertStatusWrite is the daemon's own status set back over the one a session wrote, which the
// row observes (ReassertStatusRow).
func ReassertStatusWrite(status, observed string) StatusWrite {
	return StatusWrite{Kind: ReassertStatusRow, Status: status, ObservedStatus: observed}
}

// HandOverStatusWrite hands a child of tree to Legion at status, fenced on that tree and on the
// root generation whose linger queued the write (HandOverStatusRow).
func HandOverStatusWrite(status, observed, tree string, linger uint64) StatusWrite {
	return StatusWrite{Kind: HandOverStatusRow, Status: status, ObservedStatus: observed,
		HandOverTree: tree, HandOverLinger: linger}
}

// CloseStatusWrite is the daemon's own close of an issue, to status (done) from the observed
// status Dispatch shows, with reason posted as a message before the status lands, fenced on the
// generation of the run the close ended (CloseStatusRow).
func CloseStatusWrite(status, observed, reason string, generation uint64) StatusWrite {
	return StatusWrite{Kind: CloseStatusRow, Status: status, ObservedStatus: observed, Reason: reason,
		CloseGeneration: generation}
}
