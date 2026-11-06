package files

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	// The four picture formats an upload stores, each registering itself with image.DecodeConfig
	// so PictureDimensions names no decoder of its own. The standard library decodes three of
	// them; WebP it does not decode at all, and golang.org/x/image/webp registers that one.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"

	_ "golang.org/x/image/webp"
)

// PictureDimensions is a picture's own width and height in pixels as a browser draws them, read
// from the header of the bytes the caller already holds, so the read costs no second copy of the
// file. An upload asks this of an image alone, so a video, a document and every other file
// record neither.
//
// The pair is recorded together or not at all: a header that does not decode, or that claims no
// area, answers nil for both and the upload is stored and served exactly as it was before
// Dispatch read a size (INFRA-760 R12). Which of the four decoders reads the header is the
// registry's own answer, from the bytes' magic, through the blank imports at the head of this
// file; each reads the header alone and decodes no pixel.
//
// The pair is the drawn one, not the stored one: where a picture's bytes carry an Exif
// Orientation the browsers turn the picture by, the two numbers are exchanged (turnsDimensions,
// which names the formats that are turned and the measurement behind each).
//
// This is the one declaration of the decode: the upload path (api.uploadArtifact) and the pass
// that fills the versions stored before it (BackfillDimensions) read a size the same way, so no
// version is ever recorded under two answers.
func PictureDimensions(content []byte) (width, height *int) {
	config, format, err := image.DecodeConfig(bytes.NewReader(content))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return nil, nil
	}
	drawnWidth, drawnHeight := config.Width, config.Height
	if turnsDimensions(format, content) {
		drawnWidth, drawnHeight = drawnHeight, drawnWidth
	}
	return &drawnWidth, &drawnHeight
}

// dimensionsLock is the advisory lock one pass of BackfillDimensions holds for its run, so two
// passes started together (a one-off task run twice) do not read the same rows' bytes in
// lockstep. Its own lock, not backfillLock: this pass writes no bytes and clears no row, so it
// runs beside a file pass. Session-scoped, so a pass that dies releases it with its connection.
const dimensionsLock = 0x494e_4652_4137_3630 // "INFRA760"

// ErrAnotherDimensionsPassRunning is the refusal when the advisory lock is held by another run.
var ErrAnotherDimensionsPassRunning = errors.New("another backfill-dimensions pass holds the lock; wait for it to finish")

// ErrNoFileStore is the refusal when no store is configured: a row whose bytes have been moved to
// the bucket holds none, so a pass without the store could read no picture of the deployment the
// pass is for.
var ErrNoFileStore = errors.New("backfill-dimensions needs the file store: a row whose bytes were moved holds none to read a size from")

// pictureNeedingSize is the rows BackfillDimensions walks: an image artifact's version stored
// under one of the content types an upload stores a picture under (api.artifactKind), whose size
// is not recorded. The pair is written together, so a row holding one of the two needs the pass.
const pictureNeedingSize = `a.kind = 'image'
		and v.mime in ('image/gif', 'image/jpeg', 'image/png', 'image/webp')
		and (v.width is null or v.height is null)`

// BackfillDimensions records the own size of every picture version stored before Dispatch read
// one, oldest first and one row at a time: it reads the row's bytes, from the row itself while it
// still holds them and from store once a file pass has moved them, reads the header with the
// decode the upload path uses (PictureDimensions), and writes the pair in one update that writes
// only a row still missing it. So a second pass over the same rows writes nothing, and a run
// stopped anywhere leaves each row either recorded or untouched.
//
// A row it cannot do - bytes the store does not hold, a header that does not decode - is named in
// the report and passed over, so one bad row does not hold every newer one; a picture with no
// recorded size is drawn exactly as it was, which is what every picture looked like before this
// pass. A database failure, or a cancelled context, stops the pass with that error.
func BackfillDimensions(ctx context.Context, pool Querier, store Store, out io.Writer) (PassReport, error) {
	if store == nil {
		return PassReport{}, ErrNoFileStore
	}
	release, err := acquire(ctx, pool, dimensionsLock, ErrAnotherDimensionsPassRunning)
	if err != nil {
		return PassReport{}, err
	}
	defer release()
	var report PassReport
	var cursor *fileVersion
	for {
		version, body, err := nextVersion(ctx, pool, cursor, pictureNeedingSize, true)
		if err != nil {
			return report, err
		}
		if version == nil {
			return report, nil
		}
		cursor = version
		if body == nil {
			if version.sha == nil {
				report.fail(out, version.id, errors.New("the row holds no bytes and records no sha256 to read them under"))
				continue
			}
			body, err = ReadAll(ctx, store, *version.sha)
			if err != nil {
				if ctx.Err() != nil {
					return report, ctx.Err()
				}
				report.fail(out, version.id, err)
				continue
			}
		}
		width, height := PictureDimensions(body)
		if width == nil {
			report.fail(out, version.id, fmt.Errorf("the %s header did not decode", version.mime))
			continue
		}
		tag, err := pool.Exec(ctx, `
			update artifact_versions set width = $2, height = $3
			where id = $1 and (width is null or height is null)
		`, version.id, *width, *height)
		if err != nil {
			return report, fmt.Errorf("version %s: record the size: %w", version.id, err)
		}
		if tag.RowsAffected() == 0 {
			// Recorded between the read and the update; the pair is on the row either way, and
			// nothing here counts it twice.
			continue
		}
		report.Done++
		report.Bytes += int64(len(body))
		fmt.Fprintf(out, "sized version %s (%dx%d, %d bytes)\n", version.id, *width, *height, len(body))
	}
}
