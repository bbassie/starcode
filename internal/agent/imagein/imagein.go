// Package imagein gets an attached image ready to go to a model with the
// prompt. The model APIs take JPEG, PNG, GIF and WebP up to about 5 MB,
// and scale anything over 1568 pixels on its long side down anyway; a
// phone screenshot is often over both. A bigger image is scaled down here
// and sent as JPEG.
package imagein

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"net/http"
	"os"
)

// MaxSide is the longest side an image keeps.
const MaxSide = 1568

// MaxBytes is the largest file sent as it is: 5 MB of base64.
const MaxBytes = 3_750_000

// Supported is whether a model takes images of this media type.
func Supported(mime string) bool {
	switch mime {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	}
	return false
}

// Sniff is the media type of the file at path, from its first bytes.
func Sniff(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := f.Read(head)
	return http.DetectContentType(head[:n]), nil
}

// Load reads the image at path as a model takes it: its media type and
// bytes, scaled down when it is too big.
func Load(path string) (string, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil, err
	}
	mime := http.DetectContentType(data)
	if !Supported(mime) {
		return "", nil, fmt.Errorf("%s: %s is not an image type the models take", path, mime)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		// WebP has no decoder in the standard library: send it as it is
		// when it is small enough.
		if mime == "image/webp" && len(data) <= MaxBytes {
			return mime, data, nil
		}
		return "", nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(data) <= MaxBytes && max(cfg.Width, cfg.Height) <= MaxSide {
		return mime, data, nil
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", nil, fmt.Errorf("%s: %w", path, err)
	}
	out, err := shrink(img, MaxSide)
	if err != nil {
		return "", nil, err
	}
	return "image/jpeg", out, nil
}

// shrink scales img to fit side (an average over each target pixel's
// source area, which keeps text in screenshots legible) and encodes it
// as JPEG on white, since JPEG has no transparency.
func shrink(img image.Image, side int) ([]byte, error) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w == 0 || h == 0 {
		return nil, errors.New("empty image")
	}
	scale := min(1, float64(side)/float64(max(w, h)))
	tw, th := max(1, int(float64(w)*scale+0.5)), max(1, int(float64(h)*scale+0.5))
	src := image.NewRGBA(b)
	draw.Draw(src, b, image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(src, b, img, b.Min, draw.Over)
	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	for y := 0; y < th; y++ {
		y0, y1 := y*h/th, max((y+1)*h/th, y*h/th+1)
		for x := 0; x < tw; x++ {
			x0, x1 := x*w/tw, max((x+1)*w/tw, x*w/tw+1)
			var r, g, bl, n uint32
			for sy := y0; sy < y1; sy++ {
				row := src.Pix[sy*src.Stride:]
				for sx := x0; sx < x1; sx++ {
					p := row[sx*4 : sx*4+3]
					r += uint32(p[0])
					g += uint32(p[1])
					bl += uint32(p[2])
					n++
				}
			}
			o := dst.PixOffset(x, y)
			dst.Pix[o], dst.Pix[o+1], dst.Pix[o+2], dst.Pix[o+3] = uint8(r/n), uint8(g/n), uint8(bl/n), 255
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
