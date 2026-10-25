package imagein

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, w, h int) string {
		img := image.NewNRGBA(image.Rect(0, 0, w, h))
		for i := range img.Pix {
			img.Pix[i] = byte(i * 7)
		}
		img.Set(0, 0, color.Transparent)
		var b bytes.Buffer
		png.Encode(&b, img)
		p := filepath.Join(dir, name)
		os.WriteFile(p, b.Bytes(), 0o644)
		return p
	}
	mime, data, err := Load(write("small.png", 100, 50))
	if err != nil || mime != "image/png" {
		t.Fatalf("small: %s %v", mime, err)
	}
	if cfg, _, _ := image.DecodeConfig(bytes.NewReader(data)); cfg.Width != 100 {
		t.Fatalf("small changed: %+v", cfg)
	}
	mime, data, err = Load(write("big.png", 3000, 1000))
	if err != nil || mime != "image/jpeg" {
		t.Fatalf("big: %s %v", mime, err)
	}
	if cfg, _, _ := image.DecodeConfig(bytes.NewReader(data)); cfg.Width != MaxSide || cfg.Height != 523 {
		t.Fatalf("big scaled to %+v", cfg)
	}
	os.WriteFile(filepath.Join(dir, "x.txt"), []byte("hello"), 0o644)
	if _, _, err := Load(filepath.Join(dir, "x.txt")); err == nil {
		t.Fatal("text loaded as an image")
	}
}
