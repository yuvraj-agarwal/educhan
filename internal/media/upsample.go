package media

import "image"

// Chroma upsampling, as the decoders browsers use do it.
//
// A lossy WebP, and most JPEGs, carry their colour at half resolution, and a
// browser does not show each chroma sample as a block of the pixels it
// covers. libwebp's fancy upsampler, and libjpeg-turbo's with
// do_fancy_upsampling on (its default, and Chromium's), give each pixel a
// blend of the samples nearest it: 3/4 of the nearest and 1/4 of the next
// nearest along each halved axis, so 9/16, 3/16, 3/16 and 1/16 of four
// samples when both are halved, holding to the last sample at the edges.
// Where educhan widens chroma to a sample a pixel itself — a lossy WebP with
// transparency, stored as RGBA (lossyWebPToNRGBA), and a JPEG turned on a
// side its chroma does not divide, stored at 4:4:4 (orientYCbCr) — it
// upsamples the same way, to the decoder's own rounding, so that the stored
// picture's colour edges are the ones its poster saw rather than 2×2 blocks
// of the nearest sample, which at a sharp edge are a hundred levels and more
// off what the browser showed (TestLossyWebPIsUpsampledAsLibwebpDoes,
// TestOrientTurnsEverySubsamplingAsShown).

// chromaPair returns, for coordinate v of a pixel on an axis whose chroma is
// halved, the plane index of the chroma sample nearest it and of the next
// nearest, which is the one before for an even v and the one after for an
// odd one, held to the samples there are for pixels lo to hi-1: the
// picture's bounds on that axis. It counts samples as image.YCbCr's COffset
// does.
func chromaPair(v, lo, hi int) (near, far int) {
	first, last := lo/2, (hi-1)/2
	n, f := v/2, v/2-1
	if v&1 == 1 {
		f = v/2 + 1
	}
	f = max(first, min(f, last))
	return n - first, f - first
}

// webpChromaRow writes the Cb and Cr of row y of p, a lossy WebP's 4:2:0
// picture, one sample a pixel, into cb and cr: the four samples nearest each
// pixel blended exactly as libwebp's fancy upsampler blends them
// (UpsampleRgbaLinePair, run by EmitFancyRGB, which pairs the first row with
// itself and holds to the last sample at each edge).
func webpChromaRow(cb, cr []uint8, p *image.YCbCr, y int) {
	b := p.Rect
	rn, rf := chromaPair(y, b.Min.Y, b.Max.Y)
	for i, plane := range [2][]uint8{p.Cb, p.Cr} {
		dst := cb
		if i == 1 {
			dst = cr
		}
		near, far := plane[rn*p.CStride:], plane[rf*p.CStride:]
		for x := b.Min.X; x < b.Max.X; x++ {
			cn, cf := chromaPair(x, b.Min.X, b.Max.X)
			nn, nf, fn, ff := int(near[cn]), int(near[cf]), int(far[cn]), int(far[cf])
			dst[x-b.Min.X] = uint8((((nn + 3*nf + 3*fn + ff + 8) >> 3) + nn) >> 1)
		}
	}
}

// jpegChromaRow writes the Cb and Cr of row y of src, a decoded JPEG, one
// sample a pixel, into cb and cr, as libjpeg-turbo upsamples them with
// do_fancy_upsampling on (jdsample.c): halved across and down (4:2:0), 9/16
// of the nearest sample, 3/16 of each next nearest and 1/16 of the one
// diagonally, rounding alternately up and down (h2v2_fancy_upsample); halved
// across only (4:2:2) or down only (4:4:0), 3/4 of the nearest and 1/4 of the
// next (h2v1_fancy_upsample, h1v2_fancy_upsample). A picture whose chroma is
// two samples wide or less across a halved width, and 4:1:1 and 4:1:0, which
// it upsamples by other factors, it gives each pixel the sample it lies in,
// as libjpeg-turbo does (h2v1_upsample, h2v2_upsample, int_upsample).
func jpegChromaRow(cb, cr []uint8, src *image.YCbCr, y int) {
	b := src.Rect
	fx, fy := subsampling(src.SubsampleRatio)
	if fx == 2 && (b.Max.X-1)/2-b.Min.X/2 < 2 {
		fx = 0 // two samples across or fewer: no blending, as for 4:1:1
	}
	rn, rf := chromaPair(y, b.Min.Y, b.Max.Y)
	for i, plane := range [2][]uint8{src.Cb, src.Cr} {
		dst := cb
		if i == 1 {
			dst = cr
		}
		switch {
		case fx == 2 && fy == 2:
			near, far := plane[rn*src.CStride:], plane[rf*src.CStride:]
			for x := b.Min.X; x < b.Max.X; x++ {
				cn, cf := chromaPair(x, b.Min.X, b.Max.X)
				nn, nf, fn, ff := int(near[cn]), int(near[cf]), int(far[cn]), int(far[cf])
				dst[x-b.Min.X] = uint8((9*nn + 3*fn + 3*nf + ff + 8 - x&1) >> 4)
			}
		case fx == 2 && fy == 1:
			row := plane[(y-b.Min.Y)*src.CStride:]
			for x := b.Min.X; x < b.Max.X; x++ {
				cn, cf := chromaPair(x, b.Min.X, b.Max.X)
				dst[x-b.Min.X] = uint8((3*int(row[cn]) + int(row[cf]) + 1 + x&1) >> 2)
			}
		case fx == 1 && fy == 2:
			near, far := plane[rn*src.CStride:], plane[rf*src.CStride:]
			for c := range dst[:b.Dx()] {
				dst[c] = uint8((3*int(near[c]) + int(far[c]) + 1 + y&1) >> 2)
			}
		default:
			for x := b.Min.X; x < b.Max.X; x++ {
				dst[x-b.Min.X] = plane[src.COffset(x, y)]
			}
		}
	}
}
