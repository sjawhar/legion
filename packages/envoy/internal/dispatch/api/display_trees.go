package api

import (
	"encoding/json"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// displayTreesHeader is what a caller sends to be given the display tree of every stored text in
// the answer, beside the markdown itself: `Dispatch-Trees: 1` and nothing else. The dashboard
// sets it on every request it makes (DispatchApiClient#answered); the command-line tool and every
// agent client send none, so their bytes are exactly what they were. Every tree field is
// omitempty, so the header is the only thing that puts one in a response.
const displayTreesHeader = "Dispatch-Trees"

// wantsDisplayTrees reports whether this request asked for the display trees.
func wantsDisplayTrees(r *http.Request) bool {
	return r.Header.Get(displayTreesHeader) == "1"
}

// varyOnDisplayTrees tells every cache between the browser and Dispatch that the body depends on
// the request's Dispatch-Trees header, so an answer given to the dashboard is never handed to the
// command-line tool or the other way round. Register wraps every mounted route in it rather than
// each handler that reads the header naming itself: a route that gains a tree field later cannot
// then forget the Vary, and a route that carries no stored text pays one constant header.
func varyOnDisplayTrees(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", displayTreesHeader)
		handler(w, r)
	}
}

// displayTree is the tree a stored text is shown as, or nil for an empty text, which needs none.
func displayTree(markdown string) json.RawMessage {
	return pmdoc.DisplayJSON(markdown, pmdoc.DisplayLatest)
}

// displayTreeOf is displayTree for a text a row may not hold at all.
func displayTreeOf(markdown *string) json.RawMessage {
	if markdown == nil {
		return nil
	}
	return pmdoc.DisplayJSON(*markdown, pmdoc.DisplayLatest)
}

// The read shapes below are the dashboard's own view of a stored row: each embeds the row the
// whole deployment shares and shadows the fields whose text gains a tree, so the JSON keeps every
// key it had and gains `<field>_tree` beside each text. The tree never goes on the shared row
// itself, because an event payload carries that row and no payload gains a field; an event's own
// trees ride in the envelope's `trees` map instead (eventDisplayTrees).
//
// Each shape is built from its row by shape(trees), called once the row is final - after the part
// ids, the reach and everything else a read attaches - and just before the answer is written.
// With trees false it only re-wraps what the row already held, so the bytes are the bytes the
// route wrote before this existed.

// askOptionRead is one answer choice with the trees of its label and description.
type askOptionRead struct {
	model.AskOption
	LabelTree       json.RawMessage `json:"label_tree,omitempty"`
	DescriptionTree json.RawMessage `json:"description_tree,omitempty"`
}

// askAnswerRead is a human's answer with the tree of the text they wrote with it.
type askAnswerRead struct {
	model.AskAnswer
	TextTree json.RawMessage `json:"text_tree,omitempty"`
}

// askResolutionRead is why an ask no longer needs an answer, with the tree of that reason.
type askResolutionRead struct {
	model.AskResolution
	ReasonTree json.RawMessage `json:"reason_tree,omitempty"`
}

// askEditPreviousRead is the content an ask had before an edit, with its question's and its
// options' trees.
type askEditPreviousRead struct {
	model.AskEditPrevious
	QuestionTree json.RawMessage `json:"question_tree,omitempty"`
	Options      []askOptionRead `json:"options"`
}

// askEditRead is one recorded rewording of an ask, the content it replaced carrying its trees.
type askEditRead struct {
	model.AskEdit
	Previous askEditPreviousRead `json:"previous"`
}

// askRead is an ask as the dashboard reads it: the ask, with the trees of its question, of each
// option, of its answer's text and of its resolution's reason.
type askRead struct {
	model.Ask
	QuestionTree json.RawMessage    `json:"question_tree,omitempty"`
	Options      []askOptionRead    `json:"options"`
	Answer       *askAnswerRead     `json:"answer"`
	Resolution   *askResolutionRead `json:"resolution,omitempty"`
}

// shape rebuilds the shadowing fields from the ask they stand in front of, and reads each of its
// texts for display when the caller asked for the trees.
func (a *askRead) shape(trees bool) {
	a.Options = askOptionReads(a.Ask.Options, trees)
	a.Answer = nil
	if a.Ask.Answer != nil {
		answer := askAnswerRead{AskAnswer: *a.Ask.Answer}
		if trees {
			answer.TextTree = displayTreeOf(a.Ask.Answer.Text)
		}
		a.Answer = &answer
	}
	a.Resolution = nil
	if a.Ask.Resolution != nil {
		resolution := askResolutionRead{AskResolution: *a.Ask.Resolution}
		if trees {
			resolution.ReasonTree = displayTree(a.Ask.Resolution.Reason)
		}
		a.Resolution = &resolution
	}
	if trees {
		a.QuestionTree = displayTree(a.Ask.Question)
	}
}

// askOptionReads is the options with their trees, keeping an absent list absent so a row that
// carried no options still writes null rather than an empty list.
func askOptionReads(options []model.AskOption, trees bool) []askOptionRead {
	if options == nil {
		return nil
	}
	reads := make([]askOptionRead, len(options))
	for index, option := range options {
		reads[index] = askOptionRead{AskOption: option}
		if trees {
			reads[index].LabelTree = displayTree(option.Label)
			reads[index].DescriptionTree = displayTree(option.Description)
		}
	}
	return reads
}

// newAskRead is one ask shaped for the dashboard.
func newAskRead(ask model.Ask, trees bool) askRead {
	read := askRead{Ask: ask}
	read.shape(trees)
	return read
}

// askReads is a list of asks shaped for the dashboard, an absent list staying absent.
func askReads(asks []model.Ask, trees bool) []askRead {
	if asks == nil {
		return nil
	}
	reads := make([]askRead, len(asks))
	for index := range asks {
		reads[index] = newAskRead(asks[index], trees)
	}
	return reads
}

// askAnswerReads is a thread's answers with the trees of their texts.
func askAnswerReads(answers []model.AskAnswer, trees bool) []askAnswerRead {
	if answers == nil {
		return nil
	}
	reads := make([]askAnswerRead, len(answers))
	for index, answer := range answers {
		reads[index] = askAnswerRead{AskAnswer: answer}
		if trees {
			reads[index].TextTree = displayTreeOf(answer.Text)
		}
	}
	return reads
}

// askEditReads is an ask's recorded rewordings, each previous content carrying its trees.
func askEditReads(edits []model.AskEdit, trees bool) []askEditRead {
	if edits == nil {
		return nil
	}
	reads := make([]askEditRead, len(edits))
	for index, edit := range edits {
		previous := askEditPreviousRead{
			AskEditPrevious: edit.Previous,
			Options:         askOptionReads(edit.Previous.Options, trees),
		}
		if trees {
			previous.QuestionTree = displayTree(edit.Previous.Question)
		}
		reads[index] = askEditRead{AskEdit: edit, Previous: previous}
	}
	return reads
}

// replyBlockRead is the block a reply answers, with the tree of the excerpt it stored.
type replyBlockRead struct {
	model.ReplyBlock
	TextTree json.RawMessage `json:"text_tree,omitempty"`
}

// newReplyBlockRead is the block a reply answers shaped for the dashboard, nil for a reply that
// names none.
func newReplyBlockRead(block *model.ReplyBlock, trees bool) *replyBlockRead {
	if block == nil {
		return nil
	}
	read := &replyBlockRead{ReplyBlock: *block}
	if trees {
		read.TextTree = displayTree(block.Text)
	}
	return read
}

// commentRead is a comment as the dashboard reads it: the comment, the tree of its body, and the
// block it answers carrying the tree of that block's excerpt.
type commentRead struct {
	model.Comment
	BodyTree   json.RawMessage `json:"body_tree,omitempty"`
	ReplyBlock *replyBlockRead `json:"reply_block"`
}

// shape rebuilds the shadowing fields from the comment and reads its texts for display when the
// caller asked for the trees.
func (c *commentRead) shape(trees bool) {
	c.ReplyBlock = newReplyBlockRead(c.Comment.ReplyBlock, trees)
	if trees {
		c.BodyTree = displayTree(c.Comment.Body)
	}
}

// newCommentRead is one comment shaped for the dashboard.
func newCommentRead(comment model.Comment, trees bool) commentRead {
	read := commentRead{Comment: comment}
	read.shape(trees)
	return read
}

// commentReads is a list of comments shaped for the dashboard, an absent list staying absent.
func commentReads(comments []model.Comment, trees bool) []commentRead {
	if comments == nil {
		return nil
	}
	reads := make([]commentRead, len(comments))
	for index := range comments {
		reads[index] = newCommentRead(comments[index], trees)
	}
	return reads
}

// messageRead is a message as the dashboard reads it, shaped exactly as a comment is.
type messageRead struct {
	model.Message
	BodyTree   json.RawMessage `json:"body_tree,omitempty"`
	ReplyBlock *replyBlockRead `json:"reply_block"`
}

// shape rebuilds the shadowing fields from the message and reads its texts for display when the
// caller asked for the trees.
func (m *messageRead) shape(trees bool) {
	m.ReplyBlock = newReplyBlockRead(m.Message.ReplyBlock, trees)
	if trees {
		m.BodyTree = displayTree(m.Message.Body)
	}
}

// newMessageRead is one message shaped for the dashboard.
func newMessageRead(message model.Message, trees bool) messageRead {
	read := messageRead{Message: message}
	read.shape(trees)
	return read
}

// messageReads is a list of messages shaped for the dashboard, an absent list staying absent.
func messageReads(messages []model.Message, trees bool) []messageRead {
	if messages == nil {
		return nil
	}
	reads := make([]messageRead, len(messages))
	for index := range messages {
		reads[index] = newMessageRead(messages[index], trees)
	}
	return reads
}

// artifactReviewRead is one review of a document with the tree of the reason it gave.
type artifactReviewRead struct {
	model.ArtifactReview
	ReasonTree json.RawMessage `json:"reason_tree,omitempty"`
}

// artifactReviewReads is a document's reviews shaped for the dashboard.
func artifactReviewReads(reviews []model.ArtifactReview, trees bool) []artifactReviewRead {
	if reviews == nil {
		return nil
	}
	reads := make([]artifactReviewRead, len(reviews))
	for index, review := range reviews {
		reads[index] = artifactReviewRead{ArtifactReview: review}
		if trees {
			reads[index].ReasonTree = displayTreeOf(review.Reason)
		}
	}
	return reads
}

// attachApprovalTrees fills the tree of each document's approval reason. It runs where a route
// answers a reader rather than in attachApprovals, which every read of a document goes through,
// request or not: only a reader is shown the reason, and only one that asked for the trees.
func attachApprovalTrees(trees bool, artifacts []model.Artifact) {
	if !trees {
		return
	}
	for index := range artifacts {
		if approval := artifacts[index].Approval; approval != nil {
			approval.ReasonTree = displayTreeOf(approval.Reason)
		}
	}
}
