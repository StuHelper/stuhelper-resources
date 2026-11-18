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

type ownerClosureTransaction interface {
	OwnerClosed(Principal) (bool, error)
}

func checkTxOwnerActive(tx Transaction, p Principal) error {
	port, ok := tx.(ownerClosureTransaction)
	if !ok {
		return nil
	}
	closed, err := port.OwnerClosed(p)
	if err != nil {
		return err
	}
	if closed {
		return ErrDenied
	}
	return nil
}

func (s *Service) checkOwnerActive(ctx context.Context, p Principal) error {
	if !p.Valid() {
		return ErrUnauthenticated
	}
	return s.repo.Transaction(ctx, func(tx Transaction) error {
		return checkTxOwnerActive(tx, p)
	})
}
