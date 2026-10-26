package claude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// Description is what a transcript says about itself without its conversation being rebuilt.
type Description struct {
	// Title is the session's title as `/rename` records it: the customTitle of its last
	// custom-title line (Claude Code writes one on the rename and again as each later turn ends),
	// empty when it has none.
	Title string
	// SessionID and Cwd are the first conversation line's.
	SessionID string
	Cwd       string
}

// Describe reads a transcript's content for its Description. A line that is not JSON is skipped,
// as Claude Code skips it.
func Describe(content []byte) Description {
	var d Description
	_ = describe(&d, headLines(content))
	return d
}

// DescribeFile reads the transcript at path for its Description, a line at a time.
func DescribeFile(path string) (Description, error) {
	file, err := os.Open(path)
	if err != nil {
		return Description{}, err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 64<<10)
	var d Description
	err = describe(&d, func() ([]byte, error) {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 && errors.Is(err, io.EOF) {
			err = nil
		}
		return line, err
	})
	if err != nil {
		return Description{}, fmt.Errorf("read transcript %s: %w", path, err)
	}
	return d, nil
}

// customTitle is the marker of the lines describe decodes beyond the first conversation line.
var customTitle = []byte(`"custom-title"`)

func describe(d *Description, read func() ([]byte, error)) error {
	for {
		line, err := read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if d.SessionID != "" && !bytes.Contains(line, customTitle) {
			continue
		}
		var e struct {
			Type        string `json:"type"`
			UUID        string `json:"uuid"`
			SessionID   string `json:"sessionId"`
			Cwd         string `json:"cwd"`
			CustomTitle string `json:"customTitle"`
		}
		if json.Unmarshal(bytes.TrimSpace(line), &e) != nil {
			continue
		}
		switch {
		case e.Type == "custom-title" && e.SessionID != "":
			d.Title = e.CustomTitle
		case d.SessionID == "" && transcriptTypes[e.Type] && e.UUID != "":
			d.SessionID, d.Cwd = e.SessionID, e.Cwd
		}
	}
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
