package postgres

import r "github.com/StuHelper/stuhelper-resources"

func (t *transaction) OwnedReferences(p r.Principal, ids []string) ([]string, error) {
	rows, e := t.tx.QueryContext(t.ctx, `SELECT requested.id FROM unnest($2::text[]) requested(id)
 WHERE EXISTS(SELECT 1 FROM resources.records r JOIN resources.versions v ON v.resource_id=r.id
 WHERE r.owner_key=$1 AND requested.id IN(v.data->'Version'->'metadata'->'associations'->>'school_id',
 v.data->'Version'->'metadata'->'associations'->>'course_id',v.data->'Version'->'metadata'->'associations'->>'term_id',
 v.data->'Version'->'metadata'->'associations'->>'offering_id',v.data->'Version'->'metadata'->'associations'->>'teacher_id'))
 ORDER BY requested.id`, p.Key(), ids)
	if e != nil {
		return nil, safe(e)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			return nil, safe(e)
		}
		out = append(out, id)
	}
	return out, safe(rows.Err())
}
