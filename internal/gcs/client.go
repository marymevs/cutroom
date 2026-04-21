package gcs

import (
	"context"
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

// Download fetches a GCS object and writes it to localPath.
func (c *Client) Download(ctx context.Context, objectName, localPath string) error {
	rc, err := c.client.Bucket(c.bucket).Object(objectName).NewReader(ctx)
	if err != nil {
		return fmt.Errorf("GCS NewReader: %w", err)
	}
	defer rc.Close()

	f, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("create local file: %w", err)
	}
	defer f.Close()

	_, err = io.Copy(f, rc)
	return err
}

func (c *Client) signedURL(objectName string) (string, error) {
	opts := &storage.SignedURLOptions{
		Method:  "GET",
		Expires: time.Now().Add(7 * 24 * time.Hour),
	}
	return c.client.Bucket(c.bucket).SignedURL(objectName, opts)
}

// SignedUploadURL returns a signed URL the browser can PUT the file to directly,
// bypassing Cloud Run's request body limit. contentType must match the header
// the browser will send, or GCS rejects the PUT.
func (c *Client) SignedUploadURL(objectName, contentType string) (string, error) {
	opts := &storage.SignedURLOptions{
		Method:      "PUT",
		ContentType: contentType,
		Expires:     time.Now().Add(1 * time.Hour),
	}
	return c.client.Bucket(c.bucket).SignedURL(objectName, opts)
}

// ReadSignedURL returns a time-limited GET URL for an already-uploaded object.
func (c *Client) ReadSignedURL(objectName string) (string, error) {
	return c.signedURL(objectName)
}
