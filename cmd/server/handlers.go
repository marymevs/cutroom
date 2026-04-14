package main

import (
	"encoding/json"
	"html/template"
	"net/http"
	"path/filepath"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/mary/cutroom/internal/editor"
	"github.com/mary/cutroom/internal/gcs"
)

type Handler struct {
	pipeline *editor.Pipeline
	gcs      *gcs.Client
	projects map[string]*editor.Project // in-memory for now; swap for DB later
	tmpl     *template.Template
}

func NewHandler(pipeline *editor.Pipeline, gcsClient *gcs.Client) *Handler {
	tmpl := template.Must(template.ParseGlob("web/templates/*.html"))
	return &Handler{
		pipeline: pipeline,
		gcs:      gcsClient,
		projects: make(map[string]*editor.Project),
		tmpl:     tmpl,
	}
}

func (h *Handler) Index(w http.ResponseWriter, r *http.Request) {
	h.tmpl.ExecuteTemplate(w, "index.html", map[string]any{
		"projects": h.projects,
	})
}

func (h *Handler) CreateProject(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	name := r.FormValue("name")
	if name == "" {
		name = "Untitled Project"
	}

	p := &editor.Project{
		ID:     uuid.New().String(),
		Name:   name,
		Status: editor.StatusCreated,
		Clips:  []editor.Clip{},
	}
	h.projects[p.ID] = p

	http.Redirect(w, r, "/projects/"+p.ID, http.StatusSeeOther)
}

func (h *Handler) GetProject(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	p, ok := h.projects[id]
	if !ok {
		http.NotFound(w, r)
		return
	}
	h.tmpl.ExecuteTemplate(w, "project.html", p)
}

// UploadClip receives a video file, streams it to GCS, registers it on the project.
func (h *Handler) UploadClip(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	p, ok := h.projects[id]
	if !ok {
		http.NotFound(w, r)
		return
	}

	r.ParseMultipartForm(500 << 20) // 500 MB limit per clip
	file, header, err := r.FormFile("clip")
	if err != nil {
		http.Error(w, "failed to read file: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer file.Close()

	objectName := "projects/" + id + "/clips/" + header.Filename
	gcsURL, err := h.gcs.Upload(r.Context(), objectName, file, header.Header.Get("Content-Type"))
	if err != nil {
		http.Error(w, "upload failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	clip := editor.Clip{
		ID:       uuid.New().String(),
		Name:     header.Filename,
		GCSPath:  objectName,
		GCSURL:   gcsURL,
		Duration: 0, // populated after analysis
	}
	p.Clips = append(p.Clips, clip)
	p.Status = editor.StatusUploaded

	// Return updated clip list partial for HTMX
	h.tmpl.ExecuteTemplate(w, "clips_partial.html", p)
}

// AnalyzeClips transcribes all clips and runs AI editorial analysis.
func (h *Handler) AnalyzeClips(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	p, ok := h.projects[id]
	if !ok {
		http.NotFound(w, r)
		return
	}

	p.Status = editor.StatusAnalyzing

	go func() {
		if err := h.pipeline.Analyze(p); err != nil {
			p.Status = editor.StatusError
			p.Error = err.Error()
		}
	}()

	w.Header().Set("HX-Trigger", "analysisStarted")
	h.tmpl.ExecuteTemplate(w, "status_partial.html", p)
}

// SubmitInstructions parses free-text edit instructions into a manifest via Claude.
func (h *Handler) SubmitInstructions(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	p, ok := h.projects[id]
	if !ok {
		http.NotFound(w, r)
		return
	}

	r.ParseForm()
	instructions := r.FormValue("instructions")

	manifest, err := h.pipeline.BuildManifest(r.Context(), p, instructions)
	if err != nil {
		http.Error(w, "failed to build edit manifest: "+err.Error(), http.StatusInternalServerError)
		return
	}

	p.Manifest = manifest
	p.Status = editor.StatusManifestReady

	h.tmpl.ExecuteTemplate(w, "manifest_partial.html", p)
}

// RenderVideo executes the edit manifest through FFmpeg.
func (h *Handler) RenderVideo(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	p, ok := h.projects[id]
	if !ok {
		http.NotFound(w, r)
		return
	}

	p.Status = editor.StatusRendering

	go func() {
		if err := h.pipeline.Render(p); err != nil {
			p.Status = editor.StatusError
			p.Error = err.Error()
			return
		}
		p.Status = editor.StatusDone
	}()

	h.tmpl.ExecuteTemplate(w, "status_partial.html", p)
}

func (h *Handler) GetStatus(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	p, ok := h.projects[id]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":     string(p.Status),
		"error":      p.Error,
		"outputURL":  p.OutputURL,
		"reelsURL":   p.ReelsURL,
		"captionURL": p.CaptionURL,
	})
}

func (h *Handler) DownloadResult(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	p, ok := h.projects[id]
	if !ok {
		http.NotFound(w, r)
		return
	}

	what := r.URL.Query().Get("type") // "video", "reel", "captions"
	var url string
	switch what {
	case "reel":
		url = p.ReelsURL
	case "captions":
		url = p.CaptionURL
	default:
		url = p.OutputURL
	}

	if url == "" {
		http.Error(w, "not ready", http.StatusNotFound)
		return
	}

	_ = filepath.Base(url)
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}
