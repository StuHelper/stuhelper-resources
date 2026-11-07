package resources

import (
	"context"
	"slices"
	"time"
)

type Reference struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	CanonicalID string `json:"canonical_id,omitempty"`
}
type References struct {
	Items      []Reference `json:"items"`
	MissingIDs []string    `json:"missing_ids"`
}

// NameCatalog is a minimal module port, called only after local ownership checks.
type NameCatalog interface {
	ReferenceNames(context.Context, []string) (References, error)
}

func (s *Service) MyReferences(ctx context.Context, p Principal, ids []string) (References, error) {
	if !p.Valid() {
		return References{}, ErrUnauthenticated
	}
	if len(ids) < 1 || len(ids) > 100 {
		return References{}, ErrInput
	}
	ids = slices.Clone(ids)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	for _, id := range ids {
		if !validID(id) {
			return References{}, ErrInput
		}
	}
	err := s.repo.Transaction(ctx, func(t Transaction) error {
		owned, e := t.OwnedReferences(p, ids)
		if e != nil {
			return e
		}
		if !slices.Equal(owned, ids) {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		return References{}, err
	}
	port, ok := s.deps.Catalog.(NameCatalog)
	if !ok {
		return References{}, ErrUnavailable
	}
	call, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := port.ReferenceNames(call, ids)
	if err != nil {
		return References{}, err
	}
	seen := map[string]bool{}
	for _, item := range out.Items {
		if !slices.Contains(ids, item.ID) || seen[item.ID] || !validID(item.Kind) || !text(item.Name, 500) || item.Name == "" || item.CanonicalID != "" && !validID(item.CanonicalID) {
			return References{}, ErrUnavailable
		}
		seen[item.ID] = true
	}
	for _, id := range out.MissingIDs {
		if !slices.Contains(ids, id) || seen[id] {
			return References{}, ErrUnavailable
		}
		seen[id] = true
	}
	if len(seen) != len(ids) {
		return References{}, ErrUnavailable
	}
	return out, nil
}
