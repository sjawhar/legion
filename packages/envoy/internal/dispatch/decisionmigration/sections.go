package decisionmigration

import (
	"sort"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// SectionHeadings associates each section with its original heading block, consuming repeated
// headings in document order. A heading's words alone are not a section's identity.
func SectionHeadings(entries []Entry, markdown string, blocks []model.ArtifactBlock) map[string]string {
	sort.SliceStable(blocks, func(i, j int) bool { return blocks[i].From < blocks[j].From })
	headings := map[string][]string{}
	for _, block := range blocks {
		if block.Type == "heading" {
			heading := strings.TrimSpace(markdown[block.From:block.To])
			headings[heading] = append(headings[heading], block.ID)
		}
	}
	result := make(map[string]string, len(entries))
	for _, entry := range entries {
		if entry.HeadingLevel == 0 {
			continue
		}
		heading := strings.Repeat("#", entry.HeadingLevel) + " " + entry.Title
		if ids := headings[heading]; len(ids) > 0 {
			result[entry.ID] = ids[0]
			headings[heading] = ids[1:]
		}
	}
	return result
}

// Text is the legacy section's canonical wording, including its heading, as the converted node
// must serve it. The converter wraps this content but does not rewrite it.
func (e Entry) Text() string {
	prefix := "## "
	if e.Replaced {
		prefix = "### "
	}
	if e.Body == "" {
		return prefix + e.Title
	}
	return prefix + e.Title + "\n\n" + e.Body
}
