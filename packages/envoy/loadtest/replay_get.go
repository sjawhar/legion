package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"time"
)

// replay-verify-bundle is stack.sh replay-get's last check of an unpacked bundle: readBundle, which
// refuses a file bundle.meta.json lists that is missing or whose sha256 differs, then a line of
// counts. It prints no text of the bundle.

// bundleVerification is what replay-verify-bundle prints.
type bundleVerification struct {
	Project string    `json:"project"`
	At      time.Time `json:"at"`
	Until   time.Time `json:"until"`
	// MetaSHA256 is bundle.meta.json's sha256, summary.json's replay inputs.bundle_sha256.
	MetaSHA256   string `json:"bundle_meta_sha256"`
	Files        int    `json:"files"`
	Issues       int    `json:"issues"`
	WindowEvents int    `json:"window_events"`
	Roster       int    `json:"roster"`
}

func runReplayVerifyBundle(args []string) error {
	fl := flag.NewFlagSet("replay-verify-bundle", flag.ContinueOnError)
	dir := fl.String("dir", "", "bundle directory")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if *dir == "" || fl.NArg() != 0 {
		return fmt.Errorf("usage: loadgen replay-verify-bundle -dir <bundle>")
	}
	v, err := verifyBundle(*dir)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
	return nil
}

// verifyBundle reads dir as readBundle does and counts what it holds.
func verifyBundle(dir string) (bundleVerification, error) {
	b, err := readBundle(dir)
	if err != nil {
		return bundleVerification{}, fmt.Errorf("bundle %s: %w", dir, err)
	}
	return bundleVerification{
		Project: b.Meta.Project, At: b.Meta.At, Until: b.Meta.Until, MetaSHA256: b.MetaSHA256,
		Files: len(b.Meta.Files), Issues: countExisting(b.State.Issues), WindowEvents: len(b.Window),
		Roster: len(b.Roster),
	}, nil
}
