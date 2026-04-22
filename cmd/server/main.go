package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/mary/cutroom/internal/ai"
	"github.com/mary/cutroom/internal/editor"
	"github.com/mary/cutroom/internal/gcs"
	"github.com/mary/cutroom/internal/store"
	"github.com/mary/cutroom/internal/transcribe"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	gcsBucket := os.Getenv("GCS_BUCKET")
	if gcsBucket == "" {
		log.Fatal("GCS_BUCKET env var required")
	}

	gcsClient, err := gcs.NewClient(gcsBucket)
	if err != nil {
		log.Fatalf("failed to init GCS client: %v", err)
	}

	transcriber := transcribe.NewWhisperClient(os.Getenv("OPENAI_API_KEY"))
	aiClient := ai.NewAnthropicClient(os.Getenv("ANTHROPIC_API_KEY"))
	editPipeline := editor.NewPipeline(gcsClient, transcriber, aiClient)

	projectStore, err := store.NewProjectStore(context.Background(), os.Getenv("FIRESTORE_PROJECT_ID"))
	if err != nil {
		log.Fatalf("failed to init project store: %v", err)
	}
	defer projectStore.Close()

	h := NewHandler(editPipeline, gcsClient, projectStore)

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Get("/", h.Index)
	r.Post("/projects", h.CreateProject)
	r.Get("/projects/{id}", h.GetProject)
	r.Post("/projects/{id}/clips/sign", h.SignClipUpload)
	r.Post("/projects/{id}/clips/register", h.RegisterClip)
	r.Post("/projects/{id}/analyze", h.AnalyzeClips)
	r.Post("/projects/{id}/instruct", h.SubmitInstructions)
	r.Post("/projects/{id}/manifest", h.UpdateManifest)
	r.Post("/projects/{id}/render", h.RenderVideo)
	r.Get("/projects/{id}/status", h.GetStatus)
	r.Get("/projects/{id}/analysis-status", h.GetAnalysisStatus)
	r.Get("/projects/{id}/download", h.DownloadResult)

	// Serve static files
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.Dir("web/static"))))

	log.Printf("cutroom running on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, r))
}
