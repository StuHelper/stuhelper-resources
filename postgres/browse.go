package postgres

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	r "github.com/StuHelper/stuhelper-resources"
)

// Every browse/count path uses this predicate builder. All values are bound;
// column names and SQL fragments are fixed module-owned strings.
func resourceBrowseBase(f r.Filter, projection string) (string, []any) {
	args := []any{}
	bind := func(value any) string { args = append(args, value); return fmt.Sprintf("$%d", len(args)) }
	version := "r.active_id"
	conditions := []string{}
	if f.IncludeUnpublished {
		version = "COALESCE(NULLIF(r.proposal_id,''),r.active_id)"
	} else {
		conditions = append(conditions, "r.state='published'", "v.data->'Version'->>'state'='published'")
	}
	if f.SchoolID != "" {
		conditions = append(conditions, "r.school_id="+bind(f.SchoolID))
	}
	if f.OwnerKey != "" {
		conditions = append(conditions, "r.owner_key="+bind(f.OwnerKey))
	}
	if f.CourseID != "" {
		ids := f.CourseIDs
		if len(ids) == 0 {
			ids = []string{f.CourseID}
		}
		conditions = append(conditions, "v.data->'Version'->'metadata'->'associations'->>'course_id'=ANY("+bind(ids)+"::text[])")
	}
	for _, field := range []struct{ column, value string }{{"category_id", f.CategoryID}, {"directory_id", f.DirectoryID}} {
		if field.value != "" {
			conditions = append(conditions, "v.data->'Version'->'metadata'->>'"+field.column+"'="+bind(field.value))
		}
	}
	if f.Query != "" {
		p := bind(f.Query)
		conditions = append(conditions, "(strpos(lower(v.data->'Version'->'metadata'->>'title'),lower("+p+"))>0 OR strpos(lower(v.data->'Version'->'metadata'->>'description'),lower("+p+"))>0)")
	}
	if f.FileType != "" {
		conditions = append(conditions, `lower(substring(v.data->'Version'->'file'->>'filename' from '\.([^.]+)$'))=`+bind(f.FileType))
	}
	if len(f.Attributes) > 0 {
		attrs, _ := json.Marshal(f.Attributes) // Value only contains JSON-native fields.
		conditions = append(conditions, "COALESCE(v.data->'Version'->'metadata'->'attributes','{}'::jsonb) @> "+bind(string(attrs))+"::jsonb")
	}
	where := "TRUE"
	if len(conditions) != 0 {
		where = strings.Join(conditions, " AND ")
	}
	return "SELECT " + projection + " FROM resources.records r JOIN resources.versions v ON v.id=" + version + " WHERE " + where, args
}

func (t *transaction) Browse(f r.Filter) ([]r.Record, int64, error) {
	base, args := resourceBrowseBase(f, "r.id,(v.data->'Version'->>'created_at')::timestamptz AS at")
	bind := func(value any) string { args = append(args, value); return fmt.Sprintf("$%d", len(args)) }
	cursor, order, pageOrder := `id COLLATE "C">`+bind(f.After), `id COLLATE "C"`, `page.id COLLATE "C"`
	if f.Sort == "newest" {
		// Replace the unused ID parameter instead of leaving an untyped bind.
		args = args[:len(args)-1]
		at := bind(f.CursorTime)
		cursor = "(" + at + `::timestamptz IS NULL OR (at,id COLLATE "C")<(` + at + "," + bind(f.CursorID) + ` COLLATE "C"))`
		order = `at DESC,id COLLATE "C" DESC`
		pageOrder = `page.at DESC,page.id COLLATE "C" DESC`
	}
	query := "WITH matched AS MATERIALIZED (" + base + "), page AS (SELECT id,at FROM matched WHERE " + cursor + " ORDER BY " + order + " LIMIT " + bind(f.Limit+1) + ") " +
		"SELECT COALESCE((SELECT jsonb_agg(jsonb_build_object('record',rec.data,'at',page.at) ORDER BY " + pageOrder + ") FROM page JOIN resources.records rec ON rec.id=page.id),'[]'::jsonb),(SELECT count(*) FROM matched)"
	var raw []byte
	var total int64
	if err := t.tx.QueryRowContext(t.ctx, query, args...).Scan(&raw, &total); err != nil {
		return nil, 0, safe(err)
	}
	var rows []struct {
		Record r.Record  `json:"record"`
		At     time.Time `json:"at"`
	}
	if json.Unmarshal(raw, &rows) != nil {
		return nil, 0, r.ErrUnavailable
	}
	out := make([]r.Record, 0, len(rows))
	for _, row := range rows {
		row.Record.BrowseAt = row.At
		out = append(out, row.Record)
	}
	return out, total, nil
}

func (t *transaction) ResourceCounts(f r.Filter, references map[string][]string) (r.ResourceCounts, error) {
	base, args := resourceBrowseBase(f, "v.data->'Version'->'metadata'->'associations'->>'course_id' AS course_id")
	refs, err := json.Marshal(references)
	if err != nil {
		return r.ResourceCounts{}, r.ErrInput
	}
	args = append(args, string(refs))
	query := "WITH matched AS MATERIALIZED (" + base + "), counts AS (SELECT course_id,count(*) AS n FROM matched GROUP BY course_id), " +
		"requested AS (SELECT key,value FROM jsonb_each($" + fmt.Sprint(len(args)) + "::jsonb)) SELECT jsonb_build_object('total',(SELECT count(*) FROM matched),'courses'," +
		`COALESCE((SELECT jsonb_agg(jsonb_build_object('course_id',key,'count',n) ORDER BY key COLLATE "C") FROM (SELECT req.key,COALESCE(sum(c.n),0) AS n FROM requested req ` +
		"LEFT JOIN counts c ON req.value ? c.course_id GROUP BY req.key) rows),'[]'::jsonb))"
	return read[r.ResourceCounts](t, query, args...)
}

func (t *transaction) OwnResourceSummary(owner string) (r.OwnResourceSummary, error) {
	return read[r.OwnResourceSummary](t, `SELECT jsonb_build_object('resource_count',count(*),'published_count',count(*) FILTER(WHERE r.state='published'))
 FROM resources.records r JOIN resources.versions v ON v.id=COALESCE(NULLIF(r.proposal_id,''),r.active_id) WHERE r.owner_key=$1`, owner)
}
