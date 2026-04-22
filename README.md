# Cutroom

AI-powered YouTube edit suite. Upload clips, get a finished video with title cards, captions, reel cuts, and AI-generated titles and descriptions.

## What it does

1. **Upload** — drag video clips from your phone or desktop to GCS
2. **Analyze** — Whisper transcribes with timestamps; Claude reviews as an editor (flags ums, pacing lags, finds reel moments, suggests titles)
3. **Instruct** — describe your edit in plain English; Claude + your analysis become a structured edit manifest
4. **Render** — FFmpeg executes the manifest: cuts, title cards, transitions, captions, 9:16 reel

## Stack

- **Go** — `net/http` + Chi router, HTMX for the UI
- **Google Cloud Storage** — video file storage (upload from anywhere, including phone)
- **OpenAI Whisper** — transcription with word-level timestamps
- **Anthropic Claude** — editorial analysis + manifest generation
- **FFmpeg** — video rendering, title cards, reel crop

---

## Local Setup

### Prerequisites

```bash
# Install FFmpeg
brew install ffmpeg          # macOS
sudo apt install ffmpeg      # Ubuntu/Debian

# Install Go 1.22+
brew install go
```

### Environment variables

Create a `.env` file (or export these):

```bash
GCS_BUCKET=your-gcs-bucket-name
OPENAI_API_KEY=sk-...
ANTHROPIC_API_KEY=sk-ant-...
GOOGLE_APPLICATION_CREDENTIALS=/path/to/service-account.json   # local only
WORK_DIR=/tmp/cutroom       # local scratch space for video processing
PORT=8080
```

### GCS Setup

1. Create a GCS bucket in [Google Cloud Console](https://console.cloud.google.com/storage)
2. Create a Service Account with **Storage Admin** role
3. Download the JSON key → set as `GOOGLE_APPLICATION_CREDENTIALS`
4. Enable CORS on the bucket for browser uploads:

```bash
gsutil cors set cors.json gs://your-bucket-name
```

`cors.json` (the `Location` / `x-goog-resumable` headers must be exposed so
the browser can read the resumable-session URL on upload init):
```json
[{
  "origin": ["*"],
  "method": ["GET", "POST", "PUT"],
  "responseHeader": [
    "Content-Type", "Content-Range", "Location",
    "Range", "ETag", "x-goog-resumable"
  ],
  "maxAgeSeconds": 3600
}]
```

### Run locally

```bash
go mod tidy
go run ./cmd/server
# → http://localhost:8080
```

---

## Deploy to Cloud Run (recommended)

Cloud Run is the easiest path — it handles HTTPS, scales to zero, and has native GCS auth so you don't need a service account key file in prod.

```bash
# Build and push image
gcloud builds submit --tag gcr.io/YOUR_PROJECT/cutroom

# Deploy
gcloud run deploy cutroom \
  --image gcr.io/YOUR_PROJECT/cutroom \
  --platform managed \
  --region us-east1 \
  --allow-unauthenticated \
  --set-env-vars GCS_BUCKET=your-bucket,OPENAI_API_KEY=sk-...,ANTHROPIC_API_KEY=sk-ant-... \
  --memory 2Gi \
  --cpu 2 \
  --timeout 900   # 15 min — long renders need this
```

Give the Cloud Run service account Storage Admin on your bucket:
```bash
gcloud projects add-iam-policy-binding YOUR_PROJECT \
  --member="serviceAccount:YOUR_RUN_SA@YOUR_PROJECT.iam.gserviceaccount.com" \
  --role="roles/storage.admin"
```

---

## Project structure

```
cutroom/
├── cmd/server/
│   ├── main.go          # server setup, routes
│   └── handlers.go      # HTTP handlers
├── internal/
│   ├── editor/
│   │   ├── types.go     # domain types: Project, Clip, EditManifest, etc.
│   │   └── pipeline.go  # orchestrator: Analyze → BuildManifest → Render
│   ├── gcs/
│   │   └── client.go    # GCS upload/download/signed URLs
│   ├── transcribe/
│   │   └── whisper.go   # OpenAI Whisper API client
│   └── ai/
│       └── anthropic.go # Claude: transcript analysis + manifest generation
├── web/
│   ├── templates/       # Go HTML templates (HTMX-powered)
│   └── static/          # CSS + JS
├── Dockerfile
└── go.mod
```

---

## Roadmap / v2 ideas

- [ ] Persistent storage (Postgres or Firestore) — projects survive restarts
- [ ] Auto-detect best reel moment without reviewing (one-click short)
- [ ] Custom title card styles / branding (upload your own font/colors)
- [ ] Background music layer (duck under voice, fade out)
- [ ] Tami's producer view — simplified UI to review and approve edits
- [ ] Thumbnail generation via image model
- [ ] Direct YouTube upload via YouTube Data API

---

## Notes on video file size

Cloud Run has a 32MB request body limit by default. For large video files, the upload flow should go **client → GCS directly** using a signed upload URL rather than through the Go server. That's a quick v2 refactor — generate a signed PUT URL from `/projects/{id}/upload-url`, let the browser PUT directly to GCS, then call a webhook to register the clip. For the weekend MVP, direct server upload works fine for clips under ~500MB on a fast connection.
