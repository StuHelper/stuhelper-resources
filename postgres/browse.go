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
	} else if f.EntitledKey != "" {
		ownerParam := bind(f.EntitledKey)
		now := time.Now().UTC()
		if f.EntitledNow != nil {
			now = *f.EntitledNow
		}
		nowParam := bind(now)
		conditions = append(conditions,
			"r.active_id<>''",
			"v.data->'Version'->>'state'='published'",
			"COALESCE(v.data->>'ReferenceID','')<>''",
			"r.school_id=ANY("+bind(f.AllowedSchools)+"::text[])",
			"((r.state IN ('published','unlisted') AND EXISTS(SELECT 1 FROM resources.purchases p WHERE p.owner_key="+ownerParam+" AND p.resource_id=r.id AND p.data->>'revoked'='false')) OR (r.state='published' AND EXISTS(SELECT 1 FROM resources.grants g WHERE g.subject_key="+ownerParam+" AND g.resource_id=r.id AND COALESCE(g.data->'Grant'->>'revoked','false')<>'true' AND (g.data->'Grant'->>'expires_at' IS NULL OR (g.data->'Grant'->>'expires_at')::timestamptz>"+nowParam+"))))",
		)
	} else {
		conditions = append(conditions, "r.state='published'", "v.data->'Version'->>'state'='published'")
	}
	if f.SchoolID != "" {
		conditions = append(conditions, "r.school_id="+bind(f.SchoolID))
	}
	if f.State != "" {
		conditions = append(conditions, "r.state="+bind(f.State))
	}
	if f.ProposalState != "" {
		conditions = append(conditions, "r.proposal_id<>'' AND EXISTS(SELECT 1 FROM resources.versions pv WHERE pv.id=r.proposal_id AND pv.data->'Version'->>'state'="+bind(f.ProposalState)+")")
	}
	if f.FailedOnly {
		conditions = append(conditions, "r.proposal_id<>'' AND EXISTS(SELECT 1 FROM resources.jobs j JOIN resources.versions pv ON pv.id=r.proposal_id WHERE j.state='failed' AND j.data->>'ResourceID'=r.id AND j.data->>'VersionID'=r.proposal_id AND j.data->>'Kind'=CASE pv.data->'Version'->>'state' WHEN 'pending' THEN 'review' WHEN 'preparing' THEN 'publish' END)")
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
	for _, field := range []struct{ column, value string }{{"category_id", f.CategoryID}, {"directory_id", f.DirectoryID}, {"access", f.Access}} {
		if field.value != "" {
			conditions = append(conditions, "v.data->'Version'->'metadata'->>'"+field.column+"'="+bind(field.value))
		}
	}
	for _, assoc := range []struct{ column, value string }{{"teacher_id", f.TeacherID}, {"term_id", f.TermID}} {
		if assoc.value != "" {
			conditions = append(conditions, "v.data->'Version'->'metadata'->'associations'->>'"+assoc.column+"'="+bind(assoc.value))
		}
	}
	if f.Tag != "" {
		conditions = append(conditions, "COALESCE(v.data->'Version'->'metadata'->'tags','[]'::jsonb) @> jsonb_build_array("+bind(f.Tag)+"::text)")
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
	return t.ResourceCountsWithGroup(f, references, nil)
}

func (t *transaction) ResourceCountsWithGroup(f r.Filter, references map[string][]string, groupField *r.Field) (r.ResourceCounts, error) {
	projection := "v.data->'Version'->'metadata'->'associations'->>'course_id' AS course_id"
	base, args := resourceBrowseBase(f, projection)
	bind := func(value any) string { args = append(args, value); return fmt.Sprintf("$%d", len(args)) }
	if groupField != nil {
		keyParam := bind(groupField.Key)
		base = strings.Replace(base, "SELECT "+projection+" FROM ", "SELECT "+projection+", v.data->'Version'->'metadata'->'attributes'->("+keyParam+"::text) AS group_attr FROM ", 1)
	}
	refs, err := json.Marshal(references)
	if err != nil {
		return r.ResourceCounts{}, r.ErrInput
	}
	refsParam := bind(string(refs))
	coursesJSON := "COALESCE((SELECT jsonb_agg(jsonb_build_object('course_id',key,'count',n) ORDER BY key COLLATE \"C\") FROM (SELECT req.key,COALESCE(sum(c.n),0) AS n FROM requested req " +
		"LEFT JOIN counts c ON req.value ? c.course_id GROUP BY req.key) rows),'[]'::jsonb)"
	if groupField == nil {
		query := "WITH matched AS MATERIALIZED (" + base + "), counts AS (SELECT course_id,count(*) AS n FROM matched GROUP BY course_id), " +
			"requested AS (SELECT key,value FROM jsonb_each(" + refsParam + "::jsonb)) SELECT jsonb_build_object('total',(SELECT count(*) FROM matched),'courses'," + coursesJSON + ")"
		return read[r.ResourceCounts](t, query, args...)
	}
	var groupsCTE, groupsAgg, ungroupedExpr string
	switch groupField.Kind {
	case "integer":
		groupsCTE = "SELECT ((group_attr->>'number')::bigint)::text AS k, jsonb_build_object('number',(group_attr->>'number')::bigint) AS v, count(*) AS n, (group_attr->>'number')::bigint AS ord " +
			"FROM matched WHERE group_attr IS NOT NULL AND group_attr ? 'number' AND jsonb_typeof(group_attr->'number')='number' GROUP BY (group_attr->>'number')::bigint"
		groupsAgg = "COALESCE((SELECT jsonb_agg(jsonb_build_object('key',k,'value',v,'count',n) ORDER BY ord DESC) FROM grouped),'[]'::jsonb)"
		ungroupedExpr = "(SELECT count(*) FROM matched WHERE group_attr IS NULL OR NOT (group_attr ? 'number') OR jsonb_typeof(group_attr->'number')<>'number')"
	case "choice":
		optionIDs := make([]string, 0, len(groupField.Options))
		for _, opt := range groupField.Options {
			optionIDs = append(optionIDs, opt.ID)
		}
		optParam := bind(optionIDs)
		groupsCTE = "SELECT (group_attr->>'option') AS k, jsonb_build_object('option',group_attr->>'option') AS v, count(*) AS n, array_position(" + optParam + "::text[],group_attr->>'option') AS ord " +
			"FROM matched WHERE group_attr->>'option' = ANY(" + optParam + "::text[]) GROUP BY (group_attr->>'option')"
		groupsAgg = "COALESCE((SELECT jsonb_agg(jsonb_build_object('key',k,'value',v,'count',n) ORDER BY ord ASC NULLS LAST, k COLLATE \"C\" ASC) FROM grouped),'[]'::jsonb)"
		ungroupedExpr = "(SELECT count(*) FROM matched WHERE (group_attr->>'option' = ANY(" + optParam + "::text[])) IS NOT TRUE)"
	case "text":
		groupsCTE = "SELECT (group_attr->>'text') AS k, jsonb_build_object('text',group_attr->>'text') AS v, count(*) AS n " +
			"FROM matched WHERE COALESCE(group_attr->>'text','')<>'' GROUP BY (group_attr->>'text')"
		groupsAgg = "COALESCE((SELECT jsonb_agg(jsonb_build_object('key',k,'value',v,'count',n) ORDER BY k COLLATE \"C\" ASC) FROM grouped),'[]'::jsonb)"
		ungroupedExpr = "(SELECT count(*) FROM matched WHERE COALESCE(group_attr->>'text','')='')"
	default:
		return r.ResourceCounts{}, r.ErrInput
	}
	query := "WITH matched AS MATERIALIZED (" + base + "), counts AS (SELECT course_id,count(*) AS n FROM matched GROUP BY course_id), " +
		"requested AS (SELECT key,value FROM jsonb_each(" + refsParam + "::jsonb)), grouped AS (" + groupsCTE + ") " +
		"SELECT jsonb_build_object('total',(SELECT count(*) FROM matched),'courses'," + coursesJSON + ",'group_by'," + bind(groupField.Key) + "::text,'groups'," + groupsAgg + ",'ungrouped_count'," + ungroupedExpr + ")"
	return read[r.ResourceCounts](t, query, args...)
}

func (t *transaction) OwnResourceSummary(owner string) (r.OwnResourceSummary, error) {
	return read[r.OwnResourceSummary](t, `SELECT jsonb_build_object('resource_count',count(*),'published_count',count(*) FILTER(WHERE r.state='published'))
 FROM resources.records r JOIN resources.versions v ON v.id=COALESCE(NULLIF(r.proposal_id,''),r.active_id) WHERE r.owner_key=$1`, owner)
}

func (t *transaction) DuplicateResources(school string, courseIDs []string, sha256, ownerKey string, limit int) ([]r.DuplicateResource, error) {
	return list[r.DuplicateResource](t, `SELECT jsonb_build_object(
 'id', r.id,
 'version_id', v.id,
 'title', v.data->'Version'->'metadata'->>'title',
 'category_id', v.data->'Version'->'metadata'->>'category_id',
 'access', v.data->'Version'->'metadata'->>'access',
 'filename', v.data->'Version'->'file'->>'filename',
 'size', (v.data->'Version'->'file'->>'size')::bigint,
 'owned', r.owner_key=$4,
 'created_at', (v.data->'Version'->>'created_at')::timestamptz
) FROM resources.records r JOIN resources.versions v ON v.id=r.active_id
 WHERE r.school_id=$1 AND r.state='published'
 AND v.data->'Version'->>'state'='published'
 AND v.data->'Version'->'metadata'->'associations'->>'school_id'=$1
 AND v.data->'Version'->'file'->>'sha256'=$2
 AND v.data->'Version'->'metadata'->'associations'->>'course_id'=ANY($3::text[])
 ORDER BY (v.data->'Version'->>'created_at')::timestamptz DESC, r.id COLLATE "C" DESC LIMIT $5`, school, sha256, courseIDs, ownerKey, limit)
}
