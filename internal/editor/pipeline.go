package editor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mary/cutroom/internal/ai"
	"github.com/mary/cutroom/internal/domain"
	"github.com/mary/cutroom/internal/gcs"
	"github.com/mary/cutroom/internal/transcribe"
)

// Pipeline orchestrates the analyze → manifest → render flow.
//
// gcsClient is an interface (not *gcs.Client) so tests can swap in a fake
// without touching real GCS. *gcs.Client satisfies this interface today.
type gcsClient interface {
	Download(ctx context.Context, objectName, localPath string) error
	UploadFile(ctx context.Context, objectName, localPath, contentType string) (string, error)
}

// cardResolver looks up a Card by ID. The handler layer (which already
// knows about Firestore) provides this so Pipeline doesn't grow a
// dependency on the cards package's storage implementation.
type cardResolver interface {
	Get(ctx context.Context, id string) (*domain.Card, error)
}

type Pipeline struct {
	gcs         gcsClient
	cards       cardResolver
	transcriber *transcribe.WhisperClient
	ai          *ai.AnthropicClient
	workDir     string
}

func NewPipeline(g *gcs.Client, t *transcribe.WhisperClient, a *ai.AnthropicClient, cards cardResolver) *Pipeline {
	workDir := os.Getenv("WORK_DIR")
	if workDir == "" {
		workDir = "/tmp/cutroom"
	}
	os.MkdirAll(workDir, 0755)
	return &Pipeline{gcs: g, cards: cards, transcriber: t, ai: a, workDir: workDir}
}

// ── Codec lock ──────────────────────────────────────────────────────────
//
// Every per-segment / per-card encode uses identical codec parameters so
// the final concat step can stream-copy (`-c copy`) without re-encoding.
// Drift in any of these values silently corrupts the concat output.

const (
	targetW  = 1920
	targetH  = 1080
	targetFR = 30
	targetSR = 48000
)

// encodeArgs is the canonical set of encode parameters. EVERY intermediate
// mp4 (segments AND title cards) is produced with exactly these flags so
// concatIntermediates can use `-c copy`.
var encodeArgs = []string{
	"-c:v", "libx264",
	"-preset", "veryfast",
	"-crf", "23",
	"-pix_fmt", "yuv420p",
	"-r", fmt.Sprintf("%d", targetFR),
	"-c:a", "aac",
	"-ar", fmt.Sprintf("%d", targetSR),
	"-ac", "2",
	"-movflags", "+faststart",
}

// vNorm letterboxes any source video to targetW x targetH at targetFR fps.
// Reused for segment encodes and (in PR-5) image card encodes — same
// normalization applies because intermediates must match codec params.
var vNorm = fmt.Sprintf(
	"scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2,setsar=1,fps=%d,format=yuv420p",
	targetW, targetH, targetW, targetH, targetFR,
)

// localClipPath returns the on-disk location for a clip, under the pipeline's
// workDir. Render and Analyze both compute this the same way.
func (p *Pipeline) localClipPath(projID string, clip *domain.Clip) string {
	return filepath.Join(p.workDir, projID, clip.ID+filepath.Ext(clip.Name))
}

// ensureClipsLocal downloads every clip that isn't already on disk.
func (p *Pipeline) ensureClipsLocal(ctx context.Context, proj *domain.Project) error {
	for i := range proj.Clips {
		clip := &proj.Clips[i]
		local := p.localClipPath(proj.ID, clip)
		if _, err := os.Stat(local); err == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(local), 0755); err != nil {
			return err
		}
		if err := p.gcs.Download(ctx, clip.GCSPath, local); err != nil {
			return fmt.Errorf("download clip %s: %w", clip.Name, err)
		}
	}
	return nil
}

// Analyze downloads clips, probes duration, transcribes, and runs AI editorial review.
func (p *Pipeline) Analyze(proj *domain.Project) error {
	ctx := context.Background()

	var fullTranscript strings.Builder
	for i := range proj.Clips {
		clip := &proj.Clips[i]

		// Download clip from GCS to local workdir
		localPath := p.localClipPath(proj.ID, clip)
		os.MkdirAll(filepath.Dir(localPath), 0755)

		if err := p.gcs.Download(ctx, clip.GCSPath, localPath); err != nil {
			return fmt.Errorf("download clip %s: %w", clip.Name, err)
		}

		// Probe duration via ffprobe
		duration, err := probeDuration(localPath)
		if err != nil {
			return fmt.Errorf("probe %s: %w", clip.Name, err)
		}
		clip.Duration = duration

		// Transcribe with Whisper
		segments, err := p.transcriber.Transcribe(ctx, localPath)
		if err != nil {
			return fmt.Errorf("transcribe %s: %w", clip.Name, err)
		}
		clip.Transcript = segments

		// Accumulate full transcript for AI analysis
		fullTranscript.WriteString(fmt.Sprintf("\n\n[CLIP: %s]\n", clip.Name))
		for _, seg := range segments {
			fullTranscript.WriteString(fmt.Sprintf("[%.2f-%.2f] %s\n", seg.Start, seg.End, seg.Text))
		}
	}

	// AI editorial analysis
	analysis, err := p.ai.AnalyzeTranscript(ctx, proj, fullTranscript.String())
	if err != nil {
		return fmt.Errorf("AI analysis: %w", err)
	}

	proj.Analysis = analysis
	proj.Status = domain.StatusAnalyzed
	return nil
}

// BuildManifest combines user free-text instructions + analysis + the
// user's card library into a structured edit plan. An empty library is
// the cue for the AI to skip title cards entirely (empty-library bridge).
func (p *Pipeline) BuildManifest(ctx context.Context, proj *domain.Project, library []*domain.Card, instructions string) (*domain.EditManifest, error) {
	return p.ai.BuildManifest(ctx, proj, library, instructions)
}

// localCardPath returns where a card's PNG lives on disk during render.
func (p *Pipeline) localCardPath(projID, cardID string) string {
	return filepath.Join(p.workDir, projID, "cards", cardID+".png")
}

// ensureCardsLocal resolves every TitleCard.ImageID in the manifest to a
// local PNG. Mirrors ensureClipsLocal — Cloud Run's /tmp is ephemeral and
// a render request can land on a fresh instance with no cards on disk.
//
// The orphan check is fused in: if any ImageID can't be resolved (card
// deleted out from under the manifest, hallucinated by the AI, etc.) we
// fail FAST with an actionable error rather than letting it explode
// mid-encode with a cryptic ffmpeg input failure.
func (p *Pipeline) ensureCardsLocal(ctx context.Context, projID string, m *domain.EditManifest) error {
	for i, tc := range m.TitleCards {
		if tc.ImageID == nil {
			return fmt.Errorf("title card %d has no image_id — re-pick a card and try again", i)
		}
		card, err := p.cards.Get(ctx, *tc.ImageID)
		if err != nil {
			return fmt.Errorf("look up title card %d: %w", i, err)
		}
		if card == nil {
			return fmt.Errorf("title card %d (id=%s) is no longer in your library — re-pick a card and try again", i, *tc.ImageID)
		}
		local := p.localCardPath(projID, *tc.ImageID)
		if _, err := os.Stat(local); err == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(local), 0755); err != nil {
			return err
		}
		if err := p.gcs.Download(ctx, card.GCSPath, local); err != nil {
			return fmt.Errorf("download title card %d: %w", i, err)
		}
	}
	return nil
}

// Render executes the EditManifest as a sequence of independent ffmpeg
// invocations (one per segment, one per title card) followed by a single
// concat-demuxer step that stream-copies the intermediates into the final
// mp4. This is meaningfully faster on long videos with many segments than
// the previous monolithic filter_complex graph because each encode runs in
// its own ffmpeg process and the concat does no decode/re-encode work.
//
// Failure policy: outDir is wiped at the top of every render so stale
// intermediates from a previous failed run never participate. Any encode
// error aborts immediately, sets Status=error, and leaves the partial
// intermediates in /tmp for debugging. No auto-retry. Single attempt.
func (p *Pipeline) Render(proj *domain.Project) error {
	ctx := context.Background()
	m := proj.Manifest
	if m == nil {
		return fmt.Errorf("no manifest")
	}

	// Cloud Run's /tmp is ephemeral and a render request can land on a fresh
	// instance that never ran Analyze. Re-download any clip that isn't on disk.
	if err := p.ensureClipsLocal(ctx, proj); err != nil {
		return fmt.Errorf("rehydrate clips: %w", err)
	}

	// Resolve every TitleCard.ImageID to a local PNG. Fails FAST if any
	// referenced card is missing (orphan check) — far better UX than a
	// cryptic ffmpeg input error mid-encode.
	if err := p.ensureCardsLocal(ctx, proj.ID, m); err != nil {
		return fmt.Errorf("rehydrate cards: %w", err)
	}

	outDir := filepath.Join(p.workDir, proj.ID, "out")
	// Wipe-and-recreate so partial state from a prior failed render never
	// pollutes a fresh attempt. Concat demuxer is unforgiving about stale
	// intermediates with mismatched codec params.
	if err := os.RemoveAll(outDir); err != nil {
		return fmt.Errorf("wipe outDir: %w", err)
	}
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return fmt.Errorf("create outDir: %w", err)
	}

	// 1. Generate .srt caption file.
	captionPath := filepath.Join(outDir, "captions.srt")
	if err := writeSRT(proj, captionPath); err != nil {
		return fmt.Errorf("write SRT: %w", err)
	}

	// 2. Build clipID → local-path map for segment lookups.
	clipPaths := make(map[string]string)
	for i := range proj.Clips {
		clipPaths[proj.Clips[i].ID] = p.localClipPath(proj.ID, &proj.Clips[i])
	}

	// 3. Encode each segment and any cards that follow it as their own
	//    intermediate mp4. Order matches the final concat list.
	var intermediates []string
	for i, seg := range m.Segments {
		segOut := filepath.Join(outDir, fmt.Sprintf("seg_%03d.mp4", i))
		clipPath, ok := clipPaths[seg.ClipID]
		if !ok {
			return fmt.Errorf("clip %s not found locally", seg.ClipID)
		}
		if err := encodeSegment(clipPath, seg.Start, seg.End, segOut); err != nil {
			return fmt.Errorf("encode segment %d: %w", i, err)
		}
		intermediates = append(intermediates, segOut)

		// Title cards inserted AFTER segment i. ensureCardsLocal already
		// validated that every ImageID resolves to a real on-disk PNG.
		for j, tc := range m.TitleCards {
			if tc.AfterSegment != i {
				continue
			}
			if tc.ImageID == nil {
				return fmt.Errorf("title card %d.%d has nil ImageID — orphan check should have caught this", i, j)
			}
			cardPath := p.localCardPath(proj.ID, *tc.ImageID)
			cardOut := filepath.Join(outDir, fmt.Sprintf("card_%03d_%03d.mp4", i, j))
			if err := encodeImageCard(cardPath, tc.Duration, cardOut); err != nil {
				return fmt.Errorf("encode title card after segment %d: %w", i, err)
			}
			intermediates = append(intermediates, cardOut)
		}
	}

	if len(intermediates) == 0 {
		return fmt.Errorf("manifest produced no segments to encode")
	}

	// 4. Concat the intermediates into final.mp4 — pure stream copy.
	mainOut := filepath.Join(outDir, "final.mp4")
	if err := concatIntermediates(outDir, intermediates, mainOut); err != nil {
		return fmt.Errorf("concat: %w", err)
	}

	// 5. Cut reel if specified (independent encode, untouched by the split).
	reelOut := ""
	if m.ReelSegment != nil {
		reelOut = filepath.Join(outDir, "reel.mp4")
		if err := cutReel(proj, m.ReelSegment, reelOut); err != nil {
			return fmt.Errorf("cut reel: %w", err)
		}
	}

	// 6. Upload results to GCS.
	outputGCSPath := "projects/" + proj.ID + "/output/final.mp4"
	outputURL, err := p.gcs.UploadFile(ctx, outputGCSPath, mainOut, "video/mp4")
	if err != nil {
		return fmt.Errorf("upload final video: %w", err)
	}
	proj.OutputURL = outputURL

	captionGCSPath := "projects/" + proj.ID + "/output/captions.srt"
	captionURL, err := p.gcs.UploadFile(ctx, captionGCSPath, captionPath, "text/plain")
	if err != nil {
		return fmt.Errorf("upload captions: %w", err)
	}
	proj.CaptionURL = captionURL

	if reelOut != "" {
		reelGCSPath := "projects/" + proj.ID + "/output/reel.mp4"
		reelURL, err := p.gcs.UploadFile(ctx, reelGCSPath, reelOut, "video/mp4")
		if err != nil {
			return fmt.Errorf("upload reel: %w", err)
		}
		proj.ReelsURL = reelURL
	}

	return nil
}

// encodeSegment encodes a [start, end] subrange of clipPath into an mp4 at
// outPath, normalized to the codec lock (encodeArgs + vNorm). Used for
// every segment in the manifest.
func encodeSegment(clipPath string, start, end float64, outPath string) error {
	args := []string{"-y",
		"-ss", fmt.Sprintf("%.3f", start),
		"-to", fmt.Sprintf("%.3f", end),
		"-i", clipPath,
		"-vf", vNorm,
		"-af", fmt.Sprintf("aresample=%d,aformat=sample_fmts=fltp:channel_layouts=stereo", targetSR),
	}
	args = append(args, encodeArgs...)
	args = append(args, outPath)
	return runFFmpeg(args)
}

// encodeImageCard renders an uploaded PNG as a full-frame title card mp4
// at outPath: looped image input + silence audio + the same vNorm
// letterbox the segment encoder uses (so any PNG dimension produces a
// 1920x1080 yuv420p output that concats cleanly with adjacent segments).
//
// The wedge: this is what makes the user's Canva-designed brand asset
// actually appear in the final video.
func encodeImageCard(cardPath string, duration float64, outPath string) error {
	args := []string{"-y",
		"-loop", "1",
		"-t", fmt.Sprintf("%.3f", duration),
		"-i", cardPath,
		"-f", "lavfi",
		"-t", fmt.Sprintf("%.3f", duration),
		"-i", fmt.Sprintf("anullsrc=r=%d:cl=stereo", targetSR),
		"-vf", vNorm,
	}
	args = append(args, encodeArgs...)
	args = append(args, outPath)
	return runFFmpeg(args)
}

// concatIntermediates joins the given intermediate mp4s into outPath using
// ffmpeg's concat demuxer with stream copy. No decode, no re-encode — fast.
// All inputs MUST share codec parameters (enforced via encodeArgs); drift
// produces a corrupt or unplayable file.
func concatIntermediates(outDir string, intermediates []string, outPath string) error {
	if len(intermediates) == 0 {
		return fmt.Errorf("concat: no inputs")
	}
	listPath := filepath.Join(outDir, "concat.txt")
	var listBuf bytes.Buffer
	for _, p := range intermediates {
		// concat demuxer needs absolute paths or paths relative to listPath.
		// Use abs to be safe across callers.
		abs, err := filepath.Abs(p)
		if err != nil {
			return fmt.Errorf("abs(%s): %w", p, err)
		}
		// Single-quote escape: ' becomes '\''
		fmt.Fprintf(&listBuf, "file '%s'\n", strings.ReplaceAll(abs, "'", `'\''`))
	}
	if err := os.WriteFile(listPath, listBuf.Bytes(), 0644); err != nil {
		return fmt.Errorf("write concat list: %w", err)
	}
	args := []string{"-y",
		"-f", "concat",
		"-safe", "0",
		"-i", listPath,
		"-c", "copy",
		"-movflags", "+faststart",
		outPath,
	}
	return runFFmpeg(args)
}

// runFFmpeg executes ffmpeg with the given args, capturing stderr for error
// messages. Stdout/stderr also stream to the host stderr for real-time logs.
func runFFmpeg(args []string) error {
	cmd := exec.Command("ffmpeg", args...)
	var stderr bytes.Buffer
	cmd.Stdout = os.Stderr
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)
	fmt.Fprintf(os.Stderr, "[ffmpeg] %s\n", strings.Join(cmd.Args, " "))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg: %w\n%s", err, tailLines(stderr.String(), 30))
	}
	return nil
}

func cutReel(proj *domain.Project, rs *domain.ReelSegment, outPath string) error {
	var clipPath string
	for _, c := range proj.Clips {
		if c.ID == rs.ClipID {
			clipPath = filepath.Join(os.Getenv("WORK_DIR"), proj.ID, c.ID+filepath.Ext(c.Name))
			break
		}
	}
	if clipPath == "" {
		return fmt.Errorf("reel clip not found")
	}

	// Crop to 9:16 for Reels/Shorts. Reel is independent of the main concat
	// pipeline so it can use its own codec params freely.
	cmd := exec.Command("ffmpeg", "-y",
		"-ss", fmt.Sprintf("%.3f", rs.Start),
		"-to", fmt.Sprintf("%.3f", rs.End),
		"-i", clipPath,
		"-vf", "crop=ih*9/16:ih,scale=1080:1920",
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-crf", "23",
		"-c:a", "aac",
		"-threads", "0",
		outPath,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func probeDuration(path string) (float64, error) {
	out, err := exec.Command("ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		path,
	).Output()
	if err != nil {
		return 0, err
	}
	var d float64
	fmt.Sscanf(strings.TrimSpace(string(out)), "%f", &d)
	return d, nil
}

func writeSRT(proj *domain.Project, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	i := 1
	for _, clip := range proj.Clips {
		for _, seg := range clip.Transcript {
			fmt.Fprintf(f, "%d\n%s --> %s\n%s\n\n",
				i,
				srtTimestamp(seg.Start),
				srtTimestamp(seg.End),
				strings.TrimSpace(seg.Text),
			)
			i++
		}
	}
	return nil
}

func srtTimestamp(secs float64) string {
	h := int(secs) / 3600
	m := (int(secs) % 3600) / 60
	s := int(secs) % 60
	ms := int((secs - float64(int(secs))) * 1000)
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, s, ms)
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}
