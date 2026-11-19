package resources

import (
	"context"
	"errors"
	"strconv"
)

// changeState runs under the resource lock. The record revision fences any
// publish operation already in flight when a block took effect.
func (s *Service) changeState(tx Transaction, r *Record, state string) error {
	wasBlocked := r.State == "blocked"
	r.State = state
	if r.ActiveID == "" && state == "published" {
		r.State = "draft"
	}
	if wasBlocked == (state == "blocked") || r.ProposalID == "" || r.ProposalID == r.ActiveID {
		return nil
	}
	v, err := tx.Version(r.ProposalID)
	if err != nil {
		return err
	}
	if v.Version.State != "pending" && v.Version.State != "preparing" {
		return nil
	}
	if state == "blocked" {
		if v.Version.State == "preparing" && v.Version.File != nil {
			if err := s.queue(tx, "retire_publication", r.ID, v.Version.ID, publicationJobID(r.ID, v), ""); err != nil {
				return err
			}
		}
		v.RestoreRevision = r.Revision + 1
		if err := tx.SaveVersion(v); err != nil {
			return err
		}
		return s.releaseVersionRefs(tx, r.ID, &v, "")
	}
	if v.RestoreRevision == 0 {
		v.RestoreRevision = r.Revision + 1
		if err := tx.SaveVersion(v); err != nil {
			return err
		}
	}
	if v.Version.State == "preparing" {
		return s.queuePublish(tx, r.ID, v)
	}
	if v.ModerationTaskID != "" {
		return nil
	}
	// Reuse the review operation across a block, including a lost response.
	id := hash([]string{"review", r.ID, v.Version.ID, "", ""})
	job, err := tx.Job(id)
	if errors.Is(err, ErrNotFound) {
		return s.queue(tx, "review", r.ID, v.Version.ID, "", "")
	}
	if err != nil {
		return err
	}
	job.State = "pending"
	job.Owner = ""
	job.ErrorCode = ""
	job.Attempts = 0
	job.CompletedAt = nil
	job.AvailableAt = s.now()
	job.Generation++
	return tx.SaveJob(job)
}

func publicationJobID(resourceID string, v VersionRecord) string {
	parts := []string{"publish", resourceID, v.Version.ID, "", ""}
	if v.RestoreRevision != 0 {
		parts = append(parts, strconv.FormatInt(v.RestoreRevision, 10))
	}
	return hash(parts)
}

func (s *Service) queuePublish(tx Transaction, resourceID string, v VersionRecord) error {
	return s.queueJob(tx, Job{ID: publicationJobID(resourceID, v), Kind: "publish", ResourceID: resourceID, VersionID: v.Version.ID})
}

// RetainsSource exposes only the owning module's current cleanup decision.
func (s *Service) RetainsSource(ctx context.Context, p Principal, mediaVersionID string) (bool, error) {
	if !validID(mediaVersionID) {
		return false, ErrInput
	}
	if err := s.auth(ctx, p, "media.release", ""); err != nil {
		return false, err
	}
	var keep bool
	err := s.repo.Transaction(ctx, func(tx Transaction) error {
		var err error
		keep, err = tx.RetainsSource(mediaVersionID)
		return err
	})
	return keep, err
}

func publicationCurrent(r Record, v VersionRecord, jobID string) bool {
	return r.State != "blocked" && r.ProposalID == v.Version.ID && v.Version.State == "preparing" && jobID == publicationJobID(r.ID, v)
}

// Keep the revision first delivered to moderation even when the network loses
// its receipt and a block and restore advance the resource revision.
func (s *Service) reviewRevision(ctx context.Context, resourceID, versionID string) (int64, error) {
	var revision int64
	err := s.repo.Transaction(ctx, func(tx Transaction) error {
		if err := tx.Lock("resource:" + resourceID); err != nil {
			return err
		}
		r, err := tx.Record(resourceID)
		if err != nil {
			return err
		}
		v, err := tx.Version(versionID)
		if err != nil {
			return err
		}
		if r.State == "blocked" || r.ProposalID != versionID || v.Version.State != "pending" {
			return ErrConflict
		}
		revision = v.ReviewRevision
		if revision != 0 {
			return nil
		}
		revision = r.Revision
		v.ReviewRevision = revision
		return tx.SaveVersion(v)
	})
	return revision, err
}
