package resources

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"time"
)

type AccessFactTarget struct {
	ResourceID  string `json:"resource_id"`
	SchoolID    string `json:"school_id"`
	DirectoryID string `json:"directory_id"`
	CategoryID  string `json:"category_id"`
}

type AccessFacts struct {
	Granted   bool
	Purchased bool
	Member    bool
}

type BatchReadTransaction interface {
	RecordsByIDs([]string) (map[string]Record, error)
	VersionsByIDs([]string) (map[string]VersionRecord, error)
	AccessFactsBatch(string, time.Time, []AccessFactTarget) (map[string]AccessFacts, error)
}

func (s *Service) withRequestAuthority() *Service {
	if _, ok := s.deps.Authority.(*requestAuthority); ok {
		return s
	}
	copy := *s
	copy.deps.Authority = &requestAuthority{source: s.deps.Authority, results: map[authorizationKey]error{}}
	return &copy
}

func (s *Service) version(ctx context.Context, id string) (VersionRecord, error) {
	var v VersionRecord
	e := s.repo.Transaction(ctx, func(tx Transaction) error { var e error; v, e = tx.Version(id); return e })
	return v, e
}
func denied(e error) bool { return errors.Is(e, ErrDenied) || errors.Is(e, ErrUnauthenticated) }

func loadSingleAccessFacts(tx Transaction, ownerKey string, r Record, v VersionRecord, now time.Time) (AccessFacts, error) {
	var out AccessFacts
	if ownerKey == "" {
		return out, nil
	}
	if port, ok := tx.(BatchReadTransaction); ok {
		m, err := port.AccessFactsBatch(ownerKey, now, []AccessFactTarget{{
			ResourceID:  r.ID,
			SchoolID:    r.SchoolID,
			DirectoryID: v.Version.Metadata.DirectoryID,
			CategoryID:  v.Version.Metadata.CategoryID,
		}})
		return m[r.ID], err
	}
	if r.State == "published" {
		grant, err := tx.Grant(r.ID, ownerKey)
		if err == nil && !grant.Grant.Revoked && (grant.Grant.ExpiresAt == nil || now.Before(*grant.Grant.ExpiresAt)) {
			out.Granted = true
		} else if err != nil && !errors.Is(err, ErrNotFound) {
			return out, err
		}
	}
	rights, err := tx.Rights(ownerKey, r.ID, r.SchoolID, v.Version.Metadata.DirectoryID, v.Version.Metadata.CategoryID, now)
	if err != nil {
		return out, err
	}
	out.Purchased = rights.Purchased
	out.Member = rights.Member
	return out, nil
}

// access evaluates current identity facts and each independent source of rights.
// Ordinary unlisting retains purchased delivery only; a safety block denies all.
func (s *Service) access(ctx context.Context, p Principal, r Record, v VersionRecord) error {
	return s.accessWithFacts(ctx, p, r, v, nil)
}

func (s *Service) accessWithFacts(ctx context.Context, p Principal, r Record, v VersionRecord, preloaded *AccessFacts) error {
	if r.State == "blocked" || v.Version.State == "blocked" {
		return &AccessFailure{Reason: "resource_unavailable"}
	}
	if r.ActiveID == "" || r.ActiveID != v.Version.ID || v.Version.State != "published" || v.ReferenceID == "" {
		return &AccessFailure{Reason: "resource_unavailable"}
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
	if e = s.auth(ctx, p, "discover", r.SchoolID); e != nil {
		return e
	}
	action := "download.public"
	if v.Version.Metadata.Access == "restricted" {
		action = "download.restricted"
	}
	if e = s.auth(ctx, p, action, r.SchoolID); e != nil {
		return e
	}
	if r.State == "published" && v.Version.Metadata.Access == "public" {
		return nil
	}
	var facts AccessFacts
	if preloaded != nil {
		facts = *preloaded
	} else {
		err := s.repo.Transaction(ctx, func(tx Transaction) error {
			var e error
			facts, e = loadSingleAccessFacts(tx, p.Key(), r, v, s.now())
			return e
		})
		if err != nil {
			return err
		}
	}
	if r.State == "published" && facts.Granted {
		return nil
	}
	if facts.Purchased || r.State == "published" && facts.Member {
		return nil
	}
	return &AccessFailure{Reason: "requirements_missing", Missing: []string{"resource_entitlement"}}
}

func (s *Service) Get(ctx context.Context, p Principal, id string) (Resource, error) {
	if !validID(id) {
		return Resource{}, ErrInput
	}
	s = s.withRequestAuthority()
	var r Record
	var activeV, proposalV *VersionRecord
	err := s.repo.Transaction(ctx, func(tx Transaction) error {
		var e error
		r, e = tx.Record(id)
		if e != nil {
			return e
		}
		if r.ActiveID != "" {
			v, e := tx.Version(r.ActiveID)
			if e != nil {
				return e
			}
			activeV = &v
		}
		if r.ProposalID != "" {
			if activeV != nil && r.ProposalID == activeV.Version.ID {
				cp := *activeV
				proposalV = &cp
			} else {
				v, e := tx.Version(r.ProposalID)
				if e != nil {
					return e
				}
				proposalV = &v
			}
		}
		return nil
	})
	if err != nil {
		return Resource{}, err
	}
	return s.evaluatePreloadedRecord(ctx, p, r, activeV, proposalV, nil)
}

func (s *Service) getRecord(ctx context.Context, p Principal, r Record) (Resource, error) {
	var activeV, proposalV *VersionRecord
	err := s.repo.Transaction(ctx, func(tx Transaction) error {
		if r.ActiveID != "" {
			v, e := tx.Version(r.ActiveID)
			if e != nil {
				return e
			}
			activeV = &v
		}
		if r.ProposalID != "" {
			if activeV != nil && r.ProposalID == activeV.Version.ID {
				cp := *activeV
				proposalV = &cp
			} else {
				v, e := tx.Version(r.ProposalID)
				if e != nil {
					return e
				}
				proposalV = &v
			}
		}
		return nil
	})
	if err != nil {
		return Resource{}, err
	}
	return s.evaluatePreloadedRecord(ctx, p, r, activeV, proposalV, nil)
}

func (s *Service) evaluatePreloadedRecord(ctx context.Context, p Principal, r Record, activeV, proposalV *VersionRecord, facts *AccessFacts) (Resource, error) {
	var out Resource
	var e error
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
	out = Resource{ID: r.ID, Revision: r.Revision, State: r.State, Owned: r.Owner == p, AccessReason: "not_checked"}
	if activeV != nil {
		ver := activeV.Version
		out.Current = &ver
	}
	if privileged && proposalV != nil {
		ver := proposalV.Version
		out.Proposal = &ver
	}
	if activeV == nil {
		return out, nil
	}
	e = s.accessWithFacts(ctx, p, r, *activeV, facts)
	if e == nil {
		out.DownloadAllowed = true
		out.AccessReason = "allowed"
	} else if denied(e) {
		out.AccessReason = "permission_denied"
		var failure *AccessFailure
		if errors.As(e, &failure) {
			out.AccessReason = failure.Reason
			out.MissingConditions = failure.Missing
			out.UnavailableConditions = failure.Unavailable
			out.RuleVersion = failure.RuleVersion
		}
	} else if privileged {
		// Qualification outages may pause file access but cannot hide an owned record.
		out.AccessReason = "service_unavailable"
		var failure *AccessFailure
		if errors.As(e, &failure) {
			out.MissingConditions = failure.Missing
			out.UnavailableConditions = failure.Unavailable
			out.RuleVersion = failure.RuleVersion
		}
	} else {
		return Resource{}, e
	}
	if out.DownloadAllowed {
		out.AvailableMethods = []string{"download"}
		out.UnlockReason = "already_accessible"
	} else {
		decision, err := s.unlockEligibilityFor(ctx, p, r, *activeV, e)
		if err == nil {
			out.UnlockReason = decision.Reason
			if decision.Eligible {
				out.AvailableMethods = []string{"purchase"}
			}
		} else {
			out.UnlockReason = "service_unavailable"
		}
	}
	// Unlisted content is visible only to its owner, administrators or current purchasers.
	if r.State == "unlisted" && !privileged && !out.DownloadAllowed {
		return Resource{}, ErrNotFound
	}
	return out, nil
}
func (s *Service) List(ctx context.Context, p Principal, f Filter, mine, admin bool) (Page, error) {
	// Reuse only this request's school/action decisions. Every subsequent request
	// starts with current facts; per-resource grants and state remain independent.
	s = s.withRequestAuthority()
	out := Page{Items: []Resource{}}
	if f.Limit < 1 || f.Limit > 100 || !optionalID(f.SchoolID) || !optionalID(f.CourseID) || !optionalID(f.CategoryID) || !optionalID(f.DirectoryID) || !text(f.Query, 200) || validBrowseFilters(f) != nil {
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
	if e := prepareBrowse(&f); e != nil {
		return out, e
	}
	if !mine {
		if e := s.validateFilterAssociations(ctx, Associations{SchoolID: f.SchoolID, TermID: f.TermID, TeacherID: f.TeacherID}); e != nil {
			return out, e
		}
	}
	f.CourseIDs = nil
	if f.CourseID != "" {
		// Older embedders keep their existing literal-ID list behavior. New
		// hosts provide the batch port, shared with the counts endpoint.
		if _, ok := s.deps.Catalog.(CourseCountCatalog); ok && !mine {
			refs, e := s.courseReferences(ctx, p, f.SchoolID, []string{f.CourseID})
			if e != nil {
				return out, e
			}
			f.CourseIDs = refs[f.CourseID]
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
		port, ok := tx.(BrowseTransaction)
		if !ok {
			return ErrUnavailable
		}
		var e error
		records, out.Total, e = port.Browse(f)
		return e
	})
	if e != nil {
		return out, e
	}
	more := len(records) > f.Limit
	if more {
		records = records[:f.Limit]
	}
	if len(records) > 0 {
		items, err := s.hydrateRecords(ctx, p, records)
		if err != nil {
			return Page{}, err
		}
		out.Items = items
	}
	if more {
		out.NextCursor, e = encodeBrowseCursor(f, records[len(records)-1])
		if e != nil {
			return Page{}, e
		}
	}
	return out, nil
}

func (s *Service) hydrateRecords(ctx context.Context, p Principal, records []Record) ([]Resource, error) {
	var versions map[string]VersionRecord
	var factsMap map[string]AccessFacts
	batched := false
	err := s.repo.Transaction(ctx, func(tx Transaction) error {
		port, ok := tx.(BatchReadTransaction)
		if !ok {
			return nil
		}
		batched = true
		verIDs := make([]string, 0, len(records)*2)
		seenVer := map[string]bool{}
		for _, r := range records {
			for _, vid := range []string{r.ActiveID, r.ProposalID} {
				if vid != "" && !seenVer[vid] {
					seenVer[vid] = true
					verIDs = append(verIDs, vid)
				}
			}
		}
		var e error
		versions, e = port.VersionsByIDs(verIDs)
		if e != nil {
			return e
		}
		if p.Valid() {
			targets := make([]AccessFactTarget, 0, len(records))
			for _, r := range records {
				if r.ActiveID == "" {
					continue
				}
				v, ok := versions[r.ActiveID]
				if !ok {
					continue
				}
				if r.State == "published" && v.Version.Metadata.Access == "public" {
					continue
				}
				targets = append(targets, AccessFactTarget{
					ResourceID:  r.ID,
					SchoolID:    r.SchoolID,
					DirectoryID: v.Version.Metadata.DirectoryID,
					CategoryID:  v.Version.Metadata.CategoryID,
				})
			}
			factsMap, e = port.AccessFactsBatch(p.Key(), s.now(), targets)
			if e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]Resource, 0, len(records))
	for _, record := range records {
		var v Resource
		var e error
		if !batched {
			v, e = s.getRecord(ctx, p, record)
		} else {
			var activePtr, proposalPtr *VersionRecord
			var factsPtr *AccessFacts
			if record.ActiveID != "" {
				av, ok := versions[record.ActiveID]
				if !ok {
					return nil, ErrNotFound
				}
				activePtr = &av
				f := factsMap[record.ID]
				factsPtr = &f
			}
			if record.ProposalID != "" {
				pv, ok := versions[record.ProposalID]
				if !ok {
					return nil, ErrNotFound
				}
				proposalPtr = &pv
			}
			v, e = s.evaluatePreloadedRecord(ctx, p, record, activePtr, proposalPtr, factsPtr)
		}
		if errors.Is(e, ErrNotFound) || denied(e) {
			continue
		}
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, nil
}

type schoolSelection struct {
	Selection
	schoolID string
}

func (s *Service) selectBatch(ctx context.Context, p Principal, ids []string) (map[string]schoolSelection, map[string]error, error) {
	s = s.withRequestAuthority()
	selections := make(map[string]schoolSelection, len(ids))
	itemErrs := make(map[string]error, len(ids))
	const chunkSize = 200
	for start := 0; start < len(ids); start += chunkSize {
		end := min(start+chunkSize, len(ids))
		chunk := ids[start:end]
		var records map[string]Record
		var versions map[string]VersionRecord
		var factsMap map[string]AccessFacts
		batched := false
		err := s.repo.Transaction(ctx, func(tx Transaction) error {
			port, ok := tx.(BatchReadTransaction)
			if !ok {
				return nil
			}
			batched = true
			var e error
			records, e = port.RecordsByIDs(chunk)
			if e != nil {
				return e
			}
			verIDs := make([]string, 0, len(records))
			seenVer := map[string]bool{}
			for _, rec := range records {
				if rec.ActiveID != "" && !seenVer[rec.ActiveID] {
					seenVer[rec.ActiveID] = true
					verIDs = append(verIDs, rec.ActiveID)
				}
			}
			versions, e = port.VersionsByIDs(verIDs)
			if e != nil {
				return e
			}
			if p.Valid() {
				targets := make([]AccessFactTarget, 0, len(records))
				for _, rec := range records {
					if rec.ActiveID == "" {
						continue
					}
					v, ok := versions[rec.ActiveID]
					if !ok || (rec.State == "published" && v.Version.Metadata.Access == "public") {
						continue
					}
					targets = append(targets, AccessFactTarget{
						ResourceID:  rec.ID,
						SchoolID:    rec.SchoolID,
						DirectoryID: v.Version.Metadata.DirectoryID,
						CategoryID:  v.Version.Metadata.CategoryID,
					})
				}
				factsMap, e = port.AccessFactsBatch(p.Key(), s.now(), targets)
				if e != nil {
					return e
				}
			}
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
		for _, id := range chunk {
			if !batched {
				sel, school, e := s.selectionWithSchool(ctx, p, id, true)
				if e != nil {
					itemErrs[id] = e
				} else {
					selections[id] = schoolSelection{Selection: sel, schoolID: school}
				}
				continue
			}
			rec, ok := records[id]
			if !ok {
				itemErrs[id] = ErrNotFound
				continue
			}
			if e := s.auth(ctx, p, "package", rec.SchoolID); e != nil {
				itemErrs[id] = e
				continue
			}
			if rec.ActiveID == "" {
				itemErrs[id] = ErrDenied
				continue
			}
			ver, ok := versions[rec.ActiveID]
			if !ok {
				itemErrs[id] = ErrNotFound
				continue
			}
			f := factsMap[rec.ID]
			if e := s.accessWithFacts(ctx, p, rec, ver, &f); e != nil {
				itemErrs[id] = e
				continue
			}
			if ver.Version.File == nil {
				itemErrs[id] = ErrUnavailable
				continue
			}
			selections[id] = schoolSelection{Selection: Selection{ResourceID: rec.ID, VersionID: ver.Version.ID, ReferenceID: ver.ReferenceID, File: *ver.Version.File}, schoolID: rec.SchoolID}
		}
	}
	return selections, itemErrs, nil
}

func (s *Service) selection(ctx context.Context, p Principal, id string) (Selection, error) {
	sel, _, err := s.selectionWithSchool(ctx, p, id, false)
	return sel, err
}
func (s *Service) selectionWithSchool(ctx context.Context, p Principal, id string, forPackage bool) (Selection, string, error) {
	var out Selection
	if !validID(id) {
		return out, "", ErrInput
	}
	var r Record
	var v VersionRecord
	err := s.repo.Transaction(ctx, func(tx Transaction) error {
		var e error
		r, e = tx.Record(id)
		if e != nil {
			return e
		}
		if r.ActiveID == "" {
			return nil
		}
		v, e = tx.Version(r.ActiveID)
		if e != nil {
			return e
		}
		return nil
	})
	if err != nil {
		return out, "", err
	}
	if forPackage {
		if e := s.auth(ctx, p, "package", r.SchoolID); e != nil {
			return out, "", e
		}
	}
	if r.ActiveID == "" {
		return out, "", ErrDenied
	}
	if e := s.accessWithFacts(ctx, p, r, v, nil); e != nil {
		return out, "", e
	}
	if v.Version.File == nil {
		return out, "", ErrUnavailable
	}
	return Selection{ResourceID: r.ID, VersionID: v.Version.ID, ReferenceID: v.ReferenceID, File: *v.Version.File}, r.SchoolID, nil
}

func (s *Service) StreamDownload(ctx context.Context, p Principal, id string, open func(File) io.Writer) error {
	if !validID(id) {
		return ErrInput
	}
	s = s.withRequestAuthority()
	var r Record
	var activeV *VersionRecord
	err := s.repo.Transaction(ctx, func(tx Transaction) error {
		var e error
		r, e = tx.Record(id)
		if e != nil {
			return e
		}
		if r.ActiveID != "" {
			v, e := tx.Version(r.ActiveID)
			if e != nil {
				return e
			}
			activeV = &v
		}
		return nil
	})
	if err != nil {
		return err
	}
	privileged := r.Owner == p && p.Valid()
	if !privileged {
		e := s.auth(ctx, p, "manage", r.SchoolID)
		privileged = e == nil
		if e != nil && !denied(e) {
			return e
		}
	}
	if !privileged {
		if e := s.auth(ctx, p, "discover", r.SchoolID); e != nil {
			return e
		}
		if r.State != "published" && r.State != "unlisted" {
			return ErrNotFound
		}
	}
	if activeV == nil || activeV.Version.File == nil {
		return &AccessFailure{Reason: "resource_unavailable"}
	}
	if e := s.accessWithFacts(ctx, p, r, *activeV, nil); e != nil {
		if r.State == "unlisted" && !privileged && denied(e) {
			return ErrNotFound
		}
		var detail *AccessFailure
		if denied(e) && !errors.As(e, &detail) {
			return &AccessFailure{Reason: "permission_denied"}
		}
		return e
	}
	return s.withStreamIdleTimeout(ctx, open(*activeV.Version.File), func(streamCtx context.Context, out io.Writer) error {
		return s.deps.Files.Download(streamCtx, p, activeV.ReferenceID, out)
	})
}

func (s *Service) Download(ctx context.Context, p Principal, id string, w io.Writer) (File, error) {
	s = s.withRequestAuthority()
	selected, e := s.selection(ctx, p, id)
	if e != nil {
		return File{}, e
	}
	e = s.withStreamIdleTimeout(ctx, w, func(streamCtx context.Context, out io.Writer) error {
		return s.deps.Files.Download(streamCtx, p, selected.ReferenceID, out)
	})
	return selected.File, e
}
func (s *Service) SetState(ctx context.Context, p Principal, id, key string, in StateChange) (Resource, error) {
	var out Resource
	if !p.Valid() {
		return out, ErrUnauthenticated
	}
	if !validID(id) || !validID(key) || in.Revision < 1 || !text(in.Reason, 2000) || strings.TrimSpace(in.Reason) == "" || in.State != "published" && in.State != "unlisted" && in.State != "blocked" {
		return out, ErrInput
	}
	r, e := s.record(ctx, id)
	if e != nil {
		return out, e
	}
	budget := mutationBudget{exemptOps: in.State == "blocked" || in.State == "unlisted"}
	e = s.mutationWithBudget(ctx, p, "manage", r.SchoolID, key, id, in, &budget, func(tx Transaction) (any, error) {
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
	if !p.Valid() {
		return out, ErrUnauthenticated
	}
	if !validID(id) || !validID(grantID) || !validID(key) || in.Revision < 0 || in.ExpiresAt != nil && !in.ExpiresAt.After(s.now()) {
		return out, ErrInput
	}
	if _, err := ParsePrincipalKey(in.SubjectKey); err != nil {
		return out, ErrInput
	}
	r, e := s.record(ctx, id)
	if e != nil {
		return out, e
	}
	budget := mutationBudget{exemptOps: in.Revoked && in.Revision > 0}
	e = s.mutationWithBudget(ctx, p, "grants.manage", r.SchoolID, key, id+":"+grantID, in, &budget, func(tx Transaction) (any, error) {
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
		// A new key for an already revoked grant is an ordinary update.
		budget.exemptOps = budget.exemptOps && !old.Grant.Revoked
		out = Grant{SubjectKey: in.SubjectKey, ID: grantID, Revision: in.Revision + 1, ExpiresAt: in.ExpiresAt, Revoked: in.Revoked}
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
			g.Grant.SubjectKey = g.SubjectKey
			out.Items = append(out.Items, g.Grant)
		}
		return nil
	})
	if len(out.Items) == n {
		out.NextCursor = out.Items[n-1].ID
	}
	return out, e
}

// requestCreation keeps the resource revision for the event while encoding
// exactly the public receipt in the idempotency record.
type requestCreation struct {
	RequestReceipt
	resourceRevision int64
}

func (s *Service) CreateRequest(ctx context.Context, p Principal, id, key string, in Request) (RequestReceipt, error) {
	var out RequestReceipt
	if !p.Valid() {
		return out, ErrUnauthenticated
	}
	if !validID(id) || !validID(key) || !text(in.Reason, 2000) || strings.TrimSpace(in.Reason) == "" || !text(in.Contact, 500) || len(in.Evidence) > 4 {
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
	if in.Kind != "withdrawal" && in.Kind != "appeal" && in.Kind != "complaint" {
		return out, ErrInput
	}
	if in.Kind == "complaint" && (in.Contact == "" || len(in.Evidence) == 0) {
		return out, ErrInput
	}
	r, e := s.record(ctx, id)
	if e != nil {
		return out, e
	}
	if (in.Kind == "withdrawal" || in.Kind == "appeal") && r.Owner != p {
		return out, ErrDenied
	}
	// Title disclosure is fixed at submission. A later qualification change or
	// idempotent retry cannot add a title to a receipt created without admission.
	discoverOK := false
	if !(in.Kind != "complaint" && r.Owner == p) {
		discoverOK = s.auth(ctx, p, "discover", r.SchoolID) == nil
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	e = s.mutationWithBudget(ctx, p, "request", r.SchoolID, key, id, in, &mutationBudget{addJob: true}, func(tx Transaction) (any, error) {
		if in.Kind == "complaint" {
			limiter, ok := tx.(interface{ LimitSubmission(string, time.Time) error })
			if !ok {
				return nil, ErrUnavailable
			}
			if err := limiter.LimitSubmission(p.Key(), s.now()); err != nil {
				return nil, err
			}
		}
		if e := tx.Lock("resource:" + id); e != nil {
			return nil, e
		}
		latest, e := tx.Record(id)
		if e != nil {
			return nil, e
		}
		if (in.Kind == "withdrawal" || in.Kind == "appeal") && latest.Owner != p {
			return nil, ErrDenied
		}
		versionID := latest.ActiveID
		if versionID == "" {
			versionID = latest.ProposalID
		}
		if versionID == "" {
			return nil, ErrConflict
		}
		includeTitle := in.Kind != "complaint" && latest.Owner == p && p.Valid()
		if !includeTitle && latest.State == "published" && latest.ActiveID != "" {
			includeTitle = discoverOK
		}
		out = RequestReceipt{ID: newID(), State: "pending", ResourceID: id, Kind: in.Kind, CreatedAt: s.now()}
		if includeTitle {
			version, e := tx.Version(versionID)
			if e != nil {
				return nil, e
			}
			out.ResourceTitle = version.Version.Metadata.Title
		}
		if e := tx.SaveRequest(RequestRecord{Receipt: out, ResourceID: id, VersionID: versionID, Reporter: p, Request: in}); e != nil {
			return nil, e
		}
		if e := s.queue(tx, "request", id, "", out.ID, ""); e != nil {
			return nil, e
		}
		return requestCreation{RequestReceipt: out, resourceRevision: latest.Revision}, nil
	}, &out)
	return out, e
}

type authorizationKey struct {
	Principal     Principal
	Action, Scope string
}
type requestAuthority struct {
	mu      sync.Mutex
	source  Authority
	results map[authorizationKey]error
}

func (a *requestAuthority) Check(ctx context.Context, p Principal, action, scope string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key := authorizationKey{p, action, scope}
	a.mu.Lock()
	if err, ok := a.results[key]; ok {
		a.mu.Unlock()
		return err
	}
	a.mu.Unlock()
	err := a.source.Check(ctx, p, action, scope)
	a.mu.Lock()
	a.results[key] = err
	a.mu.Unlock()
	return err
}
