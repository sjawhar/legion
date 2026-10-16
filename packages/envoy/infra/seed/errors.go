package seed

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/aws/smithy-go"
	"google.golang.org/grpc/status"
)

// failed is "<call> <resource> failed: <code>", every store's error for a call that failed: a gRPC
// status's code, an AWS error's code, or what errorClass names it. It never carries the error's own
// text, which can quote the request.
func failed(call, resource string, err error) error {
	if s, ok := status.FromError(err); ok {
		return fmt.Errorf("%s %s failed: %s", call, resource, s.Code())
	}
	return fmt.Errorf("%s %s failed: %s", call, resource, errorClass(err))
}

// sdkWrappers are the error types the SDK and fmt wrap a failure in, which name nothing of it: a
// lost answer is an OperationError around a MaxAttemptsError around a ResponseError (both the AWS
// and the smithy one are http.ResponseError) around the RequestSendError that says what happened.
var sdkWrappers = []string{"*smithy.OperationError", "*retry.MaxAttemptsError", "*http.ResponseError", "*fmt.wrapError"}

// errorClass names err without its text: its name when it has one (errorName), otherwise the type
// of the first error in its chain that is not one of the SDK's wrappers, such as RequestSendError.
func errorClass(err error) string {
	if name, ok := errorName(err); ok {
		return name
	}
	for e := err; ; {
		name := reflect.TypeOf(e).String()
		next := errors.Unwrap(e)
		if next == nil || !slices.Contains(sdkWrappers, name) {
			return name[strings.LastIndex(name, ".")+1:]
		}
		e = next
	}
}

// errorName names err when it says what happened without its text: an AWS error's code, or a
// context's end.
func errorName(err error) (string, bool) {
	var api smithy.APIError
	switch {
	case errors.As(err, &api) && api.ErrorCode() != "":
		return api.ErrorCode(), true
	case errors.Is(err, context.Canceled):
		return "canceled", true
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline exceeded", true
	}
	return "", false
}

// countVersions adds up the versions each page of a store's listing holds, page by page from the
// first (token ""), until a page names no next one. A listing that hands out one page token twice
// fails, naming call and name: neither store's own paging handles it, since the AWS SDK's paginator
// stops quietly, which would undercount, and the Secret Manager iterator follows it forever.
func countVersions(call, name string, page func(token string) (n int, next string, err error)) (int, error) {
	handedOut := map[string]bool{}
	count, token := 0, ""
	for {
		n, next, err := page(token)
		if err != nil {
			return 0, err
		}
		count += n
		if next == "" {
			return count, nil
		}
		if handedOut[next] {
			return 0, fmt.Errorf("%s %s failed: the listing handed out page token %s twice", call, name, next)
		}
		handedOut[next] = true
		token = next
	}
}
