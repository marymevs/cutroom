package main

import (
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"path/filepath"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/mary/cutroom/internal/domain"
	"github.com/mary/cutroom/internal/editor"
	"github.com/mary/cutroom/internal/gcs"
)

func renderTemplate(w http.ResponseWriter, t *template.Template, name string, data any) {
	if err := t.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("template %q execution error: %v", name, err)
	}
}

type Handler struct {
	pipeline *editor.Pipeline
	gcs      *gcs.Client
	projects map[string]*domain.Project // in-memory for now; swap for DB later
	pages    map[string]*template.Template // page name -> layout+page set
	partials *template.Template            // partials rendered standalone (HTMX)
}

func NewHandler(pipeline *editor.Pipeline, gcsClient *gcs.Client) *Handler {
	partialFiles, err := filepath.Glob("web/templates/*_partial.html")
	if err != nil {
		panic(err)
	}
	pageNames := []string{"index.html", "project.html"}
	pages := make(map[string]*template.Template, len(pageNames))
	for _, p := range pageNames {
		files := append([]string{
			"web/templates/layout.html",
			"web/templates/" + p,
		}, partialFiles...)
		pages[p] = template.Must(template.ParseFiles(files...))
	}
	partials := template.Must(template.ParseFiles(partialFiles...))
	return &Handler{
		pipeline: pipeline,
		gcs:      gcsClient,
		projects: make(map[string]*domain.Project),
		pages:    pages,
		partials: partials,
	}
}

func (h *Handler) Index(w http.ResponseWriter, r *http.Request) {
	renderTemplate(w, h.pages["index.html"], "layout.html", map[string]any{
		"projects": h.projects,
	})
}

func (h *Handler) CreateProject(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	name := r.FormValue("name")
	if name == "" {
		name = "Untitled Project"
	}

	p := &domain.Project{
		ID:     uuid.New().String(),
		Name:   name,
		Status: domain.StatusCreated,
		Clips:  []domain.Clip{},
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
	renderTemplate(w, h.pages["project.html"], "layout.html", p)
}

// SignClipUpload returns a time-limited URL the browser PUTs the file to directly,
// so video bytes never pass through Cloud Run (which caps request bodies at 32 MiB).
// The browser follows up with RegisterClip once the PUT succeeds.
func (h *Handler) SignClipUpload(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, ok := h.projects[id]; !ok {
		http.NotFound(w, r)
		return
	}

	var req struct {
		Filename    string `json:"filename"`
		ContentType string `json:"contentType"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Filename == "" {
		http.Error(w, "missing filename or contentType", http.StatusBadRequest)
		return
	}
	if req.ContentType == "" {
		req.ContentType = "application/octet-stream"
	}

	objectName := "projects/" + id + "/clips/" + uuid.New().String() + "-" + filepath.Base(req.Filename)
	url, err := h.gcs.SignedUploadURL(objectName, req.ContentType)
	if err != nil {
		http.Error(w, "sign failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"uploadURL":   url,
		"objectName":  objectName,
		"contentType": req.ContentType,
	})
}

// RegisterClip records a clip on the project after the browser has PUT the bytes
// directly to GCS. It re-signs a read URL and appends to the clip list.
func (h *Handler) RegisterClip(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	p, ok := h.projects[id]
	if !ok {
		http.NotFound(w, r)
		return
	}

	var req struct {
		Filename   string `json:"filename"`
		ObjectName string `json:"objectName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ObjectName == "" {
		http.Error(w, "missing objectName", http.StatusBadRequest)
		return
	}

	readURL, err := h.gcs.ReadSignedURL(req.ObjectName)
	if err != nil {
		http.Error(w, "sign read URL: "+err.Error(), http.StatusInternalServerError)
		return
	}

	name := req.Filename
	if name == "" {
		name = filepath.Base(req.ObjectName)
	}
	p.Clips = append(p.Clips, domain.Clip{
		ID:       uuid.New().String(),
		Name:     name,
		GCSPath:  req.ObjectName,
		GCSURL:   readURL,
		Duration: 0,
	})
	p.Status = domain.StatusUploaded

	renderTemplate(w, h.partials, "clips_partial.html", p)
}

// AnalyzeClips transcribes all clips and runs AI editorial analysis.
func (h *Handler) AnalyzeClips(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	p, ok := h.projects[id]
	if !ok {
		http.NotFound(w, r)
		return
	}

	p.Status = domain.StatusAnalyzing
	p.Error = ""

	go func() {
		if err := h.pipeline.Analyze(p); err != nil {
			p.Status = domain.StatusError
			p.Error = err.Error()
		}
	}()

	renderTemplate(w, h.partials, "analysis_status_partial.html", p)
}

// GetAnalysisStatus returns an HTML fragment reflecting the current analyze state.
// HTMX polls this while status == "analyzing"; once the goroutine flips status
// to "analyzed" or "error", this swaps the polling shell out for the final UI.
func (h *Handler) GetAnalysisStatus(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	p, ok := h.projects[id]
	if !ok {
		http.NotFound(w, r)
		return
	}
	renderTemplate(w, h.partials, "analysis_status_partial.html", p)
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
	p.Status = domain.StatusManifestReady

	renderTemplate(w, h.partials, "manifest_partial.html", p)
}

// RenderVideo executes the edit manifest through FFmpeg.
func (h *Handler) RenderVideo(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	p, ok := h.projects[id]
	if !ok {
		http.NotFound(w, r)
		return
	}

	p.Status = domain.StatusRendering

	go func() {
		if err := h.pipeline.Render(p); err != nil {
			p.Status = domain.StatusError
			p.Error = err.Error()
			return
		}
		p.Status = domain.StatusDone
	}()

	renderTemplate(w, h.partials, "status_partial.html", p)
}

func (h *Handler) GetStatus(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	p, ok := h.projects[id]
	if !ok {
		http.NotFound(w, r)
		return
	}
	renderTemplate(w, h.partials, "status_partial.html", p)
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
