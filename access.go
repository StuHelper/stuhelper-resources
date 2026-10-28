package resources

import (
	"context"
	"errors"
	"io"
	"strings"
)

func (s *Service) version(ctx context.Context, id string) (VersionRecord, error) {
	var v VersionRecord
	e := s.repo.Transaction(ctx, func(tx Transaction) error { var e error; v, e = tx.Version(id); return e })
	return v, e
}
func denied(e error) bool { return errors.Is(e, ErrDenied) || errors.Is(e, ErrUnauthenticated) }

// access evaluates current identity facts and each independent source of rights.
// Ordinary unlisting retains purchased delivery only; a safety block denies all.
func (s *Service) access(ctx context.Context, p Principal, r Record, v VersionRecord) error {
	if r.State == "blocked" || v.Version.State == "blocked" {
		return ErrDenied
	}
	if r.ActiveID == "" || r.ActiveID != v.Version.ID || v.Version.State != "published" || v.ReferenceID == "" {
		return ErrDenied
	}
	if !p.Valid() {
		return ErrUnauthenticated
	}
	e := s.auth(ctx, p, "manage", r.SchoolID)
	if e == nil {
		return nil
	}
	if !denied(e) {
		return e
	}
	action := "download.public"
	if v.Version.Metadata.Access == "restricted" {
		action = "download.restricted"
	}
	if e = s.auth(ctx, p, action, r.SchoolID); e != nil {
		return e
	}
	if r.State == "published" {
		if v.Version.Metadata.Access == "public" {
			return nil
		}
		var grant GrantRecord
		err := s.repo.Transaction(ctx, func(tx Transaction) error { var e error; grant, e = tx.Grant(r.ID, p.Key()); return e })
		if err == nil && !grant.Grant.Revoked && (grant.Grant.ExpiresAt == nil || s.now().Before(*grant.Grant.ExpiresAt)) {
			return nil
		}
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	if s.deps.Entitlements != nil {
		work, cancel := context.WithTimeout(ctx, s.cfg.OperationTimeout)
		defer cancel()
		rights, e := s.deps.Entitlements.Lookup(work, p, r.ID)
		if e != nil {
			return ErrUnavailable
		}
		if rights.Purchased || r.State == "published" && rights.Member {
			return nil
		}
	}
	return ErrDenied
}
func (s *Service) Get(ctx context.Context, p Principal, id string) (Resource, error) {
	var out Resource
	r, e := s.record(ctx, id)
	if e != nil {
		return out, e
	}
	privileged := r.Owner == p && p.Valid()
	if !privileged {
		e = s.auth(ctx, p, "manage", r.SchoolID)
		privileged = e == nil
		if e != nil && !denied(e) {
			return out, e
		}
	}
	if !privileged {
		if e = s.auth(ctx, p, "discover", r.SchoolID); e != nil {
			return out, e
		}
		if r.State != "published" && r.State != "unlisted" {
			return out, ErrNotFound
		}
	}
	e = s.repo.Transaction(ctx, func(tx Transaction) error { var e error; out, e = ownerView(tx, r, p); return e })
	if e != nil {
		return out, e
	}
	if !privileged {
		out.Proposal = nil
	}
	if out.Current == nil {
		return out, nil
	}
	v, e := s.version(ctx, out.Current.ID)
	if e != nil {
		return out, e
	}
	e = s.access(ctx, p, r, v)
	if e == nil {
		out.DownloadAllowed = true
		out.AccessReason = "allowed"
	} else if denied(e) {
		out.AccessReason = "access_required"
	} else {
		return Resource{}, e
	}
	// Unlisted content is visible only to its owner, administrators or current purchasers.
	if r.State == "unlisted" && !privileged && !out.DownloadAllowed {
		return Resource{}, ErrNotFound
	}
	return out, nil
}
func (s *Service) List(ctx context.Context, p Principal, f Filter, mine, admin bool) (Page, error) {
	out := Page{Items: []Resource{}}
	if f.Limit < 1 || f.Limit > 100 || !optionalID(f.After) || !optionalID(f.SchoolID) || !optionalID(f.CourseID) || !optionalID(f.CategoryID) || !optionalID(f.DirectoryID) || !text(f.Query, 200) {
		return out, ErrInput
	}
	if mine && admin {
		return out, ErrInput
	}
	f.OwnerKey = ""
	f.IncludeUnpublished = false
	if mine {
		if !p.Valid() {
			return out, ErrUnauthenticated
		}
		f.OwnerKey = p.Key()
		f.IncludeUnpublished = true
	}
	if admin {
		if !validID(f.SchoolID) {
			return out, ErrInput
		}
		if e := s.auth(ctx, p, "manage", f.SchoolID); e != nil {
			return out, e
		}
		f.IncludeUnpublished = true
	}
	if !mine && !admin {
		if !validID(f.SchoolID) {
			return out, ErrInput
		}
		if e := s.auth(ctx, p, "discover", f.SchoolID); e != nil {
			return out, e
		}
	}
	var records []Record
	e := s.repo.Transaction(ctx, func(tx Transaction) error {
		if len(f.Attributes) > 0 {
			if f.CategoryID == "" {
				return ErrInput
			}
			category, e := tx.Category(f.CategoryID, 0)
			if e != nil {
				return e
			}
			if e = ValidateAttributes(category, f.Attributes, true); e != nil {
				return e
			}
		}
		var e error
		records, e = tx.List(f)
		return e
	})
	if e != nil {
		return out, e
	}
	for _, record := range records {
		v, e := s.Get(ctx, p, record.ID)
		if errors.Is(e, ErrNotFound) || denied(e) {
			continue
		}
		if e != nil {
			return Page{}, e
		}
		out.Items = append(out.Items, v)
	}
	if len(records) == f.Limit {
		out.NextCursor = records[len(records)-1].ID
	}
	return out, nil
}
func (s *Service) selection(ctx context.Context, p Principal, id string) (Selection, error) {
	var out Selection
	r, e := s.record(ctx, id)
	if e != nil {
		return out, e
	}
	if r.ActiveID == "" {
		return out, ErrDenied
	}
	v, e := s.version(ctx, r.ActiveID)
	if e != nil {
		return out, e
	}
	if e = s.access(ctx, p, r, v); e != nil {
		return out, e
	}
	if v.Version.File == nil {
		return out, ErrUnavailable
	}
	return Selection{ResourceID: r.ID, VersionID: v.Version.ID, ReferenceID: v.ReferenceID, File: *v.Version.File}, nil
}
func (s *Service) Download(ctx context.Context, p Principal, id string, w io.Writer) (File, error) {
	selected, e := s.selection(ctx, p, id)
	if e != nil {
		return File{}, e
	}
	work, cancel := context.WithTimeout(ctx, s.cfg.OperationTimeout)
	defer cancel()
	e = s.deps.Files.Download(work, p, selected.ReferenceID, w)
	return selected.File, e
}
func (s *Service) SetState(ctx context.Context, p Principal, id, key string, in StateChange) (Resource, error) {
	var out Resource
	r, e := s.record(ctx, id)
	if e != nil {
		return out, e
	}
	if in.Revision < 1 || !text(in.Reason, 2000) || strings.TrimSpace(in.Reason) == "" || in.State != "published" && in.State != "unlisted" && in.State != "blocked" {
		return out, ErrInput
	}
	e = s.mutation(ctx, p, "manage", r.SchoolID, key, id, in, func(tx Transaction) (any, error) {
		if e := tx.Lock("resource:" + id); e != nil {
			return nil, e
		}
		r, e := tx.Record(id)
		if e != nil {
			return nil, e
		}
		if r.Revision != in.Revision {
			return nil, ErrConflict
		}
		if in.State != "blocked" && r.ActiveID == "" {
			return nil, ErrConflict
		}
		r.State = in.State
		r.Revision++
		if e = tx.SaveRecord(r); e != nil {
			return nil, e
		}
		return ownerView(tx, r, p)
	}, &out)
	return out, e
}
func (s *Service) PutGrant(ctx context.Context, p Principal, id, grantID, key string, in GrantWrite) (Grant, error) {
	var out Grant
	r, e := s.record(ctx, id)
	if e != nil {
		return out, e
	}
	if !validID(grantID) || in.Revision < 0 || in.SubjectKey == "" || !text(in.SubjectKey, 1100) || in.ExpiresAt != nil && !in.ExpiresAt.After(s.now()) {
		return out, ErrInput
	}
	e = s.mutation(ctx, p, "grants.manage", r.SchoolID, key, id+":"+grantID, in, func(tx Transaction) (any, error) {
		if e := tx.Lock("grant:" + id + ":" + in.SubjectKey); e != nil {
			return nil, e
		}
		old, e := tx.GrantByID(grantID)
		if errors.Is(e, ErrNotFound) {
			if in.Revision != 0 {
				return nil, ErrConflict
			}
		} else if e != nil {
			return nil, e
		} else if old.ResourceID != id || old.SubjectKey != in.SubjectKey || old.Grant.Revision != in.Revision {
			return nil, ErrConflict
		}
		out = Grant{ID: grantID, Revision: in.Revision + 1, ExpiresAt: in.ExpiresAt, Revoked: in.Revoked}
		if e = tx.SaveGrant(GrantRecord{Grant: out, ResourceID: id, SubjectKey: in.SubjectKey}); e != nil {
			return nil, e
		}
		return out, nil
	}, &out)
	return out, e
}
func (s *Service) Grants(ctx context.Context, p Principal, id string, n int, after string) (GrantPage, error) {
	out := GrantPage{Items: []Grant{}}
	if n < 1 || n > 100 || !optionalID(after) {
		return out, ErrInput
	}
	r, e := s.record(ctx, id)
	if e != nil {
		return out, e
	}
	if e = s.auth(ctx, p, "grants.manage", r.SchoolID); e != nil {
		return out, e
	}
	e = s.repo.Transaction(ctx, func(tx Transaction) error {
		gs, e := tx.Grants(id, n, after)
		if e != nil {
			return e
		}
		for _, g := range gs {
			out.Items = append(out.Items, g.Grant)
		}
		return nil
	})
	if len(out.Items) == n {
		out.NextCursor = out.Items[n-1].ID
	}
	return out, e
}
func (s *Service) CreateRequest(ctx context.Context, p Principal, id, key string, in Request) (RequestReceipt, error) {
	var out RequestReceipt
	r, e := s.record(ctx, id)
	if e != nil {
		return out, e
	}
	if !text(in.Reason, 2000) || strings.TrimSpace(in.Reason) == "" || !text(in.Contact, 500) || len(in.Evidence) > 4 {
		return out, ErrInput
	}
	for _, evidence := range in.Evidence {
		if !text(evidence, 2000) || strings.TrimSpace(evidence) == "" {
			return out, ErrInput
		}
	}
	if in.Kind == "appeal" && !validID(in.PreviousTaskID) || in.Kind != "appeal" && in.PreviousTaskID != "" {
		return out, ErrInput
	}
	switch in.Kind {
	case "withdrawal", "appeal":
		if r.Owner != p {
			return out, ErrDenied
		}
	case "complaint":
		if in.Contact == "" || len(in.Evidence) == 0 {
			return out, ErrInput
		}
	default:
		return out, ErrInput
	}
	e = s.mutation(ctx, p, "request", r.SchoolID, key, id, in, func(tx Transaction) (any, error) {
		versionID := r.ActiveID
		if versionID == "" {
			versionID = r.ProposalID
		}
		if versionID == "" {
			return nil, ErrConflict
		}
		out = RequestReceipt{ID: newID(), State: "pending", ResourceID: id, Kind: in.Kind, CreatedAt: s.now()}
		if e := tx.SaveRequest(RequestRecord{Receipt: out, ResourceID: id, VersionID: versionID, Reporter: p, Request: in}); e != nil {
			return nil, e
		}
		if e := s.queue(tx, "request", id, "", out.ID, ""); e != nil {
			return nil, e
		}
		return out, nil
	}, &out)
	return out, e
}
