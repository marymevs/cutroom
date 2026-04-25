package editor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mary/cutroom/internal/domain"
)

// makeTestClip generates a synthetic mp4 with color bars and a tone, used as
// a stand-in for an uploaded clip. ffmpeg-only — no real video file checked
// into the repo.
func makeTestClip(t *testing.T, dir, name string, duration float64) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	cmd := exec.Command("ffmpeg", "-y",
		"-f", "lavfi", "-t", fmt.Sprintf("%.3f", duration),
		"-i", "testsrc2=size=640x360:rate=30",
		"-f", "lavfi", "-t", fmt.Sprintf("%.3f", duration),
		"-i", "sine=frequency=440:sample_rate=48000",
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p",
		"-c:a", "aac",
		"-shortest",
		path,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("makeTestClip failed: %v\n%s", err, string(out))
	}
	return path
}

// probeStreams returns the first video+audio stream's codec parameters from
// path. Used to verify the codec lock survives encode + concat.
func probeStreams(t *testing.T, path string) (vCodec, pixFmt string, w, h, fps int, aCodec string, sr, ch int) {
	t.Helper()
	out, err := exec.Command("ffprobe",
		"-v", "error",
		"-show_entries", "stream=codec_type,codec_name,width,height,pix_fmt,r_frame_rate,sample_rate,channels",
		"-of", "json",
		path,
	).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var probe struct {
		Streams []struct {
			CodecType  string `json:"codec_type"`
			CodecName  string `json:"codec_name"`
			Width      int    `json:"width"`
			Height     int    `json:"height"`
			PixFmt     string `json:"pix_fmt"`
			FrameRate  string `json:"r_frame_rate"`
			SampleRate string `json:"sample_rate"`
			Channels   int    `json:"channels"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatalf("ffprobe json parse: %v\nraw: %s", err, out)
	}
	for _, s := range probe.Streams {
		switch s.CodecType {
		case "video":
			vCodec = s.CodecName
			pixFmt = s.PixFmt
			w = s.Width
			h = s.Height
			if num, _, ok := strings.Cut(s.FrameRate, "/"); ok {
				fps, _ = strconv.Atoi(num)
			}
		case "audio":
			aCodec = s.CodecName
			if s.SampleRate != "" {
				sr, _ = strconv.Atoi(s.SampleRate)
			}
			ch = s.Channels
		}
	}
	return
}

func probeDur(t *testing.T, path string) float64 {
	t.Helper()
	d, err := probeDuration(path)
	if err != nil {
		t.Fatalf("probeDuration %s: %v", path, err)
	}
	return d
}

// ── encodeSegment ────────────────────────────────────────────────────────

func TestEncodeSegment_ProducesValidMP4WithLockedCodecParams(t *testing.T) {
	dir := t.TempDir()
	src := makeTestClip(t, dir, "src.mp4", 5.0)

	out := filepath.Join(dir, "seg.mp4")
	if err := encodeSegment(src, 1.0, 4.0, out); err != nil {
		t.Fatalf("encodeSegment: %v", err)
	}

	dur := probeDur(t, out)
	if dur < 2.8 || dur > 3.2 {
		t.Errorf("expected ~3s segment, got %.3fs", dur)
	}

	vCodec, pixFmt, w, h, fps, aCodec, sr, ch := probeStreams(t, out)
	if vCodec != "h264" {
		t.Errorf("video codec: got %q want h264", vCodec)
	}
	if pixFmt != "yuv420p" {
		t.Errorf("pix_fmt: got %q want yuv420p", pixFmt)
	}
	if w != targetW || h != targetH {
		t.Errorf("dimensions: got %dx%d want %dx%d", w, h, targetW, targetH)
	}
	if fps != targetFR {
		t.Errorf("frame rate: got %d want %d", fps, targetFR)
	}
	if aCodec != "aac" {
		t.Errorf("audio codec: got %q want aac", aCodec)
	}
	if sr != targetSR {
		t.Errorf("sample rate: got %d want %d", sr, targetSR)
	}
	if ch != 2 {
		t.Errorf("channels: got %d want 2", ch)
	}
}

// ── encodeImageCard (PR-5) ───────────────────────────────────────────────

// makePNGFile writes a synthetic landscape PNG fixture to disk.
func makePNGFile(t *testing.T, path string, w, h int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("ffmpeg", "-y",
		"-f", "lavfi",
		"-i", fmt.Sprintf("color=c=red:s=%dx%d:d=0.1", w, h),
		"-frames:v", "1",
		path,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("makePNGFile: %v\n%s", err, string(out))
	}
}

func TestEncodeImageCard_ProducesValidMP4MatchingCodecLock(t *testing.T) {
	dir := t.TempDir()
	pngPath := filepath.Join(dir, "card.png")
	makePNGFile(t, pngPath, 1920, 1080)

	out := filepath.Join(dir, "card.mp4")
	if err := encodeImageCard(pngPath, 2.5, out); err != nil {
		t.Fatalf("encodeImageCard: %v", err)
	}

	dur := probeDur(t, out)
	if dur < 2.3 || dur > 2.7 {
		t.Errorf("expected ~2.5s card, got %.3fs", dur)
	}

	vCodec, pixFmt, w, h, fps, aCodec, sr, ch := probeStreams(t, out)
	if vCodec != "h264" || pixFmt != "yuv420p" || w != targetW || h != targetH || fps != targetFR {
		t.Errorf("video lock mismatch: codec=%s pix_fmt=%s %dx%d@%dfps", vCodec, pixFmt, w, h, fps)
	}
	if aCodec != "aac" || sr != targetSR || ch != 2 {
		t.Errorf("audio lock mismatch: codec=%s sr=%d ch=%d", aCodec, sr, ch)
	}
}

func TestEncodeImageCard_LetterboxesNonStandardDimensions(t *testing.T) {
	// CRITICAL: any uploaded PNG (100x100, 4096x4096, portrait, anything)
	// must produce a 1920x1080 yuv420p output that concats cleanly with
	// adjacent segments. The vNorm filter is what enforces this.
	dir := t.TempDir()
	cases := []struct{ name string; w, h int }{
		{"tiny", 100, 100},
		{"max-square", 4096, 4096},
		{"portrait-effective", 540, 960}, // would have been rejected at upload but verify render math
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			png := filepath.Join(dir, tc.name+".png")
			makePNGFile(t, png, tc.w, tc.h)
			out := filepath.Join(dir, tc.name+".mp4")
			if err := encodeImageCard(png, 1.5, out); err != nil {
				t.Fatalf("encodeImageCard %s: %v", tc.name, err)
			}
			_, _, w, h, _, _, _, _ := probeStreams(t, out)
			if w != targetW || h != targetH {
				t.Errorf("%s: dims got %dx%d want %dx%d", tc.name, w, h, targetW, targetH)
			}
		})
	}
}

// ── concatIntermediates ──────────────────────────────────────────────────

func TestConcatIntermediates_StreamCopiesWithoutReEncoding(t *testing.T) {
	// CRITICAL REGRESSION TEST: the per-segment + concat-demuxer architecture
	// is the load-bearing change in PR-3. This test proves end-to-end that
	// (a) two segments encoded with encodeArgs produce concat-compatible
	// intermediates and (b) the concat output has duration equal to the sum
	// AND codec params survived stream-copy.
	dir := t.TempDir()
	src := makeTestClip(t, dir, "src.mp4", 5.0)

	seg1 := filepath.Join(dir, "seg_000.mp4")
	if err := encodeSegment(src, 0.0, 2.0, seg1); err != nil {
		t.Fatalf("encodeSegment 1: %v", err)
	}
	seg2 := filepath.Join(dir, "seg_001.mp4")
	if err := encodeSegment(src, 2.0, 4.0, seg2); err != nil {
		t.Fatalf("encodeSegment 2: %v", err)
	}

	final := filepath.Join(dir, "final.mp4")
	if err := concatIntermediates(dir, []string{seg1, seg2}, final); err != nil {
		t.Fatalf("concatIntermediates: %v", err)
	}

	dur := probeDur(t, final)
	if dur < 3.8 || dur > 4.2 {
		t.Errorf("expected ~4s concat output (2s + 2s), got %.3fs", dur)
	}

	vCodec, pixFmt, w, h, fps, aCodec, sr, ch := probeStreams(t, final)
	if vCodec != "h264" || pixFmt != "yuv420p" || w != targetW || h != targetH || fps != targetFR {
		t.Errorf("post-concat video lock mismatch: %s %s %dx%d@%dfps", vCodec, pixFmt, w, h, fps)
	}
	if aCodec != "aac" || sr != targetSR || ch != 2 {
		t.Errorf("post-concat audio lock mismatch: %s sr=%d ch=%d", aCodec, sr, ch)
	}
}

func TestConcatIntermediates_RejectsEmptyInput(t *testing.T) {
	dir := t.TempDir()
	err := concatIntermediates(dir, nil, filepath.Join(dir, "out.mp4"))
	if err == nil {
		t.Fatal("expected error on empty input, got nil")
	}
}

// ── ensureCardsLocal + orphan check (PR-5) ───────────────────────────────

// fakeCardResolver satisfies the cardResolver interface for tests. cards
// keyed by ID — return nil for absent IDs (mimics Firestore "not found").
type fakeCardResolver struct{ cards map[string]*domain.Card }

func (f *fakeCardResolver) Get(ctx context.Context, id string) (*domain.Card, error) {
	if f == nil {
		return nil, nil
	}
	return f.cards[id], nil
}

func TestEnsureCardsLocal_DownloadsMissingSkipsPresent(t *testing.T) {
	work := t.TempDir()
	stash := t.TempDir()

	// Stage one card PNG at a known GCS path.
	makePNGFile(t, filepath.Join(stash, "cards/card-A.png"), 1920, 1080)

	g := &fakeGCS{stashDir: stash}
	resolver := &fakeCardResolver{cards: map[string]*domain.Card{
		"card-A": {ID: "card-A", GCSPath: "cards/card-A.png"},
	}}
	p := &Pipeline{gcs: g, cards: resolver, workDir: work}

	idA := "card-A"
	m := &domain.EditManifest{
		TitleCards: []domain.TitleCard{
			{AfterSegment: 0, ImageID: &idA, Duration: 3.0},
		},
	}

	if err := p.ensureCardsLocal(context.Background(), "proj-1", m); err != nil {
		t.Fatalf("ensureCardsLocal first call: %v", err)
	}
	local := p.localCardPath("proj-1", "card-A")
	if _, err := os.Stat(local); err != nil {
		t.Fatalf("expected local card on disk: %v", err)
	}

	// Second call: file exists, should be a no-op (no extra Download call).
	// We verify by removing the GCS-backed source and re-running — if it
	// tries to re-download, it'll fail because the source is gone.
	if err := os.Remove(filepath.Join(stash, "cards/card-A.png")); err != nil {
		t.Fatal(err)
	}
	if err := p.ensureCardsLocal(context.Background(), "proj-1", m); err != nil {
		t.Errorf("second call should be no-op (file already on disk): %v", err)
	}
}

func TestEnsureCardsLocal_OrphanCheck_FailsFastWithActionableError(t *testing.T) {
	// CRITICAL REGRESSION: a manifest referencing a deleted card must fail
	// FAST with a clear, user-actionable message — not midway through render
	// with a cryptic ffmpeg "input file not found" error.
	work := t.TempDir()
	stash := t.TempDir()

	g := &fakeGCS{stashDir: stash}
	resolver := &fakeCardResolver{cards: map[string]*domain.Card{}} // empty = card "deleted"
	p := &Pipeline{gcs: g, cards: resolver, workDir: work}

	idGone := "card-deleted"
	m := &domain.EditManifest{
		TitleCards: []domain.TitleCard{
			{AfterSegment: 0, ImageID: &idGone, Duration: 3.0},
		},
	}

	err := p.ensureCardsLocal(context.Background(), "proj-1", m)
	if err == nil {
		t.Fatal("expected error for orphan card, got nil")
	}
	if !strings.Contains(err.Error(), "no longer in your library") {
		t.Errorf("expected actionable 'no longer in your library' message, got: %v", err)
	}
}

func TestEnsureCardsLocal_NilImageIDFailsFast(t *testing.T) {
	// A title card with no ImageID is invalid — UpdateManifest already
	// drops these at form-save time, but render is the safety net.
	work := t.TempDir()
	g := &fakeGCS{stashDir: t.TempDir()}
	resolver := &fakeCardResolver{cards: map[string]*domain.Card{}}
	p := &Pipeline{gcs: g, cards: resolver, workDir: work}

	m := &domain.EditManifest{
		TitleCards: []domain.TitleCard{
			{AfterSegment: 0, ImageID: nil, Duration: 3.0},
		},
	}

	err := p.ensureCardsLocal(context.Background(), "proj-1", m)
	if err == nil {
		t.Fatal("expected error for nil ImageID, got nil")
	}
	if !strings.Contains(err.Error(), "no image_id") {
		t.Errorf("expected 'no image_id' message, got: %v", err)
	}
}

// ── Render orchestration (with fakeGCS) ──────────────────────────────────

// fakeGCS satisfies gcsClient by reading/writing the local file system.
// Download = copy from a stash dir into the requested local path.
// UploadFile = no-op (records the call), returns a dummy URL.
type fakeGCS struct {
	stashDir string
	uploads  []string
}

func (f *fakeGCS) Download(ctx context.Context, objectName, localPath string) error {
	src := filepath.Join(f.stashDir, objectName)
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("fake GCS: object not found: %s", objectName)
	}
	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func (f *fakeGCS) UploadFile(ctx context.Context, objectName, localPath, contentType string) (string, error) {
	if _, err := os.Stat(localPath); err != nil {
		return "", fmt.Errorf("fake GCS upload: local file missing: %s", localPath)
	}
	f.uploads = append(f.uploads, objectName)
	return "https://fake/" + objectName, nil
}

// buildRenderTestProject creates a minimal Project with one clip and a
// 2-segment manifest pointing at clipGCSPath.
func buildRenderTestProject(clipGCSPath string) *domain.Project {
	clip := domain.Clip{
		ID:      "clip-1",
		Name:    "clip.mp4",
		GCSPath: clipGCSPath,
	}
	return &domain.Project{
		ID:    "proj-render-test",
		Name:  "Render Test",
		Clips: []domain.Clip{clip},
		Manifest: &domain.EditManifest{
			Segments: []domain.Segment{
				{ClipID: "clip-1", Start: 0.0, End: 2.0, Order: 0, Description: "first"},
				{ClipID: "clip-1", Start: 2.0, End: 4.0, Order: 1, Description: "second"},
			},
		},
	}
}

func TestRender_OutDirIsWipedAtStart(t *testing.T) {
	work := t.TempDir()
	stash := t.TempDir()

	clipGCSPath := "cliponly/clip-1.mp4"
	makeTestClip(t, filepath.Join(stash, "cliponly"), "clip-1.mp4", 5.0)

	f := &fakeGCS{stashDir: stash}
	p := &Pipeline{gcs: f, workDir: work}

	proj := buildRenderTestProject(clipGCSPath)
	outDir := filepath.Join(work, proj.ID, "out")

	// Pre-populate outDir with stale junk that MUST be wiped.
	if err := os.MkdirAll(outDir, 0755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(outDir, "STALE.txt")
	if err := os.WriteFile(stale, []byte("stale"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := p.Render(proj); err != nil {
		t.Fatalf("Render: %v", err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale file not wiped: %v", err)
	}

	final := filepath.Join(outDir, "final.mp4")
	if _, err := os.Stat(final); err != nil {
		t.Fatalf("expected final.mp4 to exist: %v", err)
	}
	dur := probeDur(t, final)
	if dur < 3.6 || dur > 4.2 {
		t.Errorf("expected ~4s final (two 2s segments), got %.3fs", dur)
	}

	// Both segments uploaded? Should see final.mp4 + captions.srt at minimum.
	if len(f.uploads) < 2 {
		t.Errorf("expected at least 2 uploads (final + captions), got %d: %v", len(f.uploads), f.uploads)
	}
}

func TestRender_FailFastOnMissingClip(t *testing.T) {
	work := t.TempDir()
	stash := t.TempDir()
	// DO NOT stash the clip; ensureClipsLocal will fail.
	f := &fakeGCS{stashDir: stash}
	p := &Pipeline{gcs: f, workDir: work}

	proj := buildRenderTestProject("missing/nonexistent.mp4")
	err := p.Render(proj)
	if err == nil {
		t.Fatal("expected Render to fail when GCS download fails, got nil")
	}
	if !strings.Contains(err.Error(), "rehydrate clips") {
		t.Errorf("expected 'rehydrate clips' in error, got: %v", err)
	}
	if len(f.uploads) > 0 {
		t.Errorf("no uploads should happen on failure, got %d", len(f.uploads))
	}
}

func TestRender_RejectsManifestWithNoSegments(t *testing.T) {
	work := t.TempDir()
	stash := t.TempDir()
	makeTestClip(t, filepath.Join(stash, "x"), "clip.mp4", 3.0)

	f := &fakeGCS{stashDir: stash}
	p := &Pipeline{gcs: f, workDir: work}

	proj := buildRenderTestProject("x/clip.mp4")
	proj.Manifest.Segments = nil

	err := p.Render(proj)
	if err == nil {
		t.Fatal("expected error on empty segments, got nil")
	}
	if !strings.Contains(err.Error(), "no segments") {
		t.Errorf("expected 'no segments' in error, got: %v", err)
	}
}
