// Package going answers a question from what Dispatch holds: it searches, shows the hits to a
// text model, and streams the model's answer to the reader as server-sent events. It sits outside
// internal/ so the daemon can speak the same stream, and it holds no store and no HTTP route of
// its own: the frames of the stream, the hit they name, the chat-completions client that
// generates the text, and the filter that clears that text before a reader sees a byte of it.
//
// Every name, code and bound of the stream comes from contracts, generated from
// packages/contracts, so the server that writes a frame and the page that reads it cannot drift
// apart; nothing here declares a second copy of one.
package going

import "github.com/sjawhar/envoy/internal/contracts"

// Event is one frame of an answer's stream. Data is one of the payload types below, and the
// stream's writer marshals it as the frame's data.
type Event struct {
	Name string
	Data any
}

// Terminal reports whether this event ends a stream: exactly one of end and error does, and a
// reader stops on either.
func (e Event) Terminal() bool {
	return e.Name == contracts.GoingEventEnd || e.Name == contracts.GoingEventError
}

// TextData carries the next piece of the answer, in the order the model wrote it. It is plain
// text: every ASCII punctuation byte in it is backslash-escaped, which closes markdown's own
// constructs, so nothing a renderer is given can become a link, an image, a tag or a code span,
// by itself or by two frames meeting; and a word GFM would link on its own, which no escape
// closes, is dropped rather than sent. The links of an answer are its ItemData frames and
// nothing else.
type TextData struct {
	Text string `json:"text"`
}

// ItemData names one thing the answer cites, so the reader's page can link it. Href is the
// cited hit's own href, and it is the one link the stream carries. Title, Kind and ID are plain
// text, which a reader is shown as text and never read as markdown.
type ItemData struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Href  string `json:"href"`
	Title string `json:"title"`
}

// CutData says the answer was cut at contracts.GoingAnswerWordMax, with the words it kept.
type CutData struct {
	Words int `json:"words"`
}

// ChangedData says the server did not pass the model's answer on as the model wrote it, with how
// many pieces of it that came to: a link or image reduced to the text a reader sees of it, a
// dropped tag or autolink, and a word GFM would have linked on its own. A link to a hit the
// model was given is no such change: the item frame carries it.
type ChangedData struct {
	Dropped int `json:"dropped"`
}

// NoneData says the model produced no answer text at all. It carries no field, and is a type of
// its own so a none frame is written the same way as every other.
type NoneData struct{}

// EndData ends a stream that answered, with what the answer came to.
type EndData struct {
	Words int `json:"words"`
	Items int `json:"items"`
}

// ErrorData ends a stream that failed, naming the failure by one of the contracts.GoingError
// codes alone: a model service's own words are never shown to a reader.
type ErrorData struct {
	Code string `json:"code"`
}

// Text is the next piece of the answer, escaped as TextData says.
func Text(text string) Event {
	return Event{Name: contracts.GoingEventText, Data: TextData{Text: text}}
}

// Item names a hit the answer cites, by the fields the page links and shows.
func Item(hit Hit) Event {
	return Event{Name: contracts.GoingEventItem, Data: ItemData{
		Kind:  hit.Kind,
		ID:    hit.ID,
		Href:  hit.Href,
		Title: hit.Title,
	}}
}

// Cut says the answer stopped at its word cap, after words words.
func Cut(words int) Event {
	return Event{Name: contracts.GoingEventCut, Data: CutData{Words: words}}
}

// Changed says the server did not pass the answer on as the model wrote it: a link or an image
// stands as the text a reader sees of it, and a tag or a word GFM would have linked is gone.
func Changed(dropped int) Event {
	return Event{Name: contracts.GoingEventChanged, Data: ChangedData{Dropped: dropped}}
}

// None says the model wrote no answer. An answer that kept no link is not none, and neither is
// one the server reduced or dropped every word of: each is shown with what it came to, and
// end's items of 0 is what tells the reader it rests on no source.
func None() Event {
	return Event{Name: contracts.GoingEventNone, Data: NoneData{}}
}

// End ends a stream that answered.
func End(words, items int) Event {
	return Event{Name: contracts.GoingEventEnd, Data: EndData{Words: words, Items: items}}
}

// Fault ends a stream that failed. It is not named Error, because an Event is not an error.
func Fault(code string) Event {
	return Event{Name: contracts.GoingEventError, Data: ErrorData{Code: code}}
}

// Hit is one search hit as the answerer sees it: what the model may cite, and what the stream
// names when the answer links it.
type Hit struct {
	Kind  string
	ID    string
	Title string
	Href  string
}

// Key names a hit across kinds, since an id is unique only within its kind.
func (h Hit) Key() string { return h.Kind + ":" + h.ID }
