package resources

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"slices"
	"time"
)

type CourseResourceCount struct {
	CourseID string `json:"course_id"`
	Count    int64  `json:"count"`
}
type ResourceCounts struct {
	Total   int64                 `json:"total"`
	Courses []CourseResourceCount `json:"courses"`
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

type browseCursor struct {
	Scope string    `json:"scope"`
	ID    string    `json:"id"`
	At    time.Time `json:"at"`
}

func browseScope(f Filter) string {
	data, _ := json.Marshal([]any{f.OwnerKey, f.EntitledKey, f.IncludeUnpublished, f.SchoolID, f.CourseID, f.CategoryID, f.DirectoryID, f.Query, f.Attributes, f.Sort, f.FileType})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
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
	refs, err := port.CourseReferences(ctx, p, school, ids)
	if err != nil {
		return nil, err
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
	if !validID(f.SchoolID) || !optionalID(f.CategoryID) || !text(f.Query, 200) || !validFileType(f.FileType) || len(courses) > 100 {
		return out, ErrInput
	}
	if err := s.auth(ctx, p, "discover", f.SchoolID); err != nil {
		return out, err
	}
	courses = slices.Clone(courses)
	slices.Sort(courses)
	courses = slices.Compact(courses)
	for _, id := range courses {
		if !validID(id) {
			return out, ErrInput
		}
	}
	refs := map[string][]string{}
	if len(courses) != 0 {
		var err error
		refs, err = s.courseReferences(ctx, p, f.SchoolID, courses)
		if err != nil {
			return out, err
		}
	}
	// Exclude all non-HTTP filter state from this public aggregate boundary.
	f = Filter{SchoolID: f.SchoolID, CategoryID: f.CategoryID, Query: f.Query, FileType: f.FileType}
	err := s.repo.Transaction(ctx, func(tx Transaction) error {
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
