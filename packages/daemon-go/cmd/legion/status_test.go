package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestStatusIssueRequiresControllerGrant(t *testing.T) {
	t.Setenv("LEGION_GRANT", "")
	t.Setenv("LEGION_GRANT_FILE", "")
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "status", "LEGION-208", "todo"}, &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), "LEGION_GRANT_FILE") {
		t.Fatalf("status issue = %d, stderr %q; want missing controller grant refusal", code, errb.String())
	}
}
