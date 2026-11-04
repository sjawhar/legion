package model

// Interpretation is a document node, linked to its decision by a typed pointer. Author is
// server-recorded provenance; nil explicitly means no author was recorded for a handmade block.
type Interpretation struct {
	BlockID     string `json:"block_id"`
	DecisionRef string `json:"decision_ref"`
	Ref         string `json:"ref"`
	Text        string `json:"text"`
	Author      *Actor `json:"author"`
}
