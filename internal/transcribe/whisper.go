package transcribe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/mary/cutroom/internal/domain"
)

// whisperMaxBytes is the OpenAI Whisper API upload cap (25 MiB).
const whisperMaxBytes = 25 * 1024 * 1024

type WhisperClient struct {
	apiKey string
}

func NewWhisperClient(apiKey string) *WhisperClient {
	return &WhisperClient{apiKey: apiKey}
}

type whisperResponse struct {
	Segments []struct {
		Start float64 `json:"start"`
		End   float64 `json:"end"`
		Text  string  `json:"text"`
	} `json:"segments"`
}

// Transcribe sends a video/audio file to OpenAI Whisper and returns timestamped segments.
//
// Whisper caps uploads at 25 MiB. Video files routinely exceed that, so we
// extract an audio-only stream via ffmpeg (mono, 16 kHz, 64 kbps mp3) before
// uploading — this fits ~50 min of audio inside the limit regardless of video
// bitrate. ffmpeg is already a pipeline dependency (used for rendering and
// ffprobe).
func (c *WhisperClient) Transcribe(ctx context.Context, localPath string) ([]domain.TranscriptSegment, error) {
	audioPath := localPath + ".whisper.mp3"
	defer os.Remove(audioPath)

	extract := exec.CommandContext(ctx, "ffmpeg", "-y",
		"-i", localPath,
		"-vn",           // drop video
		"-ac", "1",      // mono
		"-ar", "16000",  // 16 kHz (Whisper's internal sample rate)
		"-c:a", "libmp3lame",
		"-b:a", "64k",
		audioPath,
	)
	if out, err := extract.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("extract audio: %w: %s", err, string(out))
	}

	// Defensive: if the extracted audio still exceeds Whisper's limit
	// (roughly >45 min at 64 kbps), fail with a clear message rather than
	// hitting a 413. Chunking is future work.
	if stat, err := os.Stat(audioPath); err == nil && stat.Size() > whisperMaxBytes {
		return nil, fmt.Errorf("extracted audio is %d bytes, exceeds Whisper %d-byte limit (clip too long; chunking not yet implemented)", stat.Size(), whisperMaxBytes)
	}

	f, err := os.Open(audioPath)
	if err != nil {
		return nil, fmt.Errorf("open extracted audio: %w", err)
	}
	defer f.Close()

	var body bytes.Buffer
	w := multipart.NewWriter(&body)

	part, err := w.CreateFormFile("file", filepath.Base(audioPath))
	if err != nil {
		return nil, err
	}
	if _, err = io.Copy(part, f); err != nil {
		return nil, err
	}
	w.WriteField("model", "whisper-1")
	w.WriteField("response_format", "verbose_json") // gives us timestamps
	w.WriteField("timestamp_granularities[]", "segment")
	w.Close()

	req, err := http.NewRequestWithContext(ctx, "POST",
		"https://api.openai.com/v1/audio/transcriptions", &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("whisper request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("whisper %d: %s", resp.StatusCode, string(b))
	}

	var wr whisperResponse
	if err := json.NewDecoder(resp.Body).Decode(&wr); err != nil {
		return nil, fmt.Errorf("decode whisper response: %w", err)
	}

	segments := make([]domain.TranscriptSegment, len(wr.Segments))
	for i, s := range wr.Segments {
		segments[i] = domain.TranscriptSegment{
			Start: s.Start,
			End:   s.End,
			Text:  s.Text,
		}
	}
	return segments, nil
}
