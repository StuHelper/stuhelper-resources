package resources

import (
	"context"
)

// RequestStatus returns only the submitting user's receipt and safe decision text.
// Private reporter evidence remains in the moderation task and is never copied.
func (s *Service) RequestStatus(ctx context.Context, p Principal, id string) (RequestReceipt, error) {
	var rec RequestRecord
	if !validID(id) {
		return RequestReceipt{}, ErrInput
	}
	e := s.repo.Transaction(ctx, func(tx Transaction) error { var e error; rec, e = tx.Request(id); return e })
	if e != nil {
		return RequestReceipt{}, e
	}
	if rec.Reporter != p || !p.Valid() {
		return RequestReceipt{}, ErrNotFound
	}
	r, e := s.record(ctx, rec.ResourceID)
	if e != nil {
		return RequestReceipt{}, e
	}
	if e = s.auth(ctx, p, "request", r.SchoolID); e != nil {
		return RequestReceipt{}, e
	}
	out := rec.Receipt
	out.TaskID = rec.ExternalID
	if rec.ExternalID != "" {
		status, e := s.deps.Moderation.Status(ctx, p, rec.ExternalID)
		if e != nil {
			return RequestReceipt{}, e
		}
		out.State = status.State
		out.DecisionReason = status.Reason
		out.CanAppeal = status.CanAppeal
	} else {
		e = s.repo.Transaction(ctx, func(tx Transaction) error {
			job, e := tx.Job(hash([]string{"request", rec.ResourceID, "", rec.Receipt.ID, ""}))
			if e != nil {
				return e
			}
			if job.State == "failed" {
				out.State = "failed"
			}
			return nil
		})
		if e != nil {
			return RequestReceipt{}, e
		}
	}
	return out, nil
}

func (s *Service) RetryRequest(ctx context.Context, p Principal, id, key string) (RequestReceipt, error) {
	var out RequestReceipt
	var rec RequestRecord
	e := s.repo.Transaction(ctx, func(tx Transaction) error { var e error; rec, e = tx.Request(id); return e })
	if e != nil {
		return out, e
	}
	if rec.Reporter != p || !p.Valid() {
		return out, ErrNotFound
	}
	r, e := s.record(ctx, rec.ResourceID)
	if e != nil {
		return out, e
	}
	e = s.mutation(ctx, p, "request", r.SchoolID, key, id, struct{}{}, func(tx Transaction) (any, error) {
		jobID := hash([]string{"request", rec.ResourceID, "", id, ""})
		if e := tx.Lock("job:" + jobID); e != nil {
			return nil, e
		}
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
		if e = tx.SaveJob(job); e != nil {
			return nil, e
		}
		return rec.Receipt, nil
	}, &out)
	return out, e
}

// Requests is a bounded owner-only receipt listing. Detail refresh performs the
// current moderation lookup without issuing one external call per list row.
func (s *Service) Requests(ctx context.Context, p Principal, n int, after string) (RequestPage, error) {
	out := RequestPage{Items: []RequestReceipt{}}
	if n < 1 || n > 100 || !optionalID(after) {
		return out, ErrInput
	}
	if e := s.auth(ctx, p, "request", ""); e != nil {
		return out, e
	}
	e := s.repo.Transaction(ctx, func(tx Transaction) error {
		rows, e := tx.Requests(p, n, after)
		if e != nil {
			return e
		}
		for _, row := range rows {
			if row.Reporter != p {
				return ErrUnavailable
			}
			receipt := row.Receipt
			receipt.ResourceID = row.ResourceID
			receipt.Kind = row.Request.Kind
			receipt.TaskID = row.ExternalID
			out.Items = append(out.Items, receipt)
		}
		if len(rows) == n {
			out.NextCursor = rows[len(rows)-1].Receipt.ID
		}
		return nil
	})
	return out, e
}
