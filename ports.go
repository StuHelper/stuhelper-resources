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

// AuthorizeFile is used by the media adapter before every file action. Reference
// ownership comes from the persisted resources version; clients cannot supply it.
func (s *Service) AuthorizeFile(ctx context.Context, p Principal, versionID, mediaVersionID, action string) error {
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
		return s.access(ctx, p, r, v)
	case "publish":
		if e = s.auth(ctx, p, "media.publish", r.SchoolID); e != nil {
			return e
		}
		if r.State == "blocked" || r.ProposalID != versionID || v.Version.State != "preparing" {
			return ErrDenied
		}
		return s.repo.Transaction(ctx, func(tx Transaction) error { _, e := currentCategory(tx, v.Version.Metadata); return e })
	case "release":
		if e = s.auth(ctx, p, "media.release", r.SchoolID); e != nil {
			return e
		}
		if r.ActiveID == versionID || r.ProposalID == versionID {
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
	e = s.mutation(ctx, p, "moderation.execute", r.SchoolID, in.ID, r.ID, in, func(tx Transaction) (any, error) {
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
			if in.Action == "restore" || latest.State == "blocked" {
				return nil, ErrConflict
			}
			if _, e = currentCategory(tx, current.Version.Metadata); e != nil {
				return nil, e
			}
			current.DecisionID = in.ID
			current.DecisionRevision = in.Revision
			current.Version.State = "rejected"
			if in.Action == "publish" {
				current.Version.State = "preparing"
				if e = s.queue(tx, "publish", r.ID, in.VersionID, "", ""); e != nil {
					return nil, e
				}
			}
			if e = tx.SaveVersion(current); e != nil {
				return nil, e
			}
		} else {
			if latest.ActiveID != in.VersionID || current.Version.State != "published" {
				return nil, ErrConflict
			}
			switch in.Action {
			case "hide":
				latest.State = "blocked"
			case "delete":
				latest.State = "unlisted"
			case "restore", "publish":
				latest.State = "published"
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
	return s.deps.Files.ReviewDownload(ctx, p, versionID, v.Version.File.VersionID, w)
}
