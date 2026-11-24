package postgres

import (
	"context"
	"database/sql"
	"errors"
)

// MaintainDiscoveryHistory scans at most one bounded page and returns terminal
// revisions with affirmative delivery evidence. Each retry gets a later turn.
func (r *Repository) MaintainDiscoveryHistory(ctx context.Context, limit int) (map[string]int64, map[string]int64, error) {
	if _, err := r.DB.ExecContext(ctx, "SELECT resources.maintain_discovery_history($1)", limit); err != nil {
		return nil, nil, safe(err)
	}
	rows, err := r.DB.QueryContext(ctx, "SELECT * FROM resources.claim_discovery_retirements($1)", limit)
	if err != nil {
		return nil, nil, safe(err)
	}
	defer rows.Close()
	candidates, sealed := map[string]int64{}, map[string]int64{}
	for rows.Next() {
		var id string
		var rev int64
		var finished bool
		if err := rows.Scan(&id, &rev, &finished); err != nil {
			return nil, nil, safe(err)
		}
		if finished {
			sealed[id] = rev
		} else {
			candidates[id] = rev
		}
	}
	return candidates, sealed, safe(rows.Err())
}
func (r *Repository) SealDiscoveryRetirement(ctx context.Context, id string, rev int64) (bool, error) {
	var ok bool
	err := r.DB.QueryRowContext(ctx, "SELECT resources.seal_discovery_retirement($1,$2)", id, rev).Scan(&ok)
	return ok, safe(err)
}
func (r *Repository) AcknowledgeDiscoveryRetirement(ctx context.Context, id string, rev int64) error {
	_, err := r.DB.ExecContext(ctx, "SELECT resources.ack_discovery_retirement($1,$2)", id, rev)
	return safe(err)
}
func (r *Repository) CurrentDiscoveryRevision(ctx context.Context, id string) (int64, error) {
	query := "SELECT revision FROM resources.discovery_history WHERE id=$1 ORDER BY revision DESC LIMIT 1"

	var rev int64
	err := r.DB.QueryRowContext(ctx, query, id).Scan(&rev)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return rev, safe(err)
}
