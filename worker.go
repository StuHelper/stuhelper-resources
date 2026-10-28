package resources

import (
	"context"
	"errors"
	"time"
)

type jobResult struct {
	reference, externalID string
	ready, obsolete       bool
}

// ProcessOne claims one recoverable operation. The host owns concurrency and
// shutdown; a cancelled attempt remains leased until it can safely be reclaimed.
func (s *Service) ProcessOne(ctx context.Context, worker string) (bool, error) {
	if !validID(worker) {
		return false, ErrInput
	}
	job, e := s.repo.Claim(ctx, worker, s.now(), s.cfg.Lease)
	if errors.Is(e, ErrNotFound) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	work, cancel := context.WithTimeout(ctx, s.cfg.OperationTimeout)
	defer cancel()
	result, operationErr := s.executeJob(work, job)
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	e = s.repo.Transaction(ctx, func(tx Transaction) error {
		if e := tx.Lock("job:" + job.ID); e != nil {
			return e
		}
		latest, e := tx.Job(job.ID)
		if e != nil {
			return e
		}
		if latest.State != "working" || latest.Owner != worker || latest.Generation != job.Generation || !s.now().Before(latest.LeaseUntil) {
			return ErrConflict
		}
		latest.Owner = ""
		latest.LeaseUntil = time.Time{}
		if operationErr != nil || !result.ready && !result.obsolete {
			latest.State = "pending"
			latest.ErrorCode = "resources_dependency_pending"
			if operationErr != nil {
				latest.ErrorCode = "resources_dependency_failed"
			}
			if latest.Attempts >= s.cfg.MaxAttempts {
				latest.State = "failed"
			}
			delay := time.Duration(1<<min(latest.Attempts, 8)) * time.Second
			latest.AvailableAt = s.now().Add(delay)
			if result.reference != "" && job.VersionID != "" {
				v, e := tx.Version(job.VersionID)
				if e != nil {
					return e
				}
				v.ReferenceID = result.reference
				if e = tx.SaveVersion(v); e != nil {
					return e
				}
			}
			return tx.SaveJob(latest)
		}
		if !result.obsolete {
			if e = s.completeJob(tx, job, result); e != nil {
				return e
			}
		}
		latest.State = "done"
		latest.ErrorCode = ""
		if e = tx.SaveJob(latest); e != nil {
			return e
		}
		return tx.Event(Event{ID: hash([]string{"job", job.ID}), Type: "resources." + job.Kind + ".completed.v1", ResourceID: job.ResourceID, Actor: "worker", Time: s.now()})
	})
	if e != nil {
		return true, e
	}
	return true, operationErr
}
func (s *Service) executeJob(ctx context.Context, job Job) (jobResult, error) {
	result := jobResult{}
	switch job.Kind {
	case "review", "publish":
		r, e := s.record(ctx, job.ResourceID)
		if e != nil {
			return result, e
		}
		v, e := s.version(ctx, job.VersionID)
		if e != nil {
			return result, e
		}
		if r.ProposalID != v.Version.ID {
			result.obsolete = true
			return result, nil
		}
		if r.State == "blocked" {
			return result, ErrConflict
		}
		if job.Kind == "review" {
			if v.Version.State != "pending" {
				result.obsolete = true
				return result, nil
			}
			result.externalID, e = s.deps.Moderation.Submit(ctx, job.ID, ReviewSubmission{ResourceID: r.ID, VersionID: v.Version.ID, SchoolID: r.SchoolID, Title: v.Version.Metadata.Title, Description: v.Version.Metadata.Description, Revision: r.Revision})
			if e == nil && !validID(result.externalID) {
				e = ErrUnavailable
			}
			result.ready = e == nil
			return result, e
		}
		if v.Version.State != "preparing" || v.Version.File == nil {
			result.obsolete = true
			return result, nil
		}
		// Check taxonomy again before asking media to commit the new publication.
		e = s.repo.Transaction(ctx, func(tx Transaction) error { _, e := currentCategory(tx, v.Version.Metadata); return e })
		if e != nil {
			return result, e
		}
		result.reference, result.ready, e = s.deps.Files.Publish(ctx, Publication{ResourceID: r.ID, VersionID: v.Version.ID, MediaVersionID: v.Version.File.VersionID, OperationID: job.ID, Access: v.Version.Metadata.Access, Owner: v.FileOwner})
		if e == nil && !validID(result.reference) {
			e = ErrUnavailable
		}
		return result, e
	case "release":
		e := s.deps.Files.Release(ctx, job.ReferenceID, job.ID)
		result.ready = e == nil
		return result, e
	case "request":
		var request RequestRecord
		e := s.repo.Transaction(ctx, func(tx Transaction) error { var e error; request, e = tx.Request(job.RequestID); return e })
		if e != nil {
			return result, e
		}
		result.externalID, e = s.deps.Moderation.Request(ctx, job.ID, request.ResourceID, request.Reporter, request.Request)
		if e == nil && !validID(result.externalID) {
			e = ErrUnavailable
		}
		result.ready = e == nil
		return result, e
	default:
		return result, ErrInput
	}
}
func (s *Service) completeJob(tx Transaction, job Job, result jobResult) error {
	switch job.Kind {
	case "review", "publish":
		if e := tx.Lock("resource:" + job.ResourceID); e != nil {
			return e
		}
		r, e := tx.Record(job.ResourceID)
		if e != nil {
			return e
		}
		v, e := tx.Version(job.VersionID)
		if e != nil {
			return e
		}
		if r.ProposalID != v.Version.ID || r.State == "blocked" {
			if result.reference != "" {
				return s.queue(tx, "release", r.ID, v.Version.ID, "", result.reference)
			}
			return nil
		}
		if job.Kind == "review" {
			v.ModerationTaskID = result.externalID
			return tx.SaveVersion(v)
		}
		if v.Version.State != "preparing" {
			return ErrConflict
		}
		if _, e = currentCategory(tx, v.Version.Metadata); e != nil {
			return e
		}
		if r.ActiveID != "" {
			old, e := tx.Version(r.ActiveID)
			if e != nil {
				return e
			}
			old.Version.State = "superseded"
			if e = tx.SaveVersion(old); e != nil {
				return e
			}
			if old.ReferenceID != "" {
				if e = s.queue(tx, "release", r.ID, old.Version.ID, "", old.ReferenceID); e != nil {
					return e
				}
			}
		}
		v.ReferenceID = result.reference
		v.Version.State = "published"
		r.ActiveID = v.Version.ID
		r.ProposalID = ""
		r.Revision++
		if r.State == "draft" {
			r.State = "published"
		}
		if e = tx.SaveVersion(v); e != nil {
			return e
		}
		return tx.SaveRecord(r)
	case "request":
		request, e := tx.Request(job.RequestID)
		if e != nil {
			return e
		}
		request.ExternalID = result.externalID
		request.Receipt.State = "submitted"
		return tx.SaveRequest(request)
	case "release":
		return nil
	default:
		return ErrInput
	}
}

// RetryPending resumes exhausted operations without changing their idempotency
// keys. Providers must return the earlier result after an ambiguous timeout.
func (s *Service) RetryPending(ctx context.Context, p Principal, id, key string, in Command) (Resource, error) {
	var out Resource
	r, e := s.record(ctx, id)
	if e != nil {
		return out, e
	}
	e = s.mutation(ctx, p, "manage", r.SchoolID, key, id, in, func(tx Transaction) (any, error) {
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
		kind := "review"
		if v.Version.State == "preparing" {
			kind = "publish"
		} else if v.Version.State != "pending" {
			return nil, ErrConflict
		}
		jobID := hash([]string{kind, id, v.Version.ID, "", ""})
		job, e := tx.Job(jobID)
		if e != nil {
			return nil, e
		}
		if job.State != "failed" {
			return nil, ErrConflict
		}
		job.State = "pending"
		job.Attempts = 0
		job.ErrorCode = ""
		job.AvailableAt = s.now()
		job.Generation++
		if e = tx.SaveJob(job); e != nil {
			return nil, e
		}
		return ownerView(tx, r, p)
	}, &out)
	return out, e
}
