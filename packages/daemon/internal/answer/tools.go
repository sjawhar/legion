package answer

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sjawhar/envoy/agentloop"
)

// tool is one of the loop's tools: its name, what the model reads of it, the JSON schema of its
// input, and what a call runs. A call answers with what the model is told and whether the tool
// refused or failed.
type tool struct {
	name        string
	description string
	schema      string
	run         func(ctx context.Context, req Request, call agentloop.ToolCall) (output string, isError bool)
}

// tools are the loop's tools, which the system prompt names and every model call offers.
var tools = []tool{{
	name: "gh",
	description: "Run GitHub's own gh command line with your GitHub access: your role's App, with full access to " +
		"your issue's repository owner and read access to the project's other owners, each call routed to the owner it " +
		"names. Use it to read code at any commit, pull requests, reviews, checks and runs, and to comment, review, push " +
		"or merge when the message asks for it and you judge you should. Every comment or review it posts carries your " +
		"Legion footer, which the tool adds. Commands unsafe on the daemon's host are refused with the reason.",
	schema: `{"type":"object","properties":{"args":{"type":"array","items":{"type":"string"},` +
		`"description":"gh's arguments without the leading gh, one per element; no shell reads them. ` +
		`For example [\"pr\",\"view\",\"123\",\"--json\",\"state,headRefOid,reviews\"]."}},` +
		`"required":["args"],"additionalProperties":false}`,
	run: runGH,
}}

// toolSet is the loop's tools as agentloop offers and runs them for req.
func toolSet(req Request) []agentloop.Tool {
	set := make([]agentloop.Tool, len(tools))
	for i, t := range tools {
		set[i] = agentloop.Tool{
			Name:        t.name,
			Description: t.description,
			Schema:      json.RawMessage(t.schema),
			Run: func(ctx context.Context, call agentloop.ToolCall) (string, bool) {
				return t.run(ctx, req, call)
			},
		}
	}
	return set
}

// runGH runs one gh call through the request's GH, numbered as agentloop numbered the call: the
// calls of the run counted on from the last one an earlier run of the question made
// (Request.Earlier), which is the seq the call is recorded and the gh_calls bound is read
// against. What the model reads is gh's output, with its exit status when it failed; a call gh
// never ran, or one that failed, is an error result.
func runGH(ctx context.Context, req Request, call agentloop.ToolCall) (string, bool) {
	var input struct {
		Args json.RawMessage `json:"args"`
	}
	args := string(call.Input)
	if json.Unmarshal(call.Input, &input) == nil && len(input.Args) > 0 {
		args = string(input.Args)
	}
	result := req.GH.Run(ctx, call.Index, args)
	switch {
	case !result.Ran:
		return result.Output, true
	case result.Exit != 0:
		return fmt.Sprintf("%s\n[gh exited %d]", result.Output, result.Exit), true
	default:
		return result.Output, false
	}
}
