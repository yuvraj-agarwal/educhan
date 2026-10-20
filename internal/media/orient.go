package media

import (
	"encoding/binary"
	"image"
)

// EXIF Orientation, as far as educhan reads it.
//
// A phone stores a photo the way its sensor was facing and writes a tag saying
// how to turn it upright. Browsers obey the tag, in a JPEG's Exif segment and
// in a PNG's eXIf chunk alike, so a stored file that lost the tag and kept its
// pixels would show sideways. Stripping metadata therefore means applying the
// tag to the pixels first, which is what orient does, and the stored file, its
// recorded dimensions and its thumbnail are all of the upright picture.
//
// The values are the TIFF/EXIF ones: which visual side the stored image's
// first row and first column are.
//
//	1  top, left      (upright)
//	2  top, right     (mirrored)
//	3  bottom, right  (turned 180°)
//	4  bottom, left   (flipped)
//	5  left, top      (transposed)
//	6  right, top     (turn 90° clockwise to view)
//	7  right, bottom  (transversed)
//	8  left, bottom   (turn 90° anticlockwise to view)

const (
	exifOrientationTag = 0x0112
	tiffTypeShort      = 3
)

// jpegOrientation reads the Orientation tag from the first Exif APP1 segment
// of a JPEG, and returns 1, upright, for a file with no such tag, a value
// outside 1–8, or anything it cannot read. A wrong guess here can only leave a
// picture turned the way it was stored; whether the file is an image at all is
// for the decoder to say, so this never fails.
//
// It walks the segments before the first scan as image/jpeg's decoder reads
// them (jpegNextSegment), which is also how libjpeg, and so Chromium, reads
// them: bytes between segments that are not a marker, an 0xFF00 pair among
// them, are skipped rather than ending the walk, so an Exif segment after them
// turns the stored picture as it turned the one the poster's browser showed.
//
// The upload is hostile until proven otherwise, so the reader works on the
// bytes in place: every offset is checked against the segment before it is
// used, nothing is allocated, and the only offset followed is the one to IFD0,
// where the tag lives. With no chain of IFDs walked there is no loop to be
// caught in, however the offsets point.
func jpegOrientation(data []byte) int {
	if len(data) < 2 || data[0] != 0xFF || data[1] != 0xD8 {
		return 1
	}
	for i := 2; ; {
		marker, segment, next, ok := jpegNextSegment(data, i)
		if !ok || marker == 0xDA {
			// The end of what the decoder reads, or the start of the first
			// scan: the metadata segments are behind us.
			return 1
		}
		if marker == 0xE1 && len(segment) >= 6 && string(segment[:5]) == "Exif\x00" {
			return tiffOrientation(segment[6:])
		}
		i = next
	}
}

// pngOrientation reads the Orientation tag from a PNG's eXIf chunk, under the
// same rules as jpegOrientation, and returns 1 for a file with no such chunk
// or one it cannot read. Only an eXIf chunk before the first IDAT counts:
// that is where the PNG specification puts it, and the only place browsers
// read it from, so one further on never turned the picture on screen. The
// chunk holds the TIFF structure itself, with no "Exif" prefix.
func pngOrientation(data []byte) int {
	i := 8 // the signature
	for i+8 <= len(data) {
		n := int64(binary.BigEndian.Uint32(data[i : i+4]))
		if n > int64(len(data)-i-12) {
			return 1
		}
		switch string(data[i+4 : i+8]) {
		case "eXIf":
			return tiffOrientation(data[i+8 : i+8+int(n)])
		case "IDAT":
			return 1
		}
		i += 12 + int(n)
	}
	return 1
}

// tiffOrientation reads the Orientation entry of IFD0 in a TIFF structure, the
// body of an Exif segment or an eXIf chunk, under the same rules as
// jpegOrientation, and the way Chromium reads it, so that the picture is
// stored turned as the poster's browser showed it: the first Orientation entry
// that is a single SHORT from 1 to 8 counts, and one that is not (of another
// type or count, or 0, or 9) is passed over rather than read as upright; and
// an IFD that claims more entries than the segment holds is read for the
// entries it does hold (TestOrientationIsReadAsChromiumReadsIt).
func tiffOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 1
	}
	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 1
	}
	if order.Uint16(tiff[2:4]) != 42 {
		return 1
	}
	// Compared as 64-bit so an offset near 4 GiB cannot wrap on a 32-bit build.
	offset := uint64(order.Uint32(tiff[4:8]))
	if offset < 8 || offset+2 > uint64(len(tiff)) {
		return 1
	}
	entries := tiff[offset+2:]
	count := min(int(order.Uint16(tiff[offset:offset+2])), len(entries)/12)
	for k := 0; k < count; k++ {
		entry := entries[12*k : 12*k+12]
		if order.Uint16(entry[0:2]) != exifOrientationTag ||
			order.Uint16(entry[2:4]) != tiffTypeShort || order.Uint32(entry[4:8]) != 1 {
			continue
		}
		if v := int(order.Uint16(entry[8:10])); v >= 1 && v <= 8 {
			return v
		}
	}
	return 1
}

// orientedPoint is where pixel (x, y) of a w×h image lands once orientation o
// is applied. For 5 to 8 the result is h wide and w high.
func orientedPoint(o, x, y, w, h int) (int, int) {
	switch o {
	case 2:
		return w - 1 - x, y
	case 3:
		return w - 1 - x, h - 1 - y
	case 4:
		return x, h - 1 - y
	case 5:
		return y, x
	case 6:
		return h - 1 - y, x
	case 7:
		return h - 1 - y, w - 1 - x
	case 8:
		return y, w - 1 - x
	}
	return x, y
}

// orient returns src turned upright according to EXIF orientation o, or src
// itself when o is 1 or unknown. Every pixel keeps its value exactly, in a
// picture of the same kind: a JPEG's YCbCr (orientYCbCr), grayscale or RGBA
// (what an RGB or a CMYK JPEG is by the time it gets here), and whatever a
// PNG decodes to, from a palette to 16-bit colour with alpha, are each turned
// a pixel's bytes at a time into a copy of the same size. That costs the
// decoded size again, which is what processingCost counts for it.
func orient(src image.Image, o int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if o < 2 || o > 8 || w <= 0 || h <= 0 {
		return src
	}
	r := orientedRect(w, h, o)
	switch s := src.(type) {
	case *image.YCbCr:
		return orientYCbCr(s, o)
	case *image.Gray:
		dst := image.NewGray(r)
		orientPixels(dst.Pix, dst.Stride, s.Pix[s.PixOffset(b.Min.X, b.Min.Y):], s.Stride, w, h, 1, o)
		return dst
	case *image.Gray16:
		dst := image.NewGray16(r)
		orientPixels(dst.Pix, dst.Stride, s.Pix[s.PixOffset(b.Min.X, b.Min.Y):], s.Stride, w, h, 2, o)
		return dst
	case *image.Paletted:
		dst := image.NewPaletted(r, s.Palette)
		orientPixels(dst.Pix, dst.Stride, s.Pix[s.PixOffset(b.Min.X, b.Min.Y):], s.Stride, w, h, 1, o)
		return dst
	case *image.RGBA:
		dst := image.NewRGBA(r)
		orientPixels(dst.Pix, dst.Stride, s.Pix[s.PixOffset(b.Min.X, b.Min.Y):], s.Stride, w, h, 4, o)
		return dst
	case *image.NRGBA:
		dst := image.NewNRGBA(r)
		orientPixels(dst.Pix, dst.Stride, s.Pix[s.PixOffset(b.Min.X, b.Min.Y):], s.Stride, w, h, 4, o)
		return dst
	case *image.RGBA64:
		dst := image.NewRGBA64(r)
		orientPixels(dst.Pix, dst.Stride, s.Pix[s.PixOffset(b.Min.X, b.Min.Y):], s.Stride, w, h, 8, o)
		return dst
	case *image.NRGBA64:
		dst := image.NewNRGBA64(r)
		orientPixels(dst.Pix, dst.Stride, s.Pix[s.PixOffset(b.Min.X, b.Min.Y):], s.Stride, w, h, 8, o)
		return dst
	}

	// No decoder educhan uses makes anything else; were one to, 16 bits a
	// channel keeps any colour it can report.
	dst := image.NewRGBA64(r)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			dx, dy := orientedPoint(o, x, y, w, h)
			dst.Set(dx, dy, src.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}

func orientedRect(w, h, o int) image.Rectangle {
	if o >= 5 {
		w, h = h, w
	}
	return image.Rect(0, 0, w, h)
}

// orientYCbCr turns a YCbCr picture. Where its chroma samples line up with the
// turned picture's, each plane is turned on its own, keeping the subsampling
// (with its factors swapped when the picture turns on its side): that costs
// the decoded size again, 1.5 bytes a pixel for an ordinary 4:2:0 photo, and
// hands the encoder the planes it would have computed.
//
// They do not line up when the turn reverses a side that is not a whole
// number of chroma samples long, whatever the ratio: an odd width at 4:2:2 or
// 4:2:0, an odd height at 4:4:0, 4:2:0 or 4:1:0, a width that is not a
// multiple of four at 4:1:1 or 4:1:0 (turnedSubsampling says which sides each
// orientation reverses). The last sample then covers fewer pixels than the
// others, and turned within its plane it would land first, covering the wrong
// ones: every colour along that side would shift by a pixel. Nor has a
// quarter-width chroma (4:1:1, 4:1:0) any counterpart in the image package
// once turned on its side. Either way every pixel takes the chroma it was
// decoded with into a picture with chroma at full resolution (4:4:4), 3 bytes
// a pixel, and jpegenc stores it at 4:4:4, not the subsampling it came in at:
// finer chroma than the upload carried and a larger file, but no colour lost.
func orientYCbCr(src *image.YCbCr, o int) *image.YCbCr {
	w, h := src.Rect.Dx(), src.Rect.Dy()
	if ratio, ok := turnedSubsampling(src.SubsampleRatio, o, w, h); ok && src.Rect.Min == (image.Point{}) {
		cw, ch := chromaSize(src.SubsampleRatio, w, h)
		dst := image.NewYCbCr(orientedRect(w, h, o), ratio)
		orientPlane(dst.Y, dst.YStride, src.Y, src.YStride, w, h, o)
		orientPlane(dst.Cb, dst.CStride, src.Cb, src.CStride, cw, ch, o)
		orientPlane(dst.Cr, dst.CStride, src.Cr, src.CStride, cw, ch, o)
		return dst
	}
	dst := image.NewYCbCr(orientedRect(w, h, o), image.YCbCrSubsampleRatio444)
	b := src.Rect
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			yi, ci := src.YOffset(b.Min.X+x, b.Min.Y+y), src.COffset(b.Min.X+x, b.Min.Y+y)
			dx, dy := orientedPoint(o, x, y, w, h)
			at := dy*dst.YStride + dx // the same offset in all three planes at 4:4:4
			dst.Y[at], dst.Cb[at], dst.Cr[at] = src.Y[yi], src.Cb[ci], src.Cr[ci]
		}
	}
	return dst
}

// subsampling is how many pixels across and down share a chroma sample at a
// subsampling ratio, or 0, 0 for a ratio it does not know.
func subsampling(ratio image.YCbCrSubsampleRatio) (int, int) {
	switch ratio {
	case image.YCbCrSubsampleRatio444:
		return 1, 1
	case image.YCbCrSubsampleRatio422:
		return 2, 1
	case image.YCbCrSubsampleRatio420:
		return 2, 2
	case image.YCbCrSubsampleRatio440:
		return 1, 2
	case image.YCbCrSubsampleRatio411:
		return 4, 1
	case image.YCbCrSubsampleRatio410:
		return 4, 2
	}
	return 0, 0
}

// turnedSubsampling is the subsampling a w×h YCbCr picture at ratio keeps
// when orientation o turns it plane by plane, and whether its planes can be
// turned that way exactly (orientYCbCr).
func turnedSubsampling(ratio image.YCbCrSubsampleRatio, o, w, h int) (image.YCbCrSubsampleRatio, bool) {
	fx, fy := subsampling(ratio)
	if fx == 0 {
		return ratio, false
	}
	mirrorsX := o == 2 || o == 3 || o == 7 || o == 8 // which sides orientedPoint reverses
	mirrorsY := o == 3 || o == 4 || o == 6 || o == 7
	if mirrorsX && w%fx != 0 || mirrorsY && h%fy != 0 {
		return ratio, false
	}
	if o < 5 {
		return ratio, true
	}
	switch ratio {
	case image.YCbCrSubsampleRatio444, image.YCbCrSubsampleRatio420:
		return ratio, true
	case image.YCbCrSubsampleRatio422:
		return image.YCbCrSubsampleRatio440, true
	case image.YCbCrSubsampleRatio440:
		return image.YCbCrSubsampleRatio422, true
	}
	return ratio, false
}

// chromaSize is the size of each chroma plane of a w×h YCbCr image at the
// origin, as image.NewYCbCr lays it out.
func chromaSize(ratio image.YCbCrSubsampleRatio, w, h int) (int, int) {
	fx, fy := subsampling(ratio)
	if fx == 0 {
		return w, h
	}
	return (w + fx - 1) / fx, (h + fy - 1) / fy
}

// orientPlane copies the w×h plane src, with stride srcStride, into dst turned
// by orientation o.
func orientPlane(dst []byte, dstStride int, src []byte, srcStride, w, h, o int) {
	for y := 0; y < h; y++ {
		row := src[y*srcStride : y*srcStride+w]
		for x, v := range row {
			dx, dy := orientedPoint(o, x, y, w, h)
			dst[dy*dstStride+dx] = v
		}
	}
}

// orientPixels is orientPlane for pixels of n bytes each.
func orientPixels(dst []byte, dstStride int, src []byte, srcStride, w, h, n, o int) {
	if n == 1 {
		orientPlane(dst, dstStride, src, srcStride, w, h, o)
		return
	}
	for y := 0; y < h; y++ {
		row := src[y*srcStride : y*srcStride+n*w]
		for x := 0; x < w; x++ {
			dx, dy := orientedPoint(o, x, y, w, h)
			at := dy*dstStride + n*dx
			copy(dst[at:at+n], row[n*x:n*x+n])
		}
	}
}
