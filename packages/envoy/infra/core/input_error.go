package core

import "fmt"

// InputError is a refusal of one input, which Input names as a component's schema does (machineId,
// natsUrls). Its message is the refusal's own, so an error reads the same with or without it. A
// caller that took the input from somewhere else, such as a command's flag, finds which input was
// refused with errors.As, never by parsing the message.
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
