package resources

import "context"

// Public resource DTOs carry no uploader identity. Keep an irreversible owner
// marker for future projections while preserving copyright attribution/files.
func (s *Service) AnonymizeOwner(ctx context.Context, p Principal, id string) error {
	if !p.Valid() || len(id) != 36 {
		return ErrInput
	}
	return s.repo.Transaction(ctx, func(t Transaction) error { return t.CloseOwner(p, id) })
}
