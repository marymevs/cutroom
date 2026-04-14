package editor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mary/cutroom/internal/ai"
	"github.com/mary/cutroom/internal/gcs"
	"github.com/mary/cutroom/internal/transcribe"
)

type Pipeline struct {
	gcs         *gcs.Client
	transcriber *transcribe.WhisperClient
	ai          *ai.AnthropicClient
	workDir     string
}

func NewPipeline(g *gcs.Client, t *transcribe.WhisperClient, a *ai.AnthropicClient) *Pipeline {
	workDir := os.Getenv("WORK_DIR")
	if workDir == "" {
		workDir = "/tmp/cutroom"
	}
	os.MkdirAll(workDir, 0755)
	return &Pipeline{gcs: g, transcriber: t, ai: a, workDir: workDir}
}

// Analyze downloads clips, probes duration, transcribes, and runs AI editorial review.
func (p *Pipeline) Analyze(proj *Project) error {
	ctx := context.Background()

	var fullTranscript strings.Builder
	for i := range proj.Clips {
		clip := &proj.Clips[i]

		// Download clip from GCS to local workdir
		localPath := filepath.Join(p.workDir, proj.ID, clip.ID+filepath.Ext(clip.Name))
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
	proj.Status = StatusAnalyzed
	return nil
}

// BuildManifest combines user free-text instructions + analysis into a structured edit plan.
func (p *Pipeline) BuildManifest(ctx context.Context, proj *Project, instructions string) (*EditManifest, error) {
	return p.ai.BuildManifest(ctx, proj, instructions)
}

// Render executes the EditManifest through FFmpeg and uploads results to GCS.
func (p *Pipeline) Render(proj *Project) error {
	ctx := context.Background()
	m := proj.Manifest
	if m == nil {
		return fmt.Errorf("no manifest")
	}

	outDir := filepath.Join(p.workDir, proj.ID, "out")
	os.MkdirAll(outDir, 0755)

	// 1. Generate .srt caption file
	captionPath := filepath.Join(outDir, "captions.srt")
	if err := writeSRT(proj, captionPath); err != nil {
		return fmt.Errorf("write SRT: %w", err)
	}

	// 2. Build and run the main FFmpeg command
	mainOut := filepath.Join(outDir, "final.mp4")
	cmd, err := buildFFmpegCommand(proj, m, mainOut)
	if err != nil {
		return fmt.Errorf("build ffmpeg command: %w", err)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg render: %w", err)
	}

	// 3. Cut reel if specified
	reelOut := ""
	if m.ReelSegment != nil {
		reelOut = filepath.Join(outDir, "reel.mp4")
		if err := cutReel(proj, m.ReelSegment, reelOut); err != nil {
			return fmt.Errorf("cut reel: %w", err)
		}
	}

	// 4. Upload results to GCS
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

// buildFFmpegCommand constructs an FFmpeg filter_complex command from the manifest.
// It handles: clip trimming, title card overlays, cross-fade transitions.
func buildFFmpegCommand(proj *Project, m *EditManifest, outPath string) (*exec.Cmd, error) {
	clipPaths := make(map[string]string)
	for _, clip := range proj.Clips {
		clipPaths[clip.ID] = filepath.Join(os.Getenv("WORK_DIR"), proj.ID, clip.ID+filepath.Ext(clip.Name))
	}

	// Build input args
	args := []string{"-y"} // overwrite output
	inputIndex := 0
	segInputMap := make(map[int]int) // segment order -> ffmpeg input index

	for _, seg := range m.Segments {
		path, ok := clipPaths[seg.ClipID]
		if !ok {
			return nil, fmt.Errorf("clip %s not found locally", seg.ClipID)
		}
		args = append(args,
			"-ss", fmt.Sprintf("%.3f", seg.Start),
			"-to", fmt.Sprintf("%.3f", seg.End),
			"-i", path,
		)
		segInputMap[seg.Order] = inputIndex
		inputIndex++
	}

	// Build filter_complex: trim each segment, add title cards, concat
	var filterParts []string
	var concatInputs []string

	for i, seg := range m.Segments {
		idx := segInputMap[seg.Order]
		filterParts = append(filterParts, fmt.Sprintf("[%d:v]setpts=PTS-STARTPTS[v%d]", idx, i))
		filterParts = append(filterParts, fmt.Sprintf("[%d:a]asetpts=PTS-STARTPTS[a%d]", idx, i))
		concatInputs = append(concatInputs, fmt.Sprintf("[v%d][a%d]", i, i))

		// Insert title card after this segment if specified
		for _, tc := range m.TitleCards {
			if tc.AfterSegment == i {
				// Generate a black video with drawtext for the title card
				cardFilter := fmt.Sprintf(
					"color=c=black:s=1920x1080:d=%.1f[card%d_raw];[card%d_raw]drawtext=text='%s':fontcolor=white:fontsize=72:x=(w-text_w)/2:y=(h-text_h)/2[card%d]",
					tc.Duration, i, i, escapeFfmpegText(tc.Text), i,
				)
				silenceFilter := fmt.Sprintf(
					"aevalsrc=0:d=%.1f[card%d_audio]",
					tc.Duration, i,
				)
				filterParts = append(filterParts, cardFilter, silenceFilter)
				concatInputs = append(concatInputs, fmt.Sprintf("[card%d][card%d_audio]", i, i))
			}
		}
	}

	n := len(concatInputs)
	filterParts = append(filterParts,
		strings.Join(concatInputs, "")+fmt.Sprintf("concat=n=%d:v=1:a=1[outv][outa]", n),
	)

	args = append(args,
		"-filter_complex", strings.Join(filterParts, ";"),
		"-map", "[outv]",
		"-map", "[outa]",
		"-c:v", "libx264",
		"-c:a", "aac",
		"-crf", "18",
		outPath,
	)

	return exec.Command("ffmpeg", args...), nil
}

func cutReel(proj *Project, rs *ReelSegment, outPath string) error {
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

	// Crop to 9:16 for Reels/Shorts
	cmd := exec.Command("ffmpeg", "-y",
		"-ss", fmt.Sprintf("%.3f", rs.Start),
		"-to", fmt.Sprintf("%.3f", rs.End),
		"-i", clipPath,
		"-vf", "crop=ih*9/16:ih,scale=1080:1920",
		"-c:v", "libx264",
		"-c:a", "aac",
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

func writeSRT(proj *Project, path string) error {
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

func escapeFfmpegText(s string) string {
	s = strings.ReplaceAll(s, "'", "\\'")
	s = strings.ReplaceAll(s, ":", "\\:")
	return s
}
