package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math/rand/v2"
	"time"
)

// randomSeed is the one seed every draw of this command comes from, and baseTime the one instant
// every timestamp it writes is an offset from. Both are printed, because two databases seeded on
// different commits are only comparable when their keys and their times are equal (the plan's
// answer 6 to the review findings).
const randomSeed = 20260325

// baseTime is the instant the seeded history starts at. It is in the past of any run, so no row
// is dated in the future, and it is a whole second in UTC, so a timestamp survives a dump and a
// restore unchanged.
var baseTime = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

// serverURL is the one Dispatch server address the seeded text writes its mentions against.
// text.ExtractAt reads a dashboard URL only against the server it is given, so every mention the
// seed writes and every mention its indexing finds are read against this one address.
const serverURL = "https://dispatch.seed.invalid"

// newRandom is the one random source of a run. math/rand/v2's PCG is a specified algorithm with a
// specified stream, so the same two words give the same sequence on every build and every host:
// a seed that drew from the global source, whose seeding is not specified, would write a
// different data set on each run.
func newRandom() *rand.Rand {
	return rand.New(rand.NewPCG(randomSeed, randomSeed^0x9e3779b97f4a7c15))
}

// uuidFor is the uuid of entity n of namespace space: the first sixteen bytes of a sha256 of the
// two, stamped with version 4 and the RFC 4122 variant so it is a uuid any reader accepts. It is
// a function of its arguments alone, so a row's uuid is the same in every run and a mention
// written into text before the row exists names the row the seed later writes.
func uuidFor(space string, n int) string {
	var buffer [8]byte
	binary.BigEndian.PutUint64(buffer[:], uint64(n))
	sum := sha256.Sum256(append([]byte(space+":"), buffer[:]...))
	sum[6] = sum[6]&0x0f | 0x40
	sum[8] = sum[8]&0x3f | 0x80
	text := make([]byte, 36)
	hex.Encode(text[0:8], sum[0:4])
	text[8] = '-'
	hex.Encode(text[9:13], sum[4:6])
	text[13] = '-'
	hex.Encode(text[14:18], sum[6:8])
	text[18] = '-'
	hex.Encode(text[19:23], sum[8:10])
	text[23] = '-'
	hex.Encode(text[24:36], sum[10:16])
	return string(text)
}

// seedSpan is how far the seeded history runs from baseTime. Every row is dated inside it, so the
// data set looks like a system a year old rather than one written in an instant, and a read that
// orders or filters by time has a spread to work on.
const seedSpan = 365 * 24 * time.Hour

// timeAt is the instant row n of n total rows is dated: baseTime plus that row's share of
// seedSpan, to the second. The order of the rows is the order of their times, which is what a
// list ordered by created_at reads.
func timeAt(n, total int) time.Time {
	if total <= 1 {
		return baseTime
	}
	offset := time.Duration(int64(seedSpan/time.Second) * int64(n) / int64(total-1) * int64(time.Second))
	return baseTime.Add(offset)
}
