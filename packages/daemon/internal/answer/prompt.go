package answer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// systemPrompt is the short system prompt the agent's resumed conversation runs under: who it is
// and that it answers while it is not running, its tools (the list, gh named), the Legion footer
// the tool puts on what it posts, the work it declines, and the reply form.
func systemPrompt(req Request) string {
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = "`" + t.name + "`"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "You are the agent whose conversation follows: the %s of issue %s (session %s, phase %s).", req.Agent.Role, req.Agent.Issue, req.Agent.Session, req.Agent.Phase)
	b.WriteString(" You are not running now, and Legion has resumed your conversation for one message to you. Answer it as yourself, from your conversation and from what your tools read.\n\n")
	fmt.Fprintf(&b, "Your tools now: %s. ", strings.Join(names, ", "))
	b.WriteString("The `gh` tool is GitHub's own command line with your GitHub access; give it gh's arguments without the leading gh. ")
	b.WriteString("Every comment or review you post with it carries your Legion footer, which the tool adds, so do not write one. ")
	b.WriteString("The other tools your conversation used (reading and editing files, running commands, your Legion and Envoy tools) are not available now: do not call them. ")
	fmt.Fprintf(&b, "This answer may make at most %d gh calls and %d model calls; your last model call offers no tool and must be your reply.\n\n", req.Bounds.GHCalls, req.Bounds.ModelCalls)
	b.WriteString("Decline work your tools cannot do, such as work that needs your working copy or a run of your phase: say plainly that you cannot do it now and what it needs, and do not promise to do it later. Say plainly when you do not know.\n\n")
	b.WriteString("Your reply is exactly one JSON object and nothing else, no code fence: {\"kind\": ..., \"text\": ..., \"needs\": ...}. The kinds:\n")
	b.WriteString("- \"answered\": text is your answer to the message; needs is null.\n")
	b.WriteString("- \"not_answered\": you cannot answer from your conversation or what your tools read; text says so to the asker, and needs says what you would need, such as your working copy.\n")
	b.WriteString("- \"needs_work\": the message asks for work your tools cannot do; text tells the asker you cannot do it now, and needs names the work.\n")
	if req.MayAskNothing {
		b.WriteString("- \"none\": the message asks nothing of you; text is \"\" and needs is null.\n")
	}
	return b.String()
}

// userMessage is the one user message after the conversation: what has changed on the issue, the
// question with who asked and where, and on a re-run the gh calls already made with their results.
func userMessage(req Request) string {
	var b strings.Builder
	if note := strings.TrimSpace(req.ChangeNote); note != "" {
		fmt.Fprintf(&b, "What has changed on your issue since you stopped:\n%s\n\n", note)
	}
	fmt.Fprintf(&b, "%s asked you, in %s:\n%s", req.Question.Asker, req.Question.Where, req.Question.Text)
	if len(req.Earlier) > 0 {
		b.WriteString("\n\nAn earlier attempt at this answer already made these gh calls; do not repeat a write among them:\n")
		for _, call := range req.Earlier {
			args, _ := json.Marshal(call.Args)
			fmt.Fprintf(&b, "\n%d. gh %s\n", call.Seq, args)
			switch {
			case call.Result == nil:
				b.WriteString("Outcome unknown: it may or may not have run.\n")
			case call.Exit == nil:
				fmt.Fprintf(&b, "Did not run:\n%s\n", *call.Result)
			default:
				fmt.Fprintf(&b, "Exit %d:\n%s\n", *call.Exit, *call.Result)
			}
		}
	}
	return b.String()
}

// form is a reply in the form.
type form struct {
	kind  Kind
	text  string
	needs string
}

// parseReply reads the last model call's text: exactly one JSON object {"kind", "text", "needs"},
// its kind one the request offers, its text a string (non-empty but for none), its needs a string
// or null, non-empty for not_answered and needs_work.
func parseReply(text string, mayAskNothing bool) (form, error) {
	trimmed := bytes.TrimSpace([]byte(text))
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return form{}, errors.New("the reply is not a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	var reply struct {
		Kind  *string `json:"kind"`
		Text  *string `json:"text"`
		Needs *string `json:"needs"`
	}
	if err := decoder.Decode(&reply); err != nil {
		return form{}, fmt.Errorf("the reply is not the reply form: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return form{}, errors.New("the reply holds more than one JSON object")
	}
	if reply.Kind == nil || reply.Text == nil {
		return form{}, errors.New("the reply has no kind or no text")
	}
	f := form{kind: Kind(*reply.Kind), text: *reply.Text}
	if reply.Needs != nil {
		f.needs = *reply.Needs
	}
	switch f.kind {
	case Answered:
	case NotAnswered, NeedsWork:
		if strings.TrimSpace(f.needs) == "" {
			return form{}, fmt.Errorf("a %s reply names no needs", f.kind)
		}
	case None:
		if !mayAskNothing {
			return form{}, errors.New("the reply is none, which this message does not offer")
		}
		return f, nil
	default:
		return form{}, fmt.Errorf("the reply's kind %q is none of the form's", f.kind)
	}
	if strings.TrimSpace(f.text) == "" {
		return form{}, fmt.Errorf("a %s reply with no text", f.kind)
	}
	return f, nil
}
