package postgres

import (
	"context"
	m "github.com/StuHelper/stuhelper-resources"
)

var _ m.OutboxReader = (*Repository)(nil)

const pendingOutboxSQL = `SELECT data FROM resources.outbox WHERE sent_at IS NULL AND NOT ((data->>'subject')=ANY(COALESCE($2::text[],ARRAY[]::text[]))) ORDER BY (data->>'source') COLLATE "C",(data->>'subject') COLLATE "C",seq LIMIT $1`

func (r *Repository) PendingOutbox(ctx context.Context, limit int, excludeSubjects []string) ([]m.Envelope, error) {
	if limit < 1 || limit > 100 || len(excludeSubjects) > 1000 {
		return nil, m.ErrInput
	}
	var out []m.Envelope
	err := r.Transaction(ctx, func(tx m.Transaction) error {
		var e error
		out, e = list[m.Envelope](tx.(*transaction), pendingOutboxSQL, limit, excludeSubjects)
		return e
	})
	return out, err
}
func (r *Repository) ConfirmOutbox(ctx context.Context, id string) error {
	if id == "" || len(id) > 200 {
		return m.ErrInput
	}
	result, e := r.DB.ExecContext(ctx, `UPDATE resources.outbox SET sent_at=COALESCE(sent_at,now()) WHERE id=$1`, id)
	if e != nil {
		return safe(e)
	}
	n, e := result.RowsAffected()
	if e != nil {
		return safe(e)
	}
	if n == 0 {
		return m.ErrNotFound
	}
	return nil
}
func (t *transaction) saveEnvelope(v m.Envelope) error {
	if v.SpecVersion != "1.0" || v.Source != "resources" || v.ID == "" || len(v.ID) > 200 || v.Subject == "" || len(v.Subject) > 200 || v.Type == "" || v.Time.IsZero() {
		return m.ErrInput
	}
	if e := t.save(`INSERT INTO resources.outbox(id,data) VALUES($1,$2)`, v, v.ID); e != nil {
		return e
	}
	_, e := t.tx.ExecContext(t.ctx, `SELECT pg_notify($1,'')`, m.OutboxChannel)
	return safe(e)
}
