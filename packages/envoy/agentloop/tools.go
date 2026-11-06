package agentloop

import (
	"context"
	"encoding/json"
	"fmt"

	aisdk "github.com/grafana/ai-sdk"
	"github.com/grafana/ai-sdk/provider"
	"github.com/grafana/ai-sdk/schema"
)

// toolOutcome is what one tool call answered: the output the tool wrote and whether the tool
// refused rather than answered. It is what the tool's Execute hands its ToModelOutput, through
// the JSON the SDK keeps between the two.
type toolOutcome struct {
	Output  string `json:"output"`
	IsError bool   `json:"isError"`
}

// toolSet is the run's tools as the SDK offers and runs them, and is nil for a run that
// registers none. A schema the SDK refuses is a run the caller cannot make, and is reported
// before any model call.
func (r *run) toolSet() (aisdk.ToolSet, error) {
	if len(r.req.Tools) == 0 {
		return nil, nil
	}
	set := make(aisdk.ToolSet, len(r.req.Tools))
	for _, t := range r.req.Tools {
		input, err := schema.SchemaFromJSON(t.Schema)
		if err != nil {
			return nil, fmt.Errorf("agentloop: tool %s's schema: %w", t.Name, err)
		}
		set[t.Name] = aisdk.Tool{
			Description: t.Description,
			InputSchema: input,
			// Execute answers with the call's outcome and never with an error, so a
			// tool that refuses goes back to the model as a tool result and the run
			// goes on. The SDK answers a call of a tool the set does not hold with a
			// tool result of its own, which is the same.
			Execute: func(ctx context.Context, raw json.RawMessage, opts aisdk.ToolExecutionOptions) (json.RawMessage, error) {
				return json.Marshal(r.call(ctx, t, opts.ToolCallID, raw))
			},
			// OnInputAvailable takes the call's index. The SDK calls it in the
			// stream's own goroutine, once per tool call of a step, in the order
			// the model asked for them and before any of that step's tools run, so
			// the indexes follow the model's order and not the order the calls are
			// executed in.
			OnInputAvailable: func(_ json.RawMessage, opts aisdk.ToolExecutionOptions) {
				r.index(opts.ToolCallID)
			},
			ToModelOutput: func(out aisdk.ToolOutputContext) (*provider.ToolResultOutput, error) {
				return modelOutput(out.Output), nil
			},
		}
	}
	return set, nil
}

// call runs one call of t under the run's tool lock, held for the whole of the caller's Run, so
// a step's tool calls run one at a time. The call's index is the one it took in the order the
// model asked for it, counting on from Request.FirstToolIndex, so the first call of the run is
// that number and one.
func (r *run) call(ctx context.Context, t Tool, id string, input json.RawMessage) toolOutcome {
	r.tool.Lock()
	defer r.tool.Unlock()
	output, isError := t.Run(ctx, ToolCall{Index: r.index(id), ID: id, Input: input})
	return toolOutcome{Output: output, IsError: isError}
}

// index is one tool call's index, by the call's id: the index the call took when the model
// asked for it, or the next index where it has none, so a call the SDK ran without announcing
// its input is still numbered.
func (r *run) index(id string) int {
	r.numbering.Lock()
	defer r.numbering.Unlock()
	if i, taken := r.indexes[id]; taken {
		return i
	}
	i := r.next
	r.indexes[id] = i
	r.next++
	return i
}

// modelOutput is what the model is told of one tool call: the tool's own output, as a result or,
// where the tool refused, as error text. An outcome that cannot be read back is error text
// naming why.
func modelOutput(raw json.RawMessage) *provider.ToolResultOutput {
	var outcome toolOutcome
	if err := json.Unmarshal(raw, &outcome); err != nil {
		return &provider.ToolResultOutput{Type: provider.ToolOutputErrorText, Text: fmt.Sprintf("the tool's output could not be read: %v", err)}
	}
	kind := provider.ToolOutputText
	if outcome.IsError {
		kind = provider.ToolOutputErrorText
	}
	return &provider.ToolResultOutput{Type: kind, Text: outcome.Output}
}
