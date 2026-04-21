package domain

// Status values for a project
type Status string

const (
	StatusCreated       Status = "created"
	StatusUploaded      Status = "uploaded"
	StatusAnalyzing     Status = "analyzing"
	StatusAnalyzed      Status = "analyzed"
	StatusManifestReady Status = "manifest_ready"
	StatusRendering     Status = "rendering"
	StatusDone          Status = "done"
	StatusError         Status = "error"
)

// Project is the top-level unit of work.
type Project struct {
	ID         string
	Name       string
	Status     Status
	Error      string
	Clips      []Clip
	Analysis   *Analysis     // populated after Analyze()
	Manifest   *EditManifest // populated after BuildManifest()
	OutputURL  string        // GCS signed URL for final video
	ReelsURL   string        // GCS signed URL for reel clip
	CaptionURL string        // GCS signed URL for .srt file
}

// Clip is a single uploaded video file.
type Clip struct {
	ID          string
	Name        string
	GCSPath     string
	GCSURL      string
	Duration    float64 // seconds, populated after probe
	Transcript  []TranscriptSegment
}

// TranscriptSegment is a word or phrase with timestamps from Whisper.
type TranscriptSegment struct {
	Start float64
	End   float64
	Text  string
}

// Analysis is the AI editorial review of all clips.
type Analysis struct {
	SuggestedCuts   []SuggestedCut
	SuggestedTitles []string
	Description     string
	ReelMoments     []ReelMoment
	RawTranscript   string
}

// SuggestedCut is a section Claude recommends removing.
type SuggestedCut struct {
	ClipID string
	Start  float64
	End    float64
	Reason string // "filler words", "pacing lag", "repeated content", etc.
}

// ReelMoment is a timestamp range Claude thinks would make a great short.
type ReelMoment struct {
	ClipID string
	Start  float64
	End    float64
	Hook   string // one-line description of why this works as a reel
}

// EditManifest is the full structured edit plan Claude generates from
// user instructions + analysis. FFmpeg executes against this.
type EditManifest struct {
	Segments    []Segment
	TitleCards  []TitleCard
	OutputCuts  []Cut        // confirmed cuts (from analysis or user)
	ReelSegment *ReelSegment // which part to export as a short
	CaptionFile string       // path to generated .srt
}

// Segment is a contiguous chunk of a source clip to include.
type Segment struct {
	ClipID string
	Start  float64
	End    float64
	Order  int
}

// TitleCard is a text overlay to insert between segments.
type TitleCard struct {
	AfterSegment int    // insert after this segment index
	Text         string
	Duration     float64 // seconds
	Style        string  // "default", "minimal", "bold"
}

// Cut is a confirmed removal from a clip.
type Cut struct {
	ClipID string
	Start  float64
	End    float64
}

// ReelSegment defines what to export as a vertical short.
type ReelSegment struct {
	ClipID string
	Start  float64
	End    float64
}
