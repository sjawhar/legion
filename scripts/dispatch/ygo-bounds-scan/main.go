// Command ygo-bounds-scan reads every stored Dispatch document from Postgres, read-only, and
// reports which would fail to load under github.com/sjawhar/ygo v1.54.1-sami.1, which refuses a
// struct, origin or delete-set clock past 2^53-1 and a client id past 2^55-1.
//
// It reads doc_updates and doc_checkpoints alone, and rebuilds each document as Dispatch's
// loader does (internal/dispatch/docs: PgVersioned.Load, stateThrough, fold): the head is a prune
// checkpoint's ceiling while one stands, else the greatest stored version; the updates through it
// are applied oldest first to one document with Dispatch's pending queue; a log of one update is
// the state as stored; a log that does not fold is merged whole; and the state is decoded as a
// room decodes it. Separately it decodes every stored update's V1 structs directly, so a refused
// document still shows which value is out of range. It prints ids and numbers only, never content.
//
// Usage, from this directory (GOWORK=off: the repository's go.work does not bind this module):
//
//	YGO_SCAN_DSN='postgres://<user>@<host>:5432/<db>' GOWORK=off go run .
//
// Every transaction is READ ONLY, and the session sets default_transaction_read_only. Exit 0: every
// document loads; 1: a document is refused (each printed as REFUSED <id> <reason>) or a read failed;
// 2: a usage error.
package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"sort"
	"unicode/utf16"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
)

// maxUpdateItems is Dispatch's pending queue (internal/dispatch/docs/persistence.go).
const maxUpdateItems = 1 << 20

const (
	maxSafeInteger = 1<<53 - 1
	maxClient      = 1<<55 - 1
)

func newDoc() *crdt.Doc { return crdt.New(crdt.WithMaxPendingItems(maxUpdateItems)) }

// maxima are the largest values a V1 update's structs and delete set hold.
type maxima struct {
	Client, StructEnd, Origin, DeleteEnd uint64
}

func (m *maxima) merge(o maxima) {
	m.Client = max(m.Client, o.Client)
	m.StructEnd = max(m.StructEnd, o.StructEnd)
	m.Origin = max(m.Origin, o.Origin)
	m.DeleteEnd = max(m.DeleteEnd, o.DeleteEnd)
}

// outOfRange names each value past what v1.54.1-sami.1 accepts.
func (m maxima) outOfRange() []string {
	var out []string
	if m.Client > maxClient {
		out = append(out, fmt.Sprintf("client id %d > 2^55-1", m.Client))
	}
	if m.StructEnd > maxSafeInteger {
		out = append(out, fmt.Sprintf("struct clock end %d > 2^53-1", m.StructEnd))
	}
	if m.Origin > maxSafeInteger {
		out = append(out, fmt.Sprintf("origin clock %d > 2^53-1", m.Origin))
	}
	if m.DeleteEnd > maxSafeInteger {
		out = append(out, fmt.Sprintf("delete-set end %d > 2^53-1", m.DeleteEnd))
	}
	return out
}

type reader struct{ d *encoding.Decoder }

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func (r reader) u() uint64   { return must(r.d.ReadVarUint()) }
func (r reader) s() string   { return must(r.d.ReadVarString()) }
func (r reader) b() []byte   { return must(r.d.ReadVarBytes()) }
func (r reader) byte1() byte { return must(r.d.ReadUint8()) }
func (r reader) skip(n int) {
	for range n {
		r.byte1()
	}
}

// any skips one lib0 "any" value.
func (r reader) any() {
	switch t := r.byte1(); t {
	case 127, 126, 121, 120:
	case 125:
		must(r.d.ReadVarInt())
	case 124:
		r.skip(4)
	case 123, 122:
		r.skip(8)
	case 119:
		r.s()
	case 118:
		for range r.u() {
			r.s()
			r.any()
		}
	case 117:
		for range r.u() {
			r.any()
		}
	case 116:
		r.b()
	default:
		panic(fmt.Sprintf("unknown any type %d", t))
	}
}

// scan decodes a V1 update's structs and delete set directly and returns their maxima.
func scan(update []byte) (m maxima, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("direct V1 decode: %v", p)
		}
	}()
	r := reader{encoding.NewDecoder(update)}
	id := func() {
		m.Client = max(m.Client, r.u())
		m.Origin = max(m.Origin, r.u())
	}
	for range r.u() {
		structs := r.u()
		m.Client = max(m.Client, r.u())
		clock := r.u()
		for range structs {
			info := r.byte1()
			var length uint64
			switch info & 0x1f {
			case 0, 10: // GC, Skip
				length = r.u()
			default:
				if info&0x80 != 0 {
					id()
				}
				if info&0x40 != 0 {
					id()
				}
				if info&0xc0 == 0 {
					if r.u() == 1 {
						r.s()
					} else {
						id()
					}
					if info&0x20 != 0 {
						r.s()
					}
				}
				switch ref := info & 0x1f; ref {
				case 1: // deleted
					length = r.u()
				case 2: // JSON
					length = r.u()
					for range length {
						r.s()
					}
				case 3: // binary
					r.b()
					length = 1
				case 4: // string, counted in UTF-16 units
					length = uint64(len(utf16.Encode([]rune(r.s()))))
				case 5: // embed
					r.s()
					length = 1
				case 6: // format
					r.s()
					r.s()
					length = 1
				case 7: // type
					if t := r.u(); t == 3 || t == 4 {
						r.s()
					}
					length = 1
				case 8: // any
					length = r.u()
					for range length {
						r.any()
					}
				case 9: // doc
					r.s()
					r.any()
					length = 1
				default:
					panic(fmt.Sprintf("unknown content ref %d", ref))
				}
			}
			clock += length
		}
		m.StructEnd = max(m.StructEnd, clock)
	}
	for range r.u() {
		m.Client = max(m.Client, r.u())
		for range r.u() {
			start := r.u()
			m.DeleteEnd = max(m.DeleteEnd, start+r.u())
		}
	}
	return m, nil
}

// applyRecovered applies update to doc, turning a panic into an error.
func applyRecovered(doc *crdt.Doc, update []byte) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return crdt.ApplyUpdateV1(doc, update, nil)
}

type result struct {
	updates int
	maxima  maxima
	refused []string
}

// loadDocument rebuilds one document as Dispatch's loader does, in a read-only transaction.
func loadDocument(ctx context.Context, conn *pgx.Conn, room string) (res result, err error) {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return res, err
	}
	defer tx.Rollback(ctx)
	var head int64
	if err := tx.QueryRow(ctx, `select coalesce(
		(select ceiling from doc_checkpoints where artifact_id = $1),
		(select max(version) from doc_updates where artifact_id = $1), 0)`, room).Scan(&head); err != nil {
		return res, fmt.Errorf("read head: %w", err)
	}
	each := func(use func([]byte) error) error {
		rows, err := tx.Query(ctx, `select update from doc_updates
			where artifact_id = $1 and version <= $2 order by version asc`, room, head)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var update []byte
			if err := rows.Scan(&update); err != nil {
				return err
			}
			if err := use(update); err != nil {
				return err
			}
		}
		return rows.Err()
	}

	// Direct decode of every stored update, and the fold Dispatch's loader makes.
	var doc *crdt.Doc
	var first []byte
	var foldErr error
	if err := each(func(update []byte) error {
		res.updates++
		m, derr := scan(update)
		if derr != nil {
			res.refused = append(res.refused, fmt.Sprintf("update %d: %v", res.updates, derr))
		}
		res.maxima.merge(m)
		if foldErr != nil {
			return nil
		}
		switch res.updates {
		case 1:
			first = update
			return nil
		case 2:
			doc = newDoc()
			if err := applyRecovered(doc, first); err != nil {
				foldErr = fmt.Errorf("fold update 1: %w", err)
				return nil
			}
			first = nil
		}
		if err := applyRecovered(doc, update); err != nil {
			foldErr = fmt.Errorf("fold update %d: %w", res.updates, err)
		}
		return nil
	}); err != nil {
		return res, fmt.Errorf("read updates: %w", err)
	}
	if res.updates == 0 {
		return res, nil
	}

	var state []byte
	misread := foldErr
	switch {
	case res.updates == 1:
		state = first
	case foldErr == nil:
		state = crdt.EncodeStateAsUpdateV1(doc, nil)
		made := doc.StateVector()
		if parked := doc.PendingStats(); parked.Items > 0 || parked.DeleteRanges > 0 {
			parts := [][]byte{state}
			if err := each(func(update []byte) error {
				rest, err := crdt.DiffUpdateV1(update, made)
				if err != nil {
					return err
				}
				parts = append(parts, rest)
				return nil
			}); err != nil {
				misread = fmt.Errorf("diff parked updates: %w", err)
			} else if state, err = crdt.MergeUpdatesV1(parts...); err != nil {
				misread = fmt.Errorf("merge parked updates: %w", err)
			}
		}
		if misread == nil {
			back := newDoc()
			if err := applyRecovered(back, state); err != nil {
				misread = fmt.Errorf("read back folded state: %w", err)
			} else if !maps.Equal(made, back.StateVector()) {
				misread = errors.New("folded state reads back with another state vector")
			}
		}
	}
	if misread != nil {
		res.refused = append(res.refused, "fold: "+misread.Error()+" (loader serves the updates merged)")
		var updates [][]byte
		if err := each(func(update []byte) error {
			updates = append(updates, update)
			return nil
		}); err != nil {
			return res, fmt.Errorf("read updates: %w", err)
		}
		if state, err = crdt.MergeUpdatesV1(updates...); err != nil {
			res.refused = append(res.refused, "merge: "+err.Error())
			res.refused = append(res.refused, res.maxima.outOfRange()...)
			return res, nil
		}
	}
	if m, derr := scan(state); derr != nil {
		res.refused = append(res.refused, "state: "+derr.Error())
	} else {
		res.maxima.merge(m)
	}
	if err := applyRecovered(newDoc(), state); err != nil {
		res.refused = append(res.refused, "room load: "+err.Error())
	}
	if err := applyRecovered(crdt.New(), state); err != nil {
		res.refused = append(res.refused, "crdt.New load: "+err.Error())
	}
	res.refused = append(res.refused, res.maxima.outOfRange()...)
	return res, nil
}

func main() {
	dsn := os.Getenv("YGO_SCAN_DSN")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "YGO_SCAN_DSN is required")
		os.Exit(2)
	}
	ctx := context.Background()
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "parse YGO_SCAN_DSN:", err)
		os.Exit(2)
	}
	config.RuntimeParams["default_transaction_read_only"] = "on"
	config.RuntimeParams["application_name"] = "ygo-bounds-scan"
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	defer conn.Close(ctx)

	var rooms []string
	if err := func() error {
		tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		rows, err := tx.Query(ctx, `select artifact_id::text from doc_updates
			union select artifact_id::text from doc_checkpoints`)
		if err != nil {
			return err
		}
		rooms, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	}(); err != nil {
		fmt.Fprintln(os.Stderr, "list documents:", err)
		os.Exit(1)
	}
	sort.Strings(rooms)

	var all maxima
	var at [4]string
	loaded, refused, updates := 0, 0, 0
	for i, room := range rooms {
		res, err := loadDocument(ctx, conn, room)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read %s: %v\n", room, err)
			os.Exit(1)
		}
		updates += res.updates
		for k, pair := range [][2]uint64{
			{all.Client, res.maxima.Client}, {all.StructEnd, res.maxima.StructEnd},
			{all.Origin, res.maxima.Origin}, {all.DeleteEnd, res.maxima.DeleteEnd},
		} {
			if pair[1] > pair[0] {
				at[k] = room
			}
		}
		all.merge(res.maxima)
		if len(res.refused) > 0 {
			refused++
			for _, reason := range res.refused {
				fmt.Printf("REFUSED %s %s\n", room, reason)
			}
		} else {
			loaded++
		}
		if (i+1)%500 == 0 {
			fmt.Fprintf(os.Stderr, "%d/%d documents\n", i+1, len(rooms))
		}
	}
	fmt.Printf("documents=%d updates=%d loaded=%d refused=%d\n", len(rooms), updates, loaded, refused)
	fmt.Printf("max client_id=%d (%s)\n", all.Client, at[0])
	fmt.Printf("max struct_clock_end=%d (%s)\n", all.StructEnd, at[1])
	fmt.Printf("max origin_clock=%d (%s)\n", all.Origin, at[2])
	fmt.Printf("max delete_set_end=%d (%s)\n", all.DeleteEnd, at[3])
	fmt.Printf("limits: clock 2^53-1=%d client 2^55-1=%d\n", uint64(maxSafeInteger), uint64(maxClient))
	if refused > 0 {
		os.Exit(1)
	}
}
