package answer

import (
	"context"
	"encoding/json"
	"fmt"

	aisdk "github.com/grafana/ai-sdk"
	"github.com/grafana/ai-sdk/provider"
	"github.com/grafana/ai-sdk/schema"
)

// tool is one of the loop's tools: its name, what the model reads of it, the JSON schema of its
// input, and what a call runs.
type tool struct {
	name        string
	description string
	schema      string
	run         func(ctx context.Context, l *loop, input json.RawMessage) ghOutput
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

// toolSet is the loop's tools as the SDK offers and runs them.
func (l *loop) toolSet() aisdk.ToolSet {
	set := make(aisdk.ToolSet, len(tools))
	for _, t := range tools {
		input, err := schema.SchemaFromJSON(json.RawMessage(t.schema))
		if err != nil {
			panic(fmt.Sprintf("answer: tool %s's schema: %v", t.name, err))
		}
		set[t.name] = aisdk.Tool{
			Description: t.description,
			InputSchema: input,
			Execute: func(ctx context.Context, raw json.RawMessage, _ aisdk.ToolExecutionOptions) (json.RawMessage, error) {
				return json.Marshal(t.run(ctx, l, raw))
			},
			ToModelOutput: func(out aisdk.ToolOutputContext) (*provider.ToolResultOutput, error) {
				return modelOutput(out.Output), nil
			},
		}
	}
	return set
}

// ghOutput is what a gh call returned: ghtool.Result as the tool's output.
type ghOutput struct {
	Output string `json:"output"`
	Exit   int    `json:"exit"`
	Ran    bool   `json:"ran"`
}

// runGH runs one gh call through the request's GH, numbered after every call before it (the
// earlier run's included). Calls run one at a time, so their numbers follow the order they ran.
func runGH(ctx context.Context, l *loop, input json.RawMessage) ghOutput {
	l.gh.Lock()
	defer l.gh.Unlock()
	seq := l.nextSeq
	l.nextSeq++
	var call struct {
		Args json.RawMessage `json:"args"`
	}
	args := string(input)
	if json.Unmarshal(input, &call) == nil && len(call.Args) > 0 {
		args = string(call.Args)
	}
	result := l.req.GH.Run(ctx, seq, args)
	return ghOutput{Output: result.Output, Exit: result.Exit, Ran: result.Ran}
}

// modelOutput is what the model reads of a gh call: gh's output, and its exit status when it
// failed; an error result when gh failed or never ran.
func modelOutput(raw json.RawMessage) *provider.ToolResultOutput {
	var out ghOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return &provider.ToolResultOutput{Type: provider.ToolOutputErrorText, Text: fmt.Sprintf("the tool's output could not be read: %v", err)}
	}
	switch {
	case !out.Ran:
		return &provider.ToolResultOutput{Type: provider.ToolOutputErrorText, Text: out.Output}
	case out.Exit != 0:
		return &provider.ToolResultOutput{Type: provider.ToolOutputErrorText, Text: fmt.Sprintf("%s\n[gh exited %d]", out.Output, out.Exit)}
	default:
		return &provider.ToolResultOutput{Type: provider.ToolOutputText, Text: out.Output}
	}
}
