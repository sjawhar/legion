package main

import (
	"os"
	"strings"
	"testing"
)

// The controller's pod (`controller: daemon`) has one init container, `workspace-init controller`,
// on the volume its Sandbox owns: it leaves that volume as it found it — nothing mounts a
// directory of it but the controller's own working root — provisions no workspace, and installs
// no gh shim, since the controller holds no GitHub credential.
func TestWorkspaceInitControllerMakesNothingOnTheControllersVolume(t *testing.T) {
	root := t.TempDir()
	code, stdout, stderr := runWorkspaceInitHere([]string{"controller", "--root", root})
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q; want 0", code, stdout, stderr)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("the controller's volume holds %v, want nothing", names)
	}
}

// The controller's init container takes only its volume's root, an absolute path, and refuses the
// provisioning token outright, as `workspace-init provision` does: nothing on the controller's pod
// ever holds it.
func TestWorkspaceInitControllerRefusesARelativeRootAndTheProvisioningToken(t *testing.T) {
	if code, _, stderr := runWorkspaceInitHere([]string{"controller", "--root", "legion"}); code != 1 || !strings.Contains(stderr, `--root must be an absolute path (got "legion")`) {
		t.Fatalf("relative root: exit %d, stderr %q", code, stderr)
	}
	root := t.TempDir()
	t.Setenv(provisionTokenFileEnv, "/var/run/legion/provision/LEGION_PROVISION_TOKEN")
	code, _, stderr := runWorkspaceInitHere([]string{"controller", "--root", root})
	if code != 1 || !strings.Contains(stderr, provisionTokenFileEnv+" is set") {
		t.Fatalf("provisioning token: exit %d, stderr %q", code, stderr)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("a refused run wrote %v (%v) to the controller's volume, want nothing", entries, err)
	}
}
