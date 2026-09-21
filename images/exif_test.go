package images

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"strings"
	"testing"
)

// jpegWithEXIFOrientation splices an APP1 segment carrying only the TIFF
// orientation tag into jpegData.
func jpegWithEXIFOrientation(t *testing.T, jpegData []byte, orientation uint16) []byte {
	t.Helper()
	if len(jpegData) < 2 || jpegData[0] != 0xff || jpegData[1] != 0xd8 {
		t.Fatal("fixture is not a JPEG")
	}
	var tiff bytes.Buffer
	tiff.WriteString("II")
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(42))
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(8))
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(1))
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(0x0112))
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(3))
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(1))
	_ = binary.Write(&tiff, binary.LittleEndian, orientation)
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(0))
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(0))
	payload := append([]byte{'E', 'x', 'i', 'f', 0, 0}, tiff.Bytes()...)
	if len(payload)+2 > 0xffff {
		t.Fatal("EXIF fixture is too large")
	}
	segment := []byte{0xff, 0xe1, 0, 0}
	binary.BigEndian.PutUint16(segment[2:], uint16(len(payload)+2))
	segment = append(segment, payload...)
	out := make([]byte, 0, len(jpegData)+len(segment))
	out = append(out, jpegData[:2]...)
	out = append(out, segment...)
	out = append(out, jpegData[2:]...)
	return out
}

func TestJPEGOrientation(t *testing.T) {
	var plain bytes.Buffer
	if err := jpeg.Encode(&plain, testImage(4, 3), nil); err != nil {
		t.Fatal(err)
	}
	if got := JPEGOrientation(plain.Bytes()); got != 1 {
		t.Fatalf("plain JPEG orientation = %d, want 1", got)
	}
	if got := JPEGOrientation(jpegWithEXIFOrientation(t, plain.Bytes(), 6)); got != 6 {
		t.Fatalf("tagged JPEG orientation = %d, want 6", got)
	}
	if got := JPEGOrientation(jpegWithEXIFOrientation(t, plain.Bytes(), 9)); got != 1 {
		t.Fatalf("out-of-range tag orientation = %d, want 1", got)
	}
	if got := JPEGOrientation([]byte("not a jpeg")); got != 1 {
		t.Fatalf("garbage orientation = %d, want 1", got)
	}
}

// Each EXIF orientation maps a 3x2 source to a known layout, whether the
// source is already NRGBA or must be converted first.
func TestApplyEXIFOrientationLayouts(t *testing.T) {
	const w, h = 3, 2
	letters := "abcdef"
	shade := func(letter byte) color.NRGBA {
		return color.NRGBA{R: letter, G: letter, B: letter, A: 255}
	}
	nrgba := image.NewNRGBA(image.Rect(2, 3, 2+w, 3+h))
	rgba := image.NewRGBA(image.Rect(0, 0, w, h))
	for i, letter := range []byte(letters) {
		x, y := i%w, i/w
		nrgba.SetNRGBA(2+x, 3+y, shade(letter))
		rgba.Set(x, y, shade(letter))
	}
	layouts := map[int][]string{
		1: {"abc", "def"},
		2: {"cba", "fed"},
		3: {"fed", "cba"},
		4: {"def", "abc"},
		5: {"ad", "be", "cf"},
		6: {"da", "eb", "fc"},
		7: {"fc", "eb", "da"},
		8: {"cf", "be", "ad"},
	}
	for orientation, rows := range layouts {
		for name, src := range map[string]image.Image{"nrgba": nrgba, "rgba": rgba} {
			got := ApplyEXIFOrientation(src, orientation)
			var b strings.Builder
			bounds := got.Bounds()
			for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
				for x := bounds.Min.X; x < bounds.Max.X; x++ {
					b.WriteByte(color.NRGBAModel.Convert(got.At(x, y)).(color.NRGBA).R)
				}
				b.WriteByte('/')
			}
			if want := strings.Join(rows, "/") + "/"; b.String() != want {
				t.Fatalf("orientation %d on %s = %q, want %q", orientation, name, b.String(), want)
			}
		}
	}
}

// A rotated JPEG within the upload caps ships byte-for-byte with its EXIF
// intact; one that must be re-encoded is baked upright, since the re-encode
// drops the metadata.
func TestNormalizeForModelAppliesEXIFOrientationWhenResizing(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 1600, 800))
	for y := 0; y < 800; y++ {
		for x := 0; x < 1600; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x / 8), G: uint8(y / 4), B: 120, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	oriented := jpegWithEXIFOrientation(t, encoded.Bytes(), 6)

	norm, err := NormalizeForModel(oriented, "portrait.jpg")
	if err != nil {
		t.Fatal(err)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(norm.Data))
	if err != nil {
		t.Fatal(err)
	}
	if format != "jpeg" || norm.MIMEType != "image/jpeg" || config.Width != 784 || config.Height != 1568 {
		t.Fatalf("oriented resize = %s %s %dx%d, want JPEG 784x1568", format, norm.MIMEType, config.Width, config.Height)
	}
	if norm.Width != 784 || norm.Height != 1568 {
		t.Fatalf("reported %dx%d, want 784x1568", norm.Width, norm.Height)
	}
	if got := JPEGOrientation(norm.Data); got != 1 {
		t.Fatalf("resized JPEG retained stale EXIF orientation %d", got)
	}

	var small bytes.Buffer
	if err := jpeg.Encode(&small, testImage(40, 30), nil); err != nil {
		t.Fatal(err)
	}
	tagged := jpegWithEXIFOrientation(t, small.Bytes(), 6)
	norm, err = NormalizeForModel(tagged, "small.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(norm.Data, tagged) || JPEGOrientation(norm.Data) != 6 {
		t.Fatal("small tagged JPEG should pass through byte-for-byte with its EXIF intact")
	}
}
