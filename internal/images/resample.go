package images

import (
	"image"
	"image/draw"
	"math"
)

// resampleTo converts the picture to a premultiplied RGBA buffer and resamples it to
// exactly the target size.
//
// Premultiplied is not an implementation detail: resampling straight alpha darkens the
// edge of a transparent PNG, because the colour stored under a transparent pixel is
// usually black and the kernel happily mixes it in. Go's image.RGBA is premultiplied by
// definition, so converting first is the whole fix.
func resampleTo(src image.Image, target Target) *image.RGBA {
	rgba := toRGBA(src)
	bounds := rgba.Bounds()
	if bounds.Dx() == target.Width && bounds.Dy() == target.Height {
		return rgba
	}
	return resampleRGBA(rgba, target.Width, target.Height)
}

// toRGBA returns a zero-origin premultiplied copy of src.
func toRGBA(src image.Image) *image.RGBA {
	if rgba, ok := src.(*image.RGBA); ok && rgba.Bounds().Min == (image.Point{}) {
		return rgba
	}
	bounds := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(dst, dst.Bounds(), src, bounds.Min, draw.Src)
	return dst
}

// contribution is one destination pixel's source support: the indices it reads and the
// normalised weights it reads them with.
type contribution struct {
	index  []int32
	weight []float32
}

// lanczos3 is the windowed sinc this resampler is named after. The normalisation is per
// destination pixel rather than per kernel, which is what keeps an edge from darkening:
// the taps that fall outside the picture are dropped and the rest are rescaled.
func lanczos3(x float64) float64 {
	if x == 0 {
		return 1
	}
	if math.Abs(x) >= 3 {
		return 0
	}
	px := math.Pi * x
	return 3 * math.Sin(px) * math.Sin(px/3) / (px * px)
}

// lanczosWeights precomputes one axis.
//
// Downscaling widens the kernel by the scale factor, which is what stops a 3000-pixel
// photograph from aliasing into a 1920-pixel one; upscaling keeps the three-tap window
// the kernel is defined with.
func lanczosWeights(dst, src int) []contribution {
	out := make([]contribution, dst)
	if src <= 0 || dst <= 0 {
		return out
	}
	scale := float64(src) / float64(dst)
	filterScale := math.Max(scale, 1)
	support := 3 * filterScale
	for i := range out {
		center := (float64(i)+0.5)*scale - 0.5
		lo := int(math.Ceil(center - support))
		hi := int(math.Floor(center + support))
		if lo < 0 {
			lo = 0
		}
		if hi > src-1 {
			hi = src - 1
		}
		index := make([]int32, 0, hi-lo+1)
		weight := make([]float32, 0, hi-lo+1)
		var sum float64
		for j := lo; j <= hi; j++ {
			w := lanczos3((center - float64(j)) / filterScale)
			if w == 0 {
				continue
			}
			index = append(index, int32(j))
			weight = append(weight, float32(w))
			sum += w
		}
		if sum != 0 {
			for k := range weight {
				weight[k] = float32(float64(weight[k]) / sum)
			}
		}
		out[i] = contribution{index: index, weight: weight}
	}
	return out
}

// resampleRGBA runs the separable filter: horizontal into a full-width, source-height
// buffer, then vertical out of it. The intermediate is one extra picture-sized
// allocation, which is the price of not transposing the image twice.
func resampleRGBA(src *image.RGBA, width, height int) *image.RGBA {
	srcW, srcH := src.Bounds().Dx(), src.Bounds().Dy()
	if srcW <= 0 || srcH <= 0 || width <= 0 || height <= 0 {
		return image.NewRGBA(image.Rect(0, 0, max(1, width), max(1, height)))
	}
	horizontal := lanczosWeights(width, srcW)
	middle := image.NewRGBA(image.Rect(0, 0, width, srcH))
	for y := 0; y < srcH; y++ {
		row := src.Pix[y*src.Stride:]
		out := middle.Pix[y*middle.Stride:]
		for x := 0; x < width; x++ {
			var r, g, b, a float64
			taps := horizontal[x]
			for k, index := range taps.index {
				w := float64(taps.weight[k])
				at := int(index) * 4
				r += float64(row[at]) * w
				g += float64(row[at+1]) * w
				b += float64(row[at+2]) * w
				a += float64(row[at+3]) * w
			}
			at := x * 4
			out[at] = clampByte(r)
			out[at+1] = clampByte(g)
			out[at+2] = clampByte(b)
			out[at+3] = clampByte(a)
		}
	}

	vertical := lanczosWeights(height, srcH)
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		taps := vertical[y]
		out := dst.Pix[y*dst.Stride:]
		for x := 0; x < width; x++ {
			var r, g, b, a float64
			at := x * 4
			for k, index := range taps.index {
				w := float64(taps.weight[k])
				from := int(index)*middle.Stride + at
				r += float64(middle.Pix[from]) * w
				g += float64(middle.Pix[from+1]) * w
				b += float64(middle.Pix[from+2]) * w
				a += float64(middle.Pix[from+3]) * w
			}
			out[at] = clampByte(r)
			out[at+1] = clampByte(g)
			out[at+2] = clampByte(b)
			out[at+3] = clampByte(a)
		}
	}
	return dst
}

// unsharp is a small cross-shaped blur subtracted from the picture again. A radius of
// about one pixel is the right size for the softness Lanczos leaves behind; anything
// wider is a different effect with visible halos.
func unsharp(img *image.RGBA, amount float64) {
	if amount <= 0 || img == nil {
		return
	}
	width, height := img.Bounds().Dx(), img.Bounds().Dy()
	if width < 3 || height < 3 {
		return
	}
	// A copy of the pixels rather than a rolling row: the vertical neighbours are read
	// before they are written, and a single buffer would let a sharpened pixel become
	// its neighbour's input.
	source := append([]byte(nil), img.Pix...)
	for y := 1; y < height-1; y++ {
		for x := 1; x < width-1; x++ {
			at := y*img.Stride + x*4
			for c := 0; c < 3; c++ {
				center := float64(source[at+c])
				blur := (4*center +
					float64(source[at-img.Stride+c]) + float64(source[at+img.Stride+c]) +
					float64(source[at-4+c]) + float64(source[at+4+c])) / 8
				img.Pix[at+c] = clampByte(center + amount*(center-blur))
			}
		}
	}
}

func clampByte(v float64) uint8 {
	switch {
	case v <= 0:
		return 0
	case v >= 255:
		return 255
	default:
		return uint8(v + 0.5)
	}
}
