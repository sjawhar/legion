package files_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/files"
	"github.com/sjawhar/envoy/internal/dispatch/files/filestest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// encodedPNG and encodedJPEG are real pictures of the given size: a header the pass's decode
// reads, as the bytes of an upload stored before Dispatch read a size are.
func encodedPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, width, height))); err != nil {
		t.Fatalf("encode a %dx%d PNG: %v", width, height, err)
	}
	return encoded.Bytes()
}

func encodedJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, width, height)), nil); err != nil {
		t.Fatalf("encode a %dx%d JPEG: %v", width, height, err)
	}
	return encoded.Bytes()
}

// seedPictureVersion writes an image artifact and one version holding body in its row under mime,
// as an upload did, and returns the version's id. width and height are the pair the row already
// carries, zero for a row carrying neither: a picture stored before Dispatch read a size carries
// none, and one stored since carries its own.
func seedPictureVersion(t *testing.T, database *store.Store, slug, mime string, body []byte, width, height int) string {
	t.Helper()
	ctx := context.Background()
	seedIssue(t, database)
	var artifactID string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ('FILES-1', 'FILES', $1, $1, 'image', '{"kind":"user","id":"alice"}')
		returning id::text
	`, slug).Scan(&artifactID); err != nil {
		t.Fatalf("seed artifact: %v", err)
	}
	var recordedWidth, recordedHeight *int
	if width != 0 && height != 0 {
		recordedWidth, recordedHeight = &width, &height
	}
	var id string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifact_versions (artifact_id, number, content, mime, size, sha256, width, height, created_at)
		values ($1, 1, $2, $3, $4, $5, $6, $7, coalesce((select max(created_at) from artifact_versions), now()) + interval '1 second')
		returning id::text
	`, artifactID, body, mime, len(body), files.SHA256(body), recordedWidth, recordedHeight).Scan(&id); err != nil {
		t.Fatalf("seed version: %v", err)
	}
	return id
}

// recordedSize is the pair the version's row carries, "none" for a row carrying neither: the pair
// is written together, so a row holding one of the two is a failure this spelling shows.
func recordedSize(t *testing.T, database *store.Store, id string) string {
	t.Helper()
	var width, height *int
	if err := database.Pool.QueryRow(context.Background(), `select width, height from artifact_versions where id = $1`, id).Scan(&width, &height); err != nil {
		t.Fatalf("read version %s: %v", id, err)
	}
	if width == nil && height == nil {
		return "none"
	}
	if width == nil || height == nil {
		return fmt.Sprintf("half a pair (%v, %v)", width, height)
	}
	return fmt.Sprintf("%dx%d", *width, *height)
}

// moveToStore clears a row's bytes into store, as backfill-files leaves a row it has moved: the
// state every picture uploaded before the bucket is in once that pass has run.
func moveToStore(t *testing.T, database *store.Store, store files.Store, id, mime string, body []byte) {
	t.Helper()
	ctx := context.Background()
	if err := store.Put(ctx, files.SHA256(body), mime, body); err != nil {
		t.Fatalf("store the bytes of version %s: %v", id, err)
	}
	if _, err := database.Pool.Exec(ctx, `update artifact_versions set content = null where id = $1`, id); err != nil {
		t.Fatalf("clear the bytes of version %s: %v", id, err)
	}
}

// The pass fills the pair of every picture stored before Dispatch read one, whether its bytes are
// still in its row or have been moved to the bucket, and names the row whose bytes are no picture
// it can read; a version that is not a picture is no candidate at all.
func TestBackfillDimensionsFillsAPictureStoredBeforeTheSizeWasRead(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	memory := filestest.NewMemory()
	inRow := seedPictureVersion(t, database, "in-row", "image/png", encodedPNG(t, 320, 200), 0, 0)
	movedBody := encodedJPEG(t, 64, 48)
	moved := seedPictureVersion(t, database, "moved", "image/jpeg", movedBody, 0, 0)
	moveToStore(t, database, memory, moved, "image/jpeg", movedBody)
	unreadable := seedPictureVersion(t, database, "unreadable", "image/png", []byte("\x89PNG\r\n\x1a\nbut no header"), 0, 0)
	notAPicture := seedFileVersion(t, database, "notes", []byte("an attachment"))

	var out strings.Builder
	report, err := files.BackfillDimensions(ctx, database.Pool, memory, &out)
	if err != nil {
		t.Fatalf("BackfillDimensions: %v", err)
	}
	if report.Done != 2 || len(report.Failed) != 1 || !strings.Contains(report.Failed[0], unreadable) {
		t.Fatalf("report = %+v, want two rows sized and %s named as failed", report, unreadable)
	}
	if got := recordedSize(t, database, inRow); got != "320x200" {
		t.Errorf("the row that still holds its bytes records %s, want 320x200", got)
	}
	if got := recordedSize(t, database, moved); got != "64x48" {
		t.Errorf("the row whose bytes were moved to the bucket records %s, want 64x48", got)
	}
	if got := recordedSize(t, database, unreadable); got != "none" {
		t.Errorf("the row whose header did not decode records %s, want none", got)
	}
	if got := recordedSize(t, database, notAPicture); got != "none" {
		t.Errorf("the attachment records %s, want none", got)
	}
	if !strings.Contains(out.String(), "sized version "+inRow) || !strings.Contains(out.String(), "FAILED version "+unreadable) {
		t.Errorf("output does not name what the pass did:\n%s", out.String())
	}
	// A moved row's bytes are read from the bucket and left there; the row stays cleared.
	if rowBytes(t, database, moved) != nil {
		t.Error("the pass put the moved row's bytes back")
	}
	if !bytes.Equal(storedBytes(t, memory, files.SHA256(movedBody)), movedBody) {
		t.Error("the pass changed the object in the bucket")
	}
}

// A version that already carries a pair is no candidate: the pass never reads its bytes and never
// writes over the size its upload recorded, whatever the bytes say now.
func TestBackfillDimensionsLeavesAVersionThatAlreadyHasItsPairAlone(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	recorded := seedPictureVersion(t, database, "recorded", "image/png", encodedPNG(t, 320, 200), 11, 7)

	var out strings.Builder
	report, err := files.BackfillDimensions(ctx, database.Pool, filestest.NewMemory(), &out)
	if err != nil {
		t.Fatalf("BackfillDimensions: %v", err)
	}
	if report.Done != 0 || len(report.Failed) != 0 || out.String() != "" {
		t.Fatalf("report = %+v, output %q, want nothing to do", report, out.String())
	}
	if got := recordedSize(t, database, recorded); got != "11x7" {
		t.Errorf("the version records %s, want the 11x7 it was stored with", got)
	}
}

// Safe to run twice: the second pass over the same rows finds nothing to do and writes nothing,
// since each write is conditioned on the row still missing its pair.
func TestBackfillDimensionsRunTwiceWritesNothingTheSecondTime(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	memory := filestest.NewMemory()
	first := seedPictureVersion(t, database, "first", "image/png", encodedPNG(t, 320, 200), 0, 0)
	second := seedPictureVersion(t, database, "second", "image/png", encodedPNG(t, 8, 6), 0, 0)

	if report, err := files.BackfillDimensions(ctx, database.Pool, memory, &strings.Builder{}); err != nil || report.Done != 2 {
		t.Fatalf("first pass: %+v, %v, want both rows sized", report, err)
	}
	var out strings.Builder
	report, err := files.BackfillDimensions(ctx, database.Pool, memory, &out)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if report.Done != 0 || report.Bytes != 0 || len(report.Failed) != 0 || out.String() != "" {
		t.Fatalf("second pass: report %+v, output %q, want nothing to do", report, out.String())
	}
	if got := recordedSize(t, database, first); got != "320x200" {
		t.Errorf("the first version records %s, want 320x200", got)
	}
	if got := recordedSize(t, database, second); got != "8x6" {
		t.Errorf("the second version records %s, want 8x6", got)
	}
}

// With no bucket configured the pass refuses to run: a row whose bytes were moved holds none, so
// a pass without the store would name every such picture as a row it could not read.
func TestBackfillDimensionsIsRefusedWithNoFileStoreConfigured(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	version := seedPictureVersion(t, database, "in-row", "image/png", encodedPNG(t, 320, 200), 0, 0)

	report, err := files.BackfillDimensions(ctx, database.Pool, nil, &strings.Builder{})
	if !errors.Is(err, files.ErrNoFileStore) {
		t.Fatalf("BackfillDimensions with no store: %+v, %v, want ErrNoFileStore", report, err)
	}
	if got := recordedSize(t, database, version); got != "none" {
		t.Errorf("the version records %s, want none: the refused pass wrote a row", got)
	}
}

// Two passes started together do not read the same rows in lockstep: the second is refused while
// the first holds the lock, and runs once it is released. A file pass holds a lock of its own, so
// the two kinds of pass do not refuse each other.
func TestBackfillDimensionsIsRefusedWhileAPassHoldsTheLock(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedPictureVersion(t, database, "one", "image/png", encodedPNG(t, 320, 200), 0, 0)
	memory := filestest.NewMemory()
	// Hold the lock the way a running pass does: on a connection kept for the duration, since an
	// advisory lock is the session's and a pooled query would hand its session back.
	holder, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection: %v", err)
	}
	var held bool
	if err := holder.QueryRow(ctx, `select pg_try_advisory_lock(x'494e465241373630'::bigint)`).Scan(&held); err != nil || !held {
		t.Fatalf("hold the lock: %v (%t)", err, held)
	}
	if _, err := files.BackfillDimensions(ctx, database.Pool, memory, &strings.Builder{}); !errors.Is(err, files.ErrAnotherDimensionsPassRunning) {
		t.Fatalf("BackfillDimensions while the lock is held: %v, want ErrAnotherDimensionsPassRunning", err)
	}
	if _, err := files.BackfillRows(ctx, database.Pool, memory, &strings.Builder{}); err != nil {
		t.Fatalf("BackfillRows while the dimensions lock is held: %v", err)
	}
	if _, err := holder.Exec(ctx, `select pg_advisory_unlock_all()`); err != nil {
		t.Fatalf("release the lock: %v", err)
	}
	holder.Release()
	if report, err := files.BackfillDimensions(ctx, database.Pool, memory, &strings.Builder{}); err != nil || report.Done != 1 {
		t.Fatalf("BackfillDimensions once the lock is released: %+v, %v", report, err)
	}
}

// exifBlock is a TIFF block holding the Orientation tag alone, in the byte order its first two
// bytes name: "II" little-endian, "MM" big-endian, both of which a camera writes. It is the
// block a JPEG's APP1 Exif segment, a PNG's eXIf chunk and a WebP's EXIF chunk each carry.
func exifBlock(mark string, orientation uint16) []byte {
	order := binary.AppendByteOrder(binary.LittleEndian)
	if mark == "MM" {
		order = binary.BigEndian
	}
	block := order.AppendUint16([]byte(mark), 42)  // the TIFF magic
	block = order.AppendUint32(block, 8)           // the first directory, right after this
	block = order.AppendUint16(block, 1)           // one entry in it
	block = order.AppendUint16(block, 0x0112)      // Orientation
	block = order.AppendUint16(block, 3)           // of type SHORT
	block = order.AppendUint32(block, 1)           // one of them
	block = order.AppendUint16(block, orientation) // the value itself, which fits in the entry
	block = order.AppendUint16(block, 0)           // the rest of the entry's four value bytes
	return order.AppendUint32(block, 0)            // no directory after this one
}

// jpegWithOrientation is a JPEG carrying its Exif where a camera puts it: an APP1 segment, right
// after the start of the picture, opening with the Exif header and then the TIFF block.
func jpegWithOrientation(t *testing.T, width, height int, mark string, orientation uint16) []byte {
	t.Helper()
	encoded := encodedJPEG(t, width, height)
	body := append([]byte("Exif\x00\x00"), exifBlock(mark, orientation)...)
	segment := append([]byte{0xff, 0xe1, byte((len(body) + 2) >> 8), byte(len(body) + 2)}, body...)
	return append(append(append([]byte{}, encoded[:2]...), segment...), encoded[2:]...)
}

// pngWithOrientation is a PNG carrying an eXIf chunk, before its image data or after it: a
// browser reads the first and passes over the second, which is why the two are distinguished.
func pngWithOrientation(t *testing.T, width, height int, orientation uint16, beforeImageData bool) []byte {
	t.Helper()
	encoded := encodedPNG(t, width, height)
	at := len(encoded) - 12 // just before IEND, which is the last chunk and twelve bytes long
	if beforeImageData {
		at = firstIDAT(t, encoded)
	}
	return append(append(append([]byte{}, encoded[:at]...), pngChunk("eXIf", exifBlock("II", orientation))...), encoded[at:]...)
}

// firstIDAT is where a PNG's image data begins: a length, a type, the data and a CRC from the
// signature on, until the type is IDAT.
func firstIDAT(t *testing.T, encoded []byte) int {
	t.Helper()
	for at := 8; at+8 <= len(encoded); at += 12 + int(binary.BigEndian.Uint32(encoded[at:])) {
		if string(encoded[at+4:at+8]) == "IDAT" {
			return at
		}
	}
	t.Fatal("the encoded PNG holds no image data")
	return 0
}

func pngChunk(kind string, data []byte) []byte {
	chunk := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
	chunk = append(append(chunk, kind...), data...)
	return binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(append([]byte(kind), data...)))
}

// webpWithOrientation is an extended WebP: the VP8X header a file needs before it may carry
// anything but a frame, with the Exif flag set and the canvas named, the lossless frame itself,
// and the EXIF chunk. The frame is the five-byte VP8L header the api rows build - the 0x2f
// signature, 14 bits of width-1, 14 bits of height-1, an alpha bit and a three-bit version,
// least significant bit first - since no decoder here reads a pixel.
func webpWithOrientation(width, height int, orientation uint16) []byte {
	frame := uint32(width-1) | uint32(height-1)<<14 | 1<<28
	vp8x := []byte{
		0x08, 0, 0, 0, // the Exif flag, then the three reserved bytes
		byte(width - 1), byte((width - 1) >> 8), byte((width - 1) >> 16),
		byte(height - 1), byte((height - 1) >> 8), byte((height - 1) >> 16),
	}
	body := append([]byte("WEBP"), riffChunk("VP8X", vp8x)...)
	body = append(body, riffChunk("VP8L", []byte{0x2f, byte(frame), byte(frame >> 8), byte(frame >> 16), byte(frame >> 24)})...)
	body = append(body, riffChunk("EXIF", exifBlock("II", orientation))...)
	return append(binary.LittleEndian.AppendUint32([]byte("RIFF"), uint32(len(body))), body...)
}

func riffChunk(kind string, data []byte) []byte {
	chunk := append(binary.LittleEndian.AppendUint32([]byte(kind), uint32(len(data))), data...)
	if len(data)%2 == 1 {
		chunk = append(chunk, 0) // a chunk is padded to an even length
	}
	return chunk
}

// The pair recorded is the one a browser draws, not the one the bytes store: a photograph whose
// Exif Orientation lays it on its side is drawn turned, so its two numbers are exchanged. Which
// formats are turned was measured, and orientation.go says what and where; a format no engine
// turns, and a chunk no engine reads, leave the stored pair alone.
func TestPictureDimensionsRecordsThePairABrowserDraws(t *testing.T) {
	for _, row := range []struct {
		name  string
		bytes []byte
		want  string
	}{
		{"a JPEG carrying no Exif", encodedJPEG(t, 40, 20), "40x20"},
		{"a JPEG left upright by its Exif", jpegWithOrientation(t, 40, 20, "II", 1), "40x20"},
		{"a JPEG turned half round, which exchanges nothing", jpegWithOrientation(t, 40, 20, "II", 3), "40x20"},
		{"a phone photograph, Orientation 6", jpegWithOrientation(t, 40, 20, "II", 6), "20x40"},
		{"the other quarter turn, Orientation 8", jpegWithOrientation(t, 40, 20, "II", 8), "20x40"},
		{"an Exif written the other way round, MM", jpegWithOrientation(t, 40, 20, "MM", 6), "20x40"},
		{"an Orientation no reader draws by", jpegWithOrientation(t, 40, 20, "II", 9), "40x20"},
		{"a PNG carrying no eXIf", encodedPNG(t, 40, 20), "40x20"},
		{"a PNG whose eXIf stands before its image data", pngWithOrientation(t, 40, 20, 6, true), "20x40"},
		{"a PNG whose eXIf stands after it, which no engine reads", pngWithOrientation(t, 40, 20, 6, false), "40x20"},
		{"a WebP with an EXIF chunk, which no engine turns by", webpWithOrientation(40, 20, 6), "40x20"},
	} {
		t.Run(row.name, func(t *testing.T) {
			width, height := files.PictureDimensions(row.bytes)
			got := "none"
			if width != nil && height != nil {
				got = fmt.Sprintf("%dx%d", *width, *height)
			}
			if got != row.want {
				t.Errorf("PictureDimensions of %s records %s, want %s", row.name, got, row.want)
			}
		})
	}
}
