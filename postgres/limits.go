package postgres

import (
	"database/sql"
	"errors"
	r "github.com/StuHelper/stuhelper-resources"
	"time"
)

func (t *transaction) LimitSubmission(owner string, now time.Time) error {
	var used int
	err := t.tx.QueryRowContext(t.ctx, `INSERT INTO resources.submission_limits(owner_key,started_at,used) VALUES($1,$2,1) ON CONFLICT(owner_key) DO UPDATE SET started_at=CASE WHEN resources.submission_limits.started_at <= $2-interval '1 minute' THEN $2 ELSE resources.submission_limits.started_at END,used=CASE WHEN resources.submission_limits.started_at <= $2-interval '1 minute' THEN 1 ELSE resources.submission_limits.used+1 END WHERE resources.submission_limits.started_at <= $2-interval '1 minute' OR resources.submission_limits.used<10 RETURNING used`, owner, now).Scan(&used)
	if errors.Is(err, sql.ErrNoRows) {
		return r.ErrRateLimited
	}
	return safe(err)
}
