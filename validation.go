package resources

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:-]{0,127}$`)
var fieldIdentifier = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var locales = []string{"zh-CN", "en-US", "fr-FR", "ru-RU", "ja-JP", "ko-KR", "es-ES", "ar-EG"}

func validID(s string) bool    { return identifier.MatchString(s) }
func optionalID(s string) bool { return s == "" || validID(s) }
func text(s string, max int) bool {
	return utf8.ValidString(s) && utf8.RuneCountInString(s) <= max && !strings.ContainsRune(s, 0)
}
func names(n Names) bool {
	if len(n) == 0 || len(n) > 8 || strings.TrimSpace(n["zh-CN"]) == "" {
		return false
	}
	for locale, value := range n {
		if !slices.Contains(locales, locale) || strings.TrimSpace(value) == "" || !text(value, 200) {
			return false
		}
	}
	return true
}
func newID() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}
func hash(value any) string {
	b, e := json.Marshal(value)
	if e != nil {
		panic(e)
	}
	v := sha256.Sum256(b)
	return hex.EncodeToString(v[:])
}
func validTerms(t Terms) bool {
	u, e := url.Parse(t.URL)
	return validID(t.Version) && digestPattern.MatchString(t.Digest) && e == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && len(t.URL) <= 2000
}

// ValidateCategory checks the configurable schema, never a category name or ID.
func ValidateCategory(c CategoryWrite) error {
	if c.Revision < 0 || !names(c.Names) || c.Order < 0 || c.Order > 10000 || len(c.Fields) > 20 {
		return ErrInput
	}
	seen := map[string]bool{}
	groupValid := c.GroupBy == ""
	for _, f := range c.Fields {
		if !fieldIdentifier.MatchString(f.Key) || seen[f.Key] || !names(f.Names) {
			return ErrInput
		}
		seen[f.Key] = true
		if f.Key == c.GroupBy && f.Groupable {
			groupValid = true
		}
		switch f.Kind {
		case "text":
			if f.MaxLength < 1 || f.MaxLength > 1000 || f.Min != nil || f.Max != nil || len(f.Options) > 0 {
				return ErrInput
			}
		case "integer":
			if f.MaxLength != 0 || len(f.Options) > 0 || f.Min == nil || f.Max == nil || *f.Min < -1000000000 || *f.Max > 1000000000 || *f.Min > *f.Max {
				return ErrInput
			}
		case "choice":
			if f.MaxLength != 0 || f.Min != nil || f.Max != nil || len(f.Options) < 1 || len(f.Options) > 100 {
				return ErrInput
			}
			options := map[string]bool{}
			for _, option := range f.Options {
				if !validID(option.ID) || !names(option.Names) || options[option.ID] {
					return ErrInput
				}
				options[option.ID] = true
			}
		default:
			return ErrInput
		}
	}
	if !groupValid {
		return ErrInput
	}
	return nil
}
func validateValue(f Field, v Value) error {
	count := 0
	for _, ok := range []bool{v.Text != nil, v.Number != nil, v.Option != nil} {
		if ok {
			count++
		}
	}
	if count != 1 {
		return ErrInput
	}
	switch f.Kind {
	case "text":
		if v.Text == nil || strings.TrimSpace(*v.Text) == "" || !text(*v.Text, f.MaxLength) {
			return ErrInput
		}
	case "integer":
		if v.Number == nil || f.Min == nil || f.Max == nil || *v.Number < *f.Min || *v.Number > *f.Max {
			return ErrInput
		}
	case "choice":
		if v.Option == nil {
			return ErrInput
		}
		found := false
		for _, o := range f.Options {
			if o.ID == *v.Option {
				found = true
				break
			}
		}
		if !found {
			return ErrInput
		}
	default:
		return ErrInput
	}
	return nil
}
func ValidateAttributes(c Category, values map[string]Value, filter bool) error {
	if len(values) > 20 {
		return ErrInput
	}
	known := map[string]Field{}
	for _, f := range c.Fields {
		known[f.Key] = f
	}
	for key, value := range values {
		f, ok := known[key]
		if !ok || filter && !f.Filterable {
			return ErrInput
		}
		if e := validateValue(f, value); e != nil {
			return e
		}
	}
	if !filter {
		for _, f := range c.Fields {
			if _, ok := values[f.Key]; f.Required && !ok {
				return ErrInput
			}
		}
	}
	return nil
}
func validateMetadata(m Metadata, terms Terms) error {
	if strings.TrimSpace(m.Title) == "" || !text(m.Title, 200) || !text(m.Description, 8000) || len(m.Tags) > 20 || !validID(m.CategoryID) || m.CategoryRevision < 1 || !optionalID(m.DirectoryID) || !slices.Contains([]string{"public", "restricted"}, m.Access) {
		return ErrInput
	}
	seen := map[string]bool{}
	for _, tag := range m.Tags {
		if strings.TrimSpace(tag) == "" || !text(tag, 64) || seen[tag] {
			return ErrInput
		}
		seen[tag] = true
	}
	a := m.Associations
	for _, id := range []string{a.SchoolID, a.CourseID, a.TermID, a.OfferingID, a.TeacherID} {
		if !optionalID(id) {
			return ErrInput
		}
	}
	if a.SchoolID == "" && (a.CourseID != "" || a.TermID != "" || a.OfferingID != "" || a.TeacherID != "") {
		return ErrInput
	}
	if a.OfferingID != "" && a.CourseID == "" {
		return ErrInput
	}
	o := m.Origin
	if !text(o.Source, 2000) || !text(o.Author, 200) || !text(o.License, 4000) {
		return ErrInput
	}
	switch o.Kind {
	case "original":
		if o.Source != "" || o.License != "" || o.Consent == nil || !o.Consent.Accepted || o.Consent.Version != terms.Version || o.Consent.Digest != terms.Digest {
			return ErrInput
		}
	case "known":
		if strings.TrimSpace(o.Source) == "" || o.Consent != nil {
			return ErrInput
		}
	case "unknown":
		if o.Source != "" || o.Consent != nil {
			return ErrInput
		}
	default:
		return ErrInput
	}
	return nil
}
