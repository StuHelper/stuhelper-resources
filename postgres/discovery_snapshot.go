package postgres

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	c "github.com/StuHelper/stuhelper-resources"
	"strings"
	"time"
)

var _ c.DiscoverySource = (*Repository)(nil)

type discoveryCursor struct {
	Visibility string
	Epoch      int64
	After      string
	Expires    time.Time
}

// DiscoverySnapshot preserves object versions using transaction visibility on
// immutable history. Retention invalidates existing cursors, never silently resets.
func (r *Repository) DiscoverySnapshot(ctx context.Context, cursor string, limit int) (out c.DiscoveryPage, err error) {
	out.Items = []c.DiscoveryDocument{}
	if limit < 1 || limit > 100 || len(cursor) > 16384 {
		return out, c.ErrInput
	}
	state := discoveryCursor{}
	if cursor != "" {
		b, e := base64.RawURLEncoding.DecodeString(cursor)
		if e != nil || json.Unmarshal(b, &state) != nil || (state.After == "" || len(state.After) > 200) || state.Visibility == "" || !time.Now().Before(state.Expires) {
			return out, c.ErrInput
		}
	}
	tx, e := r.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if e != nil {
		return out, safe(e)
	}
	defer tx.Rollback()
	var epoch int64
	if e = tx.QueryRowContext(ctx, `SELECT revision FROM resources.discovery_retention WHERE singleton`).Scan(&epoch); e != nil {
		return out, safe(e)
	}
	if cursor == "" {
		state.Epoch = epoch
		state.Expires = time.Now().Add(5 * time.Minute)
		if e = tx.QueryRowContext(ctx, `SELECT pg_current_snapshot()::text`).Scan(&state.Visibility); e != nil {
			return out, safe(e)
		}
	} else if epoch != state.Epoch {
		return out, c.ErrUnavailable
	}
	if len(state.Visibility) > 8192 || strings.ContainsAny(state.Visibility, "\n\r") {
		return out, c.ErrUnavailable
	}
	t := &transaction{tx: tx, ctx: ctx}
	rows, e := list[c.DiscoveryDocument](t, `SELECT data FROM (SELECT DISTINCT ON (id COLLATE "C") id,data FROM resources.discovery_history WHERE id COLLATE "C">$2 COLLATE "C" AND pg_visible_in_snapshot(recorded_xid,$1::pg_snapshot) ORDER BY id COLLATE "C",revision DESC) latest ORDER BY id COLLATE "C" LIMIT $3`, state.Visibility, state.After, limit+1)
	if e != nil {
		return out, e
	}
	if len(rows) > limit {
		rows = rows[:limit]
		state.After = rows[len(rows)-1].Target.ID
		b, e := json.Marshal(state)
		if e != nil {
			return out, c.ErrUnavailable
		}
		out.Next = base64.RawURLEncoding.EncodeToString(b)
	}
	out.Items = rows
	return out, safe(tx.Commit())
}
