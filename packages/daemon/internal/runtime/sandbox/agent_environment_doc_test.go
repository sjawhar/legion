package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The "Anatomy of a Sandbox pod" environment paragraph of docs/kubernetes.md tells a reader what a
// `printenv` finds where. Its claim is a negative one — the variables in the table below it are the
// agent process's, so a `printenv` run inside the container finds none of them — and a negative
// claim is exactly the kind a reader disproves in one command and then stops trusting the section
// for. That is the defect this whole document section was written against: the runbook's only
// statement of LEGION_STATE_DIR was one no pod had.
//
// A variable the worker image's own `ENV` sets is in every process of the container, whatever the
// pod spec's `env` carries and whatever the launcher hands the agent. PATH is one: the image sets
// `PATH=/opt/legion/bin:…` (packages/daemon/docker/worker.Dockerfile) and podPath renders the same
// value, its own comment saying so — "in the worker image it is the image PATH's first entry". So a
// `printenv PATH` in the container returns the documented value, and the paragraph's "finds none of
// them", read as written, is false of the table's first row.
//
// The check is therefore an accounting one, not a wording one: every variable the table lists that
// the image itself sets must be named in the paragraph, so the reader is told which of the two
// readings applies to it. It does not say how the paragraph must word the exception, and it does
// not read the table's values — a row whose value is wrong is a reader's job, as it was here.
const (
	agentEnvParagraphStart = "The variables below are the ones a role's agent process carries"
	agentEnvTableHeader    = "| variable | value | renderer |"
)

func TestTheAgentEnvironmentParagraphNamesEveryVariableTheImageItselfSets(t *testing.T) {
	prose, documented := agentEnvironmentParagraph(t)
	image := workerImageEnvironmentNames(t)
	for _, name := range documented {
		if !image[name] {
			continue
		}
		if mentionsBareName(prose, name) {
			continue
		}
		t.Errorf("docs/kubernetes.md's %q paragraph says a printenv inside the container finds none of "+
			"the variables in its table, and never names %s, which the worker image's own ENV "+
			"(packages/daemon/docker/worker.Dockerfile) sets in every process of that container: a "+
			"reader who runs printenv %s there gets the documented value and reads the paragraph as "+
			"wrong. Name it in the paragraph as the exception it is",
			agentEnvParagraphStart, name, name)
	}
}

// agentEnvironmentParagraph is the paragraph's prose, flattened, and the variable names its table
// lists in order. The prose ends where the table's header begins: a name mentioned in a row of the
// table is the row itself, never the paragraph accounting for it.
func agentEnvironmentParagraph(t *testing.T) (prose string, documented []string) {
	t.Helper()
	// The document is at the repository root, five directories above this package. Read it, and fail
	// when it cannot be read: a skip would be an absence that reads exactly like a pass.
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..", "docs", "kubernetes.md"))
	if err != nil {
		t.Fatalf("read docs/kubernetes.md: %v", err)
	}
	lines := strings.Split(string(doc), "\n")
	start := -1
	for i, line := range lines {
		if strings.Contains(line, agentEnvParagraphStart) {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("docs/kubernetes.md has no %q: the paragraph was reworded, and this check stops "+
			"checking the moment it cannot find it", agentEnvParagraphStart)
	}
	header := -1
	for i := start; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == agentEnvTableHeader {
			header = i
			break
		}
	}
	if header < 0 {
		t.Fatalf("docs/kubernetes.md has no %q after %q: the environment table's columns changed, and "+
			"an unbounded search would read a table written for something else",
			agentEnvTableHeader, agentEnvParagraphStart)
	}
	prose = strings.Join(strings.Fields(strings.Join(lines[start:header], " ")), " ")
	// The delimiter row sits between the header and the first data row; every row after it until the
	// table ends names one variable in its first cell.
	for i := header + 2; i < len(lines); i++ {
		row := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(row, "|") {
			break
		}
		cells := strings.Split(strings.Trim(row, "|"), "|")
		name := strings.Trim(strings.TrimSpace(cells[0]), "`")
		if name != "" {
			documented = append(documented, name)
		}
	}
	if len(documented) == 0 {
		t.Fatalf("the table under %q names no variable: its rows are no longer one backticked name "+
			"each, and nothing is being held to the paragraph", agentEnvTableHeader)
	}
	return prose, documented
}

// workerImageEnvironmentNames is what the worker image's own ENV instructions set, which every
// process in a role container carries however it was started. It is read from the Dockerfile rather
// than listed here: a list would have to be edited by whoever adds an ENV, which is exactly the edit
// that gets forgotten.
func workerImageEnvironmentNames(t *testing.T) map[string]bool {
	t.Helper()
	// packages/daemon is three directories above this package; the image's Dockerfile is under it.
	file, err := os.ReadFile(filepath.Join("..", "..", "..", "docker", "worker.Dockerfile"))
	if err != nil {
		t.Fatalf("read packages/daemon/docker/worker.Dockerfile: %v", err)
	}
	names := map[string]bool{}
	continued := false
	for _, line := range strings.Split(string(file), "\n") {
		trimmed := strings.TrimSpace(line)
		assignments := ""
		switch {
		case strings.HasPrefix(trimmed, "ENV "):
			assignments = strings.TrimPrefix(trimmed, "ENV ")
		case continued:
			assignments = trimmed
		default:
			continue
		}
		continued = strings.HasSuffix(trimmed, `\`)
		for _, field := range strings.Fields(strings.TrimSuffix(assignments, `\`)) {
			name, _, ok := strings.Cut(field, "=")
			if ok && name != "" {
				names[name] = true
			}
		}
	}
	if len(names) == 0 {
		t.Fatalf("packages/daemon/docker/worker.Dockerfile yields no ENV name: the parse is reading " +
			"nothing, and every documented variable would pass against an empty set")
	}
	return names
}

// mentionsBareName reports whether the prose names the variable as itself rather than inside a
// longer identifier, so DISPATCH_TOKEN_FILE never stands in for DISPATCH_TOKEN.
func mentionsBareName(prose, name string) bool {
	for offset := 0; ; {
		at := strings.Index(prose[offset:], name)
		if at < 0 {
			return false
		}
		at += offset
		before := byte(' ')
		if at > 0 {
			before = prose[at-1]
		}
		after := byte(' ')
		if end := at + len(name); end < len(prose) {
			after = prose[end]
		}
		if !identifierByte(before) && !identifierByte(after) {
			return true
		}
		offset = at + len(name)
	}
}

func identifierByte(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}
