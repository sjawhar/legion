package texttrees

import "github.com/sjawhar/envoy/internal/dispatch/model"

// AskWrittenTexts appends to texts every stored text a write of an ask writes: its question, and
// the label and description of each of its answer options. The ask's answer and the reason it was
// resolved with are not among them - they are written by the transitions, which store their own
// (api's closeAsk) - and a rewording's replaced question and labels are this same list over what
// the edit carried away (api's askEditDisplayTexts).
func AskWrittenTexts(texts []string, question string, options []model.AskOption) []string {
	texts = append(texts, question)
	for _, option := range options {
		texts = append(texts, option.Label, option.Description)
	}
	return texts
}

// AskShownTexts appends to texts every stored text an ask is shown with: the texts its write
// wrote, then the text of the answer it carries and the reason it was resolved with (api's
// askRead.shape). An ask with no answer or no resolution passes nil for it and adds nothing, and
// so does an answer whose author wrote no text with it.
//
// It is one list because two paths must name exactly the same texts: a read of an ask primes and
// stores the trees of these (api's askReadDisplayTexts), and the background fill stores the trees
// of these from the ask's own row (texttreefill's askTexts). A text one of them named and the
// other did not would be a tree re-read on every request, or one stored for no reader.
func AskShownTexts(
	texts []string,
	question string,
	options []model.AskOption,
	answer *model.AskAnswer,
	resolution *model.AskResolution,
) []string {
	texts = AskWrittenTexts(texts, question, options)
	if answer != nil && answer.Text != nil {
		texts = append(texts, *answer.Text)
	}
	if resolution != nil {
		texts = append(texts, resolution.Reason)
	}
	return texts
}
