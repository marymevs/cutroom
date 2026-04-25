// Package cards owns the title-card library: validation, server-side
// thumbnail generation, and the storage layout (originals + thumbs in GCS,
// metadata in Firestore).
//
// The wedge: creators with brand assets (Canva-designed title cards, fonts,
// colors) can upload their PNGs once and reuse them across projects. None of
// the major AI editors (Opus / Submagic / Descript / Vizard) treat user-
// supplied brand assets as first-class — that's the v2 differentiator.
package cards

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"io"
	"strings"

	"golang.org/x/image/draw"
)

// Validation limits. See the design spec — these are the input-validation
// caps to keep the upload surface DoS-resistant and the library snappy.
const (
	MaxFileBytes  = 10 * 1024 * 1024 // 10 MB
	MaxDimension  = 4096             // 4096 px on either axis
	ThumbWidth    = 320              // grid display is 160x90 retina = 320x180
	ThumbHeight   = 180
)

// pngMagic is the 8-byte PNG signature. Any file whose first 8 bytes don't
// match this is rejected even if it has a .png extension.
var pngMagic = []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}

// Result of a successful validation+decode pass on an uploaded PNG.
type DecodedPNG struct {
	OriginalBytes []byte // the raw PNG bytes (ready to upload to GCS)
	ThumbBytes    []byte // the generated 320x180 thumbnail (also PNG)
	Width         int
	Height        int
	IsPortrait    bool // height > width — letterboxes badly in 16:9; warn the user
}

// ValidationError carries a user-facing reason. The handler maps these to a
// 400 with the message visible inline under the failed file.
type ValidationError struct {
	Code    string // machine-readable: "not_png", "too_large", "too_big"
	Message string // human-readable: "Not a PNG."
}

func (e *ValidationError) Error() string { return e.Message }

func vErr(code, msg string) error { return &ValidationError{Code: code, Message: msg} }

// IsValidationError reports whether err originated from input validation
// (so the handler returns 400) vs an internal error (500).
func IsValidationError(err error) bool {
	_, ok := err.(*ValidationError)
	return ok
}

// ValidateAndDecode reads up to MaxFileBytes+1 from r, verifies the PNG
// magic + size + decoded dimensions, and produces a 320x180 thumbnail.
// Returns a typed *ValidationError on user-input issues (400-worthy) and a
// regular error on internal failures.
//
// The upload handler can call this with `?force=1` semantics by checking
// IsPortrait on the returned DecodedPNG: if the request's `force` flag is
// not set and IsPortrait is true, the handler renders the warn UI and
// returns 200 without committing the upload.
func ValidateAndDecode(r io.Reader) (*DecodedPNG, error) {
	// Read a touch over the cap so we can distinguish "exactly 10MB" from
	// "more than 10MB" — io.LimitReader to MaxFileBytes alone would silently
	// truncate and we'd accept oversized files.
	lim := io.LimitReader(r, MaxFileBytes+1)
	raw, err := io.ReadAll(lim)
	if err != nil {
		return nil, fmt.Errorf("read upload: %w", err)
	}
	if len(raw) > MaxFileBytes {
		return nil, vErr("too_large", fmt.Sprintf("File is too large (max %d MB).", MaxFileBytes/1024/1024))
	}
	if len(raw) < len(pngMagic) || !bytes.Equal(raw[:len(pngMagic)], pngMagic) {
		return nil, vErr("not_png", "Not a PNG. Title cards must be PNG files.")
	}

	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, vErr("not_png", fmt.Sprintf("Couldn't decode PNG: %s", err.Error()))
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w > MaxDimension || h > MaxDimension {
		return nil, vErr("too_big",
			fmt.Sprintf("Image is too big (%dx%d, max %dx%d).", w, h, MaxDimension, MaxDimension))
	}
	if w == 0 || h == 0 {
		return nil, vErr("not_png", "Image has zero dimensions.")
	}

	thumb, err := makeThumb(img)
	if err != nil {
		return nil, fmt.Errorf("thumbnail: %w", err)
	}

	return &DecodedPNG{
		OriginalBytes: raw,
		ThumbBytes:    thumb,
		Width:         w,
		Height:        h,
		IsPortrait:    h > w,
	}, nil
}

// makeThumb scales the source image into a 320x180 letterbox via approximate
// bilinear filtering. We always produce a 320x180 canvas (matching the grid's
// 16:9 display tile) and pad with transparent pixels so portrait sources
// don't deform — the user sees the same letterboxing they'll get in render.
//
// Why approximate bilinear (not high-quality CatmullRom): this runs on the
// upload request path on Cloud Run with 4 vCPUs serving multiple users.
// "Pretty good" at 320x180 takes ~5ms per image; pixel-perfect quality at
// thumbnail size is invisible to the eye.
func makeThumb(src image.Image) ([]byte, error) {
	dst := image.NewRGBA(image.Rect(0, 0, ThumbWidth, ThumbHeight))

	// Compute the inscribed rectangle that preserves aspect ratio inside
	// the 320x180 canvas. Anything outside stays transparent.
	srcW, srcH := src.Bounds().Dx(), src.Bounds().Dy()
	scaleW := float64(ThumbWidth) / float64(srcW)
	scaleH := float64(ThumbHeight) / float64(srcH)
	scale := scaleW
	if scaleH < scale {
		scale = scaleH
	}
	w := int(float64(srcW) * scale)
	h := int(float64(srcH) * scale)
	x := (ThumbWidth - w) / 2
	y := (ThumbHeight - h) / 2

	draw.ApproxBiLinear.Scale(dst, image.Rect(x, y, x+w, y+h), src, src.Bounds(), draw.Over, nil)

	var buf bytes.Buffer
	if err := png.Encode(&buf, dst); err != nil {
		return nil, fmt.Errorf("encode thumbnail: %w", err)
	}
	return buf.Bytes(), nil
}

// SafeName trims an uploaded filename down to a name suitable for the
// `name` field if the user didn't supply one. Strips path separators,
// extensions, and excess whitespace.
func SafeName(filename string) string {
	if i := strings.LastIndex(filename, "/"); i >= 0 {
		filename = filename[i+1:]
	}
	if i := strings.LastIndex(filename, "\\"); i >= 0 {
		filename = filename[i+1:]
	}
	if i := strings.LastIndex(filename, "."); i > 0 {
		filename = filename[:i]
	}
	return strings.TrimSpace(filename)
}
