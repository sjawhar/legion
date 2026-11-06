package files

import "encoding/binary"

// A picture's bytes may carry an Exif Orientation tag, and a browser draws the picture turned by
// it: `image-orientation: from-image` is the initial value of that property, so an upright box is
// drawn for a picture whose tag says it lies on its side. The decoders behind PictureDimensions
// read no such tag - they answer the size the bytes store - so the size Dispatch records must be
// turned here, or a page would reserve a landscape box for a portrait photograph and the text
// below it would move once the bytes landed (INFRA-760 R12).
//
// Which formats are turned was measured rather than assumed, in Chromium 158 over the DevTools
// protocol (naturalWidth/naturalHeight of a 40x20 fixture carrying Orientation 6) and in the web
// platform tests' own cross-browser results:
//
//   - JPEG, from the APP1 segment opening `Exif\0\0`: turned. Chromium answers 20x40 for values 5
//     to 8 and 40x20 for 0 to 4 and 9, which is the Exif rule that 5 to 8 are the transposed
//     eighths. wpt's css/css-images/image-orientation rows are JPEG throughout.
//   - PNG, from an `eXIf` chunk before the first IDAT: turned. Chromium answers 20x40 for a chunk
//     before IDAT and 40x20 for the same chunk after it, and
//     css/css-images/image-orientation/image-orientation-exif-png-2.html - which draws
//     F-exif-chunk-early.png (eXIf, Orientation 6, before IDAT) and F-exif-late.png (the same
//     chunk after IDAT) against a rotated and an upright reference - passes in Chrome 157,
//     Firefox 159 and Safari 27.2 on wpt.fyi. So all three engines turn by an early chunk and
//     ignore a late one, which is the boundary pngExif holds to.
//   - WebP, from the `EXIF` chunk an extended (VP8X) file may carry: not turned. Chromium answers
//     40x20 for a fixture whose VP8X sets the Exif flag and whose EXIF chunk holds the same
//     Orientation 6 that turns the JPEG and the PNG above, and neither Blink's
//     webp_image_decoder.cc, WebKit's WEBPImageDecoder.cpp nor Gecko's nsWebPDecoder.cpp names
//     Exif or orientation at all, while Gecko's nsPNGDecoder.cpp includes EXIF.h. wpt carries no
//     WebP orientation row.
//   - GIF: there is no orientation to read. GIF89a has no Exif, and a picture's size is its
//     logical screen.
//
// One case is left alone on purpose: a PNG carrying its Exif in a `zTXt` "Raw profile type exif"
// rather than an `eXIf` chunk, which image-orientation-exif-png.html covers. Safari 27.2 turns
// that one and Chrome 157 and Firefox 159 do not, so no single recorded pair is right for every
// reader, and the stored size is the pair most of them draw.

// turnsDimensions reports whether a browser draws a picture of this format, holding these bytes,
// with its stored width and height exchanged. format is image.DecodeConfig's own answer.
func turnsDimensions(format string, content []byte) bool {
	var exif []byte
	switch format {
	case "jpeg":
		exif = jpegExif(content)
	case "png":
		exif = pngExif(content)
	default:
		return false
	}
	orientation := exifOrientation(exif)
	// 1 to 4 leave the picture's axes where they are, 5 to 8 exchange them; 0, 9 and anything
	// else is no orientation, and a browser draws the picture as it is stored.
	return orientation >= 5 && orientation <= 8
}

// exifHeader opens the JPEG APP1 segment that carries Exif, and is not part of the TIFF block
// that follows it.
const exifHeader = "Exif\x00\x00"

// jpegExif is the TIFF block of the first APP1 Exif segment, nil for a JPEG carrying none. The
// walk is marker by marker from past the SOI: a 0xff, the marker, and for all but a handful a
// two-byte big-endian length that counts itself. It stops at the scan, since no header follows
// one.
func jpegExif(content []byte) []byte {
	for i := 2; i+4 <= len(content); {
		if content[i] != 0xff {
			return nil // No marker where one must be: nothing further can be read.
		}
		switch marker := content[i+1]; {
		case marker == 0xff:
			i++ // A fill byte before the marker.
		case marker == 0x01 || (marker >= 0xd0 && marker <= 0xd8):
			i += 2 // A marker standing alone, with no segment of its own.
		case marker == 0xd9 || marker == 0xda:
			return nil // The end of the picture, or the scan: no segment is left to read.
		default:
			length := int(content[i+2])<<8 | int(content[i+3])
			if length < 2 || i+2+length > len(content) {
				return nil
			}
			segment := content[i+4 : i+2+length]
			if marker == 0xe1 && len(segment) > len(exifHeader) && string(segment[:len(exifHeader)]) == exifHeader {
				return segment[len(exifHeader):]
			}
			i += 2 + length
		}
	}
	return nil
}

// pngExif is the TIFF block of an eXIf chunk standing before the first IDAT, nil for a PNG
// carrying none there. A chunk is a four-byte big-endian length, a four-byte type, the data and a
// four-byte CRC; the walk starts past the eight-byte signature. A chunk after the image data is
// passed over rather than read, since that is what every engine measured does with it.
func pngExif(content []byte) []byte {
	for i := 8; i+8 <= len(content); {
		length := int(binary.BigEndian.Uint32(content[i:]))
		if length < 0 || i+12+length > len(content) {
			return nil
		}
		switch string(content[i+4 : i+8]) {
		case "eXIf":
			return content[i+8 : i+8+length]
		case "IDAT", "IEND":
			return nil
		}
		i += 12 + length
	}
	return nil
}

// exifOrientation is the Orientation tag of a TIFF block, 0 for a block that does not hold one.
// The block opens with its byte order ("II" or "MM"), the number 42 in that order, and the offset
// of the first image file directory, which is a count of entries and then that many twelve-byte
// entries: a tag, a type, a count and four bytes holding the value itself when it fits, as a
// SHORT does.
func exifOrientation(block []byte) uint16 {
	if len(block) < 8 {
		return 0
	}
	var order binary.ByteOrder
	switch string(block[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 0
	}
	if order.Uint16(block[2:]) != 42 {
		return 0
	}
	directory := int(order.Uint32(block[4:]))
	if directory < 8 || directory+2 > len(block) {
		return 0
	}
	const (
		orientationTag = 0x0112
		shortType      = 3
	)
	for entry, left := directory+2, int(order.Uint16(block[directory:])); left > 0; entry, left = entry+12, left-1 {
		if entry+12 > len(block) {
			return 0
		}
		if order.Uint16(block[entry:]) != orientationTag {
			continue
		}
		if order.Uint16(block[entry+2:]) != shortType {
			return 0
		}
		return order.Uint16(block[entry+8:])
	}
	return 0
}
