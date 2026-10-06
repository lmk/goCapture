package main

import (
	"encoding/binary"
	"image"
	"image/color"
	"testing"
)

func TestNormalizeMethod(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"gdi", "gdi"},
		{" GDI ", "gdi"},
		{"prtsc", "prtsc"},
		{"PrintScreen", "prtsc"},
	}
	for _, tc := range cases {
		got, err := normalizeMethod(tc.in)
		if err != nil {
			t.Fatalf("normalizeMethod(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("normalizeMethod(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if _, err := normalizeMethod("dxgi"); err == nil {
		t.Fatal("expected error for unknown method")
	}
}

func TestDibToImageBottomUp32ForcesOpaque(t *testing.T) {
	const width, height = 2, 2
	data := make([]byte, 40+width*height*4)
	binary.LittleEndian.PutUint32(data[0:4], 40)
	binary.LittleEndian.PutUint32(data[4:8], width)
	binary.LittleEndian.PutUint32(data[8:12], height)
	binary.LittleEndian.PutUint16(data[12:14], 1)
	binary.LittleEndian.PutUint16(data[14:16], 32)

	// Bottom-up: first stored row is y=1, then y=0.
	// Pixels are B,G,R,A with alpha left at 0.
	put := func(row, x int, b, g, r byte) {
		off := 40 + (row*width+x)*4
		data[off] = b
		data[off+1] = g
		data[off+2] = r
	}
	put(0, 0, 255, 0, 0) // bottom-left, blue
	put(0, 1, 0, 255, 0) // bottom-right, green
	put(1, 0, 0, 0, 255) // top-left, red
	put(1, 1, 255, 255, 255)

	img, err := dibToImage(data)
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != width || img.Bounds().Dy() != height {
		t.Fatalf("bounds = %v", img.Bounds())
	}

	checks := []struct {
		x, y int
		want color.NRGBA
	}{
		{0, 0, color.NRGBA{R: 255, A: 255}},
		{1, 0, color.NRGBA{R: 255, G: 255, B: 255, A: 255}},
		{0, 1, color.NRGBA{B: 255, A: 255}},
		{1, 1, color.NRGBA{G: 255, A: 255}},
	}
	for _, c := range checks {
		got := img.NRGBAAt(c.x, c.y)
		if got != c.want {
			t.Fatalf("pixel (%d,%d) = %+v, want %+v", c.x, c.y, got, c.want)
		}
	}
}

func TestCropScreenImageUsesVirtualOrigin(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 100, 80))
	img.SetNRGBA(10, 20, color.NRGBA{R: 1, G: 2, B: 3, A: 255})

	got, err := cropScreenImage(img, image.Rect(-1910, 20, -1900, 40), -1920, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Bounds() != image.Rect(10, 20, 20, 40) {
		t.Fatalf("bounds = %v", got.Bounds())
	}
	pix := color.NRGBAModel.Convert(got.At(10, 20)).(color.NRGBA)
	if pix != (color.NRGBA{R: 1, G: 2, B: 3, A: 255}) {
		t.Fatalf("pixel = %+v", pix)
	}

	if _, err := cropScreenImage(img, image.Rect(90, 70, 120, 90), 0, 0); err == nil {
		t.Fatal("expected region outside the image to fail")
	}
}
