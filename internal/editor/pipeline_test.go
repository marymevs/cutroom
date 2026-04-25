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

// ── encodeTextCard ───────────────────────────────────────────────────────

func TestEncodeTextCard_ProducesValidMP4MatchingCodecLock(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "card.mp4")
	if err := encodeTextCard("Hello World", 2.5, out); err != nil {
		// drawtext can fail in environments without fonts. Skip rather than
		// fail so the rest of the suite still runs (Cloud Run/Docker has
		// fonts; some local dev machines may not).
		t.Skipf("encodeTextCard requires a usable font (drawtext): %v", err)
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
