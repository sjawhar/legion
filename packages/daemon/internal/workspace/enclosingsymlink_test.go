package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A state directory an operator reaches through a symlink is inside whatever repository the
// link's target sits in, and jj reads it that way: a child chdir'd to a path with a symlink in it
// asks the kernel where it stands, and the kernel answers with the resolved path, so jj walks up
// from there and opens the repository above the target. The refusal must read the directory the
// same way, because the lexical path the caller named hides that repository entirely.
//
// This is the configuration config.go:352-353 states is allowed — "The state directory itself is
// not checked: an operator may reach it through a symlink" — standing on a checkout, which is the
// live operator case the fix exists for (a legion.yaml kept inside a checkout, with state_dir
// resolved against it). Without this, provisioning writes the operator's own repository exactly
// as it did before the fix, silently, and reports success.
func TestAStateDirectoryReachedThroughASymlinkIsRefusedInsideARepository(t *testing.T) {
	run := newLocalRunner(t)
	decoy := enclosingRepository(t)
	target := filepath.Join(decoy, "legion state")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "state-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	before := readEnclosingState(t, decoy)

	req := provisionRequest(t)
	req.StateDir = link
	req.Source = FromGitHub(staticToken(testInstallationToken), link)
	working, err := Provision(context.Background(), run, req)
	if err == nil {
		t.Fatalf("Provision through the symlink %s made %s, want a refusal: the link's target is inside %s, so every jj that creates a repository there opens that repository instead", link, working.Clone, decoy)
	}
	resolved, resolveErr := filepath.EvalSymlinks(decoy)
	if resolveErr != nil {
		t.Fatal(resolveErr)
	}
	named := filepath.Join(resolved, ".jj")
	lexical := filepath.Join(decoy, ".jj")
	if !strings.Contains(err.Error(), named) && !strings.Contains(err.Error(), lexical) {
		t.Errorf("Provision = %v, want a refusal naming the enclosing %s", err, named)
	}
	assertEnclosingRepositoryUnchanged(t, decoy, before)
}
