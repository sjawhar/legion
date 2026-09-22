package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestUnknownSubcommandIsUsageError(t *testing.T) {
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "frobnicate"}, &out, &errb)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), `unknown command "frobnicate"`) {
		t.Fatalf("stderr = %q", errb.String())
	}
}

func TestVersionPrintsBuildInfo(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"legion", "version"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.HasPrefix(out.String(), "legion ") {
		t.Fatalf("stdout = %q", out.String())
	}
}
