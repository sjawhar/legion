package ghtool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
)

// ErrNotAWrite is Lookup's answer for a recorded call that posts no comment or review: there is
// nothing on GitHub to find it by, so its outcome stays unknown.
var ErrNotAWrite = errors.New("posts no comment or review")

// Found is what Lookup found on GitHub: whether a comment or review carrying the call's answer key
// exists, and, when one does, its URL and GitHub id.
type Found struct {
	Exists bool
	URL    string
	ID     int64
}

// Lookup tells a restarted answer what became of its call seq, recorded with no result because the
// daemon stopped while it ran (A18): for a call posting a comment or review, it reads the comments
// or reviews of the pull request or issue the call wrote to, with the routed owner's credentials
// as the call ran with them, and answers the one whose Legion footer carries the call's answer key
// (<question>.<seq>), or none. A call the record does not hold, one that has a result, and one
// whose target cannot be read are errors; a call posting no comment or review is ErrNotAWrite.
func Lookup(ctx context.Context, a *Answer, seq int, bounds config.AnswerBounds) (Found, error) {
	recorded, ok, err := a.host.Calls.AnswerToolCall(ctx, a.question, seq)
	switch {
	case err != nil:
		return Found{}, err
	case !ok:
		return Found{}, fmt.Errorf("gh call %s.%d is not recorded", a.question, seq)
	case recorded.Result != nil:
		return Found{}, fmt.Errorf("gh call %s.%d has its result recorded; its outcome is known", a.question, seq)
	}
	if bounds.GHCall <= 0 {
		return Found{}, errors.New("the gh tool has no time bound (answers.bounds.gh_seconds), so it runs nothing")
	}
	ctx, cancel := context.WithTimeout(ctx, bounds.GHCall)
	defer cancel()
	place, err := a.placeOf(ctx, recorded.Args)
	if err != nil {
		return Found{}, fmt.Errorf("gh call %s.%d: %w", a.question, seq, err)
	}
	argv := []string{"api", place.endpoint}
	if place.list {
		argv = append(argv, "--paginate", "--slurp")
	}
	out, err := a.read(ctx, argv)
	if err != nil {
		return Found{}, fmt.Errorf("gh call %s.%d: read %s: %w", a.question, seq, place.endpoint, err)
	}
	items, err := decodeItems(out, place.list)
	if err != nil {
		return Found{}, fmt.Errorf("gh call %s.%d: read %s: %w", a.question, seq, place.endpoint, err)
	}
	key := a.question + "." + strconv.Itoa(seq)
	for _, item := range items {
		if answerKeys(item.Body)[key] {
			return Found{Exists: true, URL: item.HTMLURL, ID: item.ID}, nil
		}
	}
	return Found{}, nil
}

// place is where a call's comment or review would be on GitHub: a REST endpoint listing them
// (list) or naming the one the call edited or submitted.
type place struct {
	endpoint string
	list     bool
}

// apiPlaces are the REST endpoints a gh api comment or review write posts to, each with the method
// that posts, and where what it posted is read: $1 is the endpoint's repository path and number.
var apiPlaces = []struct {
	method string
	path   *regexp.Regexp
	read   string
	list   bool
}{
	{"POST", regexp.MustCompile(`^(repos/[^/]+/[^/]+/issues/[^/]+/comments)$`), "$1", true},
	{"PATCH", regexp.MustCompile(`^(repos/[^/]+/[^/]+/issues/comments/[^/]+)$`), "$1", false},
	{"POST", regexp.MustCompile(`^(repos/[^/]+/[^/]+/pulls/[^/]+/reviews)$`), "$1", true},
	{"PUT", regexp.MustCompile(`^(repos/[^/]+/[^/]+/pulls/[^/]+/reviews/[^/]+)$`), "$1", false},
	{"POST", regexp.MustCompile(`^(repos/[^/]+/[^/]+/pulls/[^/]+/reviews/[^/]+)/events$`), "$1", false},
	{"POST", regexp.MustCompile(`^(repos/[^/]+/[^/]+/pulls/[^/]+/comments)(/[^/]+/replies)?$`), "$1", true},
	{"PATCH", regexp.MustCompile(`^(repos/[^/]+/[^/]+/pulls/comments/[^/]+)$`), "$1", false},
	{"POST", regexp.MustCompile(`^(repos/[^/]+/[^/]+/commits/[^/]+/comments)$`), "$1", true},
	{"PATCH", regexp.MustCompile(`^(repos/[^/]+/[^/]+/comments/[^/]+)$`), "$1", false},
}

// placeOf is where the call argv, as recorded, would have posted: for pr comment, issue comment
// and a closing or reopening comment, the issue comments of its pull request or issue; for pr
// review, the pull request's reviews; for a gh api write, the endpoint it posted to read back. A
// pr command naming its pull request by branch has its number read from GitHub.
func (a *Answer) placeOf(ctx context.Context, argv []string) (place, error) {
	words := commandWords(argv)
	key := writeOf(words)
	spec, ok := writes[key]
	if !ok {
		return place{}, ErrNotAWrite
	}
	parsed, err := parseArgs(argv, spec, key)
	if err != nil {
		return place{}, err
	}
	if key == "api" {
		if len(parsed.positionals) < 2 {
			return place{}, errors.New("its gh api call names no endpoint")
		}
		endpoint := apiPath(parsed.positionals[1])
		method := "GET"
		if len(parsed.given("field"))+len(parsed.given("raw-field")) > 0 {
			method = "POST"
		}
		if given := parsed.given("method"); len(given) > 0 {
			method = strings.ToUpper(given[len(given)-1].value)
		}
		for _, p := range apiPlaces {
			if m := p.path.FindStringSubmatchIndex(endpoint); m != nil && p.method == method {
				return place{endpoint: string(p.path.ExpandString(nil, p.read, endpoint, m)), list: p.list}, nil
			}
		}
		return place{}, ErrNotAWrite
	}
	if (key == "pr close" || key == "pr reopen") && len(parsed.given("comment")) == 0 {
		return place{}, ErrNotAWrite
	}
	repo, number, err := a.target(ctx, parsed, words[0] == "pr")
	if err != nil {
		return place{}, err
	}
	if key == "pr review" {
		return place{endpoint: "repos/" + repo + "/pulls/" + number + "/reviews", list: true}, nil
	}
	return place{endpoint: "repos/" + repo + "/issues/" + number + "/comments", list: true}, nil
}

// githubIssueURL is a pull request's or an issue's github.com URL, as gh reads one in a number's
// place: its repository and number.
var githubIssueURL = regexp.MustCompile(`(?i)^https?://(?:www\.)?github\.com/([^/]+/[^/]+)/(?:pull|issues)/([0-9]+)`)

// target is the repository (OWNER/REPO) and number a pr or issue command's arguments name: its
// selector a number, #N or a URL, its repository the URL's or -R's; a pr command's branch is read
// from GitHub with `gh pr view`. A command naming no repository ran in the answer's empty working
// directory, where gh resolves none, so its target cannot be read.
func (a *Answer) target(ctx context.Context, parsed parsedArgs, pr bool) (string, string, error) {
	selector := ""
	if len(parsed.positionals) > 2 {
		selector = parsed.positionals[2]
	}
	repo := ""
	if given := parsed.given("repo"); len(given) > 0 {
		value := given[len(given)-1].value
		for _, prefix := range []string{"https://github.com/", "http://github.com/", "github.com/"} {
			if len(value) > len(prefix) && strings.EqualFold(value[:len(prefix)], prefix) {
				value = value[len(prefix):]
				break
			}
		}
		named, err := ghrepo.Parse("its -R", strings.TrimSuffix(value, "/"))
		if err != nil {
			return "", "", err
		}
		repo = named.String()
	}
	if m := githubIssueURL.FindStringSubmatch(selector); m != nil {
		named, err := ghrepo.Parse("its URL's repository", m[1])
		if err != nil {
			return "", "", err
		}
		return named.String(), m[2], nil
	}
	if repo == "" {
		return "", "", errors.New("it names no repository, so it can have posted nowhere gh resolves from the answer's empty working directory")
	}
	if number := strings.TrimPrefix(selector, "#"); number != "" && strings.Trim(number, "0123456789") == "" {
		return repo, number, nil
	}
	if !pr || selector == "" {
		return "", "", fmt.Errorf("its selector %q names no pull request or issue the gh tool can read", selector)
	}
	out, err := a.read(ctx, []string{"pr", "view", selector, "-R", repo, "--json", "number", "--jq", ".number"})
	if err != nil {
		return "", "", fmt.Errorf("read the pull request of %q in %s: %w", selector, repo, err)
	}
	number := strings.TrimSpace(string(out))
	if number == "" || strings.Trim(number, "0123456789") != "" {
		return "", "", fmt.Errorf("gh named %q as the pull request of %q in %s", number, selector, repo)
	}
	return repo, number, nil
}

// read runs a read of the daemon's own, argv, as a call runs (routed, the scrubbed environment,
// the answer's empty working directory), and answers its whole stdout; a gh exiting non-zero is an
// error naming its stderr. It records nothing: it is no call of the model's. It logs one line.
func (a *Answer) read(ctx context.Context, argv []string) ([]byte, error) {
	_, owner, dir, err := a.route(ctx, argv)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, a.host.GH, argv...)
	cmd.Env, cmd.Dir = a.env(dir), a.cwd
	procgroup.Configure(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	exit := -1
	if cmd.ProcessState != nil {
		exit = cmd.ProcessState.ExitCode()
	}
	a.host.Log.Info("answers: gh lookup", "question", a.question, "issue", a.agent.Issue, "role", a.agent.Role,
		"app", string(a.agent.App()), "appID", a.agent.AppID, "owner", owner, "args", argv, "exit", exit)
	if err != nil {
		return nil, fmt.Errorf("gh %s: %v: %s", strings.Join(argv, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// item is a GitHub comment or review as the REST API answers it.
type item struct {
	ID      int64  `json:"id"`
	HTMLURL string `json:"html_url"`
	Body    string `json:"body"`
}

// decodeItems reads a REST answer: for a list, gh api --paginate --slurp's array of pages, each an
// array of items; otherwise one item.
func decodeItems(out []byte, list bool) ([]item, error) {
	if !list {
		var one item
		if err := json.Unmarshal(out, &one); err != nil {
			return nil, fmt.Errorf("its answer is no comment or review: %w", err)
		}
		return []item{one}, nil
	}
	var pages [][]item
	if err := json.Unmarshal(out, &pages); err != nil {
		return nil, fmt.Errorf("its answer is no list of comments or reviews: %w", err)
	}
	var items []item
	for _, page := range pages {
		items = append(items, page...)
	}
	return items, nil
}

// answerKeys are the answer keys of every Legion footer in body.
func answerKeys(body string) map[string]bool {
	keys := map[string]bool{}
	for rest := body; ; {
		at := strings.Index(rest, footerMarker)
		if at < 0 {
			return keys
		}
		rest = rest[at+len(footerMarker):]
		end := strings.Index(rest, "-->")
		if end < 0 {
			return keys
		}
		var footer struct {
			Answer string `json:"answer"`
		}
		if json.Unmarshal([]byte(rest[:end]), &footer) == nil && footer.Answer != "" {
			keys[footer.Answer] = true
		}
		rest = rest[end:]
	}
}
