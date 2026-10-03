package pmdoc

import (
	"encoding/json"
	"sort"
	"testing"
)

func TestTokenJSONTracksMarksButNotIdentityOrServerState(t *testing.T) {
	base, err := Parse(":::ask{#decision urgency=\"med\" multiple=\"false\"}\nShip it?\n:::\n")
	if err != nil {
		t.Fatalf("parse base: %v", err)
	}
	baseToken, err := base.TokenJSON()
	if err != nil {
		t.Fatalf("token base: %v", err)
	}

	identityOnly := cloneNode(base)
	identityOnly.Children[0].Attrs[BlockIDAttr] = "moved-decision"
	identityToken, err := identityOnly.TokenJSON()
	if err != nil {
		t.Fatalf("token identity: %v", err)
	}
	if string(identityToken) != string(baseToken) {
		t.Fatalf("identity changed token\nbase: %s\n got: %s", baseToken, identityToken)
	}

	serverState := cloneNode(base)
	serverState.Children[0].Attrs["state"] = "answered"
	serverToken, err := serverState.TokenJSON()
	if err != nil {
		t.Fatalf("token server state: %v", err)
	}
	if string(serverToken) != string(baseToken) {
		t.Fatalf("server state changed token\nbase: %s\n got: %s", baseToken, serverToken)
	}

	marked := cloneNode(base)
	text := marked.Children[0].Children[0].Children[0]
	text.Marks = []Mark{{Type: "proofComment", Attrs: Attrs{"id": "comment-1", "by": "user:alice"}}}
	markedToken, err := marked.TokenJSON()
	if err != nil {
		t.Fatalf("token marked: %v", err)
	}
	if string(markedToken) == string(baseToken) {
		t.Fatal("inline mark did not change token")
	}
}

// A token is the hash of TokenJSON, which callers hold across requests and server releases as an
// edit's precondition, so the encoding written as it goes is byte for byte what encoding/json makes
// of the representation built whole: a node of type, attrs, content, text and marks, its marks in
// the order of their encodings.
func TestTokenJSONIsWhatEncodingJSONMakesOfTheRepresentation(t *testing.T) {
	parsed, err := Parse("---\ntitle: <T&C>\n---\n\n# Heading \"quoted\" & <b>\n\n" +
		":::ask{#decision urgency=\"high\" multiple=\"true\" state=\"answered\"}\nShip it?\n\n- Yes: now\n- No\n:::\n\n" +
		"| a | b |\n| :- | -: |\n| x \\| y | <i>z</i> |\n\n" +
		"3. third\n4. fourth\n   - nested `code`\n\n" +
		"```go\nfunc main() {}\n```\n\n" +
		"![alt](https://example.com/i.png \"title\") [link](https://example.com?a=1&b=2) **bold _both_** \u2028 snowman \u2603 tab\there\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	marked := cloneNode(parsed)
	var text *Node
	walk(marked, func(node *Node, _ []int, _, _ int) bool {
		if text == nil && node.Type == "text" {
			text = node
		}
		return true
	})
	text.Marks = append(text.Marks,
		Mark{Type: "proofComment", Attrs: Attrs{"id": "comment-2", "by": "user:bob", "at": int64(7)}},
		Mark{Type: "proofComment", Attrs: Attrs{"id": "comment-1", "by": "user:alice", "weight": 1.5, "tags": []string{"a", "<b>"}}},
	)
	for name, tree := range map[string]*Node{"a parsed document": parsed, "a document with marks": marked} {
		got, err := tree.TokenJSON()
		if err != nil {
			t.Fatalf("%s: token: %v", name, err)
		}
		want, err := json.Marshal(tokenRepresentation(tree))
		if err != nil {
			t.Fatalf("%s: encode the representation: %v", name, err)
		}
		if string(got) != string(want) {
			t.Fatalf("%s: token encoding differs\n got: %s\nwant: %s", name, got, want)
		}
	}
}

type tokenRepresentationNode struct {
	Type    string                     `json:"type"`
	Attrs   Attrs                      `json:"attrs,omitempty"`
	Content []*tokenRepresentationNode `json:"content,omitempty"`
	Text    string                     `json:"text,omitempty"`
	Marks   []tokenMark                `json:"marks,omitempty"`
}

// tokenRepresentation is the representation TokenJSON encodes, built whole.
func tokenRepresentation(n *Node) *tokenRepresentationNode {
	out := &tokenRepresentationNode{Type: n.Type, Attrs: tokenAttrs(n.Type, n.Attrs), Text: n.Text}
	for _, mark := range n.Marks {
		out.Marks = append(out.Marks, tokenMark{Type: mark.Type, Attrs: canonicalAttrs(mark.Attrs)})
	}
	sort.Slice(out.Marks, func(left, right int) bool {
		leftJSON, _ := json.Marshal(out.Marks[left])
		rightJSON, _ := json.Marshal(out.Marks[right])
		return string(leftJSON) < string(rightJSON)
	})
	for _, child := range n.Children {
		out.Content = append(out.Content, tokenRepresentation(child))
	}
	return out
}
