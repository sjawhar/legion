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
	commit, err := handoffCommit(workspace, role)
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

// handoffCommit is the commit a completion reports, resolved with the jj the daemon resolved at
// boot, which it names on every pane as LEGION_JJ_PATH. A file-backed phase reports the commit
// carrying its committed .legion/<role>.json. The merger is not a file-backed phase — it verifies
// and publishes READY and writes no handoff (packages/pi-envoy/roles/merger.md) — so it reports
// the commit its workspace sits on.
func handoffCommit(workspace, role string) (string, error) {
	fileBacked := role != "merger" && role != "merge"
	subject := "the merger's workspace"
	if fileBacked {
		subject = filepath.Join(".legion", role+".json")
		if _, err := os.Stat(filepath.Join(workspace, subject)); err != nil {
			return "", fmt.Errorf("%s is missing from the workspace", subject)
		}
	}
	jj := os.Getenv("LEGION_JJ_PATH")
	if !filepath.IsAbs(jj) {
		return "", errors.New("LEGION_JJ_PATH is not an absolute path; the Legion daemon names the jj it resolved at boot on every pane")
	}
	if fileBacked {
		listed, err := exec.Command(jj, "-R", workspace, "file", "list", "-r", "@-", subject).Output()
		if err != nil || strings.TrimSpace(string(listed)) != subject {
			return "", fmt.Errorf("%s is not committed on the pane workspace", subject)
		}
	}
	commit, err := exec.Command(jj, "-R", workspace, "log", "-r", "@-", "--no-graph", "-T", "commit_id").Output()
	if err != nil {
		return "", fmt.Errorf("resolve the commit carrying %s: %w", subject, err)
	}
	if resolved := strings.TrimSpace(string(commit)); resolved != "" {
		return resolved, nil
	}
	return "", fmt.Errorf("resolve the commit carrying %s", subject)
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
