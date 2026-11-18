package resources

import (
	"context"
	"errors"
	"slices"
	"time"
)

type jobResult struct {
	reference, externalID string
	ready, obsolete       bool
}

func (s *Service) releaseVersionRefs(tx Transaction, resourceID string, v *VersionRecord, extraRef string) error {
	refs := []string{}
	if v.ReferenceID != "" {
		refs = append(refs, v.ReferenceID)
	}
	if extraRef != "" && !slices.Contains(refs, extraRef) {
		refs = append(refs, extraRef)
	}
	for _, ref := range refs {
		if e := s.queue(tx, "release", resourceID, v.Version.ID, "", ref); e != nil {
			return e
		}
	}
	if v.ReferenceID != "" {
		v.ReferenceID = ""
		if e := tx.SaveVersion(*v); e != nil {
			return e
		}
	}
	return nil
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
	var postCommitErr error
	e = s.repo.Transaction(ctx, func(tx Transaction) error {
		postCommitErr = nil
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
		if result.obsolete {
			if job.Kind == "publish" && job.ResourceID != "" && job.VersionID != "" {
				if e = tx.Lock("resource:" + job.ResourceID); e != nil {
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
				if r.ActiveID != v.Version.ID && (r.ProposalID != v.Version.ID || r.State == "blocked" || v.Version.State != "preparing") {
					if e = s.releaseVersionRefs(tx, r.ID, &v, result.reference); e != nil {
						return e
					}
				}
			}
		} else {
			postCommitErr, e = s.completeJob(tx, job, result)
			if e != nil {
				return e
			}
		}
		latest.State = "done"
		completedAt := s.now()
		latest.CompletedAt = &completedAt
		latest.ErrorCode = ""
		if e = tx.SaveJob(latest); e != nil {
			return e
		}
		var eventSchool string
		var eventRevision int64 = 1
		if job.ResourceID != "" {
			if r, err := tx.Record(job.ResourceID); err == nil {
				eventSchool = r.SchoolID
				if r.Revision > 0 {
					eventRevision = r.Revision
				}
			}
		}
		return tx.Event(Event{ID: hash([]string{"job", job.ID}), Type: "resources." + job.Kind + ".completed.v1", ResourceID: job.ResourceID, SchoolID: eventSchool, Actor: "worker", Revision: eventRevision, Time: s.now()})
	})
	if e != nil {
		return true, e
	}
	if postCommitErr != nil {
		return true, postCommitErr
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
		if r.ProposalID == v.Version.ID && r.State == "blocked" && job.Kind == "review" && v.Version.State == "pending" {
			// A temporary block defers review. Exhaustion leaves an explicit
			// failed job that the administrator can resume after restoration.
			return result, nil
		}
		if r.ProposalID != v.Version.ID || r.State == "blocked" {
			result.obsolete = true
			return result, nil
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
		result.externalID, e = s.deps.Moderation.Request(ctx, job.ID, request.VersionID, request.Reporter, request.Request)
		if e == nil && !validID(result.externalID) {
			e = ErrUnavailable
		}
		result.ready = e == nil
		return result, e
	default:
		return result, ErrInput
	}
}
func (s *Service) completeJob(tx Transaction, job Job, result jobResult) (error, error) {
	switch job.Kind {
	case "review", "publish":
		if e := tx.Lock("resource:" + job.ResourceID); e != nil {
			return nil, e
		}
		r, e := tx.Record(job.ResourceID)
		if e != nil {
			return nil, e
		}
		v, e := tx.Version(job.VersionID)
		if e != nil {
			return nil, e
		}
		if job.Kind == "review" && r.ProposalID == v.Version.ID && v.Version.State == "pending" {
			// A block that raced the external submission does not erase its receipt.
			v.ModerationTaskID = result.externalID
			return nil, tx.SaveVersion(v)
		}
		if r.ProposalID != v.Version.ID || r.State == "blocked" {
			if r.ActiveID != v.Version.ID {
				return nil, s.releaseVersionRefs(tx, r.ID, &v, result.reference)
			}
			return nil, nil
		}
		if job.Kind == "review" {
			return nil, nil
		}
		if v.Version.State != "preparing" {
			if r.ActiveID != v.Version.ID {
				if e = s.releaseVersionRefs(tx, r.ID, &v, result.reference); e != nil {
					return nil, e
				}
			}
			return ErrConflict, nil
		}
		if _, e = currentCategory(tx, v.Version.Metadata); e != nil {
			if errors.Is(e, ErrConflict) {
				v.Version.State = "rejected"
				if err := s.releaseVersionRefs(tx, r.ID, &v, result.reference); err != nil {
					return nil, err
				}
				if err := tx.SaveVersion(v); err != nil {
					return nil, err
				}
				return ErrConflict, nil
			}
			return nil, e
		}
		if r.ActiveID != "" {
			old, e := tx.Version(r.ActiveID)
			if e != nil {
				return nil, e
			}
			old.Version.State = "superseded"
			if e = tx.SaveVersion(old); e != nil {
				return nil, e
			}
			if old.ReferenceID != "" {
				if e = s.queue(tx, "release", r.ID, old.Version.ID, "", old.ReferenceID); e != nil {
					return nil, e
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
			return nil, e
		}
		return nil, tx.SaveRecord(r)
	case "request":
		request, e := tx.Request(job.RequestID)
		if e != nil {
			return nil, e
		}
		request.ExternalID = result.externalID
		request.Receipt.State = "submitted"
		return nil, tx.SaveRequest(request)
	case "release":
		return nil, nil
	default:
		return nil, ErrInput
	}
}

// Prefer the actionable proposal. A superseded or blocked version can
// still hold a failed release independently of the current proposal.
func retryableResourceJob(tx Transaction, r Record) (Job, error) {
	if r.State != "blocked" && r.ProposalID != "" {
		v, err := tx.Version(r.ProposalID)
		if err != nil {
			return Job{}, err
		}
		kind := ""
		switch v.Version.State {
		case "pending":
			kind = "review"
		case "preparing":
			kind = "publish"
		}
		if kind != "" {
			j, err := tx.Job(hash([]string{kind, r.ID, v.Version.ID, "", ""}))
			if err != nil && !errors.Is(err, ErrNotFound) {
				return Job{}, err
			}
			if err == nil && j.State == "failed" {
				return j, nil
			}
		}
	}
	port, ok := tx.(interface{ FailedReleaseJob(string) (Job, error) })
	if !ok {
		return Job{}, ErrConflict
	}
	j, err := port.FailedReleaseJob(r.ID)
	if errors.Is(err, ErrNotFound) {
		return Job{}, ErrConflict
	}
	return j, err
}

// RetryPending resumes exhausted operations without changing their idempotency
// keys. Providers must return the earlier result after an ambiguous timeout.
func (s *Service) RetryPending(ctx context.Context, p Principal, id, key string, in Command) (Resource, error) {
	var out Resource
	if !p.Valid() {
		return out, ErrUnauthenticated
	}
	if !validID(id) || !validID(key) || in.Revision < 1 {
		return out, ErrInput
	}
	r, e := s.record(ctx, id)
	if e != nil {
		return out, e
	}
	e = s.mutationWithBudget(ctx, p, "manage", r.SchoolID, key, id, in, &mutationBudget{addJob: true}, func(tx Transaction) (any, error) {
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
		job, e := retryableResourceJob(tx, r)
		if e != nil {
			return nil, e
		}
		job.State = "pending"
		job.Attempts = 0
		job.ErrorCode = ""
		job.AvailableAt = s.now()
		job.Generation++
		if e = tx.SaveJob(job); e != nil {
			return nil, e
		}
		view, e := ownerView(tx, r, p)
		if e == nil {
			e = populateAdminOperation(tx, r, &view)
		}
		return view, e
	}, &out)
	return out, e
}
