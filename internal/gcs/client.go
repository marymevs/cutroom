package gcs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
)

type Client struct {
	bucket string
	client *storage.Client
}

func NewClient(bucket string) (*Client, error) {
	ctx := context.Background()

	var client *storage.Client
	var err error

	credFile := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	if credFile != "" {
		client, err = storage.NewClient(ctx, option.WithCredentialsFile(credFile))
	} else {
		// Uses Application Default Credentials (works on GCE, Cloud Run, local gcloud auth)
		client, err = storage.NewClient(ctx)
	}

	if err != nil {
		return nil, fmt.Errorf("storage.NewClient: %w", err)
	}

	return &Client{bucket: bucket, client: client}, nil
}

// Upload streams an io.Reader to GCS and returns a signed URL valid for 7 days.
func (c *Client) Upload(ctx context.Context, objectName string, r io.Reader, contentType string) (string, error) {
	obj := c.client.Bucket(c.bucket).Object(objectName)
	wc := obj.NewWriter(ctx)
	wc.ContentType = contentType

	if _, err := io.Copy(wc, r); err != nil {
		return "", fmt.Errorf("io.Copy to GCS: %w", err)
	}
	if err := wc.Close(); err != nil {
		return "", fmt.Errorf("GCS writer close: %w", err)
	}

	return c.signedURL(objectName)
}

// UploadFile uploads a local file to GCS and returns a signed URL.
func (c *Client) UploadFile(ctx context.Context, objectName, localPath, contentType string) (string, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", localPath, err)
	}
	defer f.Close()
	return c.Upload(ctx, objectName, f, contentType)
}

// Download fetches a GCS object and writes it to localPath atomically:
// bytes stream into a unique per-call tmp file and the file is renamed into
// place only on a clean finish. Using a unique tmp name (rather than
// localPath+".part") keeps two concurrent Download calls for the same
// destination from truncating each other's in-flight writes.
func (c *Client) Download(ctx context.Context, objectName, localPath string) error {
	rc, err := c.client.Bucket(c.bucket).Object(objectName).NewReader(ctx)
	if err != nil {
		return fmt.Errorf("GCS NewReader: %w", err)
	}
	defer rc.Close()

	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("tmp nonce: %w", err)
	}
	tmpPath := fmt.Sprintf("%s.part.%s", localPath, hex.EncodeToString(nonce[:]))

	f, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("create local file: %w", err)
	}

	if _, err := io.Copy(f, rc); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, localPath); err != nil {
		// A racing Download may have already populated localPath; if the
		// destination exists, our work is redundant but not a failure.
		if _, statErr := os.Stat(localPath); statErr == nil {
			os.Remove(tmpPath)
			return nil
		}
		os.Remove(tmpPath)
		return err
	}
	return nil
}

func (c *Client) signedURL(objectName string) (string, error) {
	opts := &storage.SignedURLOptions{
		Method:  "GET",
		Expires: time.Now().Add(7 * 24 * time.Hour),
	}
	return c.client.Bucket(c.bucket).SignedURL(objectName, opts)
}

// SignedResumableInitURL returns a signed URL the browser POSTs to in order to
// start a GCS resumable upload session. The POST must include the header
// `x-goog-resumable: start` and a Content-Type matching contentType; GCS
// responds with 201 and a `Location` header containing a session URL that is
// valid for 7 days and accepts chunked PUTs with `Content-Range`.
//
// Resumable uploads (not single-shot PUTs) are what make big mobile uploads
// survive flaky networks: each chunk can be retried and the session can be
// resumed from wherever GCS acknowledged bytes.
func (c *Client) SignedResumableInitURL(objectName, contentType string) (string, error) {
	opts := &storage.SignedURLOptions{
		Method:      "POST",
		ContentType: contentType,
		Headers:     []string{"x-goog-resumable:start"},
		Expires:     time.Now().Add(6 * time.Hour),
	}
	return c.client.Bucket(c.bucket).SignedURL(objectName, opts)
}

// ReadSignedURL returns a time-limited GET URL for an already-uploaded object.
func (c *Client) ReadSignedURL(objectName string) (string, error) {
	return c.signedURL(objectName)
}
