package postgres

import (
	"encoding/json"
	m "github.com/StuHelper/stuhelper-resources"
	"time"
)

var _ m.BatchReadTransaction = (*transaction)(nil)

func (t *transaction) RecordsByIDs(ids []string) (map[string]m.Record, error) {
	out := make(map[string]m.Record, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := list[m.Record](t, `SELECT data FROM resources.records WHERE id=ANY($1::text[])`, ids)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.ID] = row
	}
	return out, nil
}

func (t *transaction) VersionsByIDs(ids []string) (map[string]m.VersionRecord, error) {
	out := make(map[string]m.VersionRecord, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := list[m.VersionRecord](t, `SELECT data FROM resources.versions WHERE id=ANY($1::text[])`, ids)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.Version.ID] = row
	}
	return out, nil
}

func (t *transaction) AccessFactsBatch(owner string, now time.Time, items []m.AccessFactTarget) (map[string]m.AccessFacts, error) {
	out := make(map[string]m.AccessFacts, len(items))
	if len(items) == 0 || owner == "" {
		return out, nil
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		return nil, m.ErrInput
	}
	rows, err := t.tx.QueryContext(t.ctx, `SELECT t.resource_id, COALESCE(g.data, 'null'::jsonb),
 EXISTS(SELECT 1 FROM resources.purchases p WHERE p.owner_key=$1 AND p.resource_id=t.resource_id AND p.data->>'revoked'='false'),
 EXISTS(SELECT 1 FROM resources.memberships mb JOIN resources.coverage c ON c.id=mb.plan_id CROSS JOIN LATERAL jsonb_array_elements(c.data->'rules') rule
  WHERE mb.owner_key=$1 AND (mb.data->>'valid_from')::timestamptz<=$2 AND (mb.data->>'valid_until')::timestamptz>$2
  AND rule->>'school_id'=t.school_id AND (COALESCE(rule->>'directory_id','')='' OR rule->>'directory_id'=t.directory_id) AND (COALESCE(rule->>'category_id','')='' OR rule->>'category_id'=t.category_id))
 FROM jsonb_to_recordset($3::jsonb) AS t(resource_id text, school_id text, directory_id text, category_id text)
 LEFT JOIN resources.grants g ON g.resource_id=t.resource_id AND g.subject_key=$1`, owner, now, string(encoded))
	if err != nil {
		return nil, safe(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var rawGrant []byte
		var facts m.AccessFacts
		if err = rows.Scan(&id, &rawGrant, &facts.Purchased, &facts.Member); err != nil {
			return nil, safe(err)
		}
		if len(rawGrant) > 0 && string(rawGrant) != "null" {
			var gr m.GrantRecord
			if json.Unmarshal(rawGrant, &gr) != nil {
				return nil, m.ErrUnavailable
			}
			facts.Granted = !gr.Grant.Revoked && (gr.Grant.ExpiresAt == nil || now.Before(*gr.Grant.ExpiresAt))
		}
		out[id] = facts
	}
	return out, safe(rows.Err())
}
