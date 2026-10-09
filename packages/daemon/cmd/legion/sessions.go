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
	"github.com/sjawhar/legion/daemon/internal/store"
)

// sessionsCommands is `legion sessions`, the operator's hand on Oh My Pi's session database
// (runtime.kubernetes.session_store postgres), and the one list of its subcommands.
var sessionsCommands = map[string]command{
	"import": runSessionsImport,
}

func runSessions(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runSubcommand(ctx, "sessions", sessionsCommands, args, stdout, stderr)
}

const sessionsImportUsage = "usage: legion sessions import --dsn-file <file> --claims <file|-> ([--tree <KEY>] [--tree-volume <dir>] | --mark-lost --daemon-dsn-file <file>)"

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
// mounted there (sandbox.SessionOnVolume). It prints one line per claim it copies, then one line
// for every claim of the whole list, whatever --tree says, whose recorded session the table does
// not hold (missingSessions), then a count of each outcome. It exits 1 when any claim's session
// could not be copied (a file it cannot read, or a session the table already holds with other
// content) or a recorded session of the claims it was asked to copy is missing from the table; a
// missing one of another tree is reported and leaves the exit as it is. A rerun copies nothing
// twice: a session the table already holds byte for byte is reported as copied before.
//
// With --mark-lost it copies nothing: it reports every claim whose recorded session the table does
// not hold and marks each lost in the daemon's own database (--daemon-dsn-file), as the daemon marks
// a claim whose tree volume was lost (supervise's loseSession): no session, no session file, its
// workspace lost. Under SQL storage such a claim would otherwise fail every launch, its launcher
// finding no row to resume; marked, it starts a fresh session in a workspace recovered from its
// issue's branch, as file storage starts it once its volume is gone. Run it with the daemon that
// owns that database stopped, since a running daemon holds its claims in memory and writes them
// back. Only a claim still recording the session the list says is marked.
func runSessionsImport(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("sessions import", sessionsImportUsage, stderr)
	dsnFile := flags.String("dsn-file", "", "file holding the session database's postgres:// URL (required)")
	claimsFile := flags.String("claims", "", "the claims `legion claims list --json` printed, or - for stdin (required)")
	tree := flags.String("tree", "", "copy the sessions of this tree's claims alone")
	treeVolume := flags.String("tree-volume", "", "read each session from the tree volume mounted here rather than at its recorded path")
	markLost := flags.Bool("mark-lost", false, "copy nothing; mark each claim whose session the table lacks lost in the daemon's database")
	daemonDSNFile := flags.String("daemon-dsn-file", "", "with --mark-lost, file holding the stopped daemon's own postgres_dsn")
	if code, ok := parseFlags(flags, args); !ok {
		return code
	}
	usage := func(format string, args ...any) int {
		fmt.Fprintf(stderr, "legion sessions import: "+format+"\n%s\n", append(args, sessionsImportUsage)...)
		return 2
	}
	if flags.NArg() != 0 {
		return usage("unexpected argument %q", flags.Arg(0))
	}
	for _, required := range []struct{ name, value string }{{"dsn-file", *dsnFile}, {"claims", *claimsFile}} {
		if required.value == "" {
			return usage("--%s is required", required.name)
		}
	}
	switch {
	case *markLost && *daemonDSNFile == "":
		return usage("--mark-lost needs --daemon-dsn-file, the database whose claims it marks")
	case !*markLost && *daemonDSNFile != "":
		return usage("--daemon-dsn-file is read only with --mark-lost")
	case *markLost && (*tree != "" || *treeVolume != ""):
		return usage("--mark-lost copies nothing, so it takes neither --tree nor --tree-volume")
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
	if *markLost {
		return markLostSessions(ctx, conn, claims, *daemonDSNFile, stdout, stderr)
	}

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
	missing, err := missingSessions(ctx, conn, claims)
	if err != nil {
		fmt.Fprintf(stderr, "legion sessions import: %v\n", err)
		return 1
	}
	missingHere := 0
	for _, c := range missing {
		fmt.Fprintf(stdout, "%s missing %s: the session table holds no such session (tree %s)\n", c.Token, c.SessionFile, c.Tree)
		if *tree == "" || c.Tree == *tree {
			missingHere++
		}
	}
	counts["missing"] = len(missing)
	var summary []string
	for _, outcome := range []string{"copied", "copied before", "nothing recorded", "failed", "missing"} {
		summary = append(summary, fmt.Sprintf("%s %d", outcome, counts[outcome]))
	}
	fmt.Fprintf(stdout, "legion sessions import: %s\n", strings.Join(summary, ", "))
	if counts["failed"] > 0 || missingHere > 0 {
		return 1
	}
	return 0
}

// missingSessions are the claims of the list, every tree's, that record a session the table does
// not hold: under SQL storage each would fail every launch until it is copied or marked lost.
func missingSessions(ctx context.Context, conn *pgx.Conn, claims []importedClaim) ([]importedClaim, error) {
	var missing []importedClaim
	for _, c := range claims {
		if c.SessionFile == "" {
			continue
		}
		found, err := ompsessions.Exists(ctx, conn, c.SessionFile)
		if err != nil {
			return nil, err
		}
		if !found {
			missing = append(missing, c)
		}
	}
	return missing, nil
}

// markLostSessions is --mark-lost: every claim whose recorded session the table lacks is marked lost
// in the daemon's database, by token and only while it still records that session file.
func markLostSessions(ctx context.Context, conn *pgx.Conn, claims []importedClaim, daemonDSNFile string, stdout, stderr io.Writer) int {
	missing, err := missingSessions(ctx, conn, claims)
	if err != nil {
		fmt.Fprintf(stderr, "legion sessions import: %v\n", err)
		return 1
	}
	daemonDSN, err := ompsessions.ReadDSN(daemonDSNFile)
	if err != nil {
		fmt.Fprintf(stderr, "legion sessions import: %v\n", err)
		return 1
	}
	daemon, err := pgx.Connect(ctx, daemonDSN)
	if err != nil {
		fmt.Fprintf(stderr, "legion sessions import: the daemon's database: %v\n", store.ConnectError(daemonDSN, err))
		return 1
	}
	defer daemon.Close(context.Background())
	marked, failed := 0, 0
	for _, c := range missing {
		tag, err := daemon.Exec(ctx, "UPDATE claims SET session = '', session_file = '', workspace_lost = true WHERE token = $1 AND session_file = $2",
			c.Token, c.SessionFile)
		switch {
		case err != nil:
			fmt.Fprintf(stdout, "%s failed to mark lost: %v\n", c.Token, err)
			failed++
		case tag.RowsAffected() == 0:
			fmt.Fprintf(stdout, "%s failed to mark lost: the daemon's database holds no such claim recording %s\n", c.Token, c.SessionFile)
			failed++
		default:
			fmt.Fprintf(stdout, "%s marked lost: the session table holds no %s, so it starts a fresh session in a workspace recovered from its issue's branch\n",
				c.Token, c.SessionFile)
			marked++
		}
	}
	fmt.Fprintf(stdout, "legion sessions import: marked lost %d, failed %d\n", marked, failed)
	if failed > 0 {
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
