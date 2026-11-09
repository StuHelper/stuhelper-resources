package postgres

import (
	"context"
	"encoding/json"
	aq "github.com/StuHelper/stuhelper-resources/accountquery"
	"sort"
)

// ReadAccount filters authoritative module facts before the stable exclusive page.
func (r *Repository) ReadAccount(ctx context.Context, s aq.Selection) ([]aq.Record, error) {
	scopes, _ := json.Marshal(s.Scopes)
	sensitive, _ := json.Marshal(s.SensitiveScopes)
	rows, err := r.DB.QueryContext(ctx, accountQuerySQL, s.Query.Value, s.Query.After, scopes, aq.Domain, s.Query.Limit, sensitive)
	if err != nil {
		return nil, aq.ErrUnavailable
	}
	defer rows.Close()
	out := []aq.Record{}
	for rows.Next() {
		var v aq.Record
		var fields []byte
		var secret string
		if err = rows.Scan(&v.ID, &v.Scope.School, &v.Scope.Group, &v.ChangedAt, &v.Current, &fields, &secret); err != nil {
			return nil, aq.ErrUnavailable
		}
		v.Scope.Module = aq.Domain
		v.Subject = s.Query.Value
		v.Source = aq.Domain
		var values map[string]*string
		if json.Unmarshal(fields, &values) != nil {
			return nil, aq.ErrUnavailable
		}
		names := []string{}
		for name := range values {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if values[name] != nil {
				v.Fields = append(v.Fields, aq.Field{Name: name, Category: "business", Value: *values[name]})
			}
		}

		out = append(out, v)
	}
	if rows.Err() != nil {
		return nil, aq.ErrUnavailable
	}
	return out, nil
}

const accountQuerySQL = `WITH records AS (SELECT 'resources:upload:'||r.id AS id,r.school_id AS school,'*'::text AS grp,
 r.query_changed_at AS changed,true AS current,
 jsonb_build_object('kind','upload','resource_id',r.id,'state',r.state) AS fields,''::text AS sensitive
 FROM resources.records r WHERE r.owner_key=$1
 UNION ALL
 SELECT 'resources:unlock:'||g.id,r.school_id,'*',g.query_changed_at,
 COALESCE(g.data->'Grant'->>'revoked','false')<>'true' AND (g.data->'Grant'->>'expires_at' IS NULL OR (g.data->'Grant'->>'expires_at')::timestamptz>now()),
 jsonb_build_object('kind','unlock','resource_id',r.id,'revoked',g.data->'Grant'->>'revoked','expires_at',g.data->'Grant'->>'expires_at'),''
 FROM resources.grants g JOIN resources.records r ON r.id=g.resource_id WHERE g.subject_key=$1) SELECT id,school,grp,changed,current,fields,sensitive FROM records
 WHERE id COLLATE "C">$2 COLLATE "C"
 AND EXISTS (SELECT 1 FROM jsonb_array_elements($3::jsonb) s WHERE (s->>'school'='*' OR s->>'school'=school) AND (s->>'group'='*' OR s->>'group'=grp))
 AND $4::text='resources' AND $1::text<>'' AND $6::jsonb IS NOT NULL
 ORDER BY id COLLATE "C" LIMIT $5`
