package bus

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// contractsSource reads one file of packages/contracts/src, the definitions this package mirrors.
func contractsSource(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "..", "..", "contracts", "src", name)
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(source)
}

// capture is the one submatch of pattern in source, which must match exactly once.
func capture(t *testing.T, source, what, pattern string) string {
	t.Helper()
	matches := regexp.MustCompile(pattern).FindAllStringSubmatch(source, -1)
	if len(matches) != 1 {
		t.Fatalf("%s: %d matches of %s in the TypeScript source, want exactly 1; the definition moved or was renamed", what, len(matches), pattern)
	}
	return matches[0][1]
}

// A Legion controller's notice lane and the role token whose holder it is for are built here from
// the one project token the lane names (ControllerNoticeRole), and in TypeScript by
// legionControllerNoticeSubject and controllerToken (packages/contracts/src). The generator that
// mirrors contracts into Go (packages/contracts/scripts/gen-go.ts) exports neither, so this test
// reads the TypeScript source and holds the two sides to one definition: a rename there that this
// file does not follow would leave the listener refusing a controller's registration of its own
// lane (ReachesPrivateStreamSubject reads a role token no claim names), and the controller would
// receive no notice, with nothing else failing.
func TestControllerNoticeRoleMatchesTheContractsDefinitions(t *testing.T) {
	const project = "omp"
	subjects := contractsSource(t, "subject.ts")
	roles := contractsSource(t, "legion-roles.ts")

	prefix := capture(t, subjects, "LEGION_NOTICE_TOPIC_PREFIX", `LEGION_NOTICE_TOPIC_PREFIX = "([^"]+)"`)
	if prefix != legionNoticeTopicPrefix {
		t.Fatalf("legionNoticeTopicPrefix = %q, want the contracts' LEGION_NOTICE_TOPIC_PREFIX %q", legionNoticeTopicPrefix, prefix)
	}
	laneTemplate := capture(t, subjects, "legionControllerNoticeSubject", "return `(\\$\\{LEGION_NOTICE_TOPIC_PREFIX\\}[^`]*)`")
	lane := strings.NewReplacer("${LEGION_NOTICE_TOPIC_PREFIX}", prefix, "${project}", project).Replace(laneTemplate)
	tokenTemplate := capture(t, roles, "controllerToken", "(?s)export function controllerToken\\(project: string\\): string \\{.*?return `([^`]*)`")
	wantRole := strings.ReplaceAll(tokenTemplate, "${project}", project)

	role, isLane := ControllerNoticeRole(lane)
	if !isLane {
		t.Fatalf("ControllerNoticeRole(%q) read the contracts' lane as no lane at all", lane)
	}
	if role != wantRole {
		t.Fatalf("ControllerNoticeRole(%q) = %q, want the contracts' controllerToken(%q) = %q", lane, role, project, wantRole)
	}
	if !NamesControllerNoticeLane([]string{lane}) {
		t.Fatalf("NamesControllerNoticeLane(%q) is false: a registration naming the lane would read no roles", lane)
	}
}
