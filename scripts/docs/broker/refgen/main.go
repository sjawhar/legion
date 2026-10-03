// Command refgen writes the secrets broker's generated reference pages from its Go source: the
// HTTP API from the route table and its handlers, the configuration from config.Load and
// cmd/broker, and the error codes from every refusal the API, the host helper and the
// agent-secrets CLI can answer. It parses the source with go/ast and imports nothing from the
// broker, so it needs neither the broker's dependencies nor the network.
//
//	go run . api|config|errors <repository root>
//
// It refuses to write a page from source it cannot read, a route without a comment saying what it
// does, or a variable or code without a doc comment, so a page can only be complete.
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: refgen api|config|errors <repository root>")
		os.Exit(2)
	}
	root, err := filepath.Abs(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, "refgen:", err)
		os.Exit(1)
	}
	var page string
	switch os.Args[1] {
	case "api":
		page, err = apiPage(root)
	case "config":
		page, err = configPage(root)
	case "errors":
		page, err = errorsPage(root)
	default:
		err = fmt.Errorf("unknown page %q; want api, config or errors", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "refgen:", err)
		os.Exit(1)
	}
	fmt.Print(page)
}
