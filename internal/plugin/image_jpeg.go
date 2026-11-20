package plugin

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/color"
)

// Follow JPEG markers through all scans to require a single complete image.
// Otherwise the still decoder can silently drop appended frames or video.
func stillJPEGOrientation(ctx context.Context, data []byte) (int, bool) {
	if len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 {
		return 0, false
	}
	orientation := 1
	var inScan, sawFrame, sawScan, sawExif bool
	for offset := 2; offset < len(data); {
		if ctx.Err() != nil {
			return 0, false
		}
		if inScan {
			next := bytes.IndexByte(data[offset:], 0xff)
			if next < 0 {
				return 0, false
			}
			offset += next
		} else if data[offset] != 0xff {
			return 0, false
		}
		for offset < len(data) && data[offset] == 0xff {
			offset++
		}
		if offset == len(data) {
			return 0, false
		}
		marker := data[offset]
		offset++
		switch {
		case marker == 0xd9:
			return orientation, sawScan && offset == len(data)
		case marker == 0xd8:
			return 0, false
		case marker == 0 || (marker >= 0xd0 && marker <= 0xd7):
			if !inScan {
				return 0, false
			}
			continue
		case marker == 0x01:
			continue // Standalone TEM marker.
		}
		if len(data)-offset < 2 {
			return 0, false
		}
		length := int(binary.BigEndian.Uint16(data[offset:]))
		if length < 2 || length > len(data)-offset {
			return 0, false
		}
		segment := data[offset+2 : offset+length]
		switch {
		case marker >= 0xc0 && marker <= 0xcf && marker != 0xc4 && marker != 0xc8 && marker != 0xcc:
			// Only one start of frame is allowed before the first scan.
			if sawFrame || sawScan {
				return 0, false
			}
			sawFrame = true
		case marker == 0xda && !sawFrame:
			return 0, false
		case marker == 0xe2 && bytes.HasPrefix(segment, []byte("MPF\x00")):
			return 0, false
		case marker == 0xe1 && bytes.HasPrefix(segment, []byte("Exif\x00\x00")):
			if sawExif {
				return 0, false
			}
			sawExif = true
			orientation = exifOrientation(segment[6:])
			if orientation == 0 {
				return 0, false
			}
		}
		offset += length
		inScan = marker == 0xda
		sawScan = sawScan || inScan
	}
	return 0, false
}

// The TIFF IFD0 orientation is a single inline SHORT. Zero signals that
// EXIF was malformed or the orientation could not safely be interpreted.
func exifOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 0
	}
	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 0
	}
	offset := int(order.Uint32(tiff[4:8]))
	if order.Uint16(tiff[2:4]) != 42 || offset < 8 || offset > len(tiff)-2 {
		return 0
	}
	entries := int(order.Uint16(tiff[offset:]))
	table := tiff[offset+2:]
	if entries > len(table)/12 {
		return 0
	}
	for i := range entries {
		entry := table[i*12 : i*12+12]
		if order.Uint16(entry) != 0x0112 {
			continue
		}
		value := int(order.Uint16(entry[8:]))
		if order.Uint16(entry[2:]) != 3 || order.Uint32(entry[4:]) != 1 || value < 1 || value > 8 {
			return 0
		}
		return value
	}
	return 1
}

// Map pixels to the displayed orientation without making a second full-size
// copy. The output JPEG has the orientation applied directly to its pixels.
type orientedJPEG struct {
	image.Image
	orientation int
}

func (j orientedJPEG) Bounds() image.Rectangle {
	b := j.Image.Bounds()
	if j.orientation >= 5 {
		return image.Rect(0, 0, b.Dy(), b.Dx())
	}
	return image.Rect(0, 0, b.Dx(), b.Dy())
}

func (j orientedJPEG) At(x, y int) color.Color {
	b := j.Image.Bounds()
	w, h := b.Dx(), b.Dy()
	switch j.orientation {
	case 2:
		x = w - 1 - x
	case 3:
		x, y = w-1-x, h-1-y
	case 4:
		y = h - 1 - y
	case 5:
		x, y = y, x
	case 6:
		x, y = y, h-1-x
	case 7:
		x, y = w-1-y, h-1-x
	case 8:
		x, y = w-1-y, x
	}
	return j.Image.At(x+b.Min.X, y+b.Min.Y)
}
