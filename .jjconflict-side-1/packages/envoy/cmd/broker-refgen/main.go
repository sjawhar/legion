// Command broker-refgen writes the secrets broker's generated reference pages from its Go source:
// api.md, the HTTP API from the route table and its handlers; config.md, the configuration from
// config.Load and cmd/broker; and errors.md, every refusal the API, the host helper and the
// agent-secrets CLI can answer. It parses the source with go/ast and runs none of it.
//
//	broker-refgen <repository root> <out dir>
//
// docs/site/generators/broker-reference.sh runs it at site build. It refuses to write a page from
// source it cannot read, a route without a comment saying what it does, or a variable or code
// without a doc comment, so a page can only be complete, and it builds all three pages before it
// writes any.
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: broker-refgen <repository root> <out dir>")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, "broker-refgen:", err)
		os.Exit(1)
	}
}

func run(rootArg, out string) error {
	root, err := filepath.Abs(rootArg)
	if err != nil {
		return err
	}
	a, err := readAPI(root)
	if err != nil {
		return err
	}
	api, err := apiPage(a)
	if err != nil {
		return err
	}
	config, err := configPage(root)
	if err != nil {
		return err
	}
	errs, err := errorsPage(root, a)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	for name, page := range map[string]string{"api.md": api, "config.md": config, "errors.md": errs} {
		if err := os.WriteFile(filepath.Join(out, name), []byte(page), 0o644); err != nil {
			return err
		}
	}
	return nil
}
