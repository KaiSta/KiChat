package images

import (
	"encoding/binary"
	"image"
)

// jpegOrientation returns the EXIF orientation of a JPEG, 1 when it has none or the
// header cannot be read.
//
// This is a deliberately small reader: one tag, one segment, no dependency. The full
// EXIF package would bring a parser for a format this app otherwise never touches, and
// the only question here is which way up the picture goes.
func jpegOrientation(data []byte) int {
	if len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 {
		return 1
	}
	for i := 2; i+4 <= len(data); {
		if data[i] != 0xff {
			i++
			continue
		}
		marker := data[i+1]
		// Standalone markers carry no length: SOI, TEM, RST0–RST7.
		if marker == 0xd8 || marker == 0x01 || (marker >= 0xd0 && marker <= 0xd7) {
			i += 2
			continue
		}
		// Start of scan and end of image: no metadata past this point.
		if marker == 0xda || marker == 0xd9 {
			break
		}
		if i+4 > len(data) {
			break
		}
		length := int(binary.BigEndian.Uint16(data[i+2 : i+4]))
		if length < 2 || i+2+length > len(data) {
			break
		}
		payload := data[i+4 : i+2+length]
		if marker == 0xe1 && len(payload) >= 6 && string(payload[:6]) == "Exif\x00\x00" {
			return tiffOrientation(payload[6:])
		}
		i += 2 + length
	}
	return 1
}

// tiffOrientation reads tag 0x0112 out of the first IFD.
func tiffOrientation(data []byte) int {
	if len(data) < 8 {
		return 1
	}
	var order binary.ByteOrder
	switch string(data[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 1
	}
	if order.Uint16(data[2:4]) != 42 {
		return 1
	}
	offset := int(order.Uint32(data[4:8]))
	if offset < 8 || offset+2 > len(data) {
		return 1
	}
	count := int(order.Uint16(data[offset : offset+2]))
	for i := 0; i < count; i++ {
		entry := offset + 2 + i*12
		if entry+12 > len(data) {
			return 1
		}
		if order.Uint16(data[entry:entry+2]) != 0x0112 {
			continue
		}
		// Type 3 is SHORT, and the value is inline for a count of one.
		if order.Uint16(data[entry+2:entry+4]) != 3 || order.Uint32(data[entry+4:entry+8]) < 1 {
			return 1
		}
		value := int(order.Uint16(data[entry+8 : entry+10]))
		if value >= 1 && value <= 8 {
			return value
		}
		return 1
	}
	return 1
}

// orient applies the eight EXIF orientations. The mappings are the inverse of how the
// camera wrote them: for each destination pixel, find the source pixel that belongs
// there. Orientations 5–8 swap the axes, which is why the destination size is computed
// before the loop.
func orient(src image.Image, orientation int) image.Image {
	if orientation < 2 || orientation > 8 {
		return src
	}
	rgba := toRGBA(src)
	srcW, srcH := rgba.Bounds().Dx(), rgba.Bounds().Dy()
	dstW, dstH := srcW, srcH
	if orientation >= 5 {
		dstW, dstH = srcH, srcW
	}
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	for y := 0; y < dstH; y++ {
		for x := 0; x < dstW; x++ {
			var sx, sy int
			switch orientation {
			case 2: // mirror horizontally
				sx, sy = srcW-1-x, y
			case 3: // rotate 180°
				sx, sy = srcW-1-x, srcH-1-y
			case 4: // mirror vertically
				sx, sy = x, srcH-1-y
			case 5: // transpose
				sx, sy = y, x
			case 6: // rotate 90° clockwise
				sx, sy = y, srcH-1-x
			case 7: // transverse
				sx, sy = srcW-1-y, srcH-1-x
			case 8: // rotate 270° clockwise
				sx, sy = srcW-1-y, x
			}
			from := sy*rgba.Stride + sx*4
			to := y*dst.Stride + x*4
			copy(dst.Pix[to:to+4], rgba.Pix[from:from+4])
		}
	}
	return dst
}
