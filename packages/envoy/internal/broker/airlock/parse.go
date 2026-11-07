package airlock

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Parse reads an airlock.json file. A field the broker does not know is refused rather than
// ignored, so a broker from before a new field refuses a file that uses it instead of
// serving the file with that field's rule missing. Anything after the file's one JSON object
// is refused for the same reason: a second value no reader applies would be a rule its author
// believes is in force. Parse does not validate the file it read; the caller calls Validate.
func Parse(data []byte) (Rules, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var rules Rules
	if err := dec.Decode(&rules); err != nil {
		return Rules{}, parseError(data, err)
	}
	if dec.More() {
		return Rules{}, fmt.Errorf("a second JSON value follows the rules, at %s", position(data, dec.InputOffset()+1))
	}
	return rules, nil
}

// parseError names the place the decoder stopped at, which is what a person needs to find
// it in the file: the byte offset encoding/json counts, and the line and column that offset
// falls on. An unknown field carries no offset in Go's error, so there that error's own
// text is the whole report.
func parseError(data []byte, err error) error {
	var syntax *json.SyntaxError
	var wrongType *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntax):
		return fmt.Errorf("not JSON: %s, at %s", syntax, position(data, syntax.Offset))
	case errors.As(err, &wrongType):
		field := wrongType.Field
		if field == "" {
			field = "the file"
		}
		return fmt.Errorf("%s is not a %s, at %s", field, wrongType.Type, position(data, wrongType.Offset))
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		return errors.New(strings.TrimPrefix(err.Error(), "json: "))
	default:
		return fmt.Errorf("not an airlock rules file: %s", err)
	}
}

// position reads a decoder offset as "byte offset N, line L, column C". encoding/json
// counts the bytes it had read when it stopped, so the offset is the one-based index of the
// byte it stopped on. A column counts bytes, not characters, as the offset does.
func position(data []byte, offset int64) string {
	end := offset - 1
	if end < 0 {
		end = 0
	}
	if end > int64(len(data)) {
		end = int64(len(data))
	}
	line, column := 1, 1
	for _, b := range data[:end] {
		if b == '\n' {
			line, column = line+1, 1
		} else {
			column++
		}
	}
	return fmt.Sprintf("byte offset %d, line %d, column %d", offset, line, column)
}
