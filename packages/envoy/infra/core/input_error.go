package core

import "fmt"

// InputError is a refusal of one input of a component, which Input names as the component's schema
// does (machineId, natsUrls). Its message is the refusal's own, so an error reads the same with or
// without it. The refusals `envoy-infra render` shares with a component carry it, so the command
// can name the flag that set the refused input.
type InputError struct {
	Input string
	Err   error
}

func (e *InputError) Error() string { return e.Err.Error() }
func (e *InputError) Unwrap() error { return e.Err }

// InputErrorf returns an InputError refusing input, with the message fmt.Errorf makes of format
// and args.
func InputErrorf(input, format string, args ...any) error {
	return &InputError{Input: input, Err: fmt.Errorf(format, args...)}
}
