package cards

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

// makePNG returns a synthetic PNG of the given dimensions filled with a
// solid color. Used to exercise the validation paths without checked-in
// fixture files.
func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: 100, B: 50, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

func TestValidateAndDecode_AcceptsValidLandscapePNG(t *testing.T) {
	raw := makePNG(t, 1920, 1080)
	d, err := ValidateAndDecode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ValidateAndDecode: %v", err)
	}
	if d.Width != 1920 || d.Height != 1080 {
		t.Errorf("dims: got %dx%d want 1920x1080", d.Width, d.Height)
	}
	if d.IsPortrait {
		t.Error("expected IsPortrait=false for 1920x1080")
	}
	if len(d.OriginalBytes) != len(raw) {
		t.Errorf("OriginalBytes len: got %d want %d", len(d.OriginalBytes), len(raw))
	}
	if len(d.ThumbBytes) == 0 {
		t.Error("expected non-empty thumbnail")
	}

	// Thumbnail must be a valid PNG decoded back to ThumbWidth x ThumbHeight.
	thumb, err := png.Decode(bytes.NewReader(d.ThumbBytes))
	if err != nil {
		t.Fatalf("decode thumb: %v", err)
	}
	if thumb.Bounds().Dx() != ThumbWidth || thumb.Bounds().Dy() != ThumbHeight {
		t.Errorf("thumb dims: got %dx%d want %dx%d",
			thumb.Bounds().Dx(), thumb.Bounds().Dy(), ThumbWidth, ThumbHeight)
	}
}

func TestValidateAndDecode_PortraitDetected(t *testing.T) {
	// CRITICAL: Canva exports vertical for Reels/Stories. Without portrait
	// detection the user uploads a tall card, opens the rendered video,
	// sees a tiny postage stamp, and loses trust in the tool.
	raw := makePNG(t, 1080, 1920)
	d, err := ValidateAndDecode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ValidateAndDecode: %v", err)
	}
	if !d.IsPortrait {
		t.Error("expected IsPortrait=true for 1080x1920")
	}
}

func TestValidateAndDecode_RejectsNonPNG(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{"jpeg-magic", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 0x4A, 0x46, 0x49, 0x46}},
		{"empty", []byte{}},
		{"text", []byte("hello world this is not a png at all")},
		{"truncated-magic", []byte{0x89, 0x50}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidateAndDecode(bytes.NewReader(tc.data))
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("expected *ValidationError, got %T: %v", err, err)
			}
			if ve.Code != "not_png" {
				t.Errorf("code: got %q want %q", ve.Code, "not_png")
			}
			if !IsValidationError(err) {
				t.Error("IsValidationError should return true")
			}
		})
	}
}

func TestValidateAndDecode_RejectsTooLarge(t *testing.T) {
	// Construct a "PNG" prefix of MaxFileBytes+1 — actual decode wouldn't
	// succeed but we expect the size check to fire first. The first 8
	// bytes are valid PNG magic so we don't bail on the magic check.
	big := make([]byte, MaxFileBytes+1)
	copy(big, []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A})

	_, err := ValidateAndDecode(bytes.NewReader(big))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "too_large" {
		t.Errorf("expected too_large ValidationError, got %v", err)
	}
}

func TestValidateAndDecode_RejectsTooBigDimensions(t *testing.T) {
	// MaxDimension+1 on either axis fails. Use a smaller-actual-size PNG
	// because building an actual 4097x4097 in test is expensive.
	raw := makePNG(t, MaxDimension+1, 100)
	_, err := ValidateAndDecode(bytes.NewReader(raw))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "too_big" {
		t.Errorf("expected too_big ValidationError, got %v", err)
	}
}

func TestValidateAndDecode_AcceptsAtMaxDimension(t *testing.T) {
	// Boundary: exactly MaxDimension is allowed. 4096x100 is small enough
	// to build cheaply.
	raw := makePNG(t, MaxDimension, 100)
	if _, err := ValidateAndDecode(bytes.NewReader(raw)); err != nil {
		t.Errorf("expected MaxDimension to be accepted, got %v", err)
	}
}

func TestMakeThumb_LetterboxesPortrait(t *testing.T) {
	// 100x500 portrait → 320x180 canvas with the inscribed image centered.
	src := image.NewRGBA(image.Rect(0, 0, 100, 500))
	for y := 0; y < 500; y++ {
		for x := 0; x < 100; x++ {
			src.Set(x, y, color.RGBA{R: 255, G: 0, B: 0, A: 255})
		}
	}
	out, err := makeThumb(src)
	if err != nil {
		t.Fatalf("makeThumb: %v", err)
	}
	thumb, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if thumb.Bounds().Dx() != ThumbWidth || thumb.Bounds().Dy() != ThumbHeight {
		t.Fatalf("thumb dims: got %dx%d want %dx%d",
			thumb.Bounds().Dx(), thumb.Bounds().Dy(), ThumbWidth, ThumbHeight)
	}
	// The corners should be transparent (letterbox padding) and the
	// middle column should be red. Sample a few pixels to verify.
	cornerR, _, _, cornerA := thumb.At(5, 5).RGBA()
	if cornerA != 0 {
		t.Errorf("expected transparent corner, got alpha %d (red %d)", cornerA, cornerR)
	}
	midR, _, _, midA := thumb.At(ThumbWidth/2, ThumbHeight/2).RGBA()
	if midA == 0 || midR == 0 {
		t.Errorf("expected red center, got rgba(%d,_,_,%d)", midR, midA)
	}
}

func TestSafeName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"hello.png", "hello"},
		{"  spaces around.PNG  ", "spaces around"},
		{"path/to/file.png", "file"},
		{"path\\to\\file.png", "file"},
		{"no-extension", "no-extension"},
		{".dotfile.png", ".dotfile"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := SafeName(tc.in); got != tc.want {
			t.Errorf("SafeName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestValidationError_AsError(t *testing.T) {
	ve := &ValidationError{Code: "not_png", Message: "Nope."}
	var asErr error = ve
	if asErr.Error() != "Nope." {
		t.Errorf("Error(): got %q want %q", asErr.Error(), "Nope.")
	}
	if !strings.Contains(ve.Code, "not_png") {
		t.Errorf("code lookup: %q", ve.Code)
	}
}
