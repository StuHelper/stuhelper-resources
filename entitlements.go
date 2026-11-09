package resources

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"
)

// Coverage rules are a union; dimensions within each rule are an intersection.
// Directory matching is exact, so moving a child never silently broadens a plan.
type CoverageRule struct {
	SchoolID    string `json:"school_id"`
	DirectoryID string `json:"directory_id,omitempty"`
	CategoryID  string `json:"category_id,omitempty"`
}
type Coverage struct {
	PlanID   string         `json:"plan_id"`
	Revision int64          `json:"revision"`
	Rules    []CoverageRule `json:"rules"`
}
type PurchaseSource struct {
	OrderID    string    `json:"order_id"`
	Owner      Principal `json:"owner"`
	ResourceID string    `json:"resource_id"`
	Revision   int64     `json:"revision"`
	Revoked    bool      `json:"revoked"`
}
type Membership struct {
	Owner      Principal `json:"owner"`
	PlanID     string    `json:"plan_id"`
	ValidFrom  time.Time `json:"valid_from"`
	ValidUntil time.Time `json:"valid_until"`
	Revision   int64     `json:"revision"`
}
type UnlockDecision struct {
	Eligible bool   `json:"eligible"`
	Reason   string `json:"reason"`
}

func (s *Service) GetCoverage(ctx context.Context, p Principal, id string) (Coverage, error) {
	var out Coverage
	if !validID(id) {
		return out, ErrInput
	}
	if !p.Valid() {
		return out, ErrUnauthenticated
	}
	e := s.auth(ctx, p, "coverage.manage", "")
	if e != nil {
		if !denied(e) {
			return out, e
		}
		if e = s.auth(ctx, p, "discover", ""); e != nil {
			return out, e
		}
	}
	e = s.repo.Transaction(ctx, func(tx Transaction) error { var e error; out, e = tx.Coverage(id); return e })
	return out, e
}
func (s *Service) PutCoverage(ctx context.Context, p Principal, id, key string, in Coverage) (Coverage, error) {
	var out Coverage
	if !validID(id) || id != in.PlanID || in.Revision < 0 || in.Rules == nil || len(in.Rules) > 100 {
		return out, ErrInput
	}
	for _, r := range in.Rules {
		if !validID(r.SchoolID) || !optionalID(r.DirectoryID) || !optionalID(r.CategoryID) {
			return out, ErrInput
		}
	}
	e := s.mutation(ctx, p, "coverage.manage", "", key, id, in, func(tx Transaction) (any, error) {
		if e := tx.Lock("coverage:" + id); e != nil {
			return nil, e
		}
		old, e := tx.Coverage(id)
		if errors.Is(e, ErrNotFound) {
			if in.Revision != 0 {
				return nil, ErrConflict
			}
		} else if e != nil {
			return nil, e
		} else if old.Revision != in.Revision {
			return nil, ErrConflict
		}
		for _, r := range in.Rules {
			if r.DirectoryID != "" {
				d, e := tx.Directory(r.DirectoryID)
				if e != nil {
					return nil, e
				}
				if d.SchoolID != r.SchoolID {
					return nil, ErrInput
				}
			}
			if r.CategoryID != "" {
				if _, e := tx.Category(r.CategoryID, 0); e != nil {
					return nil, e
				}
			}
		}
		in.Revision++
		return in, tx.SaveCoverage(in)
	}, &out)
	return out, e
}
func (s *Service) UnlockEligibility(ctx context.Context, p Principal, id string) (UnlockDecision, error) {
	no := func(reason string) (UnlockDecision, error) { return UnlockDecision{Reason: reason}, nil }
	if !validID(id) {
		return UnlockDecision{}, ErrInput
	}
	if !p.Valid() {
		return no("unauthenticated")
	}
	r, e := s.record(ctx, id)
	if errors.Is(e, ErrNotFound) {
		return no("not_found")
	}
	if e != nil {
		return UnlockDecision{}, e
	}
	if r.ActiveID == "" {
		return no("resource_unavailable")
	}
	v, e := s.version(ctx, r.ActiveID)
	if e != nil {
		return UnlockDecision{}, e
	}
	return s.unlockEligibilityFor(ctx, p, r, v, s.access(ctx, p, r, v))
}
func (s *Service) unlockEligibilityFor(ctx context.Context, p Principal, r Record, v VersionRecord, accessErr error) (UnlockDecision, error) {
	no := func(reason string) (UnlockDecision, error) { return UnlockDecision{Reason: reason}, nil }
	if r.State != "published" || v.Version.State != "published" || v.ReferenceID == "" {
		return no("resource_unavailable")
	}
	if accessErr == nil {
		return no("already_accessible")
	}
	if !denied(accessErr) {
		return UnlockDecision{}, accessErr
	}
	var e error

	for _, action := range []string{"discover", "purchase"} {
		if e = s.auth(ctx, p, action, r.SchoolID); e != nil {
			if denied(e) {
				return no("requirements_missing")
			}
			return UnlockDecision{}, e
		}
	}
	if v.Version.Metadata.Access != "restricted" {
		return no("not_purchasable")
	}
	return UnlockDecision{Eligible: true, Reason: "eligible"}, nil
}
func (s *Service) localRights(ctx context.Context, p Principal, r Record, v VersionRecord) (Entitlement, error) {
	var out Entitlement
	e := s.repo.Transaction(ctx, func(tx Transaction) error {
		var e error
		out, e = tx.Rights(p.Key(), r.ID, r.SchoolID, v.Version.Metadata.DirectoryID, v.Version.Metadata.CategoryID, s.now())
		return e
	})
	return out, e
}

// Consume is a trusted relay boundary. Authentication of the producer belongs to
// backend. Unknown fields are rejected to keep commercial data out of this store.
func (s *Service) Consume(ctx context.Context, event Envelope) error {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.OperationTimeout)
	defer cancel()
	if event.DataContentType != "application/json" || event.SpecVersion != "1.0" || event.Source != "billing" || !text(event.ID, 200) || event.ID == "" || !text(event.Subject, 200) || event.Subject == "" || event.Time.IsZero() || len(event.Data) > 8192 {
		return ErrInput
	}
	var purchase PurchaseSource
	var member Membership
	decode := func(v any) error {
		d := json.NewDecoder(bytes.NewReader(event.Data))
		d.DisallowUnknownFields()
		if d.Decode(v) != nil {
			return ErrInput
		}
		if d.Decode(new(any)) != io.EOF {
			return ErrInput
		}
		return nil
	}
	// The transport DTO deliberately excludes local revoked state.
	var paid struct {
		OrderID    string    `json:"order_id"`
		Owner      Principal `json:"owner"`
		ResourceID string    `json:"resource_id"`
		Revision   int64     `json:"revision"`
	}
	switch event.Type {
	case "billing.resource-unlock.paid.v1", "billing.resource-unlock.refunded.v1":
		if e := decode(&paid); e != nil {
			return e
		}
		if !validID(paid.OrderID) || !validID(paid.ResourceID) || !paid.Owner.Valid() || paid.Revision < 1 {
			return ErrInput
		}
		purchase = PurchaseSource{OrderID: paid.OrderID, Owner: paid.Owner, ResourceID: paid.ResourceID, Revision: paid.Revision, Revoked: event.Type == "billing.resource-unlock.refunded.v1"}
	case "billing.membership.changed.v1":
		if e := decode(&member); e != nil {
			return e
		}
		if !validID(member.PlanID) || !member.Owner.Valid() || member.Revision < 1 || member.ValidFrom.IsZero() || member.ValidUntil.IsZero() {
			return ErrInput
		}
	default:
		return ErrInput
	}
	return s.repo.Transaction(ctx, func(tx Transaction) error {
		// Global inbox lock prevents the same event ID racing across subjects.
		if e := tx.Lock("billing-inbox"); e != nil {
			return e
		}
		seen, e := tx.Inbox(event.ID)
		if e != nil {
			return e
		}
		if seen {
			return nil
		}
		if purchase.OrderID != "" {
			old, e := tx.Purchase(purchase.OrderID)
			if e != nil && !errors.Is(e, ErrNotFound) {
				return e
			}
			if e == nil {
				if old.Owner != purchase.Owner || old.ResourceID != purchase.ResourceID {
					return ErrConflict
				}
				if old.Revision >= purchase.Revision {
					return tx.SaveInbox(event.ID)
				}
			}
			if !purchase.Revoked {
				if e = tx.Lock("resource:" + purchase.ResourceID); e != nil {
					return e
				}
				r, e := tx.Record(purchase.ResourceID)
				reason := ""
				if errors.Is(e, ErrNotFound) {
					reason = "resource_unavailable"
				} else if e != nil {
					return e
				} else if r.State != "published" || r.ActiveID == "" {
					reason = "resource_unavailable"
				} else {
					v, e := tx.Version(r.ActiveID)
					if e != nil {
						return e
					}
					if v.Version.State != "published" || v.ReferenceID == "" {
						reason = "resource_unavailable"
					}
				}
				typ := "resources.entitlement.granted.v1"
				data := struct {
					OrderID    string    `json:"order_id"`
					Owner      Principal `json:"owner"`
					ResourceID string    `json:"resource_id"`
					Revision   int64     `json:"revision"`
					Reason     string    `json:"reason,omitempty"`
				}{purchase.OrderID, purchase.Owner, purchase.ResourceID, purchase.Revision, reason}
				if reason != "" {
					purchase.Revoked = true
					typ = "resources.entitlement.rejected.v1"
				}

				raw, e := json.Marshal(data)
				if e != nil {
					return e
				}
				if e = tx.Envelope(Envelope{DataContentType: "application/json", SpecVersion: "1.0", ID: hash([]string{event.Source, event.ID}), Source: "resources", Subject: event.Subject, Type: typ, Time: s.now(), Data: raw}); e != nil {
					return e
				}
			}
			if e = tx.SavePurchase(purchase); e != nil {
				return e
			}
		} else {
			old, e := tx.Membership(member.Owner.Key(), member.PlanID)
			if e != nil && !errors.Is(e, ErrNotFound) {
				return e
			}
			if e == nil && old.Revision >= member.Revision {
				return tx.SaveInbox(event.ID)
			}
			if e = tx.SaveMembership(member); e != nil {
				return e
			}
		}
		return tx.SaveInbox(event.ID)
	})
}
