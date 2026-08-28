package images

import (
	"bytes"
	"encoding/binary"
	"image"
)

// JPEGOrientation reads the TIFF orientation value from a JPEG APP1
// segment. Missing or malformed metadata is treated as the normal orientation.
func JPEGOrientation(data []byte) int {
	if len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 {
		return 1
	}
	for pos := 2; pos < len(data); {
		if data[pos] != 0xff {
			return 1
		}
		for pos < len(data) && data[pos] == 0xff {
			pos++
		}
		if pos >= len(data) {
			return 1
		}
		marker := data[pos]
		pos++
		if marker == 0xd9 || marker == 0xda {
			return 1
		}
		if marker == 0x01 || marker >= 0xd0 && marker <= 0xd7 {
			continue
		}
		if pos+2 > len(data) {
			return 1
		}
		length := int(binary.BigEndian.Uint16(data[pos : pos+2]))
		if length < 2 || pos+length > len(data) {
			return 1
		}
		if marker == 0xe1 {
			if orientation := tiffEXIFOrientation(data[pos+2 : pos+length]); orientation != 1 {
				return orientation
			}
		}
		pos += length
	}
	return 1
}

func tiffEXIFOrientation(payload []byte) int {
	if len(payload) < 14 || !bytes.Equal(payload[:6], []byte{'E', 'x', 'i', 'f', 0, 0}) {
		return 1
	}
	tiff := payload[6:]
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
	ifdOffset := int(order.Uint32(tiff[4:8]))
	if ifdOffset < 0 || ifdOffset+2 > len(tiff) {
		return 1
	}
	entryCount := int(order.Uint16(tiff[ifdOffset : ifdOffset+2]))
	entries := ifdOffset + 2
	for i := 0; i < entryCount; i++ {
		entry := entries + i*12
		if entry+12 > len(tiff) {
			return 1
		}
		if order.Uint16(tiff[entry:entry+2]) != 0x0112 || order.Uint16(tiff[entry+2:entry+4]) != 3 || order.Uint32(tiff[entry+4:entry+8]) < 1 {
			continue
		}
		orientation := int(order.Uint16(tiff[entry+8 : entry+10]))
		if orientation >= 1 && orientation <= 8 {
			return orientation
		}
		return 1
	}
	return 1
}

// ApplyEXIFOrientation rotates or flips src so the stored EXIF orientation
// renders upright. Orientation values outside 2-8 return src unchanged.
func ApplyEXIFOrientation(src image.Image, orientation int) image.Image {
	if src == nil || orientation <= 1 || orientation > 8 {
		return src
	}
	bounds := src.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	dstWidth, dstHeight := width, height
	if orientation >= 5 {
		dstWidth, dstHeight = height, width
	}
	if nrgba, ok := src.(*image.NRGBA); ok {
		return applyEXIFOrientationNRGBA(nrgba, orientation, dstWidth, dstHeight)
	}
	dst := image.NewNRGBA(image.Rect(0, 0, dstWidth, dstHeight))
	for y := 0; y < dstHeight; y++ {
		for x := 0; x < dstWidth; x++ {
			srcX, srcY := exifSourcePoint(orientation, width, height, x, y)
			dst.Set(x, y, src.At(bounds.Min.X+srcX, bounds.Min.Y+srcY))
		}
	}
	return dst
}

func applyEXIFOrientationNRGBA(src *image.NRGBA, orientation, dstWidth, dstHeight int) *image.NRGBA {
	bounds := src.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	dst := image.NewNRGBA(image.Rect(0, 0, dstWidth, dstHeight))
	for y := 0; y < dstHeight; y++ {
		for x := 0; x < dstWidth; x++ {
			srcX, srcY := exifSourcePoint(orientation, width, height, x, y)
			srcOffset := src.PixOffset(bounds.Min.X+srcX, bounds.Min.Y+srcY)
			dstOffset := dst.PixOffset(x, y)
			copy(dst.Pix[dstOffset:dstOffset+4], src.Pix[srcOffset:srcOffset+4])
		}
	}
	return dst
}

func exifSourcePoint(orientation, width, height, x, y int) (int, int) {
	switch orientation {
	case 2:
		return width - 1 - x, y
	case 3:
		return width - 1 - x, height - 1 - y
	case 4:
		return x, height - 1 - y
	case 5:
		return y, x
	case 6:
		return y, height - 1 - x
	case 7:
		return width - 1 - y, height - 1 - x
	case 8:
		return width - 1 - y, x
	default:
		return x, y
	}
}
