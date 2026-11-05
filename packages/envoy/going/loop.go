package going

import (
	"context"
	"errors"
	"strings"
	"time"
)

// This file is the seam the shared model loop replaces. The loop that calls a model, hands a tool
// call to the tool and calls the model again inside the run's bounds is landing as
// github.com/sjawhar/envoy/agentloop (dispatch://INFRA-1311), which every caller of a model in
// this tree will share; this route needs that loop before it exists, so it assembles the calls
// here and nowhere else, in the names the agreed interface
// (dispatch://INFRA-1311/artifact/agentloop-interface-md) gives them.
//
// What is mirrored, so the swap is a replacement and not a redesign: loopRequest's System, Input,
// Tools and Bounds are agentloop.Request's own fields, each tool carries its own Run closure
// because a tool's budget is that tool's logic, loopBounds.Calls bounds model calls alone,
// OnText takes each content delta as it arrives, and loopResult carries the text and a loopFault
// of time, calls or provider_error. runLoop returns an error for a callback's error or the
// context ending, with the result so far, as agentloop.Run does.
//
// The swap is: replace runLoop with agentloop.Run, convert this package's Message and ToolCall to
// agentloop's Message, Block and ToolCall, and give agentloop an Endpoint in place of the Client.
// It needs Go 1.27.3 (dispatch://INFRA-1312) and agentloop on main first, which is why nothing
// else in this package assembles a model call.

// loopFault names why a run ended with no answer, in agentloop.Fault's own words. The route maps
// them to the stream's error codes at its boundary, since the fault names are agentloop's and the
// codes are the stream's.
type loopFault string

const (
	// loopFaultTime is the run reaching the time it had, which includes a call the quota could
	// not pace inside that time.
	loopFaultTime loopFault = "time"
	// loopFaultCalls is the call budget running out with the model still calling tools.
	loopFaultCalls loopFault = "calls"
	// loopFaultProviderError is the model service refusing or failing a call.
	loopFaultProviderError loopFault = "provider_error"
)

// loopBounds is what one run may spend: the time the whole run has, and the model calls it may
// make. A tool's own budget is not here, because it is that tool's logic.
type loopBounds struct {
	Time  time.Duration
	Calls int
}

// loopTool is one tool the model may call. Run answers the model with the tool's own result and
// says whether that result is an error, which is how a refused call, a failed one and an unknown
// tool all reach the model as something it can act on rather than ending the run.
type loopTool struct {
	Name        string
	Description string
	// Schema is the arguments' JSON Schema, as Tool.Parameters carries it; agentloop takes the
	// same schema as json.RawMessage.
	Schema map[string]any
	Run    func(ctx context.Context, call ToolCall) (output string, isError bool)
}

// loopRequest is one run of the loop: the system message, the input the run opens with, the tools
// and the bounds, with OnText taking every content delta of every turn so the answer reaches the
// reader while the model is still writing it.
type loopRequest struct {
	// Model is the chat client the call is made through. agentloop takes an Endpoint instead and
	// builds its own provider from it, which is the one field of this request the swap replaces
	// rather than converts.
	Model  Client
	System string
	Input  []Message
	Tools  []loopTool
	Bounds loopBounds
	OnText func(delta string) error
}

// loopResult is what one run came to: the text the model wrote, and the fault that ended the run
// with no answer (empty when the model answered). The text is here because the agreed Result
// carries it; this route reads the answer through OnText, which is what lets it stream.
type loopResult struct {
	Text  string
	Fault loopFault
	// Detail says what a provider error was, for a log and never for a reader: Answerer.Run
	// writes it once, where that fault becomes the stream's ANSWER_FAILED.
	Detail string
}

// runLoop makes the model calls of one run: it sends the system message and the input, hands each
// tool call the model makes to that tool and sends its result back, and stops at the first turn
// that answers, at the call bound, or at a fault. The error it returns is a callback's error or
// the context ending, with the result the run reached, exactly as agentloop.Run reports them.
func runLoop(ctx context.Context, request loopRequest) (loopResult, error) {
	if request.Bounds.Time > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, request.Bounds.Time)
		defer cancel()
	}
	messages := make([]Message, 0, len(request.Input)+1+2*request.Bounds.Calls)
	if request.System != "" {
		messages = append(messages, Message{Role: "system", Content: request.System})
	}
	messages = append(messages, request.Input...)
	tools := make([]Tool, len(request.Tools))
	for i, tool := range request.Tools {
		tools[i] = Tool{Name: tool.Name, Description: tool.Description, Parameters: tool.Schema}
	}

	var text strings.Builder
	var callbackErr error
	onText := func(delta string) error {
		text.WriteString(delta)
		if request.OnText == nil {
			return nil
		}
		if err := request.OnText(delta); err != nil {
			callbackErr = err
			return err
		}
		return nil
	}

	for range request.Bounds.Calls {
		calls, err := request.Model.Chat(ctx, ChatRequest{Messages: messages, Tools: tools}, onText)
		switch {
		case callbackErr != nil:
			// A callback's error ends the run at that frame: the caller it belongs to is the one
			// that must hear it, so it is returned rather than named a fault.
			return loopResult{Text: text.String()}, callbackErr
		case err != nil && isDeadline(ctx, err):
			return loopResult{Text: text.String(), Fault: loopFaultTime}, err
		case err != nil:
			return loopResult{Text: text.String(), Fault: loopFaultProviderError, Detail: err.Error()}, nil
		case len(calls) == 0:
			// The turn carried no tool call, so it was the answer.
			return loopResult{Text: text.String()}, nil
		}
		messages = append(messages, Message{Role: "assistant", ToolCalls: calls})
		for _, call := range calls {
			output, _ := runTool(ctx, request.Tools, call)
			messages = append(messages, Message{Role: "tool", ToolCallID: call.ID, Content: output})
		}
	}
	return loopResult{Text: text.String(), Fault: loopFaultCalls}, nil
}

// runTool hands one call to the tool it names. A name no tool carries is answered to the model
// rather than ended on, because a model that called a tool that is not there can still answer
// from what it has.
func runTool(ctx context.Context, tools []loopTool, call ToolCall) (string, bool) {
	for _, tool := range tools {
		if tool.Name == call.Name {
			return tool.Run(ctx, call)
		}
	}
	return "There is no tool of that name. Answer from what you have.", true
}

// isDeadline reports whether err is this run running out of time: the context ended, or a call
// could not be made inside the time the run had left. Both are one ending, so the caller needs no
// second code for the quota.
func isDeadline(ctx context.Context, err error) bool {
	return ctx.Err() != nil ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled)
}
