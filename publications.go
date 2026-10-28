package resources

import (
	"context"
	"errors"
	"slices"
)

func (s *Service) validateDraft(ctx context.Context, m Metadata) error {
	if e := validateMetadata(m, s.cfg.Terms); e != nil {
		return e
	}
	work, cancel := context.WithTimeout(ctx, s.cfg.OperationTimeout)
	defer cancel()
	if e := s.deps.Catalog.Validate(work, m.Associations); e != nil {
		if errors.Is(e, ErrInput) || errors.Is(e, ErrNotFound) {
			return ErrInput
		}
		return ErrUnavailable
	}
	return nil
}
func validateDirectory(tx Transaction, m Metadata) error {
	if m.DirectoryID == "" {
		return nil
	}
	d, e := tx.Directory(m.DirectoryID)
	if e != nil {
		return e
	}
	if !d.Enabled || d.SchoolID != m.Associations.SchoolID || d.CourseID != "" && d.CourseID != m.Associations.CourseID {
		return ErrInput
	}
	return nil
}
func (s *Service) CreateDraft(ctx context.Context, p Principal, key string, in DraftWrite) (Resource, error) {
	var out Resource
	if in.Revision != 0 {
		return out, ErrInput
	}
	if e := s.auth(ctx, p, "upload", in.Metadata.Associations.SchoolID); e != nil {
		return out, e
	}
	if e := s.validateDraft(ctx, in.Metadata); e != nil {
		return out, e
	}
	e := s.mutation(ctx, p, "upload", in.Metadata.Associations.SchoolID, key, "", in, func(tx Transaction) (any, error) {
		count, _, _, e := tx.Counts()
		if e != nil {
			return nil, e
		}
		if count >= s.cfg.MaxRecords {
			return nil, ErrCapacity
		}
		category, e := currentCategory(tx, in.Metadata)
		if e != nil {
			return nil, e
		}
		if e = validateDirectory(tx, in.Metadata); e != nil {
			return nil, e
		}
		r := Record{ID: newID(), Owner: p, SchoolID: in.Metadata.Associations.SchoolID, Revision: 1, State: "draft", ProposalID: newID(), NextVersion: 2, CreatedAt: s.now()}
		v := VersionRecord{ResourceID: r.ID, Version: Version{ID: r.ProposalID, Number: 1, Metadata: in.Metadata, Category: category, State: "draft", CreatedAt: s.now()}}
		if e = tx.SaveRecord(r); e != nil {
			return nil, e
		}
		if e = tx.SaveVersion(v); e != nil {
			return nil, e
		}
		return Resource{ID: r.ID, Revision: r.Revision, State: r.State, Proposal: &v.Version, Owned: true, AccessReason: "not_published"}, nil
	}, &out)
	return out, e
}
func (s *Service) UpdateDraft(ctx context.Context, p Principal, id, key string, in DraftWrite) (Resource, error) {
	var out Resource
	r, e := s.record(ctx, id)
	if e != nil {
		return out, e
	}
	if r.Owner != p {
		if e = s.auth(ctx, p, "manage", r.SchoolID); e != nil {
			return out, e
		}
	}
	if in.Revision < 1 || in.Metadata.Associations.SchoolID != r.SchoolID {
		return out, ErrInput
	}
	if r.Owner == p {
		if e = s.auth(ctx, p, "upload", r.SchoolID); e != nil {
			return out, e
		}
	}
	if e = s.validateDraft(ctx, in.Metadata); e != nil {
		return out, e
	}
	action := "upload"
	if r.Owner != p {
		action = "manage"
	}
	e = s.mutation(ctx, p, action, r.SchoolID, key, id, in, func(tx Transaction) (any, error) {
		if e := tx.Lock("resource:" + id); e != nil {
			return nil, e
		}
		latest, e := tx.Record(id)
		if e != nil {
			return nil, e
		}
		if latest.Revision != in.Revision || latest.State == "blocked" {
			return nil, ErrConflict
		}
		category, e := currentCategory(tx, in.Metadata)
		if e != nil {
			return nil, e
		}
		if e = validateDirectory(tx, in.Metadata); e != nil {
			return nil, e
		}
		var proposal VersionRecord
		if latest.ProposalID != "" {
			proposal, e = tx.Version(latest.ProposalID)
			if e != nil {
				return nil, e
			}
			if proposal.Version.State != "draft" && proposal.Version.State != "rejected" && proposal.Version.State != "pending" && proposal.Version.State != "preparing" {
				return nil, ErrConflict
			}
		}
		if proposal.Version.ID == "" || proposal.Version.State == "rejected" || proposal.Version.State == "pending" || proposal.Version.State == "preparing" {
			previous := proposal
			if previous.Version.ID != "" {
				previous.Version.State = "superseded"
				if e = tx.SaveVersion(previous); e != nil {
					return nil, e
				}
				if previous.ReferenceID != "" {
					if e = s.queue(tx, "release", id, previous.Version.ID, "", previous.ReferenceID); e != nil {
						return nil, e
					}
				}
			}
			proposal = VersionRecord{ResourceID: id, Version: Version{ID: newID(), Number: latest.NextVersion, State: "draft", CreatedAt: s.now()}}
			latest.NextVersion++
			if previous.Version.File != nil {
				proposal.Version.File = previous.Version.File
				proposal.FileOwner = previous.FileOwner
				proposal.Version.Metadata.Access = previous.Version.Metadata.Access
			}
			if proposal.Version.File == nil && latest.ActiveID != "" {
				old, e := tx.Version(latest.ActiveID)
				if e != nil {
					return nil, e
				}
				proposal.Version.File = old.Version.File
				proposal.FileOwner = old.FileOwner
				proposal.Version.Metadata.Access = old.Version.Metadata.Access
			}
		}
		// A differently protected publication needs a file from the matching media profile.
		if proposal.Version.Metadata.Access != "" && proposal.Version.Metadata.Access != in.Metadata.Access {
			proposal.Version.File = nil
			proposal.FileOwner = Principal{}
		}
		proposal.Version.Metadata = in.Metadata
		proposal.Version.Category = category
		latest.ProposalID = proposal.Version.ID
		latest.Revision++
		if e = tx.SaveVersion(proposal); e != nil {
			return nil, e
		}
		if e = tx.SaveRecord(latest); e != nil {
			return nil, e
		}
		return ownerView(tx, latest, p)
	}, &out)
	return out, e
}
func (s *Service) AttachFile(ctx context.Context, p Principal, id, key string, in FileLink) (Resource, error) {
	var out Resource
	r, e := s.record(ctx, id)
	if e != nil {
		return out, e
	}
	if r.Owner != p {
		if e = s.auth(ctx, p, "manage", r.SchoolID); e != nil {
			return out, e
		}
	}
	if in.Revision < 1 || !validID(in.MediaVersionID) {
		return out, ErrInput
	}
	if r.Owner == p {
		if e = s.auth(ctx, p, "upload", r.SchoolID); e != nil {
			return out, e
		}
	}
	work, cancel := context.WithTimeout(ctx, s.cfg.OperationTimeout)
	defer cancel()
	file, e := s.deps.Files.Inspect(work, p, in.MediaVersionID)
	if e != nil {
		return out, e
	}
	if file.Owner != p || file.Module != "resources" || file.File.VersionID != in.MediaVersionID || file.File.Size < 1 || file.File.Size > s.cfg.MaxFileSize || !slices.Contains(s.cfg.AllowedTypes, file.File.ContentType) || !digestPattern.MatchString(file.File.SHA256) || !slices.Contains([]string{"awaiting_approval", "approved", "ready"}, file.State) {
		return out, ErrDenied
	}
	action := "upload"
	if r.Owner != p {
		action = "manage"
	}
	e = s.mutation(ctx, p, action, r.SchoolID, key, id, in, func(tx Transaction) (any, error) {
		if e := tx.Lock("resource:" + id); e != nil {
			return nil, e
		}
		r, e := tx.Record(id)
		if e != nil {
			return nil, e
		}
		if r.Revision != in.Revision || r.ProposalID == "" || r.State == "blocked" {
			return nil, ErrConflict
		}
		v, e := tx.Version(r.ProposalID)
		if e != nil {
			return nil, e
		}
		if v.Version.State != "draft" || file.Access != v.Version.Metadata.Access {
			return nil, ErrConflict
		}
		v.Version.File = &file.File
		v.FileOwner = file.Owner
		r.Revision++
		if e = tx.SaveVersion(v); e != nil {
			return nil, e
		}
		if e = tx.SaveRecord(r); e != nil {
			return nil, e
		}
		return ownerView(tx, r, p)
	}, &out)
	return out, e
}
func ownerView(tx Transaction, r Record, p Principal) (Resource, error) {
	out := Resource{ID: r.ID, Revision: r.Revision, State: r.State, Owned: r.Owner == p, AccessReason: "not_checked"}
	if r.ActiveID != "" {
		v, e := tx.Version(r.ActiveID)
		if e != nil {
			return out, e
		}
		out.Current = &v.Version
	}
	if r.ProposalID != "" {
		v, e := tx.Version(r.ProposalID)
		if e != nil {
			return out, e
		}
		out.Proposal = &v.Version
	}
	return out, nil
}
func (s *Service) Submit(ctx context.Context, p Principal, id, key string, in Command) (Resource, error) {
	var out Resource
	r, e := s.record(ctx, id)
	if e != nil {
		return out, e
	}
	action := "upload"
	if r.Owner != p {
		action = "manage"
	}
	returnOut := func(tx Transaction) (any, error) {
		if e := tx.Lock("resource:" + id); e != nil {
			return nil, e
		}
		r, e := tx.Record(id)
		if e != nil {
			return nil, e
		}
		if r.Revision != in.Revision || r.ProposalID == "" || r.State == "blocked" {
			return nil, ErrConflict
		}
		v, e := tx.Version(r.ProposalID)
		if e != nil {
			return nil, e
		}
		if v.Version.State != "draft" || v.Version.File == nil {
			return nil, ErrConflict
		}
		if _, e = currentCategory(tx, v.Version.Metadata); e != nil {
			return nil, e
		}
		if e = validateDirectory(tx, v.Version.Metadata); e != nil {
			return nil, e
		}
		v.Version.State = "pending"
		r.Revision++
		if e = tx.SaveVersion(v); e != nil {
			return nil, e
		}
		if e = tx.SaveRecord(r); e != nil {
			return nil, e
		}
		if e = s.queue(tx, "review", r.ID, v.Version.ID, "", ""); e != nil {
			return nil, e
		}
		return ownerView(tx, r, p)
	}
	e = s.mutation(ctx, p, action, r.SchoolID, key, id, in, returnOut, &out)
	return out, e
}
func (s *Service) ApplyDecision(ctx context.Context, p Principal, id, key string, in Decision) (Resource, error) {
	var out Resource
	r, e := s.record(ctx, id)
	if e != nil {
		return out, e
	}
	if !validID(in.ID) || in.Revision < 1 || !validID(in.VersionID) || !slices.Contains([]string{"approve", "reject"}, in.Action) || !text(in.Reason, 2000) {
		return out, ErrInput
	}
	e = s.mutation(ctx, p, "moderation.execute", r.SchoolID, key, id, in, func(tx Transaction) (any, error) {
		if e := tx.Lock("resource:" + id); e != nil {
			return nil, e
		}
		r, e := tx.Record(id)
		if e != nil {
			return nil, e
		}
		v, e := tx.Version(in.VersionID)
		if e != nil {
			return nil, e
		}
		if v.ResourceID != id || r.ProposalID != in.VersionID || r.Revision != in.Revision || v.Version.State != "pending" || r.State == "blocked" {
			return nil, ErrConflict
		}
		if _, e = currentCategory(tx, v.Version.Metadata); e != nil {
			return nil, e
		}
		v.DecisionID = in.ID
		v.DecisionRevision = in.Revision
		v.Version.State = "rejected"
		if in.Action == "approve" {
			v.Version.State = "preparing"
			if e = s.queue(tx, "publish", r.ID, v.Version.ID, "", ""); e != nil {
				return nil, e
			}
		}
		r.Revision++
		if e = tx.SaveVersion(v); e != nil {
			return nil, e
		}
		if e = tx.SaveRecord(r); e != nil {
			return nil, e
		}
		return ownerView(tx, r, p)
	}, &out)
	return out, e
}
func (s *Service) queue(tx Transaction, kind, resource, version, request, reference string) error {
	id := hash([]string{kind, resource, version, request, reference})
	old, e := tx.Job(id)
	if e == nil {
		if old.State == "done" {
			return nil
		}
		return nil
	}
	if !errors.Is(e, ErrNotFound) {
		return e
	}
	return tx.SaveJob(Job{ID: id, Kind: kind, ResourceID: resource, VersionID: version, RequestID: request, ReferenceID: reference, State: "pending", Generation: 1, AvailableAt: s.now()})
}
