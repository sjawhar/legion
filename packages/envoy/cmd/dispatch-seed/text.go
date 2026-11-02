package main

import (
	"fmt"
	"math/rand/v2"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// mentionKinds is every node kind refs/graph.go's nodeLoaders resolves that text can name, in the
// order the seed cycles them, so `refs` holds mentions to each. The one kind left out, `session`,
// has no grammar in text/refs.go - no `dispatch://` reference names a session - so the one
// mention to a session is written into `refs` directly (writeSessionMention).
var mentionKinds = []string{
	"issue", "artifact", "ask", "comment", "message", "component", "block", "ask_block",
	"comment_block", "message_block",
}

// buildMentionTargets lists the asks and comments a reference can name. A reference to an ask or
// a comment carries its owner in the path, and the owner this seed's text names is always an
// issue, so the items a document belongs to are left out.
func (p *seedPlan) buildMentionTargets() {
	for index, ask := range p.asks {
		if ask.Issue >= 0 {
			p.issueAsks = append(p.issueAsks, index)
		}
	}
	for index, comment := range p.comments {
		if comment.Issue >= 0 {
			p.issueComments = append(p.issueComments, index)
		}
	}
}

// documentReference is the `dispatch://` reference naming document index: an issue's spec by its
// own path, any other document of an issue or a project by `artifact/<slug>`.
func (p *seedPlan) documentReference(index int) string {
	document := p.documents[index]
	if document.Primary {
		return "dispatch://" + p.issues[document.Issue].Key + "/spec"
	}
	if document.Issue >= 0 {
		return "dispatch://" + p.issues[document.Issue].Key + "/artifact/" + document.Slug
	}
	return "dispatch://" + p.projects[document.Project].Key + "/artifact/" + document.Slug
}

// mentionText is the reference the nth mention of kind kindIndex writes. Cycling the kinds and
// the targets spreads the mentions over every arm the backlink count reads and over many targets,
// so no one node carries them all.
func (p *seedPlan) mentionText(kindIndex, nth int) string {
	switch mentionKinds[kindIndex] {
	case "issue":
		return "dispatch://" + p.issues[nth%len(p.issues)].Key
	case "artifact":
		return p.documentReference(nth % len(p.documents))
	case "ask":
		ask := p.asks[p.issueAsks[nth%len(p.issueAsks)]]
		return "dispatch://" + p.issues[ask.Issue].Key + "/ask/" + ask.ID
	case "comment":
		comment := p.comments[p.issueComments[nth%len(p.issueComments)]]
		return "dispatch://" + p.issues[comment.Issue].Key + "/comment/" + comment.ID
	case "message":
		message := p.messages[nth%len(p.messages)]
		return "dispatch://" + p.issues[message.Issue].Key + "/message/" + message.ID
	case "component":
		component := p.components[nth%len(p.components)]
		return "dispatch://" + p.projects[component.Project].Key + "/component/" + component.ID
	case "block":
		return p.documentReference(nth%len(p.documents)) + "#b-" + text.EncodeBlockID(firstParagraphBlock)
	case "ask_block":
		ask := p.asks[p.issueAsks[nth%len(p.issueAsks)]]
		return "dispatch://" + p.issues[ask.Issue].Key + "/ask/" + ask.ID + "#b-" + askQuestionBlock
	case "comment_block":
		comment := p.comments[p.issueComments[nth%len(p.issueComments)]]
		return "dispatch://" + p.issues[comment.Issue].Key + "/comment/" + comment.ID + "#b-b1"
	default:
		message := p.messages[nth%len(p.messages)]
		return "dispatch://" + p.issues[message.Issue].Key + "/message/" + message.ID + "#b-b1"
	}
}

// firstParagraphBlock is the block id of the first paragraph of every latest version. Every
// latest version is built to one shape (documentMarkdown) and the block ids are minted in
// document order from a counter restarted for each document (blockIDs), so b1 is the heading of
// every document and b2 its first paragraph: a mention naming `#b-b2` of a document names a
// block that document holds. The id of a table's body row, which an anchor on a table row names,
// is read off the rendered tree instead (renderVersion), because a table's rows carry ids the
// renderer reports no offset for.
const firstParagraphBlock = "b2"

// documentMarkdown is the markdown of one version of a document. Only the latest version carries
// the shape the measurements read - a heading, seventeen paragraphs and two tables, twenty blocks
// of which a tenth are tables - because only the latest version is block-indexed and only the latest
// version is what search, the related-item check and every anchor read look at. An older version
// is paragraphs alone, which is the bulk `artifact_versions` carries in production.
func (p *seedPlan) documentMarkdown(index, version int, random *rand.Rand, bytes int) string {
	document := p.documents[index]
	latest := version == document.Versions
	var builder strings.Builder
	builder.Grow(bytes + 512)
	builder.WriteString("# ")
	builder.WriteString(p.vocabulary.phrase(random, 5))
	builder.WriteString("\n\n")
	if !latest {
		for paragraph := range paragraphsPerVersion {
			if paragraph > 0 {
				builder.WriteString("\n\n")
			}
			p.vocabulary.paragraph(&builder, random, bytes/paragraphsPerVersion)
		}
		builder.WriteString("\n")
		return builder.String()
	}
	mentions := p.documentMentions(index)
	for paragraph := range paragraphsPerVersion {
		if paragraph > 0 {
			builder.WriteString("\n\n")
		}
		if paragraph < len(mentions) {
			builder.WriteString(mentions[paragraph])
			builder.WriteByte(' ')
		}
		p.vocabulary.paragraph(&builder, random, bytes/paragraphsPerVersion)
	}
	for range tablesPerVersion {
		builder.WriteString("\n\n| ")
		builder.WriteString(p.vocabulary.phrase(random, 2))
		builder.WriteString(" | ")
		builder.WriteString(p.vocabulary.phrase(random, 2))
		builder.WriteString(" |\n| --- | --- |\n| ")
		builder.WriteString(p.vocabulary.phrase(random, 3))
		builder.WriteString(" | ")
		builder.WriteString(p.vocabulary.phrase(random, 3))
		builder.WriteString(" |")
	}
	builder.WriteString("\n")
	return builder.String()
}

// documentMentions is the references the latest version of document index writes, one per
// paragraph. A document of a chain names the next document of its chain and a block of it, which
// is the artifact-to-artifact and artifact-to-block chain refs.Closure walks; every other
// document that carries a mention names one node of one kind.
func (p *seedPlan) documentMentions(index int) []string {
	document := p.documents[index]
	if document.Chain >= 0 {
		return []string{
			p.documentReference(document.Chain),
			p.documentReference(document.Chain) + "#b-" + text.EncodeBlockID(firstParagraphBlock),
		}
	}
	if share(index, len(p.documents), p.sizes.DocumentMentions) == 0 {
		return nil
	}
	return []string{p.mentionText(index%len(mentionKinds), index)}
}

// itemBody is the body of a comment or a message: blocks paragraphs, the first of them opening
// with the item's mention when it writes one. Blocks are separated by a blank line, which is what
// itemblocks.Split cuts an item's blocks on.
func (p *seedPlan) itemBody(random *rand.Rand, blocks int, mention string, bytes int) string {
	var builder strings.Builder
	builder.Grow(bytes + 256)
	for block := range blocks {
		if block > 0 {
			builder.WriteString("\n\n")
		}
		if block == 0 && mention != "" {
			builder.WriteString(mention)
			builder.WriteByte(' ')
		}
		p.vocabulary.paragraph(&builder, random, bytes/blocks)
	}
	return builder.String()
}

// askQuestion is an ask's question, with its mention when it writes one. The question is a part
// of the ask (refs.AskParts), so a mention it carries is a row of `refs` and a row of
// `ask_block_refs`: the 100,000 mentions of `refs` are twice the 50,000 rows the three block
// mention tables of the documents, the comments and the messages hold, and the parts of the asks
// hold their own beside them.
func (p *seedPlan) askQuestion(random *rand.Rand, mention string, bytes int) string {
	var builder strings.Builder
	if mention != "" {
		builder.WriteString(mention)
		builder.WriteByte(' ')
	}
	p.vocabulary.paragraph(&builder, random, bytes)
	return builder.String()
}

// askOptions is the options an ask offers. A label is four words, long enough that two options of
// one ask do not draw the same text, which the API refuses (validateAskOptions). Every other
// option carries a description, so the data set holds both readings of an option's part text:
// "<label> — <description>" and the label alone (refs.AskParts).
func (p *seedPlan) askOptions(random *rand.Rand, count int) []model.AskOption {
	options := make([]model.AskOption, count)
	for index := range options {
		options[index].Label = p.vocabulary.phrase(random, 4)
		if index%2 == 0 {
			options[index].Description = p.vocabulary.phrase(random, 8)
		}
	}
	return options
}

// anchorJSON is the stored anchor of an anchored ask or comment: the document it is in, the block
// it fell on and the version it was taken at, as model.Anchor spells them. A table-row anchor
// names the table's body row, which is the anchor the document edit reads refuse to delete.
func anchorJSON(documentID, blockID string, version int, quote string) string {
	return fmt.Sprintf(
		`{"artifact_id":%q,"block_id":%q,"mark_id":%q,"version":%d,"quote":%q,"orphaned":false}`,
		documentID, blockID, "mark-"+blockID+"-"+documentID[:8], version, quote)
}
