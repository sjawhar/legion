package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"path"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A report is a person telling Dispatch's maintainers about a problem with Dispatch itself, from
// the page they saw it on (INFRA-971). POST /api/v1/reports files it as an issue in the project
// the report settings name (settings_reports.go), owned by that project's default assignee rather
// than by the reporter: the person's words, with any pictures they
// pasted shown inline as files of the new issue, then where it happened: the page, the deployed
// commit, the page's own build, the browser and the window, and who reported it. When the
// reporter asks an agent to fix it, the issue is created with the route the settings name, so its
// issue.created event - the reporter's, a person's, which notifies - is published to the role's
// holder (outbox/publisher.go, addRoute). The pictures, the spec and the route commit in one
// transaction, so the agent is never woken before the pictures it is shown exist.

const (
	// maxReportPictures is how many pictures one report takes; their bytes together stay within
	// one upload's maxArtifactBlobSize, the most the keyed layer spools (keyedBodyMax).
	maxReportPictures = 10
	// reportTitleRunes is how much of the words' first line titles the issue.
	reportTitleRunes = 80
	// The page details are the browser's own; these bound what a page could send.
	maxReportPage      = 2048
	maxReportPageBuild = 200
	maxReportUserAgent = 1024
	maxReportViewport  = 100_000
	// reportLabel marks every report's issue, so a project's reports list on their own.
	reportLabel = "report"
)

// reportInput is a report's fields: the JSON body, or a multipart body's `report` part.
type reportInput struct {
	Body      string         `json:"body"`
	Page      string         `json:"page"`
	PageBuild string         `json:"page_build"`
	UserAgent string         `json:"user_agent"`
	Viewport  reportViewport `json:"viewport"`
	AskAgent  bool           `json:"ask_agent"`
}

type reportViewport struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// reportPicture is one picture of a report, read and checked, its name the one it is stored under.
type reportPicture struct {
	upload artifactUploadInput
	sha    string
}

// createReport answers POST /api/v1/reports.
func (s *server) createReport(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	input, pictures, err := s.readReport(w, r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	settings, err := loadReportSettings(r.Context(), s.deps.Store.Pool)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if settings.Project == nil {
		writeError(w, "REPORTS_NOT_CONFIGURED", http.StatusConflict,
			"reports are not set up: a person names the project they go to in Settings → Problem reports")
		return
	}
	var route *string
	if input.AskAgent {
		if settings.AgentRoute == nil {
			writeError(w, "REPORTS_AGENT_NOT_CONFIGURED", http.StatusConflict,
				"no agent fixes reports: a person names its role in Settings → Problem reports")
			return
		}
		route = settings.AgentRoute
	}
	title := reportTitle(input.Body)
	if length := len16(title); length > contracts.IssueTitleMax {
		capExceeded(w, "title", length, contracts.IssueTitleMax)
		return
	}
	// A file's object is written before the transaction opens, as an upload's is (putUploadedFile).
	for _, picture := range pictures {
		if !s.putUploadedFile(w, r, "image", picture.sha, picture.upload) {
			return
		}
	}

	ctx := r.Context()
	tx, err := s.begin(ctx)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	documentCtx, ledger := s.deps.Docs.Join(ctx, tx)
	defer ledger.Discard()
	var stored []model.Artifact
	inserted, err := s.insertIssue(ctx, tx, documentCtx, ledger, issueInsert{
		project:                  *settings.Project,
		title:                    title,
		labels:                   []string{reportLabel},
		assigneeIsProjectDefault: true,
		route:                    route,
		actor:                    actor,
		beforeSpec: func(ctx context.Context, tx pgx.Tx, key string) (string, error) {
			written, err := s.insertReportPictures(ctx, tx, key, pictures, actor)
			if err != nil {
				return "", err
			}
			stored = written
			return reportSpec(key, input, stored, s.deps.Commit, actor, route), nil
		},
	})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// Each picture's own event follows the issue's, so a reader of the issue's log meets the
	// issue before its files.
	published := []model.Event{inserted.event}
	for _, artifact := range stored {
		event, err := s.appendEvent(ctx, tx, ownerForArtifact(artifact).event(
			"artifact.created", actor, artifactCreatedEventPayload(artifact, model.ReferenceChanges{}),
		))
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		published = append(published, event)
	}
	if err := ledger.Commit(ctx); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// Each picture's event is the issue's too, after the one insertIssue counted.
	inserted.issue.LastSeq += len(stored)
	// A seeded spec never passed through a live edit (createIssue says why this is queued).
	s.deps.Docs.ScheduleSettlement(inserted.specID)
	s.publish(published...)
	// Whether the agent's role has a holder is the issue read's to answer (route_status): judging it
	// here could write the shared roster row outside this keyed write's transaction.
	WriteJSON(w, http.StatusCreated, inserted.issue)
}

// readReport reads a report's body - JSON, or multipart with its fields in a `report` part and
// each picture a `picture` part - and checks every field and picture before anything is written.
func (s *server) readReport(w http.ResponseWriter, r *http.Request) (reportInput, []reportPicture, error) {
	requestType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return reportInput{}, nil, errorf(http.StatusBadRequest, "REPORT_INPUT", "a report is JSON or multipart")
	}
	var input reportInput
	var files []*multipart.FileHeader
	switch requestType {
	case "application/json":
		if err := decodeJSON(r, &input); err != nil {
			return reportInput{}, nil, err
		}
	case "multipart/form-data":
		r.Body = http.MaxBytesReader(w, r.Body, maxArtifactBlobSize+(1<<20))
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			// The size refusals are 413 and a file the server cannot spool its own failure, as an
			// upload's are (multipartArtifactUpload).
			var tooLarge *http.MaxBytesError
			var spool *fs.PathError
			switch {
			case errors.As(err, &tooLarge):
				return reportInput{}, nil, errUploadTooLarge
			case errors.Is(err, multipart.ErrMessageTooLarge):
				return reportInput{}, nil, errorf(http.StatusRequestEntityTooLarge, "CAP_EXCEEDED", "the report's form fields are too large to read")
			case errors.As(err, &spool):
				return reportInput{}, nil, fmt.Errorf("spool a report's pictures: %w", err)
			default:
				return reportInput{}, nil, errorf(http.StatusBadRequest, "REPORT_INPUT", "invalid multipart body: %v", err)
			}
		}
		// The pictures are read into memory below, so their spooled files go when this returns
		// (multipartArtifactUpload says why the request's own cleanup never reaches them).
		defer func() {
			if err := r.MultipartForm.RemoveAll(); err != nil {
				slog.Error("dispatch: remove a report's spooled pictures", "error", err)
			}
		}()
		if refusal := unstorableForm(r.MultipartForm.Value); refusal != nil {
			return reportInput{}, nil, refusal
		}
		// The `report` part is JSON, whose \u0000 escape the form's own check reads as six
		// characters: it is checked as a JSON body is (decodeJSON).
		if refusal := unstorableJSON("report", []byte(r.FormValue("report"))); refusal != nil {
			return reportInput{}, nil, refusal
		}
		for field := range r.MultipartForm.File {
			if field != "picture" {
				return reportInput{}, nil, errorf(http.StatusBadRequest, "REPORT_INPUT", "a report's files are its `picture` parts, not %q", field)
			}
		}
		decoder := json.NewDecoder(strings.NewReader(r.FormValue("report")))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			return reportInput{}, nil, errorf(http.StatusBadRequest, "REPORT_INPUT", "the `report` part is not a report's JSON: %v", err)
		}
		files = r.MultipartForm.File["picture"]
	default:
		return reportInput{}, nil, errorf(http.StatusBadRequest, "REPORT_INPUT", "a report is JSON or multipart")
	}
	if err := checkReportInput(&input); err != nil {
		return reportInput{}, nil, err
	}
	pictures, err := readReportPictures(files)
	if err != nil {
		return reportInput{}, nil, err
	}
	return input, pictures, nil
}

// checkReportInput holds a report's fields to what a page sends, trimming each.
func checkReportInput(input *reportInput) error {
	if strings.TrimSpace(input.Body) == "" {
		return errorf(http.StatusBadRequest, "REPORT_INPUT", "body is required: say what went wrong")
	}
	if length := len16(input.Body); length > contracts.DispatchBodyMax {
		return capExceededError("body", length, contracts.DispatchBodyMax)
	}
	input.Page = strings.TrimSpace(input.Page)
	input.PageBuild = strings.TrimSpace(input.PageBuild)
	input.UserAgent = strings.TrimSpace(input.UserAgent)
	if input.Page == "" {
		return errorf(http.StatusBadRequest, "REPORT_INPUT", "page is required: the address of the page the problem is on")
	}
	// The browser supplies these and the person cannot shorten them (a filtered page's address can
	// be long), so one past its bound is cut rather than refusing the report.
	input.Page = clipped(input.Page, maxReportPage)
	input.PageBuild = clipped(input.PageBuild, maxReportPageBuild)
	input.UserAgent = clipped(input.UserAgent, maxReportUserAgent)
	if input.Viewport.Width <= 0 || input.Viewport.Height <= 0 ||
		input.Viewport.Width > maxReportViewport || input.Viewport.Height > maxReportViewport {
		return errorf(http.StatusBadRequest, "REPORT_INPUT", "viewport must give the window's width and height in pixels, each from 1 to %d", maxReportViewport)
	}
	return nil
}

// clipped is value cut to at most limit UTF-16 units, the last of them an ellipsis when it is cut.
func clipped(value string, limit int) string {
	if len16(value) <= limit {
		return value
	}
	runes := []rune(value)
	for len16(string(runes))+1 > limit {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + "…"
}

// readReportPictures reads each picture part, refusing a file that is not a picture by its bytes
// (artifactKind, the reading an upload makes), and names them apart: a clipboard names every
// pasted picture image.png, and two files of one issue cannot share a name.
func readReportPictures(files []*multipart.FileHeader) ([]reportPicture, error) {
	if len(files) > maxReportPictures {
		return nil, errorf(http.StatusBadRequest, "REPORT_INPUT", "a report takes at most %d pictures, not %d", maxReportPictures, len(files))
	}
	pictures := make([]reportPicture, 0, len(files))
	// The issue's spec is spec.md: no picture takes its name.
	taken := map[string]bool{"spec.md": true}
	total := 0
	for _, header := range files {
		name := strings.TrimSpace(path.Base(strings.ReplaceAll(header.Filename, "\\", "/")))
		if name == "" || name == "." || name == "/" {
			name = "picture"
		}
		if refusal := unstorableText("picture name", name); refusal != nil {
			return nil, refusal
		}
		file, err := header.Open()
		if err != nil {
			return nil, fmt.Errorf("open a report's picture: %w", err)
		}
		content, err := io.ReadAll(file)
		_ = file.Close()
		if err != nil {
			return nil, fmt.Errorf("read a report's picture: %w", err)
		}
		total += len(content)
		if total > maxArtifactBlobSize {
			return nil, errUploadTooLarge
		}
		declared := header.Header.Get("Content-Type")
		mediaType, _, err := mime.ParseMediaType(declared)
		if err != nil {
			mediaType = "application/octet-stream"
		}
		kind, storedType := artifactKind(mediaType, declared, content)
		if kind != "image" {
			return nil, errorf(http.StatusBadRequest, "REPORT_INPUT",
				"%s is not a picture: a report takes PNG, JPEG, GIF or WebP pictures", name)
		}
		name = distinctPictureName(name, taken)
		if refusal := newArtifactNameRefusal(name); refusal != nil {
			return nil, refusal
		}
		taken[name] = true
		checksum := sha256.Sum256(content)
		pictures = append(pictures, reportPicture{
			upload: artifactUploadInput{name: name, content: content, contentType: storedType},
			sha:    hex.EncodeToString(checksum[:]),
		})
	}
	return pictures, nil
}

// distinctPictureName is name, or, when an earlier picture of the report took it, name with the
// first free `-2`, `-3`, ... before its extension.
func distinctPictureName(name string, taken map[string]bool) string {
	if !taken[name] {
		return name
	}
	extension := path.Ext(name)
	stem := strings.TrimSuffix(name, extension)
	for suffix := 2; ; suffix++ {
		candidate := fmt.Sprintf("%s-%d%s", stem, suffix, extension)
		if !taken[candidate] {
			return candidate
		}
	}
}

// insertReportPictures writes each picture as a file of the new issue key, at version 1, as an
// upload writes a new file (storeArtifact), and answers the artifacts it wrote with that version.
func (s *server) insertReportPictures(ctx context.Context, tx pgx.Tx, key string, pictures []reportPicture, actor model.Actor) ([]model.Artifact, error) {
	target := artifactTarget{IssueKey: &key}
	actorJSON, err := encodeJSON(actor)
	if err != nil {
		return nil, err
	}
	stored := make([]model.Artifact, 0, len(pictures))
	for _, picture := range pictures {
		slug, err := s.nextArtifactSlug(ctx, tx, target, picture.upload.name)
		if err != nil {
			return nil, err
		}
		if refusal := newArtifactKeyRefusal(target, slug); refusal != nil {
			return nil, refusal
		}
		artifact, err := scanArtifact(tx.QueryRow(ctx, `
			insert into artifacts (issue_key, project_key, session_id, slug, name, kind, is_primary, created_by)
			values ($1, (select project_key from issues where key = $1), null, $2, $3, 'image', false, $4)
			returning `+artifactColumns,
			key, slug, picture.upload.name, actorJSON))
		if err != nil {
			return nil, err
		}
		version, err := s.insertFileVersion(ctx, tx, artifact.ID, 1, picture.upload, picture.sha, actor)
		if err != nil {
			return nil, err
		}
		artifact.Versions = []model.Version{version}
		stored = append(stored, artifact)
	}
	return stored, nil
}

// reportTitle is the issue's title: the first line of the words that holds any, cut at a word
// boundary to reportTitleRunes with an ellipsis.
func reportTitle(body string) string {
	var line string
	for candidate := range strings.SplitSeq(body, "\n") {
		if line = strings.Join(strings.Fields(candidate), " "); line != "" {
			break
		}
	}
	runes := []rune(line)
	if len(runes) <= reportTitleRunes {
		return line
	}
	cut := string(runes[:reportTitleRunes-1])
	if space := strings.LastIndex(cut, " "); space > len(cut)/2 {
		cut = cut[:space]
	}
	return strings.TrimRightFunc(cut, unicode.IsSpace) + "…"
}

// reportSpec is the new issue's spec: the person's words as they wrote them, each picture shown
// inline as a comment shows a pasted one (`![caption](dispatch://KEY/artifact/<slug>@v1)`), then
// where it happened. Every value the browser supplied is a code span, so nothing it holds is read
// as Markdown.
func reportSpec(key string, input reportInput, pictures []model.Artifact, commit string, reporter model.Actor, route *string) string {
	var spec strings.Builder
	spec.WriteString(closedMarkdown(strings.TrimSpace(input.Body)))
	spec.WriteString("\n\n")
	for _, picture := range pictures {
		fmt.Fprintf(&spec, "![%s](dispatch://%s/artifact/%s@v%d)\n\n",
			pictureCaption(picture.Name), key, picture.Slug, picture.Versions[0].Number)
	}
	spec.WriteString("## Where it happened\n\n")
	fmt.Fprintf(&spec, "- Page: %s\n", codeSpan(input.Page))
	if commit != "" {
		fmt.Fprintf(&spec, "- Dispatch: commit %s\n", codeSpan(commit))
	} else {
		spec.WriteString("- Dispatch: this server's build names no commit\n")
	}
	if input.PageBuild != "" {
		fmt.Fprintf(&spec, "- Page build: %s\n", codeSpan(input.PageBuild))
	}
	if input.UserAgent != "" {
		fmt.Fprintf(&spec, "- Browser: %s\n", codeSpan(input.UserAgent))
	}
	fmt.Fprintf(&spec, "- Window: %d × %d\n", input.Viewport.Width, input.Viewport.Height)
	fmt.Fprintf(&spec, "- Reported by: %s\n", reporter.ID)
	if route != nil {
		fmt.Fprintf(&spec, "- Asked an agent to fix it: %s\n", codeSpan(*route))
		spec.WriteString("\nThe reporter asks the agent holding this issue's route to investigate the problem and open a pull request that fixes it.\n")
	}
	return spec.String()
}

// closedMarkdown is words as the spec holds them, with every block they open closed: the words
// as Dispatch's own Markdown writer renders them (pmdoc), which ends a fenced code block left
// open, so a pasted log with no closing fence does not swallow the pictures and the details
// written after it. Words the document cannot hold - block HTML, a malformed decision block -
// would refuse the whole report, so they are kept verbatim in a code block instead.
func closedMarkdown(words string) string {
	tree, err := pmdoc.Parse(words)
	if err == nil {
		err = pmdoc.AskContentError(tree)
	}
	var rendered string
	if err == nil {
		rendered, err = pmdoc.Render(tree)
	}
	if err != nil {
		fence := strings.Repeat("`", max(3, longestRun(words, '`')+1))
		return fence + "text\n" + words + "\n" + fence
	}
	return strings.TrimSpace(rendered)
}

// longestRun is the length of the longest run of char in value.
func longestRun(value string, char rune) int {
	longest, run := 0, 0
	for _, c := range value {
		if c == char {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return longest
}

// pictureCaption is a file's name as the caption of its picture: `[`, `]` and backticks escaped
// and runs of whitespace one space, the rule the dashboard writes a pasted picture's caption by
// (contracts/src/dispatch-text-rules.ts, pictureCaption).
func pictureCaption(name string) string {
	var caption strings.Builder
	for _, char := range strings.Join(strings.Fields(name), " ") {
		if char == '\\' || char == '[' || char == ']' || char == '`' {
			caption.WriteByte('\\')
		}
		caption.WriteRune(char)
	}
	return caption.String()
}

// codeSpan is value as one Markdown code span on one line: its control characters spaces, fenced
// by one more backtick than its longest run, and padded when it starts or ends with one.
func codeSpan(value string) string {
	value = strings.Map(func(char rune) rune {
		if unicode.IsControl(char) {
			return ' '
		}
		return char
	}, value)
	fence := strings.Repeat("`", longestRun(value, '`')+1)
	if strings.HasPrefix(value, "`") || strings.HasSuffix(value, "`") {
		value = " " + value + " "
	}
	return fence + value + fence
}
