package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
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
	"import":    runSessionsImport,
	"mark-lost": runSessionsMarkLost,
}

func runSessions(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runSubcommand(ctx, "sessions", sessionsCommands, args, stdout, stderr)
}

const (
	sessionsImportUsage   = "usage: legion sessions import --dsn-file <file> --claims <file|-> [--tree <KEY>] [--tree-volume <dir>]"
	sessionsMarkLostUsage = "usage: legion sessions mark-lost --dsn-file <file> --claims <file|-> --daemon-dsn-file <file>"
)

// sessionsInput is what both `legion sessions` commands read first: --dsn-file and --claims,
// registered on their flag set by addSessionsInput.
type sessionsInput struct {
	command, usage      string
	dsnFile, claimsFile *string
}

func addSessionsInput(command, usage string, flags *flag.FlagSet) sessionsInput {
	return sessionsInput{
		command: command, usage: usage,
		dsnFile:    flags.String("dsn-file", "", "file holding the session database's postgres:// URL (required)"),
		claimsFile: flags.String("claims", "", "the claims `legion claims list --json` printed, or - for stdin (required)"),
	}
}

// open refuses a positional argument or a missing required flag, its own two and required's
// (name, value) pairs, as a usage error, then reads the claims list and connects to the session
// database. code is the command's exit when conn is nil.
func (in sessionsInput) open(ctx context.Context, flags *flag.FlagSet, required [][2]string, stderr io.Writer) (claims []importedClaim, conn *pgx.Conn, code int) {
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "legion sessions %s: unexpected argument %q\n%s\n", in.command, flags.Arg(0), in.usage)
		return nil, nil, 2
	}
	for _, pair := range append([][2]string{{"dsn-file", *in.dsnFile}, {"claims", *in.claimsFile}}, required...) {
		if pair[1] == "" {
			fmt.Fprintf(stderr, "legion sessions %s: --%s is required\n%s\n", in.command, pair[0], in.usage)
			return nil, nil, 2
		}
	}
	claims, err := readImportedClaims(*in.claimsFile)
	if err != nil {
		fmt.Fprintf(stderr, "legion sessions %s: %v\n", in.command, err)
		return nil, nil, 1
	}
	conn, err = ompsessions.Connect(ctx, *in.dsnFile)
	if err != nil {
		fmt.Fprintf(stderr, "legion sessions %s: %v\n", in.command, err)
		return nil, nil, 1
	}
	return claims, conn, 0
}

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
func runSessionsImport(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("sessions import", sessionsImportUsage, stderr)
	input := addSessionsInput("import", sessionsImportUsage, flags)
	tree := flags.String("tree", "", "copy the sessions of this tree's claims alone")
	treeVolume := flags.String("tree-volume", "", "read each session from the tree volume mounted here rather than at its recorded path")
	if code, ok := parseFlags(flags, args); !ok {
		return code
	}
	claims, conn, code := input.open(ctx, flags, nil, stderr)
	if conn == nil {
		return code
	}
	defer conn.Close(context.Background())

	selected := func(c importedClaim) bool { return *tree == "" || c.Tree == *tree }
	counts := map[string]int{}
	matched := 0
	for _, c := range claims {
		if !selected(c) {
			continue
		}
		matched++
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
			counts["refused"]++
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
	if matched == 0 && *tree != "" {
		fmt.Fprintf(stderr, "legion sessions import: the claims list %s has no claim of %s\n", *input.claimsFile, *tree)
		return 1
	}
	missing, err := missingSessions(ctx, conn, claims)
	if err != nil {
		fmt.Fprintf(stderr, "legion sessions import: %v\n", err)
		return 1
	}
	missingHere := 0
	for _, c := range missing {
		fmt.Fprintf(stdout, "%s missing %s: the session table holds no such session (tree %s)\n", c.Token, c.SessionFile, c.Tree)
		if selected(c) {
			missingHere++
		}
	}
	counts["missing"] = len(missing)
	var summary []string
	for _, outcome := range []string{"copied", "copied before", "nothing recorded", "failed", "refused", "missing"} {
		summary = append(summary, fmt.Sprintf("%s %d", outcome, counts[outcome]))
	}
	fmt.Fprintf(stdout, "legion sessions import: %s\n", strings.Join(summary, ", "))
	if counts["failed"] > 0 || counts["refused"] > 0 || missingHere > 0 {
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
		_, found, err := ompsessions.Written(ctx, conn, c.SessionFile)
		if err != nil {
			return nil, err
		}
		if !found {
			missing = append(missing, c)
		}
	}
	return missing, nil
}

// runSessionsMarkLost is `legion sessions mark-lost`: it copies nothing, reports every claim whose
// recorded session the table does not hold and marks each lost in the daemon's own database
// (--daemon-dsn-file), as the daemon marks a claim whose tree volume was lost (supervise's
// loseSession): no session, no session file, its workspace lost. Under SQL storage such a claim
// would otherwise fail every launch, its launcher finding no row to resume; marked, it starts a
// fresh session in a workspace recovered from its issue's branch, as file storage starts it once its
// volume is gone. Run it with the daemon that owns that database stopped, since a running daemon
// holds its claims in memory and writes them back; stopped, its database is the record, so a claim
// it records with a session file the list does not give that claim refuses the run before anything
// is marked (unlistedClaims). Only a claim still recording the session the list says is marked.
func runSessionsMarkLost(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("sessions mark-lost", sessionsMarkLostUsage, stderr)
	input := addSessionsInput("mark-lost", sessionsMarkLostUsage, flags)
	daemonDSNFile := flags.String("daemon-dsn-file", "", "file holding the stopped daemon's own postgres_dsn (required)")
	if code, ok := parseFlags(flags, args); !ok {
		return code
	}
	claims, conn, code := input.open(ctx, flags, [][2]string{{"daemon-dsn-file", *daemonDSNFile}}, stderr)
	if conn == nil {
		return code
	}
	defer conn.Close(context.Background())
	missing, err := missingSessions(ctx, conn, claims)
	if err != nil {
		fmt.Fprintf(stderr, "legion sessions mark-lost: %v\n", err)
		return 1
	}
	daemonDSN, err := ompsessions.ReadDSN(*daemonDSNFile)
	if err != nil {
		fmt.Fprintf(stderr, "legion sessions mark-lost: the daemon's database: %v\n", err)
		return 1
	}
	// The daemon's claims table is read and written here with plain statements, not through
	// internal/store: the database is the stopped daemon's, at the schema that release left
	// (legion-v10.0.0's migrations, through 0032), and the store is this release's, whose Open and
	// Migrate would move it to this release's schema before the dump the rollback restores. The
	// statements name only the columns 0032 already has: token, project, session, session_file and
	// workspace_lost.
	daemon, err := pgx.Connect(ctx, daemonDSN)
	if err != nil {
		fmt.Fprintf(stderr, "legion sessions mark-lost: the daemon's database: %v\n", store.ConnectError(daemonDSN, err))
		return 1
	}
	defer daemon.Close(context.Background())
	unlisted, err := unlistedClaims(ctx, daemon, claims)
	if err != nil {
		fmt.Fprintf(stderr, "legion sessions mark-lost: the daemon's database: %v\n", err)
		return 1
	}
	for _, c := range unlisted {
		fmt.Fprintf(stdout, "%s records %s in the daemon's database, which the claims list does not give it\n", c.Token, c.SessionFile)
	}
	if len(unlisted) > 0 {
		fmt.Fprintf(stderr, "legion sessions mark-lost: the daemon's database records %d session file(s) the claims list lacks, so a claim launched after the list was saved; marked nothing: stop every writer, save the list again and copy again\n", len(unlisted))
		return 1
	}
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
	fmt.Fprintf(stdout, "legion sessions mark-lost: marked lost %d, failed %d\n", marked, failed)
	if failed > 0 {
		return 1
	}
	return 0
}

// unlistedClaims are the claims the daemon's database records a session file for that the claims
// list does not give them, among the claims of the projects the list's claims belong to, since a
// database two legions share holds both: a claim the daemon launched, or relaunched onto a new
// session, after the list was saved. Neither the copy nor mark-lost reaches such a claim, and
// under SQL storage it would fail every launch. A database that holds none of the list's claims is
// refused: it is not the database of the daemon that printed the list.
func unlistedClaims(ctx context.Context, daemon *pgx.Conn, claims []importedClaim) ([]importedClaim, error) {
	if len(claims) == 0 {
		return nil, nil
	}
	listed := make(map[string]string, len(claims))
	tokens := make([]string, 0, len(claims))
	for _, c := range claims {
		listed[c.Token] = c.SessionFile
		tokens = append(tokens, c.Token)
	}
	rows, err := daemon.Query(ctx, "SELECT DISTINCT project FROM claims WHERE token = ANY($1)", tokens)
	if err != nil {
		return nil, fmt.Errorf("read the projects of the listed claims: %w", err)
	}
	projects, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the projects of the listed claims: %w", err)
	}
	if len(projects) == 0 {
		return nil, fmt.Errorf("it holds none of the %d claims the list names, so it is not the database of the daemon that printed the list", len(claims))
	}
	rows, err = daemon.Query(ctx, "SELECT token, session_file FROM claims WHERE project = ANY($1) AND session_file <> '' ORDER BY token", projects)
	if err != nil {
		return nil, fmt.Errorf("read the claims of %s: %w", strings.Join(projects, ", "), err)
	}
	defer rows.Close()
	var unlisted []importedClaim
	for rows.Next() {
		var c importedClaim
		if err := rows.Scan(&c.Token, &c.SessionFile); err != nil {
			return nil, fmt.Errorf("read the claims of %s: %w", strings.Join(projects, ", "), err)
		}
		if file, ok := listed[c.Token]; !ok || file != c.SessionFile {
			unlisted = append(unlisted, c)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the claims of %s: %w", strings.Join(projects, ", "), err)
	}
	return unlisted, nil
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
