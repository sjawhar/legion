// Package deliveryaudit holds the read-only SQL that audits whether a person's Dispatch comments
// reached the agent sessions they were owed to (INFRA-596): comments.sql lists every person's comment
// of a window with each session or role it was owed or addressed to, and trace.sql shows everything
// recorded about one comment's delivery. An operator runs them with psql (README.md); the package's
// test runs the same embedded bytes through pgx, with each psql variable bound as a parameter, over
// records the real API handlers and outbox wrote.
package deliveryaudit

import (
	_ "embed"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// CommentsSQL is comments.sql: one select over the psql variables from and to.
//
//go:embed comments.sql
var CommentsSQL string

// TraceSQL is trace.sql: several selects over the psql variable comment.
//
//go:embed trace.sql
var TraceSQL string

// psqlVariable matches psql's quoted interpolation of a variable, :'name'.
var psqlVariable = regexp.MustCompile(`:'([A-Za-z_][A-Za-z0-9_]*)'`)

// Bind rewrites script's psql variables (:'name') as positional parameters, the first of names as
// $1 and so on, so the statement psql runs is the statement a driver runs. A variable script uses
// that names lacks, or a name script never uses, is an error.
func Bind(script string, names ...string) (string, error) {
	used := map[string]bool{}
	var missing []string
	bound := psqlVariable.ReplaceAllStringFunc(script, func(match string) string {
		name := psqlVariable.FindStringSubmatch(match)[1]
		index := slices.Index(names, name)
		if index < 0 {
			if !slices.Contains(missing, name) {
				missing = append(missing, name)
			}
			return match
		}
		used[name] = true
		return "$" + strconv.Itoa(index+1)
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("script uses psql variables %v that are not bound", missing)
	}
	for _, name := range names {
		if !used[name] {
			return "", fmt.Errorf("script never uses psql variable %q", name)
		}
	}
	return bound, nil
}

// Statements splits script into its statements: each ends with a semicolon that ends its line,
// which is how both scripts are written. A piece holding only comments and blank lines is dropped.
func Statements(script string) []string {
	var statements []string
	var current strings.Builder
	code := false
	for line := range strings.Lines(script) {
		current.WriteString(line)
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "--") {
			code = true
		}
		if strings.HasSuffix(trimmed, ";") && !strings.HasPrefix(trimmed, "--") {
			if code {
				statements = append(statements, current.String())
			}
			current.Reset()
			code = false
		}
	}
	if code {
		statements = append(statements, current.String())
	}
	return statements
}
