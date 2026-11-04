package memorygroup

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/config"
)

// Each word draws the group it names: `off` draws none; `on` draws memory; and `random` draws
// what the coin says, both ways, flipping it exactly once. A word with a settled answer flips no
// coin at all, which is why the coin those cases hand in fails the test when it is called. A word
// the draw does not know — the empty mode of a project whose file names no `memory` block among
// them, which config.Memory.Word turns into `off` before any draw reads it — is an error that
// names it, and a coin that fails is the draw's failure.
func TestEachMemoryModeDrawsTheGroupItsWordNames(t *testing.T) {
	// refusing is the coin of a mode that must decide without one.
	refusing := func() (bool, error) {
		t.Error("a mode that decides by itself flipped the coin")
		return false, nil
	}
	flips := 0
	pinned := func(memory bool) func() (bool, error) {
		return func() (bool, error) {
			flips++
			return memory, nil
		}
	}
	for _, tc := range []struct {
		name string
		mode config.MemoryMode
		coin func() (bool, error)
		want Group
	}{
		{name: "off", mode: config.MemoryOff, coin: refusing, want: GroupNone},
		{name: "on", mode: config.MemoryOn, coin: refusing, want: GroupMemory},
		{name: "random, the coin memory", mode: config.MemoryRandom, coin: pinned(true), want: GroupMemory},
		{name: "random, the coin none", mode: config.MemoryRandom, coin: pinned(false), want: GroupNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := draw(tc.mode, tc.coin)
			if err != nil {
				t.Fatalf("draw under mode %q: %v", tc.mode, err)
			}
			if got != tc.want {
				t.Fatalf("draw under mode %q = %q, want %q", tc.mode, got, tc.want)
			}
		})
	}
	if flips != 2 {
		t.Fatalf("the random draws flipped the coin %d times, want one flip each", flips)
	}

	for _, unknown := range []config.MemoryMode{"sometimes", ""} {
		group, err := draw(unknown, refusing)
		if err == nil || !strings.Contains(err.Error(), `"`+string(unknown)+`"`) {
			t.Fatalf("draw under the unknown mode %q = %q, %v; want an error naming it", unknown, group, err)
		}
	}

	refused := errors.New("the random source is closed")
	if group, err := draw(config.MemoryRandom, func() (bool, error) { return true, refused }); !errors.Is(err, refused) || group != "" {
		t.Fatalf("draw with a failing coin = %q, %v; want no group and the coin's own error", group, err)
	}
}

// The coin the daemon draws `random` with is crypto/rand's, not math/rand's. Many draws give both
// groups, so the coin is no constant; and the source is crypto/rand.Reader itself, which this test
// proves by putting a reader of its own there: CryptoCoin reads that reader — a math/rand coin
// would not touch it — takes its byte's low bit, and reports what it refuses to read.
func TestTheDefaultCoinReadsCryptoRandAndNotMathRand(t *testing.T) {
	drawn := map[Group]int{}
	for range 400 {
		group, err := draw(config.MemoryRandom, CryptoCoin)
		if err != nil {
			t.Fatalf("draw with the default coin: %v", err)
		}
		drawn[group]++
	}
	if drawn[GroupMemory] == 0 || drawn[GroupNone] == 0 || drawn[GroupMemory]+drawn[GroupNone] != 400 {
		t.Fatalf("400 random draws = %d memory and %d none, want a fair coin's both outcomes", drawn[GroupMemory], drawn[GroupNone])
	}

	original := rand.Reader
	t.Cleanup(func() { rand.Reader = original })
	source := &countingReader{}
	rand.Reader = source
	for _, bit := range []struct {
		byte byte
		want bool
	}{{byte: 0xFF, want: true}, {byte: 0x00, want: false}} {
		source.give = bit.byte
		got, err := CryptoCoin()
		if err != nil {
			t.Fatalf("CryptoCoin over a reader of bytes %#x: %v", bit.byte, err)
		}
		if got != bit.want {
			t.Fatalf("CryptoCoin over a reader of bytes %#x = %t, want %t: the coin read some other source", bit.byte, got, bit.want)
		}
	}
	if source.reads != 2 {
		t.Fatalf("CryptoCoin read crypto/rand.Reader %d times in two flips, want one read each: the coin reads another source", source.reads)
	}

	source.err = errors.New("no entropy here")
	if got, err := CryptoCoin(); !errors.Is(err, source.err) || got {
		t.Fatalf("CryptoCoin over a refusing reader = %t, %v; want false and the reader's own error", got, err)
	}
}

// DrawAndRecord is one call for both of the draw's halves: it draws the group the block's mode
// gives the tree and writes it onto the tree's root with the mode as a draw records it and the
// instant it was handed, in the caller's own transaction. A project whose file names no block is
// written `off`, the word an operator could have written, and never the empty string. The draw's
// failure — a coin that will not flip — is the call's, and so is the write's, and a write that
// changed no row is neither: the record decides which roots are drawn, and the helper reports
// what the record answers.
func TestDrawAndRecordDrawsTheGroupAndWritesItOnTheTreesRoot(t *testing.T) {
	at := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	refusing := func() (bool, error) {
		t.Error("a mode that decides by itself flipped the coin")
		return false, nil
	}
	for _, tc := range []struct {
		name   string
		memory config.Memory
		coin   func() (bool, error)
		want   drawn
	}{
		{name: "a project with no memory block", memory: config.Memory{}, coin: refusing,
			want: drawn{root: "LEGION-1", group: string(GroupNone), mode: string(config.MemoryOff), at: at}},
		{name: "off", memory: config.Memory{Mode: config.MemoryOff}, coin: refusing,
			want: drawn{root: "LEGION-1", group: string(GroupNone), mode: string(config.MemoryOff), at: at}},
		{name: "on", memory: config.Memory{Mode: config.MemoryOn}, coin: refusing,
			want: drawn{root: "LEGION-1", group: string(GroupMemory), mode: string(config.MemoryOn), at: at}},
		{name: "random, the coin memory", memory: config.Memory{Mode: config.MemoryRandom},
			coin: func() (bool, error) { return true, nil },
			want: drawn{root: "LEGION-1", group: string(GroupMemory), mode: string(config.MemoryRandom), at: at}},
		{name: "random, the coin none", memory: config.Memory{Mode: config.MemoryRandom},
			coin: func() (bool, error) { return false, nil },
			want: drawn{root: "LEGION-1", group: string(GroupNone), mode: string(config.MemoryRandom), at: at}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			records := &recordingDraws{}
			if err := DrawAndRecord(context.Background(), nil, records, "LEGION-1", tc.memory, tc.coin, at); err != nil {
				t.Fatalf("DrawAndRecord under %+v: %v", tc.memory, err)
			}
			if len(records.draws) != 1 || records.draws[0] != tc.want {
				t.Fatalf("DrawAndRecord under %+v wrote %+v, want one draw %+v", tc.memory, records.draws, tc.want)
			}
		})
	}

	refused := errors.New("the random source is closed")
	records := &recordingDraws{}
	err := DrawAndRecord(context.Background(), nil, records, "LEGION-1", config.Memory{Mode: config.MemoryRandom},
		func() (bool, error) { return true, refused }, at)
	if !errors.Is(err, refused) || !strings.Contains(err.Error(), "LEGION-1") {
		t.Fatalf("DrawAndRecord with a failing coin = %v, want the coin's own error naming the tree", err)
	}
	if len(records.draws) != 0 {
		t.Fatalf("DrawAndRecord with a failing coin wrote %+v, want nothing written", records.draws)
	}

	refusedWrite := errors.New("the transaction is gone")
	records = &recordingDraws{err: refusedWrite}
	if err := DrawAndRecord(context.Background(), nil, records, "LEGION-1", config.Memory{Mode: config.MemoryOn}, refusing, at); !errors.Is(err, refusedWrite) {
		t.Fatalf("DrawAndRecord over a refusing record = %v, want the record's own error", err)
	}

	records = &recordingDraws{wroteNothing: true}
	if err := DrawAndRecord(context.Background(), nil, records, "LEGION-1", config.Memory{Mode: config.MemoryOn}, refusing, at); err != nil {
		t.Fatalf("DrawAndRecord over a record that wrote no row = %v, want no error: the record decides which roots are drawn", err)
	}
}

// drawn is one call of the record's write, as the fake below keeps it.
type drawn struct {
	root, group, mode string
	at                time.Time
}

// recordingDraws stands in for the record: it keeps every write, answers err when it is set, and
// reports a row written unless wroteNothing says otherwise. It takes no transaction, since
// DrawAndRecord hands its own straight through.
type recordingDraws struct {
	draws        []drawn
	err          error
	wroteNothing bool
}

func (r *recordingDraws) DrawMemory(_ context.Context, _ pgx.Tx, root, group, mode string, at time.Time) (bool, error) {
	if r.err != nil {
		return false, r.err
	}
	r.draws = append(r.draws, drawn{root: root, group: group, mode: mode, at: at})
	return !r.wroteNothing, nil
}

var _ Records = (*recordingDraws)(nil)

// countingReader stands in for crypto/rand.Reader: it serves give for every byte asked of it,
// counts the reads, and refuses them all once err is set.
type countingReader struct {
	give  byte
	err   error
	reads int
}

func (r *countingReader) Read(p []byte) (int, error) {
	r.reads++
	if r.err != nil {
		return 0, r.err
	}
	for i := range p {
		p[i] = r.give
	}
	return len(p), nil
}

var _ io.Reader = (*countingReader)(nil)
