package postgres

import r "github.com/StuHelper/stuhelper-resources"

func (t *transaction) CloseOwner(p r.Principal, id string) error {
	if e := t.Lock("closure-owner:" + p.Key()); e != nil {
		return e
	}
	_, e := t.tx.ExecContext(t.ctx, `INSERT INTO resources.closed_owners(owner_key,closure_id)VALUES($1,$2)ON CONFLICT(owner_key)DO NOTHING`, p.Key(), id)
	if e != nil {
		return safe(e)
	}
	var saved string
	if e = t.tx.QueryRowContext(t.ctx, `SELECT closure_id FROM resources.closed_owners WHERE owner_key=$1`, p.Key()).Scan(&saved); e != nil {
		return safe(e)
	}
	if saved != id {
		return r.ErrConflict
	}
	return nil
}
func (t *transaction) closedRecord(v r.Record) error {
	if e := t.Lock("closure-owner:" + v.Owner.Key()); e != nil {
		return e
	}
	var closed, newRecord bool
	e := t.tx.QueryRowContext(t.ctx, `SELECT EXISTS(SELECT 1 FROM resources.closed_owners WHERE owner_key=$1),NOT EXISTS(SELECT 1 FROM resources.records WHERE id=$2)`, v.Owner.Key(), v.ID).Scan(&closed, &newRecord)
	if e != nil {
		return safe(e)
	}
	if closed && newRecord {
		return r.ErrDenied
	}
	return nil
}
