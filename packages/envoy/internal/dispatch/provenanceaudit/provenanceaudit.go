// Package provenanceaudit measures where conversations cited by requirements and
// people's participation on requirement-bearing issues are recorded.
package provenanceaudit

import _ "embed"

// RequirementSourcesSQL prints one summary result and up to ten cited requirements.
//
//go:embed requirement_sources.sql
var RequirementSourcesSQL string
