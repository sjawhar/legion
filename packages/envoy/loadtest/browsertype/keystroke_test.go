package main

import (
	"math/rand/v2"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// The load generator's own writes quote a paragraph of each document it drives: `loadgen` replaces
// "Revision token rN" with the next token on every edit, and reads it back before an upload. A
// browser peer that types into that paragraph breaks the quote, so every later edit of that
// document answers 404 TARGET_NOT_FOUND and every upload stops with "revision token found false",
// and the document gains no version. Measured in the stored-order cluster run
// lt-20261029t183153-14f8, step 10x-typing: 212 edits and 61 uploads failed that way, and the 20
// typed documents ended with 0 to 3 versions where the step's plan writes 13 to 17.
//
// So a peer must leave a protected paragraph alone. The driver's `-protect TEXT` flag names the
// text that marks such a paragraph; the value reaches this function, so adding the parameter and
// changing this call is part of the fix. Weakening what the test asserts is not: after the fix a
// peer never edits a paragraph whose text holds a protected marker, and it still types into every
// other paragraph.
func TestAPeerNeverTypesIntoAProtectedParagraph(t *testing.T) {
	const protected = "Revision token r0"
	document := crdt.New()
	fragment := document.GetXmlFragment(fragmentName)
	var protectedText, openText *crdt.YXmlText
	document.Transact(func(txn *crdt.Transaction) {
		for index, text := range []string{protected, "A paragraph the peers may type into."} {
			paragraph := crdt.NewYXmlElement("paragraph")
			fragment.InsertElement(txn, index, paragraph)
			run := crdt.NewYXmlText()
			paragraph.InsertText(txn, 0, run)
			run.Insert(txn, 0, text, nil)
			if index == 0 {
				protectedText = run
			} else {
				openText = run
			}
		}
	})

	random := rand.New(rand.NewPCG(1, 2))
	before := openText.ToXML()
	for range 2000 {
		document.Transact(func(txn *crdt.Transaction) { keystroke(txn, fragment, random, []string{protected}) })
	}

	if got := protectedText.ToXML(); got != protected {
		t.Errorf("a peer typed into the protected paragraph: it holds %q, want %q", got, protected)
	}
	if after := openText.ToXML(); after == before {
		t.Errorf("a peer typed into no other paragraph either: the open paragraph still holds %q", after)
	}
	if texts := paragraphTextsLen(document, fragment); texts != 2 {
		t.Errorf("the document holds %d paragraph texts, want 2", texts)
	}
}

// paragraphTextsLen counts the paragraph texts a peer can reach, inside one transaction.
func paragraphTextsLen(document *crdt.Doc, fragment *crdt.YXmlFragment) int {
	var texts int
	document.Transact(func(*crdt.Transaction) { texts = len(paragraphTexts(fragment)) })
	return texts
}

// TestAProtectedParagraphIsFoundAnywhereInTheTree pins where the protection must look: the load
// generator's quoted paragraph can sit inside a list item, a quote or a table cell, and
// paragraphTexts reaches every one of them, so a protection that only reads the top level would
// leave those documents broken in the same way.
func TestAProtectedParagraphIsFoundAnywhereInTheTree(t *testing.T) {
	const protected = "Revision token r0"
	document := crdt.New()
	fragment := document.GetXmlFragment(fragmentName)
	var protectedText *crdt.YXmlText
	document.Transact(func(txn *crdt.Transaction) {
		item := crdt.NewYXmlElement("list_item")
		fragment.InsertElement(txn, 0, item)
		paragraph := crdt.NewYXmlElement("paragraph")
		item.InsertElement(txn, 0, paragraph)
		run := crdt.NewYXmlText()
		paragraph.InsertText(txn, 0, run)
		run.Insert(txn, 0, protected, nil)
		protectedText = run

		open := crdt.NewYXmlElement("paragraph")
		fragment.InsertElement(txn, 1, open)
		openRun := crdt.NewYXmlText()
		open.InsertText(txn, 0, openRun)
		openRun.Insert(txn, 0, "A paragraph the peers may type into.", nil)
	})

	random := rand.New(rand.NewPCG(3, 4))
	for range 2000 {
		document.Transact(func(txn *crdt.Transaction) { keystroke(txn, fragment, random, []string{protected}) })
	}

	if got := protectedText.ToXML(); got != protected {
		t.Errorf("a peer typed into the protected paragraph inside a list item: it holds %q, want %q", got, protected)
	}
}
