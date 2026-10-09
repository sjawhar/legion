package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
)

// sessionsCommands is `legion sessions`, the operator's hand on Oh My Pi's session database
// (runtime.kubernetes.session_store postgres), and the one list of its subcommands.
var sessionsCommands = map[string]command{
	"import": runSessionsImport,
}

func runSessions(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runSubcommand(ctx, "sessions", sessionsCommands, args, stdout, stderr)
}

const sessionsImportUsage = "usage: legion sessions import --dsn-file <file> --claims <file|-> [--tree <KEY>] [--tree-volume <dir>]"

// importedClaim is what `legion sessions import` reads of one claim `legion claims list --json`
// prints: its token, tree and recorded session file. Every other member is the daemon's and
// ignored, so the list an earlier release prints reads alike.
type importedClaim struct {
	Token       string `json:"token"`
	Tree        string `json:"tree"`
	SessionFile string `json:"sessionFile"`
}

// runSessionsImport is `legion sessions import`: the one-off copy of every recorded session file
// into the session table, before a deployment that kept its sessions as files on its tree volumes
// turns runtime.kubernetes.session_store postgres on, and before those volumes go. It reads the
// claims `legion claims list --json` printed (--claims, `-` for stdin), each claim of --tree alone
// when given, and copies each claim's session file into the table whose URL --dsn-file holds,
// under the path the claim recorded, which is the key Oh My Pi resumes the claim's session by
// (ompsessions.Import). The file is read at that path, or, with --tree-volume, from the tree volume
// mounted there (sandbox.SessionOnVolume). It prints one line per claim, then a count of each
// outcome, and exits 1 when any claim's session could not be copied: a file it cannot read, or a
// session the table already holds with other content. A rerun copies nothing twice: a session the
// table already holds byte for byte is reported as copied before.
func runSessionsImport(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("sessions import", sessionsImportUsage, stderr)
	dsnFile := flags.String("dsn-file", "", "file holding the session database's postgres:// URL (required)")
	claimsFile := flags.String("claims", "", "the claims `legion claims list --json` printed, or - for stdin (required)")
	tree := flags.String("tree", "", "copy the sessions of this tree's claims alone")
	treeVolume := flags.String("tree-volume", "", "read each session from the tree volume mounted here rather than at its recorded path")
	if code, ok := parseFlags(flags, args); !ok {
		return code
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "legion sessions import: unexpected argument %q\n%s\n", flags.Arg(0), sessionsImportUsage)
		return 2
	}
	for _, required := range []struct{ name, value string }{{"dsn-file", *dsnFile}, {"claims", *claimsFile}} {
		if required.value == "" {
			fmt.Fprintf(stderr, "legion sessions import: --%s is required\n%s\n", required.name, sessionsImportUsage)
			return 2
		}
	}
	claims, err := readImportedClaims(*claimsFile)
	if err != nil {
		fmt.Fprintf(stderr, "legion sessions import: %v\n", err)
		return 1
	}
	conn, err := ompsessions.Connect(ctx, *dsnFile)
	if err != nil {
		fmt.Fprintf(stderr, "legion sessions import: %v\n", err)
		return 1
	}
	defer conn.Close(context.Background())

	counts := map[string]int{}
	for _, c := range claims {
		if *tree != "" && c.Tree != *tree {
			continue
		}
		if c.SessionFile == "" {
			fmt.Fprintf(stdout, "%s recorded no session: nothing to copy\n", c.Token)
			counts["nothing recorded"]++
			continue
		}
		outcome, err := importSession(ctx, conn, c.SessionFile, *treeVolume)
		var conflict *ompsessions.ConflictError
		switch {
		case errors.As(err, &conflict):
			fmt.Fprintf(stdout, "%s refused %s: %v\n", c.Token, c.SessionFile, err)
			counts["failed"]++
		case err != nil:
			fmt.Fprintf(stdout, "%s failed %s: %v\n", c.Token, c.SessionFile, err)
			counts["failed"]++
		case outcome.Outcome == ompsessions.Copied:
			fmt.Fprintf(stdout, "%s copied %s (%s)\n", c.Token, c.SessionFile, outcome.Summary)
			counts["copied"]++
		default:
			fmt.Fprintf(stdout, "%s copied before %s (identical, %s)\n", c.Token, c.SessionFile, outcome.Summary)
			counts["copied before"]++
		}
	}
	var summary []string
	for _, outcome := range []string{"copied", "copied before", "nothing recorded", "failed"} {
		summary = append(summary, fmt.Sprintf("%s %d", outcome, counts[outcome]))
	}
	fmt.Fprintf(stdout, "legion sessions import: %s\n", strings.Join(summary, ", "))
	if counts["failed"] > 0 {
		return 1
	}
	return 0
}

// readImportedClaims reads the claims list from file, or from stdin when file is `-`, in token
// order.
func readImportedClaims(file string) ([]importedClaim, error) {
	var body []byte
	var err error
	if file == "-" {
		body, err = io.ReadAll(os.Stdin)
	} else {
		body, err = os.ReadFile(file)
	}
	if err != nil {
		return nil, fmt.Errorf("read the claims list %s: %w", file, err)
	}
	var list struct {
		Claims *[]importedClaim `json:"claims"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("the claims list %s is not what `legion claims list --json` prints: %w", file, err)
	}
	if list.Claims == nil {
		return nil, fmt.Errorf("the claims list %s has no claims member, which `legion claims list --json` prints", file)
	}
	claims := *list.Claims
	slices.SortFunc(claims, func(a, b importedClaim) int { return strings.Compare(a.Token, b.Token) })
	return claims, nil
}

// importedSession is what importSession did with one session, and the content it read.
type importedSession struct {
	Outcome ompsessions.Outcome
	Summary ompsessions.Summary
}

// importSession copies the session recorded at file into the table: read at file itself, or from
// the tree volume mounted at treeVolume when one is given, with the file's modification time.
func importSession(ctx context.Context, conn *pgx.Conn, file, treeVolume string) (importedSession, error) {
	source := file
	if treeVolume != "" {
		onVolume, err := sandbox.SessionOnVolume(file, treeVolume)
		if err != nil {
			return importedSession{}, err
		}
		source = onVolume
	}
	info, err := os.Stat(source)
	if err != nil {
		return importedSession{}, fmt.Errorf("read the session file: %w", err)
	}
	content, err := os.ReadFile(source)
	if err != nil {
		return importedSession{}, fmt.Errorf("read the session file: %w", err)
	}
	outcome, err := ompsessions.Import(ctx, conn, file, content, info.ModTime())
	if err != nil {
		return importedSession{}, err
	}
	return importedSession{Outcome: outcome, Summary: ompsessions.Summarize(content)}, nil
}
