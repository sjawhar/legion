package pmdoc

import (
	"testing"

	"github.com/reearth/ygo/crdt"
)

func TestReadMatchesProseMirrorJSON(t *testing.T) {
	for _, fx := range loadFixtures(t) {
		t.Run(fx.Name, func(t *testing.T) {
			doc := crdt.New()
			if err := crdt.ApplyUpdateV1(doc, fx.YjsUpdateV1, nil); err != nil {
				t.Fatalf("apply browser update: %v", err)
			}
			got, err := Read(doc.GetXmlFragment("prosemirror"))
			if err != nil {
				t.Fatal(err)
			}
			want, err := FromJSON(fx.PMJSON)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(want) {
				g, _ := got.JSON()
				t.Fatalf("Read() differs from browser JSON\n got: %s\nwant: %s", g, fx.PMJSON)
			}
		})
	}
}
