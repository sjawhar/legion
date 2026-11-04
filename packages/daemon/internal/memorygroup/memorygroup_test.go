package memorygroup

import (
	"crypto/rand"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/config"
)

// Each mode draws the group its word names: `off`, and the empty mode of a project whose file
// names no `memory` block, draw none; `on` draws memory; and `random` draws what the coin says,
// both ways, flipping it exactly once. A mode with a settled answer flips no coin at all, which is
// why the coin those cases hand in fails the test when it is called. An unknown mode — one no
// loader produces — is an error that names the word, and a coin that fails is the draw's failure.
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
		{name: "a project with no memory block", mode: "", coin: refusing, want: GroupNone},
		{name: "off", mode: config.MemoryOff, coin: refusing, want: GroupNone},
		{name: "on", mode: config.MemoryOn, coin: refusing, want: GroupMemory},
		{name: "random, the coin memory", mode: config.MemoryRandom, coin: pinned(true), want: GroupMemory},
		{name: "random, the coin none", mode: config.MemoryRandom, coin: pinned(false), want: GroupNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Draw(tc.mode, tc.coin)
			if err != nil {
				t.Fatalf("Draw under mode %q: %v", tc.mode, err)
			}
			if got != tc.want {
				t.Fatalf("Draw under mode %q = %q, want %q", tc.mode, got, tc.want)
			}
		})
	}
	if flips != 2 {
		t.Fatalf("the random draws flipped the coin %d times, want one flip each", flips)
	}

	group, err := Draw("sometimes", refusing)
	if err == nil || !strings.Contains(err.Error(), `"sometimes"`) {
		t.Fatalf(`Draw under an unknown mode = %q, %v; want an error naming "sometimes"`, group, err)
	}

	refused := errors.New("the random source is closed")
	if group, err := Draw(config.MemoryRandom, func() (bool, error) { return true, refused }); !errors.Is(err, refused) || group != "" {
		t.Fatalf("Draw with a failing coin = %q, %v; want no group and the coin's own error", group, err)
	}
}

// The coin the daemon draws `random` with is crypto/rand's, not math/rand's. Many draws give both
// groups, so the coin is no constant; and the source is crypto/rand.Reader itself, which this test
// proves by putting a reader of its own there: CryptoCoin reads that reader — a math/rand coin
// would not touch it — takes its byte's low bit, and reports what it refuses to read.
func TestTheDefaultCoinReadsCryptoRandAndNotMathRand(t *testing.T) {
	drawn := map[Group]int{}
	for range 400 {
		group, err := Draw(config.MemoryRandom, CryptoCoin)
		if err != nil {
			t.Fatalf("Draw with the default coin: %v", err)
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
