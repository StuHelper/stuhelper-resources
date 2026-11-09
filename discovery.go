package resources

import (
	"context"
	"time"
)

// DiscoveryDocument is the public search projection. It deliberately has no
// body, identity, evidence, origin, payment or rating fields.
type DiscoveryTarget struct {
	Source string `json:"source"`
	ID     string `json:"id"`
	Kind   string `json:"kind"`
}
type DiscoveryDocument struct {
	Target     DiscoveryTarget `json:"target"`
	Revision   int64           `json:"revision"`
	Title      string          `json:"title"`
	SchoolID   string          `json:"school_id"`
	Visible    bool            `json:"visible"`
	CourseID   string          `json:"course_id,omitempty"`
	TeacherIDs []string        `json:"teacher_ids,omitempty"`
	At         time.Time       `json:"at"`
}
type DiscoveryPage struct {
	Items []DiscoveryDocument
	Next  string
}

// DiscoverySource is for trusted rebuild jobs, not an unauthenticated HTTP route.
type DiscoverySource interface {
	DiscoverySnapshot(context.Context, string, int) (DiscoveryPage, error)
}

// DiscoveryCurrent returns only published metadata permitted by the requester's
// current discovery policy. Ownership or management access does not expose drafts.
func (s *Service) DiscoveryCurrent(ctx context.Context, p Principal, ids []string) ([]DiscoveryDocument, error) {
	if !p.Valid() {
		return nil, ErrUnauthenticated
	}
	if len(ids) < 1 || len(ids) > 100 {
		return nil, ErrInput
	}
	for _, id := range ids {
		if !validID(id) {
			return nil, ErrInput
		}
	}
	var candidates []DiscoveryDocument
	e := s.repo.Transaction(ctx, func(t Transaction) error { var e error; candidates, e = t.DiscoveryCurrent(ids); return e })
	if e != nil {
		return nil, e
	}
	allowed := map[string]bool{}
	for _, d := range candidates {
		permitted, checked := allowed[d.SchoolID]
		if !checked {
			e = s.auth(ctx, p, "discover", d.SchoolID)
			if e != nil && !denied(e) {
				return nil, e
			}
			permitted = e == nil
			allowed[d.SchoolID] = permitted
		}
	}
	// Authorization may call an external provider. Read publication state again
	// afterwards so a concurrent unlisting cannot reuse the earlier candidate.
	e = s.repo.Transaction(ctx, func(t Transaction) error { var e error; candidates, e = t.DiscoveryCurrent(ids); return e })
	if e != nil {
		return nil, e
	}
	out := []DiscoveryDocument{}
	for _, d := range candidates {
		if allowed[d.SchoolID] {
			out = append(out, d)
		}
	}
	return out, nil
}
