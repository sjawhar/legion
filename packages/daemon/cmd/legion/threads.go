package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/api"
	legionclaim "github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/reviewthreads"
)

// threadsCommands is `legion threads`'s subcommands, the one list of them `legion threads --help`
// names.
var threadsCommands = map[string]command{
	"resolve": runResolveThreads,
}

func runThreads(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runSubcommand(ctx, "threads", threadsCommands, args, stdout, stderr)
}

// runResolveThreads is `legion threads resolve`: as the App of the role running it, or with --gh
// as whoever the caller's own gh authenticates as, it resolves every unresolved review thread the
// rule (reviewthreads.Resolution) closes and names every other one as left open. --gh is for a
// session outside a Legion pane, which has no grant: it applies the same rule through the session's
// own gh (ghGraphQL), from any directory, and knows none of Legion's role Apps, so no thread counts
// as a bot's. Inside a pane --gh is refused before anything runs: the pane's gh is `legion gh`,
// which refuses a GraphQL body it cannot read, and the pane has its grant. In the reviewer's pane
// it asks the daemon instead (resolveThroughDaemon): GitHub lets only the pull request's author's
// App resolve its threads, and the reviewer acts as the review App.
func runResolveThreads(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("threads resolve", "usage: legion threads resolve --pr <number> --repo <owner>/<repo> [--gh]", stderr)
	pr := flags.String("pr", "", "pull request number (required)")
	repo := flags.String("repo", "", "repository owner/name (required)")
	useGh := flags.Bool("gh", false, "authenticate with your own gh instead of a Legion grant: for a session outside a Legion pane")
	if code, ok := parseFlags(flags, args); !ok {
		return code
	}
	if flags.NArg() != 0 {
		flags.Usage()
		return 2
	}
	repository, err := ghrepo.Parse("--repo", *repo)
	if err != nil {
		fmt.Fprintf(stderr, "legion threads resolve: %v\n", err)
		return 2
	}
	number, err := strconv.Atoi(*pr)
	if err != nil || number <= 0 {
		fmt.Fprintln(stderr, "legion threads resolve: --pr must be a positive pull request number")
		return 2
	}
	var call reviewthreads.GraphQL
	var apps *reviewthreads.Apps
	switch {
	case *useGh:
		if _, set := os.LookupEnv("LEGION_GRANT_FILE"); set {
			fmt.Fprintln(stderr, "legion threads resolve: --gh is for a session outside a Legion pane; this pane names a grant (LEGION_GRANT_FILE), so run legion threads resolve without --gh")
			return 1
		}
		call = ghGraphQL(repository, stderr)
	case legionclaim.Role(os.Getenv("LEGION_ROLE")) == legionclaim.RoleReviewer:
		return resolveThroughDaemon(ctx, repository, number, stdout, stderr)
	default:
		response, err := redeemGrant(ctx, "/legion/v1/gh-token")
		if err != nil {
			hint := ""
			if errors.Is(err, errNoGrant) {
				hint = "; a session outside a Legion pane has no grant and adds --gh to resolve through its own gh"
			}
			fmt.Fprintf(stderr, "legion threads resolve: Unable to redeem LEGION_GRANT: %v%s\n", err, hint)
			return 1
		}
		defer response.Body.Close()
		var credential githubTokenResponse
		if err := json.NewDecoder(response.Body).Decode(&credential); err != nil || credential.Token == "" {
			fmt.Fprintln(stderr, "legion threads resolve: daemon returned an invalid GitHub credential response")
			return 1
		}
		apps, err = reviewthreads.AppsFrom(credential.LegionAppLogins)
		if err != nil {
			fmt.Fprintf(stderr, "legion threads resolve: daemon returned an invalid GitHub credential response: %v\n", err)
			return 1
		}
		call = reviewthreads.TokenGraphQL(os.Getenv("LEGION_GITHUB_GRAPHQL_URL"), credential.Token)
	}
	// The rule is the daemon's too (reviewthreads); threads_test.go pins it end to end.
	outcomes, err := reviewthreads.Resolve(ctx, call, repository, number, func(thread reviewthreads.Thread) (reviewthreads.Acceptance, string) {
		return reviewthreads.Resolution(thread, apps)
	})
	return printOutcomes(outcomes, 0, err, stdout, stderr)
}

// resolveThroughDaemon is `legion threads resolve` in the reviewer's pane: the daemon resolves, as
// the implement App, each thread on the pull request of the issue the pane's grant is for that a
// bot outside Legion's role Apps opened and whose newest submitted comment is the reviewer's own
// Accepted: (POST /legion/v1/threads/resolve), and answers each unresolved thread's outcome but for
// the threads holding the implement App's pending draft, which it only counts, and the thread
// GitHub refused to resolve when it refused one. The command prints them as its own path does, and
// either a refusal or a counted thread fails it. The reviewer never holds the implement App's token.
func resolveThroughDaemon(ctx context.Context, repository ghrepo.Repository, number int, stdout, stderr io.Writer) int {
	grant, err := grantFromEnvironment()
	if err != nil {
		fmt.Fprintf(stderr, "legion threads resolve: %v\n", err)
		return 1
	}
	response, err := postDaemon(ctx, "/legion/v1/threads/resolve", api.ThreadsResolveRequest{GrantID: grant, Repo: repository.String(), Number: number})
	if err != nil {
		fmt.Fprintf(stderr, "legion threads resolve: %v\n", err)
		return 1
	}
	defer response.Body.Close()
	var answer api.ThreadsResolveResponse
	if err := json.NewDecoder(response.Body).Decode(&answer); err != nil {
		fmt.Fprintf(stderr, "legion threads resolve: daemon returned an invalid answer: %v\n", err)
		return 1
	}
	if refused := answer.Refused; refused != nil {
		err = &reviewthreads.Refused{URL: refused.URL, Err: errors.New(refused.Error)}
	}
	return printOutcomes(answer.Threads, answer.Withheld, err, stdout, stderr)
}

// errPendingDraft fails a run that left open threads holding the implement App's pending draft:
// they close only once that App submits or discards its pending review, which the caller cannot do.
var errPendingDraft = errors.New("the implement App's pending review must be submitted or discarded before the threads holding its draft can close")

// printOutcomes prints each thread's outcome, then the count of threads left open unnamed because
// their newest comment is the implement App's pending draft, or that there was no unresolved thread.
// err fails the command, and so does any such thread when nothing else did.
func printOutcomes(outcomes []reviewthreads.Outcome, withheld int, err error, stdout, stderr io.Writer) int {
	for _, outcome := range outcomes {
		fmt.Fprintln(stdout, outcome)
	}
	switch {
	case withheld == 1:
		fmt.Fprintln(stdout, "1 unresolved thread holds the implement App's pending draft and was left open")
	case withheld > 1:
		fmt.Fprintf(stdout, "%d unresolved threads hold the implement App's pending draft and were left open\n", withheld)
	}
	if err == nil && withheld > 0 {
		err = errPendingDraft
	}
	if err != nil {
		fmt.Fprintf(stderr, "legion threads resolve: %v\n", err)
		return 1
	}
	if len(outcomes) == 0 {
		fmt.Fprintln(stdout, "no unresolved threads")
	}
	return 0
}

// ghGraphQL calls GitHub through the caller's own gh (`gh api graphql --input -`), for a session
// outside a Legion pane, which has no grant to redeem. gh gets the caller's environment, which
// decides whose credential it uses, with GH_REPO set to the repository, so a gh that picks its
// credential by repository (the devbox shim routes to that owner's App) authenticates for it from
// any directory. A failed gh fails with gh's own message; a successful one has its stderr copied to
// stderr verbatim, since that is where such a gh says the call acts as someone else (an inherited
// GH_TOKEN, a fallback personal token).
func ghGraphQL(repository ghrepo.Repository, stderr io.Writer) reviewthreads.GraphQL {
	return func(ctx context.Context, query string, variables map[string]any, into any) error {
		body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
		if err != nil {
			return err
		}
		gh := exec.CommandContext(ctx, "gh", "api", "graphql", "--input", "-")
		gh.Env = append(os.Environ(), "GH_REPO="+repository.String())
		gh.Stdin = bytes.NewReader(body)
		var out, errOut bytes.Buffer
		gh.Stdout, gh.Stderr = &out, &errOut
		if err := gh.Run(); err != nil {
			var exited *exec.ExitError
			if !errors.As(err, &exited) {
				return fmt.Errorf("run gh api graphql: %w", err)
			}
			message := strings.TrimSpace(errOut.String())
			if message == "" {
				message = strings.TrimSpace(out.String())
			}
			return fmt.Errorf("gh api graphql failed (exit %d): %s", exited.ExitCode(), message)
		}
		if errOut.Len() > 0 {
			_, _ = stderr.Write(errOut.Bytes())
		}
		return reviewthreads.Result(out.Bytes(), into)
	}
}
