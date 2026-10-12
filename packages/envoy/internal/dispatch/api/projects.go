package api

import (
	"fmt"
	"net/http"
	"strings"
	"unicode"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func (s *server) listProjects(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select p.key, p.name, (
			select count(*)
			from asks a
			left join issues i on i.key = a.issue_key
			left join artifacts ar on ar.id = a.artifact_id
			where a.state = 'open'
			  and coalesce(i.project_key, ar.project_key) = p.key
			  and (i.key is null or i.closed_at is null)
		), p.created_at
		from projects p
		order by p.key
	`)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer rows.Close()
	projects := []model.Project{}
	for rows.Next() {
		var project model.Project
		if err := rows.Scan(&project.Key, &project.Name, &project.OpenAsks, &project.CreatedAt); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, projects)
}

func (s *server) createProject(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Key   string       `json:"key"`
		Name  string       `json:"name"`
		Actor *model.Actor `json:"actor"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	input.Key = strings.TrimSpace(input.Key)
	input.Name = strings.TrimSpace(input.Name)
	if !projectKeyPattern.MatchString(input.Key) {
		writeError(w, "INVALID_PROJECT", http.StatusBadRequest, projectKeyProblem("key", input.Key))
		return
	}
	if input.Name == "" {
		writeError(w, "INVALID_PROJECT", http.StatusBadRequest, "project name is required")
		return
	}
	// Any bearer writes a name into the project list every dashboard reads, and no route renames a
	// project, so a name is capped as an issue title is and shows a reader what it says.
	if length := len16(input.Name); length > contracts.ProjectNameMax {
		capExceeded(w, "name", length, contracts.ProjectNameMax)
		return
	}
	if problem := projectNameProblem(input.Name); problem != "" {
		writeError(w, "INVALID_PROJECT", http.StatusBadRequest, problem)
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	var project model.Project
	if err := tx.QueryRow(r.Context(), `
		insert into projects (key, name) values ($1, $2) returning key, name, created_at
	`, input.Key, input.Name).Scan(&project.Key, &project.Name, &project.CreatedAt); err != nil {
		if isUniqueViolation(err) {
			writeError(w, "PROJECT_EXISTS", http.StatusConflict, "a project with this key already exists")
			return
		}
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, projectOwner(project.Key).event("project.created", actor, project))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(event)
	WriteJSON(w, http.StatusCreated, project)
}

// projectNameProblem refuses a project name that would mislead its reader: one holding a control
// character, one holding a bidi embedding, override or isolate (U+202A-U+202E, U+2066-U+2069), the
// controls a Trojan Source spoof reorders text with, and one with no visible character (a letter,
// number, punctuation or symbol other than the blank fillers U+115F, U+1160, U+3164, U+FFA0 and
// U+2800; combining marks, format characters and whitespace don't count). Every other format
// character and combining mark does visible work in real names and stays beside a visible one: the
// joiner inside an emoji, a flag's tag characters, a direction mark (U+200E, U+200F, U+061C) in
// mixed-direction text, a soft hyphen, an accent written as a combining mark. It answers "" for a
// name it takes.
func projectNameProblem(name string) string {
	visible := false
	for _, character := range name {
		switch {
		case unicode.Is(unicode.Cc, character):
			return fmt.Sprintf("name holds U+%04X, a control character", character)
		case (character >= 0x202A && character <= 0x202E) || (character >= 0x2066 && character <= 0x2069):
			return fmt.Sprintf("name holds U+%04X, a bidi control that reorders the text a reader sees", character)
		case visibleCharacter(character):
			visible = true
		}
	}
	if !visible {
		return "name has no visible character"
	}
	return ""
}

// blankGlyphs are characters Unicode files as letters or symbols that render as blank space: the
// Hangul fillers (U+115F, U+1160, U+3164, U+FFA0, category Lo) and the Braille pattern blank
// (U+2800, So). A name of only these looks empty wherever it is shown.
var blankGlyphs = map[rune]bool{0x115F: true, 0x1160: true, 0x3164: true, 0xFFA0: true, 0x2800: true}

// visibleCharacter reports whether the character is a letter, number, punctuation or symbol
// (Unicode L, N, P, S) other than the blank fillers.
func visibleCharacter(character rune) bool {
	return unicode.In(character, unicode.L, unicode.N, unicode.P, unicode.S) && !blankGlyphs[character]
}
