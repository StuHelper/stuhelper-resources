package postgres

import (
	"context"
	"time"
)

// RecordClosureCompletion receives the durable completion fact from the host.
func (r *Repository) RecordClosureCompletion(ctx context.Context, owner, id string, at time.Time) error {
	_, err := r.DB.ExecContext(ctx, "SELECT resources.mark_closure_completed($1,$2,$3)", owner, id, at)
	return err
}
func (r *Repository) PruneClosureMarkers(ctx context.Context, limit int) (int64, error) {
	var n int64
	err := r.DB.QueryRowContext(ctx, "SELECT resources.prune_closure_markers($1)", limit).Scan(&n)
	return n, err
}

// PurgeTransferredComplaint consumes the terminal retention decision from moderation.
func (r *Repository) PurgeTransferredComplaint(ctx context.Context, task string) error {
	_, err := r.DB.ExecContext(ctx, "SELECT resources.purge_transferred_complaint($1)", task)
	return safe(err)
}
