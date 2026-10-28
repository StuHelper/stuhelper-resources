package resources

import "context"

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
