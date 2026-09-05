package core

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Urgency is the dispatch thread urgency level.
type Urgency string

const (
	UrgencyLow      Urgency = "low"
	UrgencyMed      Urgency = "med"
	UrgencyHigh     Urgency = "high"
	UrgencyBlocking Urgency = "blocking"
)

// Marker kinds. Every marker is `<!-- dispatch:<kind>\n<yaml>-->` at the very
// start of an issue body or comment. The dashboard writes answer and urgency
// markers; this package writes thread and ask markers.
const (
	KindThread  = "thread"
	KindAsk     = "ask"
	KindAnswer  = "answer"
	KindUrgency = "urgency"
)

// QuestionOption is one selectable option in a QuestionInfo.
type QuestionOption struct {
	Label       string `json:"label" yaml:"label"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
}

// QuestionInfo describes a structured question attached to a turn. AskID is
// assigned by this package (AskIDFor) and never supplied by a caller.
type QuestionInfo struct {
	AskID    string           `json:"askId,omitempty" yaml:"askId,omitempty"`
	Question string           `json:"question" yaml:"question"`
	Header   string           `json:"header,omitempty" yaml:"header,omitempty"`
	Options  []QuestionOption `json:"options" yaml:"options"`
	Multiple *bool            `json:"multiple,omitempty" yaml:"multiple,omitempty"`
	Custom   *bool            `json:"custom,omitempty" yaml:"custom,omitempty"`
}

// Origin captures which session a dispatch turn came from and where a human
// can find it. Every field is optional; the calling plugin fills what its
// host can supply, and each empty field is dropped from the rendered marker.
type Origin struct {
	Host         string `json:"host,omitempty" yaml:"host,omitempty"`
	Machine      string `json:"machine,omitempty" yaml:"machine,omitempty"`
	Cwd          string `json:"cwd,omitempty" yaml:"cwd,omitempty"`
	Tmux         string `json:"tmux,omitempty" yaml:"tmux,omitempty"`
	Pane         string `json:"pane,omitempty" yaml:"pane,omitempty"`
	SessionID    string `json:"sessionId,omitempty" yaml:"sessionId,omitempty"`
	SessionTitle string `json:"sessionTitle,omitempty" yaml:"sessionTitle,omitempty"`
}

// MetaMarker is the dispatch:thread marker at the top of an issue body.
type MetaMarker struct {
	RequestID string         `yaml:"requestId"`
	Urgency   Urgency        `yaml:"urgency"`
	Origin    *Origin        `yaml:"origin,omitempty"`
	Ask       []QuestionInfo `yaml:"ask,omitempty"`
}

// AskMarker is the dispatch:ask marker at the top of a follow-up comment.
type AskMarker struct {
	RequestID string         `yaml:"requestId"`
	Origin    *Origin        `yaml:"origin,omitempty"`
	Ask       []QuestionInfo `yaml:"ask,omitempty"`
}

const (
	markerOpen  = "<!-- dispatch:"
	markerClose = "-->"
)

// AskIDFor names the i-th ask of a turn whose request id is requestID: the
// first ask reuses the request id, later ones append ".<index>".
func AskIDFor(requestID string, index int) string {
	if index == 0 {
		return requestID
	}
	return fmt.Sprintf("%s.%d", requestID, index)
}

// WithAskIDs returns a copy of ask with AskID assigned per AskIDFor. An empty
// ask stays nil so the marker omits the key.
func WithAskIDs(ask []QuestionInfo, requestID string) []QuestionInfo {
	if len(ask) == 0 {
		return nil
	}
	out := make([]QuestionInfo, len(ask))
	for i, q := range ask {
		q.AskID = AskIDFor(requestID, i)
		out[i] = q
	}
	return out
}

// BuildMetaMarker renders the dispatch:thread marker for an issue body.
func BuildMetaMarker(m MetaMarker) (string, error) { return buildMarker(KindThread, m) }

// BuildAskMarker renders the dispatch:ask marker for a follow-up comment.
func BuildAskMarker(m AskMarker) (string, error) { return buildMarker(KindAsk, m) }

func buildMarker(kind string, payload any) (string, error) {
	data, err := commentSafeYAML(payload)
	if err != nil {
		return "", fmt.Errorf("dispatch: marshal %s marker: %w", kind, err)
	}
	return markerOpen + kind + "\n" + string(data) + markerClose, nil
}

// commentSafeYAML marshals v as YAML that can sit inside an HTML comment. An
// HTML comment ends at the first "-->", so every string scalar containing
// "-->", "<!--", or "--!>" is emitted double-quoted and those sequences are
// rewritten with YAML \u escapes, which are legal only inside double quotes
// and decode back to the original characters. The emitter folds double-quoted
// scalars only at spaces, so the sequences survive folding intact.
func commentSafeYAML(v any) ([]byte, error) {
	var node yaml.Node
	if err := node.Encode(v); err != nil {
		return nil, err
	}
	forceDoubleQuotes(&node)
	out, err := yaml.Marshal(&node)
	if err != nil {
		return nil, err
	}
	text := string(out)
	text = strings.ReplaceAll(text, "-->", `--\u003e`)
	text = strings.ReplaceAll(text, "<!--", `\u003c!--`)
	text = strings.ReplaceAll(text, "--!>", `--!\u003e`)
	return []byte(text), nil
}

func containsCommentDelimiter(s string) bool {
	return strings.Contains(s, "-->") || strings.Contains(s, "<!--") || strings.Contains(s, "--!>")
}

func forceDoubleQuotes(n *yaml.Node) {
	if n.Kind == yaml.ScalarNode && n.Tag == "!!str" && containsCommentDelimiter(n.Value) {
		n.Style = yaml.DoubleQuotedStyle
	}
	for _, child := range n.Content {
		forceDoubleQuotes(child)
	}
}

// splitMarker returns the kind and YAML text of the marker at the start of
// body. It accepts the HTML-comment form and legacy front matter
// ("---\n<yaml>\n---"); a legacy block names its kind with a `kind:` key and
// is a thread marker when that key is absent.
func splitMarker(body string) (kind, yamlText string, ok bool) {
	if strings.HasPrefix(body, markerOpen) {
		rest := body[len(markerOpen):]
		newline := strings.IndexByte(rest, '\n')
		if newline < 0 {
			return "", "", false
		}
		kind = strings.TrimSpace(rest[:newline])
		rest = rest[newline+1:]
		end := strings.Index(rest, "\n"+markerClose)
		if end < 0 {
			return "", "", false
		}
		return kind, rest[:end] + "\n", true
	}
	if strings.HasPrefix(body, "---\n") {
		after := body[4:]
		end := strings.Index(after, "\n---")
		if end < 0 {
			return "", "", false
		}
		yamlText = after[:end]
		var head struct {
			Kind string `yaml:"kind"`
		}
		if err := yaml.Unmarshal([]byte(yamlText), &head); err != nil {
			return "", "", false
		}
		if head.Kind == "" {
			head.Kind = KindThread
		}
		return head.Kind, yamlText, true
	}
	return "", "", false
}

// ParseMetaMarker reads the dispatch:thread marker at the start of an issue
// body, in either encoding. Returns nil when there is none, when it is another
// kind, or when it is invalid; nil is the whole signal, and the caller owns
// the user-facing message.
func ParseMetaMarker(body string) *MetaMarker {
	kind, text, ok := splitMarker(body)
	if !ok || kind != KindThread {
		return nil
	}
	var m MetaMarker
	if err := yaml.Unmarshal([]byte(text), &m); err != nil || m.RequestID == "" {
		return nil
	}
	switch m.Urgency {
	case UrgencyLow, UrgencyMed, UrgencyHigh, UrgencyBlocking:
		return &m
	default:
		return nil
	}
}

// ParseAskMarker reads the dispatch:ask marker at the start of a comment.
// Returns nil when the comment carries none or it is invalid.
func ParseAskMarker(body string) *AskMarker {
	kind, text, ok := splitMarker(body)
	if !ok || kind != KindAsk {
		return nil
	}
	var m AskMarker
	if err := yaml.Unmarshal([]byte(text), &m); err != nil || m.RequestID == "" {
		return nil
	}
	return &m
}

// BuildThreadBody renders the canonical thread body: marker, subject, then
// the Context and Question sections. The reader has not seen the caller's
// transcript, so both sections carry their own heading.
func BuildThreadBody(marker, subject, context, question string) string {
	return fmt.Sprintf("%s\n\n**%s**\n\n## Context\n\n%s\n\n## Question\n\n%s", marker, subject, context, question)
}

// BuildFollowUpBody renders a follow-up comment: marker, then the Context and
// Question sections of the new turn.
func BuildFollowUpBody(marker, context, question string) string {
	return fmt.Sprintf("%s\n\n## Context\n\n%s\n\n## Question\n\n%s", marker, context, question)
}
