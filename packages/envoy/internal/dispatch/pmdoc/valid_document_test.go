package pmdoc

import (
	"errors"
	"reflect"
	"testing"
)

// The zero ValidDocument holds no tree: each of its methods refuses it, without panicking, with
// the error its exported counterpart gives a nil tree.
func TestAZeroValidDocumentRefusesAsItsExportedCounterpartsRefuseANilTree(t *testing.T) {
	var zero ValidDocument
	if zero.Node() != nil {
		t.Fatalf("zero ValidDocument's tree = %v, want nil", zero.Node())
	}
	type refusal struct {
		name      string
		got, want func() (any, error)
		message   string
	}
	for _, tc := range []refusal{
		{
			name: "BlockPathsAndTableRows",
			got: func() (any, error) {
				paths, rows, err := zero.BlockPathsAndTableRows()
				return []any{paths, rows}, err
			},
			want: func() (any, error) {
				paths, rows, err := BlockPathsAndTableRows(nil)
				return []any{paths, rows}, err
			},
			message: "outside Proof schema: BlockPathsAndTableRows wants a document",
		},
		{
			name:    "TableRows",
			got:     func() (any, error) { return zero.TableRows() },
			want:    func() (any, error) { return TableRows(nil) },
			message: "outside Proof schema: TableRows wants a document",
		},
		{
			name:    "TableDescendantIDs",
			got:     func() (any, error) { return zero.TableDescendantIDs() },
			want:    func() (any, error) { return TableDescendantIDs(nil) },
			message: "outside Proof schema: TableDescendantIDs wants a document",
		},
		{
			name: "RenderWithBlockOffsets",
			got: func() (any, error) {
				markdown, offsets, err := zero.RenderWithBlockOffsets()
				return []any{markdown, offsets}, err
			},
			want: func() (any, error) {
				markdown, offsets, err := RenderWithBlockOffsets(nil)
				return []any{markdown, offsets}, err
			},
			message: "outside Proof schema: Render wants a doc, got nil",
		},
		{
			name: "TokenJSON",
			got: func() (any, error) {
				var spans []TokenSpan
				encoded, err := zero.TokenJSON(&spans)
				return []any{encoded, spans}, err
			},
			want: func() (any, error) {
				var spans []TokenSpan
				encoded, err := EncodeTokenJSON(nil, &spans)
				return []any{encoded, spans}, err
			},
			message: "outside Proof schema: nil node",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, gotErr := tc.got()
			want, wantErr := tc.want()
			if wantErr == nil || wantErr.Error() != tc.message {
				t.Fatalf("%s(nil) error = %v, want %q", tc.name, wantErr, tc.message)
			}
			if gotErr == nil || gotErr.Error() != wantErr.Error() {
				t.Fatalf("zero ValidDocument.%s error = %v, want %v", tc.name, gotErr, wantErr)
			}
			if !errors.Is(gotErr, ErrSchema) || errors.Is(gotErr, ErrPanic) {
				t.Fatalf("zero ValidDocument.%s error %v: ErrSchema %v, ErrPanic %v, want ErrSchema alone", tc.name, gotErr, errors.Is(gotErr, ErrSchema), errors.Is(gotErr, ErrPanic))
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("zero ValidDocument.%s results = %#v, want %#v", tc.name, got, want)
			}
		})
	}
}
