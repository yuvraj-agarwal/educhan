package media

import (
	"encoding/binary"
	"image"
)

// EXIF Orientation, as far as educhan reads it.
//
// A phone stores a photo the way its sensor was facing and writes a tag saying
// how to turn it upright. Browsers obey the tag, so a stored JPEG that lost the
// tag and kept its pixels would show sideways. Stripping metadata therefore
// means applying the tag to the pixels first, which is what orient does, and
// the stored file, its recorded dimensions and its thumbnail are all of the
// upright picture.
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
// The upload is hostile until proven otherwise, so the reader works on the
// bytes in place: every offset is checked against the segment before it is
// used, nothing is allocated, and the only offset followed is the one to IFD0,
// where the tag lives. With no chain of IFDs walked there is no loop to be
// caught in, however the offsets point.
func jpegOrientation(data []byte) int {
	if len(data) < 2 || data[0] != 0xFF || data[1] != 0xD8 {
		return 1
	}
	i := 2
	for i+4 <= len(data) {
		if data[i] != 0xFF {
			return 1
		}
		marker := data[i+1]
		switch {
		case marker == 0xFF:
			// A fill byte before the marker proper.
			i++
			continue
		case marker == 0x01 || (marker >= 0xD0 && marker <= 0xD8):
			// TEM, RSTn and SOI stand alone, with no length.
			i += 2
			continue
		case marker == 0xDA || marker == 0xD9:
			// Start of scan or end of image: the metadata segments are behind us.
			return 1
		}
		length := int(data[i+2])<<8 | int(data[i+3])
		if length < 2 || length > len(data)-i-2 {
			return 1
		}
		segment := data[i+4 : i+2+length]
		if marker == 0xE1 && len(segment) >= 6 && string(segment[:5]) == "Exif\x00" {
			return tiffOrientation(segment[6:])
		}
		i += 2 + length
	}
	return 1
}

// tiffOrientation reads the Orientation entry of IFD0 in a TIFF structure, the
// body of an Exif segment, under the same rules as jpegOrientation.
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
	count := int(order.Uint16(tiff[offset : offset+2]))
	if count > len(entries)/12 {
		// An IFD claiming more entries than the segment holds is not one we
		// can trust any part of.
		return 1
	}
	for k := 0; k < count; k++ {
		entry := entries[12*k : 12*k+12]
		if order.Uint16(entry[0:2]) != exifOrientationTag {
			continue
		}
		if order.Uint16(entry[2:4]) != tiffTypeShort || order.Uint32(entry[4:8]) != 1 {
			return 1
		}
		if v := int(order.Uint16(entry[8:10])); v >= 1 && v <= 8 {
			return v
		}
		return 1
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
// itself when o is 1 or unknown.
//
// What a JPEG decodes to is almost always a YCbCr image, so that is turned
// plane by plane into another YCbCr image, keeping its chroma subsampling (with
// the factors swapped when the picture turns on its side). That costs the
// decoded size again, 1.5 bytes a pixel for an ordinary 4:2:0 photo, where an
// RGBA copy would cost 4, and hands the encoder the same planes it would have
// computed. Grayscale is one plane. Anything else, CMYK say, goes pixel by
// pixel into RGBA.
func orient(src image.Image, o int) image.Image {
	if o < 2 || o > 8 {
		return src
	}
	switch s := src.(type) {
	case *image.YCbCr:
		if dst, ok := orientYCbCr(s, o); ok {
			return dst
		}
	case *image.Gray:
		if s.Rect.Min == (image.Point{}) {
			w, h := s.Rect.Dx(), s.Rect.Dy()
			dst := image.NewGray(orientedRect(w, h, o))
			orientPlane(dst.Pix, dst.Stride, s.Pix, s.Stride, w, h, o)
			return dst
		}
	}

	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dst := image.NewRGBA(orientedRect(w, h, o))
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

// orientYCbCr turns each plane of src on its own. It declines, leaving the
// caller to fall back to RGBA, for an image that does not start at the origin
// and for a subsampling with no counterpart once turned on its side (4:1:1 and
// 4:1:0 have none in the image package).
func orientYCbCr(src *image.YCbCr, o int) (*image.YCbCr, bool) {
	if src.Rect.Min != (image.Point{}) {
		return nil, false
	}
	ratio := src.SubsampleRatio
	switch ratio {
	case image.YCbCrSubsampleRatio444, image.YCbCrSubsampleRatio420:
	case image.YCbCrSubsampleRatio422:
		if o >= 5 {
			ratio = image.YCbCrSubsampleRatio440
		}
	case image.YCbCrSubsampleRatio440:
		if o >= 5 {
			ratio = image.YCbCrSubsampleRatio422
		}
	default:
		if o >= 5 {
			return nil, false
		}
	}
	w, h := src.Rect.Dx(), src.Rect.Dy()
	cw, ch := chromaSize(src.SubsampleRatio, w, h)
	dst := image.NewYCbCr(orientedRect(w, h, o), ratio)
	orientPlane(dst.Y, dst.YStride, src.Y, src.YStride, w, h, o)
	orientPlane(dst.Cb, dst.CStride, src.Cb, src.CStride, cw, ch, o)
	orientPlane(dst.Cr, dst.CStride, src.Cr, src.CStride, cw, ch, o)
	return dst, true
}

// chromaSize is the size of each chroma plane of a w×h YCbCr image at the
// origin, as image.NewYCbCr lays it out.
func chromaSize(ratio image.YCbCrSubsampleRatio, w, h int) (int, int) {
	switch ratio {
	case image.YCbCrSubsampleRatio422:
		return (w + 1) / 2, h
	case image.YCbCrSubsampleRatio420:
		return (w + 1) / 2, (h + 1) / 2
	case image.YCbCrSubsampleRatio440:
		return w, (h + 1) / 2
	case image.YCbCrSubsampleRatio411:
		return (w + 3) / 4, h
	case image.YCbCrSubsampleRatio410:
		return (w + 3) / 4, (h + 1) / 2
	}
	return w, h
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
