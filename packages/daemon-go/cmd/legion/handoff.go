package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

var handoffPhases = map[string]bool{
	"architect": true, "plan": true, "implement": true, "test": true, "review": true, "merge": true,
	// Claim roles remain the command's role vocabulary; these aliases keep existing phase handoff
	// paths readable while the stage transitions use implement/test/review/merge.
	"planner": true, "implementer": true, "tester": true, "reviewer": true, "merger": true,
}

func runHandoff(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: legion handoff write|read|message|messages|complete [flags]")
		return 2
	}
	switch args[0] {
	case "write":
		return runHandoffWrite(args[1:], stdout, stderr)
	case "read":
		return runHandoffRead(args[1:], stdout, stderr)
	case "message":
		return runHandoffMessage(args[1:], stdout, stderr)
	case "messages":
		return runHandoffMessages(args[1:], stdout, stderr)
	case "complete":
		return runHandoffComplete(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "legion handoff: unknown subcommand %q\n", args[0])
		return 2
	}
}

func handoffFlags(name string, stderr io.Writer) (*flag.FlagSet, *string) {
	flags := newFlags("handoff "+name, stderr)
	workspace := flags.String("workspace", "", "workspace directory (default current directory)")
	return flags, workspace
}

func resolveWorkspace(value string) (string, error) {
	if value != "" {
		return filepath.Abs(value)
	}
	return os.Getwd()
}

func validHandoffPhase(value string) bool { return handoffPhases[value] }

func runHandoffWrite(args []string, stdout, stderr io.Writer) int {
	flags, workspaceFlag := handoffFlags("write", stderr)
	phase := flags.String("phase", "", "handoff phase (required)")
	data := flags.String("data", "", "handoff JSON object (required)")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *phase == "" || *data == "" || !validHandoffPhase(*phase) {
		fmt.Fprintln(stderr, "usage: legion handoff write --phase <phase> --data <json-object> [--workspace <dir>]")
		return 2
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(*data), &payload); err != nil || payload == nil {
		fmt.Fprintln(stderr, "legion handoff write: data must be a JSON object")
		return 1
	}
	for _, reserved := range []string{"schemaVersion", "phase", "completed"} {
		if _, present := payload[reserved]; present {
			fmt.Fprintf(stderr, "legion handoff write: handoff data field %s is not allowed\n", reserved)
			return 1
		}
	}
	workspace, err := resolveWorkspace(*workspaceFlag)
	if err != nil {
		fmt.Fprintf(stderr, "legion handoff write: %v\n", err)
		return 1
	}
	payload["schemaVersion"], payload["phase"], payload["completed"] = 1, *phase, time.Now().UTC().Format(time.RFC3339Nano)
	path := filepath.Join(workspace, ".legion", *phase+".json")
	if err := atomicJSON(path, payload); err != nil {
		fmt.Fprintf(stderr, "legion handoff write: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "[handoff] Wrote %s handoff to %s\n", *phase, path)
	return 0
}

func runHandoffRead(args []string, stdout, stderr io.Writer) int {
	flags, workspaceFlag := handoffFlags("read", stderr)
	phase := flags.String("phase", "", "optional handoff phase")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || (*phase != "" && !validHandoffPhase(*phase)) {
		fmt.Fprintln(stderr, "usage: legion handoff read [--phase <phase>] [--workspace <dir>]")
		return 2
	}
	workspace, err := resolveWorkspace(*workspaceFlag)
	if err != nil {
		fmt.Fprintf(stderr, "legion handoff read: %v\n", err)
		return 1
	}
	if *phase != "" {
		value, err := readHandoff(filepath.Join(workspace, ".legion", *phase+".json"))
		if err != nil {
			fmt.Fprintf(stderr, "legion handoff read: %v\n", err)
			return 1
		}
		writeIndentedJSON(stdout, value)
		return 0
	}
	all := map[string]any{}
	for phase := range handoffPhases {
		path := filepath.Join(workspace, ".legion", phase+".json")
		if value, err := readHandoff(path); err == nil {
			all[phase] = value
		}
	}
	writeIndentedJSON(stdout, all)
	return 0
}

func runHandoffMessage(args []string, stdout, stderr io.Writer) int {
	flags, workspaceFlag := handoffFlags("message", stderr)
	from, to, body := flags.String("from", "", "source phase (required)"), flags.String("to", "", "destination phase (required)"), flags.String("body", "", "message body (required)")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || !validHandoffPhase(*from) || !validHandoffPhase(*to) || strings.TrimSpace(*body) == "" {
		fmt.Fprintln(stderr, "usage: legion handoff message --from <phase> --to <phase> --body <text> [--workspace <dir>]")
		return 2
	}
	workspace, err := resolveWorkspace(*workspaceFlag)
	if err != nil {
		fmt.Fprintf(stderr, "legion handoff message: %v\n", err)
		return 1
	}
	id, err := messageID()
	if err != nil {
		fmt.Fprintf(stderr, "legion handoff message: %v\n", err)
		return 1
	}
	path := filepath.Join(workspace, ".legion", "messages", id+"-"+*from+"-to-"+*to+".json")
	if err := atomicJSON(path, map[string]any{"from": *from, "to": *to, "body": *body, "timestamp": time.Now().UTC().Format(time.RFC3339Nano)}); err != nil {
		fmt.Fprintf(stderr, "legion handoff message: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "[handoff] Wrote message from %s to %s\n", *from, *to)
	return 0
}

func runHandoffMessages(args []string, stdout, stderr io.Writer) int {
	flags, workspaceFlag := handoffFlags("messages", stderr)
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: legion handoff messages [--workspace <dir>]")
		return 2
	}
	workspace, err := resolveWorkspace(*workspaceFlag)
	if err != nil {
		fmt.Fprintf(stderr, "legion handoff messages: %v\n", err)
		return 1
	}
	entries, err := os.ReadDir(filepath.Join(workspace, ".legion", "messages"))
	if os.IsNotExist(err) {
		writeIndentedJSON(stdout, []any{})
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "legion handoff messages: %v\n", err)
		return 1
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	messages := []any{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if value, err := readHandoff(filepath.Join(workspace, ".legion", "messages", entry.Name())); err == nil {
			messages = append(messages, value)
		}
	}
	writeIndentedJSON(stdout, messages)
	return 0
}

func runHandoffComplete(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags, workspaceFlag := handoffFlags("complete", stderr)
	summary := flags.String("summary", "", "phase summary (required)")
	verdict := flags.String("verdict", "", "tester verdict: pass or fail")
	ready := flags.Bool("ready", false, "merger has published READY")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || strings.TrimSpace(*summary) == "" {
		fmt.Fprintln(stderr, "usage: legion handoff complete --summary <text> [--verdict pass|fail] [--ready] [--workspace <dir>]")
		return 2
	}
	role := os.Getenv("LEGION_ROLE")
	if !validHandoffPhase(role) {
		fmt.Fprintln(stderr, "legion handoff complete: LEGION_ROLE must name the claimed phase")
		return 1
	}
	if role == "tester" || role == "test" {
		if *verdict != "pass" && *verdict != "fail" {
			fmt.Fprintln(stderr, "legion handoff complete: --verdict pass|fail is required for a tester")
			return 1
		}
	} else if *verdict != "" {
		fmt.Fprintln(stderr, "legion handoff complete: --verdict is only valid for a tester")
		return 1
	}
	if *ready && role != "merger" && role != "merge" {
		fmt.Fprintln(stderr, "legion handoff complete: --ready is only valid for a merger")
		return 1
	}
	workspace, err := resolveWorkspace(*workspaceFlag)
	if err != nil {
		fmt.Fprintf(stderr, "legion handoff complete: %v\n", err)
		return 1
	}
	commit, err := handoffCommit(ctx, workspace, role)
	if err != nil {
		fmt.Fprintf(stderr, "legion handoff complete: %v\n", err)
		return 1
	}
	grant, err := grantFromEnvironment()
	if err != nil {
		fmt.Fprintf(stderr, "legion handoff complete: %v\n", err)
		return 1
	}
	body, err := json.Marshal(map[string]any{"grantId": grant, "summary": *summary, "verdict": *verdict, "ready": *ready, "commit": commit})
	if err != nil {
		fmt.Fprintf(stderr, "legion handoff complete: %v\n", err)
		return 1
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, daemonURL()+"/legion/v1/handoff/complete", strings.NewReader(string(body)))
	if err != nil {
		fmt.Fprintf(stderr, "legion handoff complete: %v\n", err)
		return 1
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		fmt.Fprintf(stderr, "legion handoff complete: %v\n", err)
		return 1
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		answer, _ := io.ReadAll(response.Body)
		fmt.Fprintf(stderr, "legion handoff complete: daemon returned %d: %s\n", response.StatusCode, strings.TrimSpace(string(answer)))
		return 1
	}
	fmt.Fprintln(stdout, "[handoff] Reported phase completion")
	return 0
}

// handoffFiles names the handoff file each file-backed role writes: the phase word its role prompt
// gives `legion handoff write --phase` (packages/pi-envoy/roles/*.md), whichever vocabulary the
// pane's LEGION_ROLE uses. The merger is not file-backed — it verifies and publishes READY and
// writes no handoff (packages/pi-envoy/roles/merger.md).
var handoffFiles = map[string]string{
	"planner": "plan", "plan": "plan",
	"implementer": "implement", "implement": "implement",
	"tester": "test", "test": "test",
	"reviewer": "review", "review": "review",
}

// handoffCommit is the commit a completion reports, resolved with the jj the daemon resolved at
// boot, which it names on every pane as LEGION_JJ_PATH. A file-backed role reports the commit that
// carries its handoff: the last commit on the issue branch that changed .legion/<phase>.json,
// which in the end game is the committed .legion/ deletion. The handoff it wrote last must be
// committed — none of it only in the working copy — and it must have been committed on this
// branch, never inherited from the base: a pane whose handoff is still uncommitted would otherwise
// report a commit that carries another issue's file. The daemon refuses a carrying commit the role
// already reported in its previous phase. When nothing on the branch changed the handoff, the
// phase decides, not the role: the implementer's retro and production check write none (the
// production check runs after the squash merge deleted the branch), and report the commit the
// workspace stands on, as the merger always does; the daemon's state names the phase. Paths reach
// jj as root-anchored filesets, so --workspace works from any directory.
func handoffCommit(ctx context.Context, workspace, role string) (string, error) {
	jj := os.Getenv("LEGION_JJ_PATH")
	word, fileBacked := handoffFiles[role]
	file := filepath.Join(".legion", word+".json")
	if !filepath.IsAbs(jj) {
		message := "LEGION_JJ_PATH is not an absolute path; the Legion daemon names the jj it resolved at boot on every pane"
		if fileBacked {
			message += ", and it resolves the commit carrying " + file
		}
		return "", errors.New(message)
	}
	if !fileBacked {
		return standingCommit(jj, workspace)
	}
	fileset := fmt.Sprintf("root:%q", filepath.ToSlash(file))
	uncommitted, err := jjOutput(jj, workspace, file, "diff", "-r", "@", "--name-only", fileset)
	if err != nil {
		return "", err
	}
	if uncommitted != "" {
		return "", fmt.Errorf("%s has changes in the working copy that are not committed: commit this phase's handoff (jj commit) before completing", file)
	}
	carrying, err := jjOutput(jj, workspace, file, "log", "-r", "latest((::@- ~ ::trunk()) & files("+fileset+"))", "--no-graph", "-T", "commit_id")
	if err != nil {
		return "", err
	}
	if carrying != "" {
		// A committed deletion is not a handoff this role wrote: the implementer's end-game
		// .legion/ deletion carries every role's file away.
		if _, err := os.Stat(filepath.Join(workspace, file)); err == nil {
			if err := ownHandoff(jj, workspace, file, carrying); err != nil {
				return "", err
			}
		}
		return carrying, nil
	}
	refusal := fmt.Errorf("%s is not committed on this issue's branch (only the base branch carries it): write and commit this phase's handoff", file)
	if _, err := os.Stat(filepath.Join(workspace, file)); err != nil {
		refusal = fmt.Errorf("%s is missing from the workspace: write this phase's handoff with legion handoff write --phase %s", file, word)
	}
	current, err := issuePhase(ctx)
	if err != nil {
		return "", fmt.Errorf("%w (the daemon's state, which names whether this phase writes a handoff, is unreadable: %v)", refusal, err)
	}
	if !phase.FileBacked(current) {
		return standingCommit(jj, workspace)
	}
	return "", refusal
}

// ownHandoff refuses a handoff commit another App authored: every role of an issue shares the
// workspace, so a handoff written into the previous role's commit would be reported as this
// role's. The pane's App identity is JJ_USER/JJ_EMAIL, which the daemon sets on every pane it gives
// one; a pane without one has nothing to compare.
func ownHandoff(jj, workspace, file, commit string) error {
	user := os.Getenv("JJ_USER")
	if user == "" {
		return nil
	}
	author, err := jjOutput(jj, workspace, file, "log", "-r", commit, "--no-graph", "-T", `author.name() ++ "\n" ++ author.email()`)
	if err != nil {
		return err
	}
	name, email, _ := strings.Cut(author, "\n")
	if name == user && email == os.Getenv("JJ_EMAIL") {
		return nil
	}
	return fmt.Errorf("%s is carried by commit %s, authored by %s <%s>, not this pane's %s: run jj new, then write and commit this phase's handoff again", file, commit, name, email, user)
}

// standingCommit is the commit the workspace stands on, reported by a phase that writes no handoff.
func standingCommit(jj, workspace string) (string, error) {
	return jjOutput(jj, workspace, "the workspace", "log", "-r", "@-", "--no-graph", "-T", "commit_id")
}

// issuePhase reads the pane's issue phase (LEGION_ISSUE) from the daemon's state document.
func issuePhase(ctx context.Context) (phase.Phase, error) {
	issue := os.Getenv("LEGION_ISSUE")
	if issue == "" {
		return "", errors.New("LEGION_ISSUE is not set")
	}
	body, err := get(ctx, daemonURL()+"/legion/v1/state")
	if err != nil {
		return "", err
	}
	var state api.State
	if err := json.Unmarshal(body, &state); err != nil {
		return "", fmt.Errorf("decode the daemon's state: %w", err)
	}
	recorded, ok := state.Issues[issue]
	if !ok {
		return "", fmt.Errorf("the daemon's state records no issue %s", issue)
	}
	return recorded.Phase, nil
}

// jjOutput runs the boot-resolved jj on the pane workspace and returns its trimmed output.
func jjOutput(jj, workspace, subject string, args ...string) (string, error) {
	command := exec.Command(jj, append([]string{"-R", workspace}, args...)...)
	command.Dir = workspace
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("resolve the commit carrying %s: jj %s: %w", subject, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(output)), nil
}

func atomicJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(path), ".handoff-")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()
	if _, err := temporary.Write(encoded); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, path)
}

func readHandoff(path string) (any, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var value any
	if err := json.Unmarshal(encoded, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func writeIndentedJSON(writer io.Writer, value any) {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err == nil {
		_, _ = fmt.Fprintln(writer, string(encoded))
	}
}

func messageID() (string, error) {
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return time.Now().UTC().Format("20060102150405") + "-" + hex.EncodeToString(raw[:]), nil
}
