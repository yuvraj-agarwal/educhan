package media

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// Fixtures for the metadata tests, built in code so every byte of them is
// explained here: a camera's file is a stock image plus segments, chunks and
// extensions spliced in, and these helpers do the splicing.

// Needles are the strings the metadata fixtures carry. Each is checked to be
// in the fixture before it is checked to be gone from what is stored, so a
// fixture that stopped carrying one fails rather than passing for nothing.
const (
	needleCamera  = "educhan-test-camera"
	needleDatum   = "WGS-84"
	needleGPS     = "GPS"
	needleXMP     = "http://ns.adobe.com/xap"
	needleXMPGPS  = "exif:GPSLatitude"
	needlePS      = "Photoshop 3.0"
	needleIPTC    = "educhan test caption"
	needleICC     = "educhan-test-icc"
	needleComment = "educhan test comment: taken at 51.5N"
)

/* Pictures ----------------------------------------------------------------- */

// Quadrant colours: the stored picture's top-left, top-right, bottom-left and
// bottom-right quarters, far enough apart to survive JPEG twice over.
var (
	quadTL = color.RGBA{220, 30, 30, 255}   // red
	quadTR = color.RGBA{30, 200, 30, 255}   // green
	quadBL = color.RGBA{30, 30, 220, 255}   // blue
	quadBR = color.RGBA{240, 240, 240, 255} // white
)

// quadrants builds a w×h picture in the four quadrant colours.
func quadrants(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := quadTL
			switch {
			case x >= w/2 && y < h/2:
				c = quadTR
			case x < w/2 && y >= h/2:
				c = quadBL
			case x >= w/2 && y >= h/2:
				c = quadBR
			}
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

func encodeJPEGImage(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return buf.Bytes()
}

/* JPEG --------------------------------------------------------------------- */

// jpegSegment is one marker segment: the marker, a length counting itself, and
// the payload.
func jpegSegment(marker byte, payload []byte) []byte {
	n := len(payload) + 2
	return append([]byte{0xFF, marker, byte(n >> 8), byte(n)}, payload...)
}

// withSegments splices segments in straight after a JPEG's SOI marker, which
// is where a camera puts its APP segments.
func withSegments(jpg []byte, segments ...[]byte) []byte {
	out := append([]byte(nil), jpg[:2]...)
	for _, s := range segments {
		out = append(out, s...)
	}
	return append(out, jpg[2:]...)
}

func exifSegment(tiff []byte) []byte {
	return jpegSegment(0xE1, append([]byte("Exif\x00\x00"), tiff...))
}

func xmpPacket() string {
	return `<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">` +
		`<rdf:Description xmlns:exif="http://ns.adobe.com/exif/1.0/" ` + needleXMPGPS + `="51,30.0N"/>` +
		`</rdf:RDF></x:xmpmeta>`
}

func xmpSegment() []byte {
	return jpegSegment(0xE1, []byte(needleXMP+"/1.0/\x00"+xmpPacket()))
}

// photoshopSegment is an APP13 carrying an IPTC caption (record 2, dataset
// 120) in an 8BIM resource 0x0404, the way Photoshop and newsroom tools write
// one.
func photoshopSegment() []byte {
	iptc := append([]byte{0x1C, 0x02, 0x78, 0x00, byte(len(needleIPTC))}, needleIPTC...)
	var p []byte
	p = append(p, needlePS+"\x00"...)
	p = append(p, "8BIM"...)
	p = append(p, 0x04, 0x04, 0x00, 0x00) // resource id, empty pascal name
	p = binary.BigEndian.AppendUint32(p, uint32(len(iptc)))
	p = append(p, iptc...)
	if len(iptc)%2 == 1 {
		p = append(p, 0)
	}
	return jpegSegment(0xED, p)
}

// iccSegment is an APP2 ICC profile chunk, one of one, holding a description
// rather than a real profile; decoders skip it either way.
func iccSegment() []byte {
	return jpegSegment(0xE2, append([]byte("ICC_PROFILE\x00\x01\x01"), needleICC...))
}

func commentSegment() []byte {
	return jpegSegment(0xFE, []byte(needleComment))
}

// cameraJPEG is a phone photo as it leaves the phone: stored on its side with
// Orientation 6, EXIF with a make and a GPS position, XMP, an IPTC caption, an
// ICC profile and a comment. Upright, it is the w×h quadrant picture.
func cameraJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	stored := quadrantsStoredFor(w, h, 6)
	return withSegments(encodeJPEGImage(t, stored),
		exifSegment(gpsTIFF(binary.LittleEndian, 6)),
		xmpSegment(),
		photoshopSegment(),
		iccSegment(),
		commentSegment(),
	)
}

// quadrantsStoredFor is the quadrant picture as a camera tagging it with
// orientation o stores it: turned so that applying o brings it back to a w×h
// upright picture. Only 1 and 6 are needed here.
func quadrantsStoredFor(w, h, o int) image.Image {
	upright := quadrants(w, h)
	if o == 1 {
		return upright
	}
	// Orientation 6 is viewed by turning 90° clockwise, so the camera stored
	// the upright picture turned 90° anticlockwise: h wide and w high.
	stored := image.NewRGBA(image.Rect(0, 0, h, w))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			stored.Set(y, w-1-x, upright.At(x, y))
		}
	}
	return stored
}

/* TIFF (the body of EXIF) -------------------------------------------------- */

type tiffOrder interface {
	binary.ByteOrder
	binary.AppendByteOrder
}

type tiffEntry struct {
	tag, typ uint16
	count    uint32
	value    []byte // encoded; up to four bytes sit in the entry itself
}

func shortEntry(order tiffOrder, tag, v uint16) tiffEntry {
	return tiffEntry{tag: tag, typ: 3, count: 1, value: order.AppendUint16(nil, v)}
}

func longEntry(order tiffOrder, tag uint16, v uint32) tiffEntry {
	return tiffEntry{tag: tag, typ: 4, count: 1, value: order.AppendUint32(nil, v)}
}

func asciiEntry(tag uint16, s string) tiffEntry {
	return tiffEntry{tag: tag, typ: 2, count: uint32(len(s) + 1), value: []byte(s + "\x00")}
}

func tiffHeader(order tiffOrder) []byte {
	b := []byte("MM")
	if order == binary.ByteOrder(binary.LittleEndian) {
		b = []byte("II")
	}
	b = order.AppendUint16(b, 42)
	return order.AppendUint32(b, 8) // IFD0 straight after the header
}

// appendIFD appends an IFD to a TIFF under construction, whose length is
// therefore the IFD's offset, followed by the values too long for their
// entries.
func appendIFD(buf []byte, order tiffOrder, entries []tiffEntry, next uint32) []byte {
	extra := len(buf) + 2 + 12*len(entries) + 4
	var data []byte
	buf = order.AppendUint16(buf, uint16(len(entries)))
	for _, e := range entries {
		buf = order.AppendUint16(buf, e.tag)
		buf = order.AppendUint16(buf, e.typ)
		buf = order.AppendUint32(buf, e.count)
		if len(e.value) <= 4 {
			inline := make([]byte, 4)
			copy(inline, e.value)
			buf = append(buf, inline...)
			continue
		}
		buf = order.AppendUint32(buf, uint32(extra+len(data)))
		data = append(data, e.value...)
		if len(data)%2 == 1 {
			data = append(data, 0)
		}
	}
	buf = order.AppendUint32(buf, next)
	return append(buf, data...)
}

// orientationTIFF is EXIF holding nothing but an orientation.
func orientationTIFF(order tiffOrder, o uint16) []byte {
	return appendIFD(tiffHeader(order), order, []tiffEntry{shortEntry(order, 0x0112, o)}, 0)
}

// gpsTIFF is EXIF as a phone writes it, cut down: IFD0 with the make and the
// orientation, and a GPS IFD with a position, a datum and a processing method.
func gpsTIFF(order tiffOrder, o uint16) []byte {
	ifd0 := func(gpsOffset uint32) []byte {
		return appendIFD(tiffHeader(order), order, []tiffEntry{
			asciiEntry(0x010F, needleCamera), // Make
			shortEntry(order, 0x0112, o),     // Orientation
			longEntry(order, 0x8825, gpsOffset),
		}, 0)
	}
	buf := ifd0(uint32(len(ifd0(0)))) // the GPS IFD follows IFD0 and its values
	rational := func(n, d uint32) []byte {
		return order.AppendUint32(order.AppendUint32(nil, n), d)
	}
	latitude := append(append(rational(51, 1), rational(30, 1)...), rational(0, 1)...)
	return appendIFD(buf, order, []tiffEntry{
		asciiEntry(0x0001, "N"),                                                          // GPSLatitudeRef
		{tag: 0x0002, typ: 5, count: 3, value: latitude},                                 // GPSLatitude
		asciiEntry(0x0012, needleDatum),                                                  // GPSMapDatum
		{tag: 0x001B, typ: 7, count: 11, value: []byte("ASCII\x00\x00\x00" + needleGPS)}, // GPSProcessingMethod
	}, 0)
}

/* PNG ---------------------------------------------------------------------- */

func pngChunk(typ string, data []byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
	out = append(out, typ...)
	out = append(out, data...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(out[4:]))
}

// withPNGChunks splices chunks in after a PNG's IHDR, the first chunk.
func withPNGChunks(p []byte, chunks ...[]byte) []byte {
	const ihdrEnd = 8 + 4 + 4 + 13 + 4 // signature, IHDR length, type, data, CRC
	out := append([]byte(nil), p[:ihdrEnd]...)
	for _, c := range chunks {
		out = append(out, c...)
	}
	return append(out, p[ihdrEnd:]...)
}

// pngChunkTypes lists the chunk types in a PNG, in order.
func pngChunkTypes(t *testing.T, p []byte) []string {
	t.Helper()
	var types []string
	for i := 8; i+8 <= len(p); {
		n := int(binary.BigEndian.Uint32(p[i : i+4]))
		types = append(types, string(p[i+4:i+8]))
		i += 12 + n
	}
	return types
}

/* WebP --------------------------------------------------------------------- */

// webpFile assembles a WebP from chunks, computing the RIFF size.
func webpFile(chunks ...riffChunk) []byte {
	out := []byte("RIFF\x00\x00\x00\x00WEBP")
	for _, c := range chunks {
		out = appendChunk(out, c)
	}
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(out)-8))
	return out
}

func vp8xChunk(flags byte, w, h int) riffChunk {
	p := make([]byte, 10)
	p[0] = flags
	put24(p[4:7], uint32(w-1))
	put24(p[7:10], uint32(h-1))
	return riffChunk{id: "VP8X", payload: p}
}

// anmfChunk is one animation frame at (x, y), w×h, holding chunks.
func anmfChunk(x, y, w, h int, chunks ...riffChunk) riffChunk {
	p := make([]byte, anmfHeaderLen)
	put24(p[0:3], uint32(x/2))
	put24(p[3:6], uint32(y/2))
	put24(p[6:9], uint32(w-1))
	put24(p[9:12], uint32(h-1))
	put24(p[12:15], 100) // duration, ms
	for _, c := range chunks {
		p = appendChunk(p, c)
	}
	return riffChunk{id: "ANMF", payload: p}
}

func put24(b []byte, v uint32) {
	b[0], b[1], b[2] = byte(v), byte(v>>8), byte(v>>16)
}

// webpChunks reads back the top-level chunks of a WebP the test built or got.
func webpChunks(t *testing.T, data []byte) []riffChunk {
	t.Helper()
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		t.Fatalf("not a RIFF WebP: % x", data[:min(len(data), 16)])
	}
	if got := binary.LittleEndian.Uint32(data[4:8]); int(got) != len(data)-8 {
		t.Fatalf("RIFF size = %d, want %d (the file's length less 8)", got, len(data)-8)
	}
	var chunks []riffChunk
	if !walkChunks(data[12:], func(c riffChunk) { chunks = append(chunks, c) }) {
		t.Fatal("chunks do not parse")
	}
	return chunks
}

func chunkIDs(chunks []riffChunk) []string {
	var ids []string
	for _, c := range chunks {
		ids = append(ids, c.id)
	}
	return ids
}
