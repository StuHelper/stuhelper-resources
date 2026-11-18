package resources

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
)

type CourseResourceCount struct {
	CourseID string `json:"course_id"`
	Count    int64  `json:"count"`
}
type ResourceGroupCount struct {
	Key   string `json:"key"`
	Value Value  `json:"value"`
	Count int64  `json:"count"`
}
type ResourceCounts struct {
	Total          int64                 `json:"total"`
	Courses        []CourseResourceCount `json:"courses"`
	GroupBy        string                `json:"group_by,omitempty"`
	Groups         []ResourceGroupCount  `json:"groups,omitempty"`
	UngroupedCount int64                 `json:"ungrouped_count,omitempty"`
}
type OwnResourceSummary struct {
	ResourceCount  int64 `json:"resource_count"`
	PublishedCount int64 `json:"published_count"`
}

// CourseCountCatalog resolves current school-scoped course references in one
// bounded public call. Aliases are owned by catalog, not inferred from titles.
type CourseCountCatalog interface {
	CourseReferences(context.Context, Principal, string, []string) (map[string][]string, error)
}

type BrowseTransaction interface {
	Browse(Filter) ([]Record, int64, error)
	ResourceCounts(Filter, map[string][]string) (ResourceCounts, error)
	OwnResourceSummary(string) (OwnResourceSummary, error)
}

type GroupCountsTransaction interface {
	ResourceCountsWithGroup(Filter, map[string][]string, *Field) (ResourceCounts, error)
}

type browseCursor struct {
	Scope string    `json:"scope"`
	ID    string    `json:"id"`
	At    time.Time `json:"at"`
}

func browseScope(f Filter) string {
	fields := []any{f.OwnerKey, f.IncludeUnpublished, f.SchoolID, f.CourseID, f.CategoryID, f.DirectoryID, f.TeacherID, f.TermID, f.Tag, f.Access, f.Query, f.Attributes, f.Sort, f.FileType}
	if f.EntitledKey != "" {
		fields = append(fields, f.EntitledKey)
	}
	data, _ := json.Marshal(fields)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func validBrowseFilters(f Filter) error {
	if !optionalID(f.TeacherID) || !optionalID(f.TermID) {
		return ErrInput
	}
	if f.Tag != "" && (strings.TrimSpace(f.Tag) == "" || !text(f.Tag, 64)) {
		return ErrInput
	}
	if f.Access != "" && f.Access != "public" && f.Access != "restricted" {
		return ErrInput
	}
	return nil
}

func (s *Service) validateFilterAssociations(ctx context.Context, a Associations) error {
	if a.TermID == "" && a.TeacherID == "" {
		return nil
	}
	work, cancel := context.WithTimeout(ctx, s.cfg.OperationTimeout)
	defer cancel()
	if e := s.deps.Catalog.Validate(work, a); e != nil {
		if errors.Is(e, ErrInput) || errors.Is(e, ErrNotFound) {
			return ErrInput
		}
		if errors.Is(e, ErrDenied) {
			return ErrDenied
		}
		return ErrUnavailable
	}
	return nil
}

func validFileType(value string) bool {
	if len(value) > 12 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func prepareBrowse(f *Filter) error {
	if f.Sort == "" {
		f.Sort = "id"
	}
	if !slices.Contains([]string{"id", "newest"}, f.Sort) || !validFileType(f.FileType) || len(f.After) > 2000 {
		return ErrInput
	}
	f.CursorTime, f.CursorID = nil, ""
	if f.Sort == "id" {
		if !optionalID(f.After) {
			return ErrInput
		}
		return nil
	}
	if f.After == "" {
		return nil
	}
	data, err := base64.RawURLEncoding.Strict().DecodeString(f.After)
	var cursor browseCursor
	if err != nil || json.Unmarshal(data, &cursor) != nil || cursor.Scope != browseScope(*f) || !validID(cursor.ID) || cursor.At.IsZero() {
		return ErrInput
	}
	f.CursorTime, f.CursorID = &cursor.At, cursor.ID
	return nil
}

func encodeBrowseCursor(f Filter, record Record) (string, error) {
	if f.Sort == "id" {
		return record.ID, nil
	}
	// The repository records the selected version time in a private transient
	// field so pagination never depends on the viewer's proposal projection.
	if record.BrowseAt.IsZero() {
		return "", ErrUnavailable
	}
	data, err := json.Marshal(browseCursor{Scope: browseScope(f), ID: record.ID, At: record.BrowseAt})
	if err != nil {
		return "", ErrUnavailable
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func (s *Service) courseReferences(ctx context.Context, p Principal, school string, ids []string) (map[string][]string, error) {
	port, ok := s.deps.Catalog.(CourseCountCatalog)
	if !ok {
		return nil, ErrUnavailable
	}
	work, cancel := context.WithTimeout(ctx, s.cfg.OperationTimeout)
	defer cancel()
	refs, err := port.CourseReferences(work, p, school, ids)
	if err != nil {
		return nil, normalizeDepError(ctx, err)
	}
	for _, id := range ids {
		if len(refs[id]) == 0 || len(refs[id]) > 1000 {
			return nil, ErrUnavailable
		}
		for _, alias := range refs[id] {
			if !validID(alias) {
				return nil, ErrUnavailable
			}
		}
	}
	return refs, nil
}

// Counts counts discoverable published metadata, the same set as the public
// list. Download rights are evaluated separately by the existing download API.
func (s *Service) Counts(ctx context.Context, p Principal, f Filter, courses []string) (ResourceCounts, error) {
	out := ResourceCounts{Courses: []CourseResourceCount{}}
	if !p.Valid() {
		return out, ErrUnauthenticated
	}
	if !validID(f.SchoolID) || !optionalID(f.CourseID) || !optionalID(f.CategoryID) || !optionalID(f.DirectoryID) || !text(f.Query, 200) || !validFileType(f.FileType) || validBrowseFilters(f) != nil || len(courses) > 100 || len(f.Attributes) > 0 && f.CategoryID == "" {
		return out, ErrInput
	}
	courses = slices.Clone(courses)
	slices.Sort(courses)
	courses = slices.Compact(courses)
	for _, id := range courses {
		if !validID(id) {
			return out, ErrInput
		}
	}
	if err := s.auth(ctx, p, "discover", f.SchoolID); err != nil {
		return out, err
	}
	if err := s.validateFilterAssociations(ctx, Associations{SchoolID: f.SchoolID, TermID: f.TermID, TeacherID: f.TeacherID}); err != nil {
		return out, err
	}
	refs := map[string][]string{}
	var courseIDs []string
	lookup := slices.Clone(courses)
	_, hasBatchCatalog := s.deps.Catalog.(CourseCountCatalog)
	if f.CourseID != "" && hasBatchCatalog {
		lookup = append(lookup, f.CourseID)
		slices.Sort(lookup)
		lookup = slices.Compact(lookup)
	}
	if len(courses) != 0 || len(lookup) != 0 {
		resolved, err := s.courseReferences(ctx, p, f.SchoolID, lookup)
		if err != nil {
			return out, err
		}
		for _, id := range courses {
			refs[id] = resolved[id]
		}
		if f.CourseID != "" {
			courseIDs = resolved[f.CourseID]
		}
	}
	// Exclude all non-HTTP filter state from this public aggregate boundary.
	f = Filter{SchoolID: f.SchoolID, CourseID: f.CourseID, CourseIDs: courseIDs, CategoryID: f.CategoryID, DirectoryID: f.DirectoryID, TeacherID: f.TeacherID, TermID: f.TermID, Tag: f.Tag, Access: f.Access, Query: f.Query, FileType: f.FileType, Attributes: f.Attributes}
	err := s.repo.Transaction(ctx, func(tx Transaction) error {
		var groupField *Field
		if f.CategoryID != "" {
			category, err := tx.Category(f.CategoryID, 0)
			if err == nil {
				if len(f.Attributes) > 0 {
					if err = ValidateAttributes(category, f.Attributes, true); err != nil {
						return err
					}
				}
				if category.GroupBy != "" {
					for i := range category.Fields {
						if category.Fields[i].Key == category.GroupBy && category.Fields[i].Groupable {
							field := category.Fields[i]
							groupField = &field
							break
						}
					}
				}
			} else if errors.Is(err, ErrNotFound) {
				if len(f.Attributes) > 0 {
					return ErrNotFound
				}
			} else {
				return err
			}
		}
		if groupPort, ok := tx.(GroupCountsTransaction); ok {
			var err error
			out, err = groupPort.ResourceCountsWithGroup(f, refs, groupField)
			if err == nil && groupField != nil {
				out.GroupBy = groupField.Key
			}
			return err
		}
		port, ok := tx.(BrowseTransaction)
		if !ok {
			return ErrUnavailable
		}
		var err error
		out, err = port.ResourceCounts(f, refs)
		return err
	})
	return out, err
}

func (s *Service) OwnSummary(ctx context.Context, p Principal) (OwnResourceSummary, error) {
	var out OwnResourceSummary
	if !p.Valid() {
		return out, ErrUnauthenticated
	}
	err := s.repo.Transaction(ctx, func(tx Transaction) error {
		port, ok := tx.(BrowseTransaction)
		if !ok {
			return ErrUnavailable
		}
		var err error
		out, err = port.OwnResourceSummary(p.Key())
		return err
	})
	return out, err
}
