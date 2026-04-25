package cards

import (
	"context"
	"fmt"

	"cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/mary/cutroom/internal/domain"
)

// CardStore persists Card records in Firestore. Mirrors the shape of
// store.ProjectStore so the handler layer talks to a single style of API.
type CardStore struct {
	client *firestore.Client
}

const cardsCollection = "cards"

func NewCardStore(client *firestore.Client) *CardStore {
	return &CardStore{client: client}
}

func (s *CardStore) Save(ctx context.Context, c *domain.Card) error {
	if c.ID == "" {
		return fmt.Errorf("card.Save: empty ID")
	}
	if _, err := s.client.Collection(cardsCollection).Doc(c.ID).Set(ctx, c); err != nil {
		return fmt.Errorf("firestore Set card %s: %w", c.ID, err)
	}
	return nil
}

func (s *CardStore) Get(ctx context.Context, id string) (*domain.Card, error) {
	doc, err := s.client.Collection(cardsCollection).Doc(id).Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("firestore Get card %s: %w", id, err)
	}
	var c domain.Card
	if err := doc.DataTo(&c); err != nil {
		return nil, fmt.Errorf("decode card %s: %w", id, err)
	}
	return &c, nil
}

// List returns every card. Newest-first ordering by CreatedAt — design spec
// says "most-recently-created first" so the most-relevant card sits closest
// to the upload affordance.
func (s *CardStore) List(ctx context.Context) ([]*domain.Card, error) {
	iter := s.client.Collection(cardsCollection).
		OrderBy("CreatedAt", firestore.Desc).
		Documents(ctx)
	defer iter.Stop()

	var out []*domain.Card
	for {
		doc, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("firestore list cards: %w", err)
		}
		var c domain.Card
		if err := doc.DataTo(&c); err != nil {
			return nil, fmt.Errorf("decode card %s: %w", doc.Ref.ID, err)
		}
		out = append(out, &c)
	}
	return out, nil
}

func (s *CardStore) Delete(ctx context.Context, id string) error {
	if _, err := s.client.Collection(cardsCollection).Doc(id).Delete(ctx); err != nil {
		return fmt.Errorf("firestore Delete card %s: %w", id, err)
	}
	return nil
}
