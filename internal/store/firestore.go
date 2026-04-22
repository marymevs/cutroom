package store

import (
	"context"
	"fmt"
	"os"

	"cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/mary/cutroom/internal/domain"
)

const projectsCollection = "projects"

// ProjectStore persists Project records to Firestore so work survives
// page refreshes, server restarts, and Cloud Run instance churn.
type ProjectStore struct {
	client *firestore.Client
}

// NewProjectStore opens a Firestore client. projectID may be empty to
// auto-detect from GOOGLE_APPLICATION_CREDENTIALS or the Cloud Run metadata server.
func NewProjectStore(ctx context.Context, projectID string) (*ProjectStore, error) {
	var opts []option.ClientOption
	if cred := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"); cred != "" {
		opts = append(opts, option.WithCredentialsFile(cred))
	}
	if projectID == "" {
		projectID = firestore.DetectProjectID
	}
	client, err := firestore.NewClient(ctx, projectID, opts...)
	if err != nil {
		return nil, fmt.Errorf("firestore.NewClient: %w", err)
	}
	return &ProjectStore{client: client}, nil
}

func (s *ProjectStore) Close() error { return s.client.Close() }

// Save writes the full project document, overwriting any existing record.
func (s *ProjectStore) Save(ctx context.Context, p *domain.Project) error {
	_, err := s.client.Collection(projectsCollection).Doc(p.ID).Set(ctx, p)
	if err != nil {
		return fmt.Errorf("firestore Set %s: %w", p.ID, err)
	}
	return nil
}

// Get returns the project, or (nil, nil) if it does not exist.
func (s *ProjectStore) Get(ctx context.Context, id string) (*domain.Project, error) {
	doc, err := s.client.Collection(projectsCollection).Doc(id).Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("firestore Get %s: %w", id, err)
	}
	var p domain.Project
	if err := doc.DataTo(&p); err != nil {
		return nil, fmt.Errorf("decode project %s: %w", id, err)
	}
	return &p, nil
}

// List returns every project document. Order is unspecified.
func (s *ProjectStore) List(ctx context.Context) ([]*domain.Project, error) {
	iter := s.client.Collection(projectsCollection).Documents(ctx)
	defer iter.Stop()

	var out []*domain.Project
	for {
		doc, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("firestore list: %w", err)
		}
		var p domain.Project
		if err := doc.DataTo(&p); err != nil {
			return nil, fmt.Errorf("decode project %s: %w", doc.Ref.ID, err)
		}
		out = append(out, &p)
	}
	return out, nil
}
