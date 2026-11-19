package resources

import (
	"context"
	"io"
	"slices"
)

// Inspection is the minimal current content state for the moderation router.
// The account must have the current school-scoped moderation permission.
type Inspection struct {
	ResourceID, VersionID, SchoolID, Owner, Title, Description, State string
	Revision                                                          int64
	Visible                                                           bool
}

func (s *Service) InspectReview(ctx context.Context, p Principal, versionID string) (Inspection, error) {
	var out Inspection
	if !validID(versionID) {
		return out, ErrInput
	}
	v, e := s.version(ctx, versionID)
	if e != nil {
		return out, e
	}
	r, e := s.record(ctx, v.ResourceID)
	if e != nil {
		return out, e
	}
	if e = s.auth(ctx, p, "moderation.inspect", r.SchoolID); e != nil {
		return out, e
	}
	if r.ProposalID != versionID && r.ActiveID != versionID {
		return out, ErrConflict
	}
	return Inspection{ResourceID: r.ID, VersionID: versionID, SchoolID: r.SchoolID, Owner: r.Owner.Key(), Title: v.Version.Metadata.Title, Description: v.Version.Metadata.Description, State: v.Version.State, Revision: r.Revision, Visible: r.ActiveID == versionID && r.State == "published"}, nil
}

type FileBinding struct{ VersionID, MediaVersionID, ReferenceID, PublicationID string }

// AuthorizeFile checks the binding against the current proposal or published version.
func (s *Service) AuthorizeFile(ctx context.Context, p Principal, b FileBinding, action string) error {
	versionID, mediaVersionID := b.VersionID, b.MediaVersionID
	if !validID(versionID) || !validID(mediaVersionID) {
		return ErrInput
	}
	v, e := s.version(ctx, versionID)
	if e != nil {
		return e
	}
	r, e := s.record(ctx, v.ResourceID)
	if e != nil {
		return e
	}
	if v.Version.File == nil || v.Version.File.VersionID != mediaVersionID {
		return ErrDenied
	}
	switch action {
	case "download":
		if b.ReferenceID == "" || b.ReferenceID != v.ReferenceID {
			return ErrDenied
		}
		return s.access(ctx, p, r, v)
	case "publish":
		if e = s.auth(ctx, p, "media.publish", r.SchoolID); e != nil {
			return e
		}
		if !publicationCurrent(r, v, b.PublicationID) {
			return ErrDenied
		}
		return s.repo.Transaction(ctx, func(tx Transaction) error { _, e := currentCategory(tx, v.Version.Metadata); return e })
	case "release":
		if e = s.auth(ctx, p, "media.release", r.SchoolID); e != nil {
			return e
		}
		if b.ReferenceID == "" || (v.ReferenceID == b.ReferenceID && (r.ActiveID == versionID || (r.ProposalID == versionID && r.State != "blocked" && v.Version.State == "preparing"))) {
			return ErrDenied
		}
		return nil
	default:
		return ErrInput
	}
}

// ApplyContentDecision is the moderation delivery boundary. A single durable
// replay key covers both initial review and later state changes, so a retry after
// an ambiguous response does not reinterpret a now-published version.
func (s *Service) ApplyContentDecision(ctx context.Context, p Principal, in Decision) (Resource, error) {
	var out Resource
	if !validID(in.ID) || !validID(in.VersionID) || in.Revision < 1 || !text(in.Reason, 2000) || !slices.Contains([]string{"publish", "hide", "restore", "delete"}, in.Action) {
		return out, ErrInput
	}
	v, e := s.version(ctx, in.VersionID)
	if e != nil {
		return out, e
	}
	r, e := s.record(ctx, v.ResourceID)
	if e != nil {
		return out, e
	}
	budget := mutationBudget{exemptOps: in.Action == "hide" || in.Action == "delete"}
	e = s.mutationWithBudget(ctx, p, "moderation.execute", r.SchoolID, in.ID, r.ID, in, &budget, func(tx Transaction) (any, error) {
		if e := tx.Lock("resource:" + r.ID); e != nil {
			return nil, e
		}
		latest, e := tx.Record(r.ID)
		if e != nil {
			return nil, e
		}
		current, e := tx.Version(in.VersionID)
		if e != nil {
			return nil, e
		}
		if latest.Revision != in.Revision {
			return nil, ErrConflict
		}
		if latest.ProposalID == in.VersionID && current.Version.State == "pending" {
			if latest.State == "blocked" || in.Action == "restore" {
				return nil, ErrConflict
			}
			if in.Action == "publish" {
				budget.addJob = true
				if e = s.checkBudget(tx, budget, 1); e != nil {
					return nil, e
				}
			}
			if e = s.applyProposalDecision(tx, latest, &current, in, in.Action == "publish"); e != nil {
				return nil, e
			}
		} else {
			if latest.ActiveID != in.VersionID || current.Version.State != "published" {
				return nil, ErrConflict
			}
			var state string
			switch in.Action {
			case "hide":
				state = "blocked"
			case "delete":
				if latest.State == "blocked" && latest.ProposalID != "" && latest.ProposalID != latest.ActiveID {
					proposal, err := tx.Version(latest.ProposalID)
					if err != nil {
						return nil, err
					}
					proposal.Version.State = "superseded"
					if err = tx.SaveVersion(proposal); err != nil {
						return nil, err
					}
					latest.ProposalID = ""
				}
				state = "unlisted"
			case "restore", "publish":
				state = "published"
			}
			if state != "" {
				if err := s.changeState(tx, &latest, state); err != nil {
					return nil, err
				}
			}
		}
		latest.Revision++
		if e = tx.SaveRecord(latest); e != nil {
			return nil, e
		}
		return ownerView(tx, latest, p)
	}, &out)
	return out, e
}

func (s *Service) ReviewResource(ctx context.Context, p Principal, versionID string) (Resource, error) {
	v, e := s.InspectReview(ctx, p, versionID)
	if e != nil {
		return Resource{}, e
	}
	var out Resource
	e = s.repo.Transaction(ctx, func(tx Transaction) error {
		record, e := tx.Record(v.ResourceID)
		if e != nil {
			return e
		}
		out, e = ownerView(tx, record, p)
		return e
	})
	return out, e
}
func (s *Service) DownloadReviewFile(ctx context.Context, p Principal, versionID string, w io.Writer) error {
	if _, e := s.InspectReview(ctx, p, versionID); e != nil {
		return e
	}
	v, e := s.version(ctx, versionID)
	if e != nil {
		return e
	}
	if v.Version.File == nil {
		return ErrNotFound
	}
	return s.withStreamIdleTimeout(ctx, w, func(streamCtx context.Context, out io.Writer) error {
		return s.deps.Files.ReviewDownload(streamCtx, p, versionID, v.Version.File.VersionID, out)
	})
}

// InspectComplaint returns only target routing metadata. The caller must have
// login-only request access; no title, description, file or content is returned.
// A persisted complaint may still refer to a historical version after an update.
func (s *Service) InspectComplaint(ctx context.Context, p Principal, versionID string) (Inspection, error) {
	if !validID(versionID) {
		return Inspection{}, ErrInput
	}
	if e := s.auth(ctx, p, "request", ""); e != nil {
		return Inspection{}, e
	}
	v, e := s.version(ctx, versionID)
	if e != nil {
		return Inspection{}, e
	}
	record, e := s.record(ctx, v.ResourceID)
	if e != nil {
		return Inspection{}, e
	}
	return Inspection{ResourceID: record.ID, VersionID: versionID, SchoolID: record.SchoolID, Owner: record.Owner.Key(), Revision: record.Revision, Visible: record.State == "published" && record.ActiveID == versionID}, nil
}
