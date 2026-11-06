package ompsessions

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/jackc/pgx/v5"
)

// The custom entries pi-envoy writes into a session (packages/pi-envoy/extensions/envoy.ts):
// roleClaimEntry `{role}` each time the session claims an Envoy role (`{role: null}`, an older
// release's hand-back), managedEntry `{session_id}` when Legion drives the session.
const (
	roleClaimEntry = "envoy-role-claim"
	managedEntry   = "legion-managed-session"
)

// headBytes bounds what a scan reads of a session's head for its title slot and header, which are
// far smaller: the slot is 256 bytes and the header one short record.
const headBytes = 64 << 10

// Description is what a session says about itself: its title, header and the Legion role it
// claimed.
type Description struct {
	// Title and TitleSource are the title Oh My Pi shows (its title slot's, else the header's),
	// empty when it has none.
	Title       string
	TitleSource string
	// ID, Timestamp, Cwd and ParentSession are the header's; ParentSession is set for a subagent's
	// session.
	ID            string
	Timestamp     string
	Cwd           string
	ParentSession string
	// RoleClaim is the role token the session's last envoy-role-claim entry names, empty when it
	// has none or that entry handed the role back.
	RoleClaim string
	// ManagedSession is the session id the session's legion-managed-session entry names, empty
	// when it has none.
	ManagedSession string
}

// Describe reads a session's content for its Description: the title slot and header from its first
// records, and the role claim entries from every record. A line that is not JSON is skipped, as Oh
// My Pi skips it.
func Describe(content []byte) Description {
	var d Description
	_ = lineScanner{read: headLines(content)}.describe(&d, false)
	return d
}

// DescribeFile reads the session file at path for its Description, a line at a time. withClaims
// false stops after the header, reading only the file's first lines.
func DescribeFile(path string, withClaims bool) (Description, error) {
	file, err := os.Open(path)
	if err != nil {
		return Description{}, err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 64<<10)
	scanner := lineScanner{read: func() ([]byte, error) {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 && errors.Is(err, io.EOF) {
			err = nil
		}
		return line, err
	}}
	var d Description
	if err := scanner.describe(&d, !withClaims); err != nil {
		return Description{}, fmt.Errorf("read session %s: %w", path, err)
	}
	return d, nil
}

type lineScanner struct {
	read func() ([]byte, error)
}

// describe fills d from the scanner's lines, stopping after the header when headOnly.
func (s lineScanner) describe(d *Description, headOnly bool) error {
	first, sawHeader := true, false
	var headerTitle, headerSource string
	var slot *slotLine
	for {
		line, err := s.read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		line = bytes.TrimSpace(line)
		wasFirst := first
		first = false
		if len(line) == 0 || !json.Valid(line) {
			continue
		}
		if wasFirst {
			if parsed, ok := parseSlot(line); ok {
				slot = parsed
				continue
			}
		}
		if !sawHeader {
			sawHeader = true
			var header struct {
				Type          string `json:"type"`
				ID            string `json:"id"`
				Title         string `json:"title"`
				TitleSource   string `json:"titleSource"`
				Timestamp     string `json:"timestamp"`
				Cwd           string `json:"cwd"`
				ParentSession string `json:"parentSession"`
			}
			if json.Unmarshal(line, &header) == nil && header.Type == "session" {
				d.ID, d.Timestamp, d.Cwd, d.ParentSession = header.ID, header.Timestamp, header.Cwd, header.ParentSession
				headerTitle, headerSource = header.Title, header.TitleSource
			}
			if headOnly {
				break
			}
			continue
		}
		if !bytes.Contains(line, []byte(roleClaimEntry)) && !bytes.Contains(line, []byte(managedEntry)) {
			continue
		}
		var custom struct {
			Type       string `json:"type"`
			CustomType string `json:"customType"`
			Data       struct {
				Role      *string `json:"role"`
				SessionID *string `json:"session_id"`
			} `json:"data"`
		}
		if json.Unmarshal(line, &custom) != nil || custom.Type != "custom" {
			continue
		}
		switch {
		case custom.CustomType == roleClaimEntry:
			d.RoleClaim = ""
			if custom.Data.Role != nil {
				d.RoleClaim = *custom.Data.Role
			}
		case custom.CustomType == managedEntry && custom.Data.SessionID != nil:
			d.ManagedSession = *custom.Data.SessionID
		}
	}
	// The title slot, when the file has one, is the title: an empty one clears the header's
	// (session-loader.ts applyTitleSlot).
	if slot != nil {
		d.Title, d.TitleSource = slot.Title, slot.Source
	} else {
		d.Title, d.TitleSource = headerTitle, headerSource
	}
	return nil
}

// slotLine is the fixed-width first line Oh My Pi keeps a session's title in
// (session-title-slot.ts).
type slotLine struct {
	Title  string
	Source string
}

// parseSlot is Oh My Pi's parseTitleSlotLine.
func parseSlot(line []byte) (*slotLine, bool) {
	var slot struct {
		Type      string  `json:"type"`
		V         int     `json:"v"`
		Title     *string `json:"title"`
		Source    *string `json:"source"`
		UpdatedAt *string `json:"updatedAt"`
		Pad       *string `json:"pad"`
	}
	if json.Unmarshal(line, &slot) != nil || slot.Type != "title" || slot.V != 1 || slot.Title == nil || slot.UpdatedAt == nil || slot.Pad == nil {
		return nil, false
	}
	parsed := &slotLine{Title: *slot.Title}
	if slot.Source != nil {
		if *slot.Source != "auto" && *slot.Source != "user" {
			return nil, false
		}
		parsed.Source = *slot.Source
	}
	return parsed, true
}

// Titled is a session of the table with its title and header.
type Titled struct {
	Path string
	Description
}

// Titles lists every session the table holds with its title and header: the title from the row's
// title column, where Oh My Pi keeps a title it sets under SQL storage, or, when that is NULL (a
// row Import wrote), from the title slot or header of the content's head; the header from the
// content's head. RoleClaim and ManagedSession are not read: Describe over the session's whole
// content reads them. A database no Oh My Pi has opened holds none.
func Titles(ctx context.Context, conn *pgx.Conn) ([]Titled, error) {
	rows, err := conn.Query(ctx, "SELECT path, title, title_source, substr(content, 1, $1) FROM omp_session_files ORDER BY path", headBytes)
	if err != nil {
		if isUndefinedTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	var titled []Titled
	for rows.Next() {
		var path, head string
		var title, source *string
		if err := rows.Scan(&path, &title, &source, &head); err != nil {
			return nil, fmt.Errorf("list sessions: %w", err)
		}
		var d Description
		if err := (lineScanner{read: headLines([]byte(head))}).describe(&d, true); err != nil {
			return nil, err
		}
		if title != nil {
			d.Title, d.TitleSource = *title, ""
			if source != nil {
				d.TitleSource = *source
			}
		}
		titled = append(titled, Titled{Path: path, Description: d})
	}
	if err := rows.Err(); err != nil {
		if isUndefinedTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	return titled, nil
}

func headLines(content []byte) func() ([]byte, error) {
	return func() ([]byte, error) {
		if len(content) == 0 {
			return nil, io.EOF
		}
		line := content
		if i := bytes.IndexByte(content, '\n'); i >= 0 {
			line, content = content[:i], content[i+1:]
		} else {
			content = nil
		}
		return line, nil
	}
}
