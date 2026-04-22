#!/usr/bin/env bash
set -euo pipefail

PROJECT=cutroom-908ad
REGION=us-east4
SERVICE=cutroom
BUCKET=cutroom-908ad.firebasestorage.app

gcloud config set project "$PROJECT"

gcloud services enable \
  run.googleapis.com \
  cloudbuild.googleapis.com \
  artifactregistry.googleapis.com \
  secretmanager.googleapis.com \
  iamcredentials.googleapis.com \
  firestore.googleapis.com

# Create secrets (skips if they already exist). Prompts for the key value.
for name in ANTHROPIC_API_KEY OPENAI_API_KEY; do
  if ! gcloud secrets describe "$name" >/dev/null 2>&1; then
    read -rsp "Paste $name: " val; echo
    printf "%s" "$val" | gcloud secrets create "$name" --data-file=-
  fi
done

# Grant Cloud Run runtime SA access to read the secrets.
PROJECT_NUMBER=$(gcloud projects describe "$PROJECT" --format='value(projectNumber)')
RUNTIME_SA="${PROJECT_NUMBER}-compute@developer.gserviceaccount.com"
for name in ANTHROPIC_API_KEY OPENAI_API_KEY; do
  gcloud secrets add-iam-policy-binding "$name" \
    --member="serviceAccount:$RUNTIME_SA" \
    --role="roles/secretmanager.secretAccessor" >/dev/null
done

# Deploy from source — Cloud Run uses your Dockerfile automatically.
gcloud run deploy "$SERVICE" \
  --source . \
  --region "$REGION" \
  --allow-unauthenticated \
  --memory 4Gi \
  --cpu 4 \
  --no-cpu-throttling \
  --timeout 3600 \
  --set-env-vars "GCS_BUCKET=$BUCKET,WORK_DIR=/tmp/cutroom" \
  --set-secrets "ANTHROPIC_API_KEY=ANTHROPIC_API_KEY:latest,OPENAI_API_KEY=OPENAI_API_KEY:latest"

# Grant the Cloud Run runtime SA access to the bucket + ability to sign URLs.
SA=$(gcloud run services describe "$SERVICE" --region "$REGION" --format='value(spec.template.spec.serviceAccountName)')
SA=${SA:-"$(gcloud projects describe $PROJECT --format='value(projectNumber)')-compute@developer.gserviceaccount.com"}

gcloud storage buckets add-iam-policy-binding "gs://$BUCKET" \
  --member="serviceAccount:$SA" --role="roles/storage.objectAdmin"

gcloud iam service-accounts add-iam-policy-binding "$SA" \
  --member="serviceAccount:$SA" --role="roles/iam.serviceAccountTokenCreator"

# Allow the runtime SA to read/write Firestore for project persistence.
gcloud projects add-iam-policy-binding "$PROJECT" \
  --member="serviceAccount:$SA" --role="roles/datastore.user" \
  --condition=None >/dev/null

echo "Done. URL:"
gcloud run services describe "$SERVICE" --region "$REGION" --format='value(status.url)'
