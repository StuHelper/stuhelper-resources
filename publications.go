package resources

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode"
)

type DuplicateTransaction interface {
	DuplicateResources(school string, courseIDs []string, sha256, ownerKey string, limit int) ([]DuplicateResource, error)
}
type OwnerClosureTransaction interface {
	IsClosedOwner(Principal) (bool, error)
}

func (s *Service) verifyInstructor(ctx context.Context, p Principal, a Associations) error {
	if e := s.auth(ctx, p, "upload.instructor", a.SchoolID); e != nil {
		return e
	}
	port, ok := s.deps.Catalog.(InstructorCatalog)
	if !ok {
		return ErrUnavailable
	}
	work, cancel := context.WithTimeout(ctx, s.cfg.OperationTimeout)
	defer cancel()
	if e := port.VerifyCourseInstructor(work, p, a); e != nil {
		if errors.Is(e, ErrDenied) || errors.Is(e, ErrInput) || errors.Is(e, ErrNotFound) {
			return ErrDenied
		}
		return ErrUnavailable
	}
	return nil
}

func (s *Service) FindDuplicates(ctx context.Context, p Principal, schoolID, courseID, sha256 string) (DuplicateResourceList, error) {
	out := DuplicateResourceList{Items: []DuplicateResource{}}
	if !p.Valid() {
		return out, ErrUnauthenticated
	}
	if !validID(schoolID) || !validID(courseID) || !digestPattern.MatchString(sha256) {
		return out, ErrInput
	}
	if e := s.auth(ctx, p, "discover", schoolID); e != nil {
		return out, e
	}
	if e := s.auth(ctx, p, "upload", schoolID); e != nil {
		return out, e
	}
	courseIDs := []string{courseID}
	if _, ok := s.deps.Catalog.(CourseCountCatalog); ok {
		refs, e := s.courseReferences(ctx, p, schoolID, []string{courseID})
		if e != nil {
			return out, e
		}
		courseIDs = refs[courseID]
	} else {
		work, cancel := context.WithTimeout(ctx, s.cfg.OperationTimeout)
		defer cancel()
		if e := s.deps.Catalog.Validate(work, Associations{SchoolID: schoolID, CourseID: courseID}); e != nil {
			if errors.Is(e, ErrInput) || errors.Is(e, ErrNotFound) {
				return out, ErrInput
			}
			if errors.Is(e, ErrDenied) {
				return out, ErrDenied
			}
			return out, ErrUnavailable
		}
	}
	e := s.repo.Transaction(ctx, func(tx Transaction) error {
		port, ok := tx.(DuplicateTransaction)
		if !ok {
			return ErrUnavailable
		}
		var err error
		out.Items, err = port.DuplicateResources(schoolID, courseIDs, sha256, p.Key(), 20)
		return err
	})
	return out, e
}

type DeleteTransaction interface {
	DeleteDraftRecord(Record) error
	DeletedRecordSchool(string) (string, error)
}

func sanitizeDecisionReason(reason string) string {
	reason = strings.ReplaceAll(reason, "\r\n", "\n")
	cleaned := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, reason)
	cleaned = strings.TrimSpace(cleaned)
	if !text(cleaned, 2000) {
		return ""
	}
	return cleaned
}

func (s *Service) inspectDraftFile(ctx context.Context, p Principal, schoolID, mediaVersionID string) (FileEvidence, error) {
	work, cancel := context.WithTimeout(ctx, s.cfg.OperationTimeout)
	defer cancel()
	file, e := s.deps.Files.Inspect(work, p, mediaVersionID)
	if e != nil {
		return FileEvidence{}, normalizeDepError(ctx, e)
	}
	if file.SchoolID != schoolID {
		return FileEvidence{}, ErrDenied
	}
	if file.Owner != p || file.Module != "resources" || file.File.VersionID != mediaVersionID || !validFilename(file.File.Filename) || file.File.Size < 1 || file.File.Size > s.cfg.MaxFileSize || !slices.Contains(s.cfg.AllowedTypes, file.File.ContentType) || !digestPattern.MatchString(file.File.SHA256) || !slices.Contains([]string{"awaiting_approval", "approved", "ready"}, file.State) {
		return FileEvidence{}, ErrDenied
	}
	return file, nil
}

func (s *Service) validateDraft(ctx context.Context, m Metadata) error {
	if e := validateMetadata(m); e != nil {
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
	if !p.Valid() {
		return out, ErrUnauthenticated
	}
	if !validID(key) || in.Revision != 0 {
		return out, ErrInput
	}
	if e := s.auth(ctx, p, "upload", in.Metadata.Associations.SchoolID); e != nil {
		return out, e
	}
	if !optionalID(in.MediaVersionID) || in.Submit && in.MediaVersionID == "" {
		return out, ErrInput
	}
	if e := s.checkOwnerActive(ctx, p); e != nil {
		return out, e
	}
	if hit, e := s.precheckReplay(ctx, p, "upload", "", key, in, &out); e != nil || hit {
		return out, e
	}
	if e := s.validateDraft(ctx, in.Metadata); e != nil {
		return out, e
	}
	if in.Metadata.Origin.VerifiedInstructor {
		if e := s.verifyInstructor(ctx, p, in.Metadata.Associations); e != nil {
			return out, e
		}
	}
	var attached *FileEvidence
	if in.MediaVersionID != "" {
		file, e := s.inspectDraftFile(ctx, p, in.Metadata.Associations.SchoolID, in.MediaVersionID)
		if e != nil {
			return out, e
		}
		if file.Access != in.Metadata.Access {
			return out, ErrConflict
		}
		attached = &file
	}
	e := s.mutationWithBudget(ctx, p, "upload", in.Metadata.Associations.SchoolID, key, "", in, &mutationBudget{addRecord: true, addJob: in.Submit}, func(tx Transaction) (any, error) {
		if e := checkTxOwnerActive(tx, p); e != nil {
			return nil, e
		}
		category, e := currentCategory(tx, in.Metadata)
		if e != nil {
			return nil, e
		}
		if e = validateDirectory(tx, in.Metadata); e != nil {
			return nil, e
		}
		metadata := in.Metadata
		metadata.Origin, e = s.bindCurrentConsent(ctx, metadata.Origin)
		if e != nil {
			return nil, e
		}
		now := s.now()
		r := Record{ID: newID(), Owner: p, SchoolID: in.Metadata.Associations.SchoolID, Revision: 1, State: "draft", ProposalID: newID(), NextVersion: 2, CreatedAt: now}
		v := VersionRecord{ResourceID: r.ID, Version: Version{ID: r.ProposalID, Number: 1, Metadata: metadata, Category: category, State: "draft", CreatedAt: now}}
		if attached != nil {
			v.Version.File = &attached.File
			v.FileOwner = attached.Owner
		}
		if in.Submit {
			v.Version.VerifiedInstructor = in.Metadata.Origin.VerifiedInstructor
			v.Version.State = "pending"
			v.Version.SubmittedAt = &now
		}
		if e = tx.SaveRecord(r); e != nil {
			return nil, e
		}
		if e = tx.SaveVersion(v); e != nil {
			return nil, e
		}
		if in.Submit {
			if e = s.queue(tx, "review", r.ID, v.Version.ID, "", ""); e != nil {
				return nil, e
			}
		}
		return Resource{ID: r.ID, Revision: r.Revision, State: r.State, Proposal: &v.Version, Owned: true, AccessReason: "not_published"}, nil
	}, &out)
	return out, e
}
func (s *Service) authorizeDraftEdit(ctx context.Context, p Principal, r Record, schoolMatches bool) (string, error) {
	if r.Owner != p {
		if e := s.auth(ctx, p, "manage", r.SchoolID); e != nil {
			return "", e
		}
		if !schoolMatches {
			return "", ErrInput
		}
		return "manage", nil
	}
	if !schoolMatches {
		return "", ErrInput
	}
	if e := s.auth(ctx, p, "upload", r.SchoolID); e != nil {
		return "", e
	}
	return "upload", nil
}

// A manager can resubmit the same published work with its recorded license.
// An instructor claim still needs the owner's current qualification.
func (s *Service) validateManagedReview(ctx context.Context, tx Transaction, r Record, draft VersionRecord, submit bool) error {
	origin := draft.Version.Metadata.Origin
	if submit && origin.VerifiedInstructor {
		return ErrDenied
	}
	if origin.Kind != "original" {
		return nil
	}
	if !validConsent(origin.Consent) {
		return ErrDenied
	}
	failure := ErrDenied
	if !submit || draft.FileOwner == r.Owner {
		switch _, err := s.bindCurrentConsent(ctx, origin); err {
		case nil:
			return nil
		case ErrUnavailable:
			failure = err
		}
	}
	if origin.VerifiedInstructor || r.ActiveID == "" || draft.Version.File == nil {
		return failure
	}
	active, err := tx.Version(r.ActiveID)
	if err != nil {
		return err
	}
	published := active.Version
	if published.State != "published" || published.File == nil || *published.File != *draft.Version.File ||
		published.Metadata.Access != draft.Version.Metadata.Access || !sameOriginLicense(origin, published.Metadata.Origin) {
		return failure
	}
	return nil
}

func (s *Service) managedDraftMetadata(ctx context.Context, tx Transaction, r Record, source VersionRecord, in DraftWrite) (Metadata, error) {
	if source.Version.ID == "" {
		if r.ActiveID == "" {
			return Metadata{}, ErrConflict
		}
		var err error
		source, err = tx.Version(r.ActiveID)
		if err != nil {
			return Metadata{}, err
		}
	}
	stored := source.Version.Metadata
	metadata := in.Metadata
	if given := metadata.Origin.Consent; given != nil && given.DocumentID == "" && stored.Origin.Consent != nil {
		// Older clients send only version and digest. The stored identifier
		// remains authoritative for a manager editing the same license.
		consent := *given
		consent.DocumentID = stored.Origin.Consent.DocumentID
		metadata.Origin.Consent = &consent
	}
	if !sameOriginLicense(metadata.Origin, stored.Origin) || metadata.Access != stored.Access ||
		metadata.Origin.VerifiedInstructor && !stored.Origin.VerifiedInstructor ||
		stored.Origin.VerifiedInstructor && metadata.Associations != stored.Associations {
		return Metadata{}, ErrDenied
	}
	metadata.Origin = stored.Origin
	inReview := source.Version.State == "pending" || source.Version.State == "preparing"
	if in.Submit || inReview && stored.Origin.Kind == "original" {
		if err := s.validateManagedReview(ctx, tx, r, source, in.Submit); err != nil {
			return Metadata{}, err
		}
	}
	return metadata, nil
}

func (s *Service) applyProposalDecision(tx Transaction, r Record, v *VersionRecord, in Decision, approve bool) error {
	if v.ResourceID != r.ID || r.ProposalID != in.VersionID || r.Revision != in.Revision || v.Version.State != "pending" || r.State == "blocked" {
		return ErrConflict
	}
	if _, e := currentCategory(tx, v.Version.Metadata); e != nil {
		return e
	}
	v.DecisionID = in.ID
	v.DecisionRevision = in.Revision
	v.Version.DecisionReason = sanitizeDecisionReason(in.Reason)
	v.Version.State = "rejected"
	if approve {
		v.Version.State = "preparing"
		if e := s.queuePublish(tx, r.ID, *v); e != nil {
			return e
		}
	}
	return tx.SaveVersion(*v)
}

func (s *Service) UpdateDraft(ctx context.Context, p Principal, id, key string, in DraftWrite) (Resource, error) {
	var out Resource
	if !p.Valid() {
		return out, ErrUnauthenticated
	}
	if !validID(id) || !validID(key) || in.Revision < 1 || !optionalID(in.MediaVersionID) {
		return out, ErrInput
	}
	r, e := s.record(ctx, id)
	if e != nil {
		return out, e
	}
	action, e := s.authorizeDraftEdit(ctx, p, r, in.Metadata.Associations.SchoolID == r.SchoolID)
	if e != nil {
		return out, e
	}
	if e = s.checkOwnerActive(ctx, p); e != nil {
		return out, e
	}
	if hit, e := s.precheckReplay(ctx, p, action, id, key, in, &out); e != nil || hit {
		return out, e
	}
	ownerWrite := p == r.Owner
	if !ownerWrite && in.MediaVersionID != "" {
		return out, ErrDenied
	}
	if e = s.validateDraft(ctx, in.Metadata); e != nil {
		return out, e
	}
	if ownerWrite && in.Metadata.Origin.VerifiedInstructor {
		if e = s.verifyInstructor(ctx, p, in.Metadata.Associations); e != nil {
			return out, e
		}
	}
	var attached *FileEvidence
	if in.MediaVersionID != "" {
		file, e := s.inspectDraftFile(ctx, p, r.SchoolID, in.MediaVersionID)
		if e != nil {
			return out, e
		}
		if file.Access != in.Metadata.Access {
			return out, ErrConflict
		}
		attached = &file
	}
	e = s.mutationWithBudget(ctx, p, action, r.SchoolID, key, id, in, &mutationBudget{addJob: in.Submit}, func(tx Transaction) (any, error) {
		if e := checkTxOwnerActive(tx, p); e != nil {
			return nil, e
		}
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
		metadata := in.Metadata
		if ownerWrite {
			metadata.Origin, e = s.bindCurrentConsent(ctx, metadata.Origin)
		} else {
			metadata, e = s.managedDraftMetadata(ctx, tx, latest, proposal, in)
		}
		if e != nil {
			return nil, e
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
			if proposal.Version.File == nil && latest.ActiveID != "" && (ownerWrite || previous.Version.ID == "") {
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
		if attached != nil {
			proposal.Version.File = &attached.File
			proposal.FileOwner = attached.Owner
		}
		proposal.Version.Metadata = metadata
		proposal.Version.Category = category
		if in.Submit {
			if proposal.Version.File == nil {
				return nil, ErrConflict
			}
			now := s.now()
			proposal.Version.VerifiedInstructor = metadata.Origin.VerifiedInstructor
			proposal.Version.State = "pending"
			proposal.Version.SubmittedAt = &now
			proposal.Version.DecisionReason = ""
		}
		latest.ProposalID = proposal.Version.ID
		latest.Revision++
		if e = tx.SaveVersion(proposal); e != nil {
			return nil, e
		}
		if e = tx.SaveRecord(latest); e != nil {
			return nil, e
		}
		if in.Submit {
			if e = s.queue(tx, "review", id, proposal.Version.ID, "", ""); e != nil {
				return nil, e
			}
		}
		return ownerView(tx, latest, p)
	}, &out)
	return out, e
}
func (s *Service) AttachFile(ctx context.Context, p Principal, id, key string, in FileLink) (Resource, error) {
	var out Resource
	if !p.Valid() {
		return out, ErrUnauthenticated
	}
	if !validID(id) || !validID(key) || in.Revision < 1 || !validID(in.MediaVersionID) {
		return out, ErrInput
	}
	r, e := s.record(ctx, id)
	if e != nil {
		return out, e
	}
	action, e := s.authorizeDraftEdit(ctx, p, r, true)
	if e != nil {
		return out, e
	}
	if e = s.checkOwnerActive(ctx, p); e != nil {
		return out, e
	}
	if hit, e := s.precheckReplay(ctx, p, action, id, key, in, &out); e != nil || hit {
		return out, e
	}
	if p != r.Owner {
		return out, ErrDenied
	}
	file, e := s.inspectDraftFile(ctx, p, r.SchoolID, in.MediaVersionID)
	if e != nil {
		return out, normalizeDepError(ctx, e)
	}
	e = s.mutation(ctx, p, action, r.SchoolID, key, id, in, func(tx Transaction) (any, error) {
		if e := checkTxOwnerActive(tx, p); e != nil {
			return nil, e
		}
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
		v.Version.Metadata.Origin, e = s.bindCurrentConsent(ctx, v.Version.Metadata.Origin)
		if e != nil {
			return nil, e
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
func publishedVersionView(v Version) *Version {
	v.DecisionReason = ""
	v.SubmittedAt = nil
	return &v
}

func ownerView(tx Transaction, r Record, p Principal) (Resource, error) {
	out := Resource{ID: r.ID, Revision: r.Revision, State: r.State, Owned: r.Owner == p, AccessReason: "not_checked"}
	if r.ActiveID != "" {
		v, e := tx.Version(r.ActiveID)
		if e != nil {
			return out, e
		}
		out.Current = publishedVersionView(v.Version)
	}
	if r.ProposalID != "" {
		v, e := tx.Version(r.ProposalID)
		if e != nil {
			return out, e
		}
		out.Proposal = &v.Version
	}
	if err := redactClosedInstructor(tx, r.Owner, &out); err != nil {
		return Resource{}, err
	}
	return out, nil
}

func hasInstructor(r *Resource) bool {
	for _, v := range []*Version{r.Current, r.Proposal} {
		if v != nil && (v.VerifiedInstructor || v.Metadata.Origin.VerifiedInstructor) {
			return true
		}
	}
	return false
}

func clearInstructor(r *Resource) {
	for _, v := range []*Version{r.Current, r.Proposal} {
		if v != nil {
			v.VerifiedInstructor = false
			v.Metadata.Origin.VerifiedInstructor = false
		}
	}
}

func redactClosedInstructor(tx Transaction, owner Principal, r *Resource) error {
	if !hasInstructor(r) {
		return nil
	}
	if port, ok := tx.(OwnerClosureTransaction); ok {
		closed, err := port.IsClosedOwner(owner)
		if err != nil {
			return err
		}
		if !closed {
			return nil
		}
	}
	clearInstructor(r)
	return nil
}

func (s *Service) Submit(ctx context.Context, p Principal, id, key string, in Command) (Resource, error) {
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
	if r.ProposalID != "" && r.Owner == p {
		pv, e := s.version(ctx, r.ProposalID)
		if e != nil {
			return out, e
		}
		if pv.Version.Metadata.Origin.VerifiedInstructor {
			if e = s.verifyInstructor(ctx, p, pv.Version.Metadata.Associations); e != nil {
				return out, e
			}
		}
	}
	action := "upload"
	if r.Owner != p {
		action = "manage"
	}
	returnOut := func(tx Transaction) (any, error) {
		if e := checkTxOwnerActive(tx, p); e != nil {
			return nil, e
		}
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
		if p != r.Owner {
			e = s.validateManagedReview(ctx, tx, r, v, true)
		} else {
			v.Version.Metadata.Origin, e = s.bindCurrentConsent(ctx, v.Version.Metadata.Origin)
		}
		if e != nil {
			return nil, e
		}
		if _, e = currentCategory(tx, v.Version.Metadata); e != nil {
			return nil, e
		}
		if e = validateDirectory(tx, v.Version.Metadata); e != nil {
			return nil, e
		}
		v.Version.VerifiedInstructor = v.Version.Metadata.Origin.VerifiedInstructor
		now := s.now()
		v.Version.State = "pending"
		v.Version.SubmittedAt = &now
		v.Version.DecisionReason = ""
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
	e = s.mutationWithBudget(ctx, p, action, r.SchoolID, key, id, in, &mutationBudget{addJob: true}, returnOut, &out)
	return out, e
}
func (s *Service) WithdrawProposal(ctx context.Context, p Principal, id, key string, in Command) (Resource, error) {
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
	if r.Owner != p {
		return out, ErrDenied
	}
	payload := struct {
		Op       string `json:"op"`
		Revision int64  `json:"revision"`
	}{Op: "withdraw", Revision: in.Revision}
	e = s.mutation(ctx, p, "upload", r.SchoolID, key, id, payload, func(tx Transaction) (any, error) {
		if e := checkTxOwnerActive(tx, p); e != nil {
			return nil, e
		}
		if e := tx.Lock("resource:" + id); e != nil {
			return nil, e
		}
		latest, e := tx.Record(id)
		if e != nil {
			return nil, e
		}
		if latest.Revision != in.Revision || latest.ProposalID == "" || latest.State == "blocked" {
			return nil, ErrConflict
		}
		previous, e := tx.Version(latest.ProposalID)
		if e != nil {
			return nil, e
		}
		if previous.Version.State != "pending" {
			return nil, ErrConflict
		}
		previous.Version.State = "superseded"
		if e = tx.SaveVersion(previous); e != nil {
			return nil, e
		}
		draft := VersionRecord{
			ResourceID: id,
			FileOwner:  previous.FileOwner,
			Version: Version{
				ID:        newID(),
				Number:    latest.NextVersion,
				Metadata:  previous.Version.Metadata,
				Category:  previous.Version.Category,
				File:      previous.Version.File,
				State:     "draft",
				CreatedAt: s.now(),
			},
		}
		latest.NextVersion++
		latest.ProposalID = draft.Version.ID
		latest.Revision++
		if e = tx.SaveVersion(draft); e != nil {
			return nil, e
		}
		if e = tx.SaveRecord(latest); e != nil {
			return nil, e
		}
		return ownerView(tx, latest, p)
	}, &out)
	return out, e
}
func (s *Service) DeleteDraft(ctx context.Context, p Principal, id, key string, revision int64) (DeleteReceipt, error) {
	var out DeleteReceipt
	if !p.Valid() {
		return out, ErrUnauthenticated
	}
	if !validID(id) || !validID(key) || revision < 0 {
		return out, ErrInput
	}
	schoolID := ""
	r, e := s.record(ctx, id)
	if errors.Is(e, ErrNotFound) {
		operation := hash([]string{p.Key(), "upload", id, key})
		err := s.repo.Transaction(ctx, func(tx Transaction) error {
			if _, err := tx.Replay(operation); err != nil {
				return err
			}
			port, ok := tx.(DeleteTransaction)
			if !ok {
				return ErrUnavailable
			}
			var err error
			schoolID, err = port.DeletedRecordSchool(id)
			return err
		})
		if err != nil {
			return out, err
		}
	} else if e != nil {
		return out, e
	} else {
		if r.Owner != p {
			return out, ErrDenied
		}
		schoolID = r.SchoolID
	}
	payload := struct {
		Op       string `json:"op"`
		Revision int64  `json:"revision"`
	}{Op: "delete", Revision: revision}
	e = s.mutation(ctx, p, "upload", schoolID, key, id, payload, func(tx Transaction) (any, error) {
		if e := checkTxOwnerActive(tx, p); e != nil {
			return nil, e
		}
		if e := tx.Lock("resource:" + id); e != nil {
			return nil, e
		}
		latest, e := tx.Record(id)
		if e != nil {
			return nil, e
		}
		if latest.Owner != p {
			return nil, ErrDenied
		}
		if revision > 0 && latest.Revision != revision {
			return nil, ErrConflict
		}
		if latest.ActiveID != "" || latest.State != "draft" || latest.ProposalID == "" {
			return nil, ErrConflict
		}
		proposal, e := tx.Version(latest.ProposalID)
		if e != nil {
			return nil, e
		}
		if proposal.Version.State != "draft" && proposal.Version.State != "rejected" {
			return nil, ErrConflict
		}
		port, ok := tx.(DeleteTransaction)
		if !ok {
			return nil, ErrUnavailable
		}
		if e = port.DeleteDraftRecord(latest); e != nil {
			return nil, e
		}
		return DeleteReceipt{ID: id, Deleted: true}, nil
	}, &out)
	return out, e
}
func (s *Service) ApplyDecision(ctx context.Context, p Principal, id, key string, in Decision) (Resource, error) {
	var out Resource
	if !p.Valid() {
		return out, ErrUnauthenticated
	}
	if !validID(id) || !validID(key) || !validID(in.ID) || in.Revision < 1 || !validID(in.VersionID) || !slices.Contains([]string{"approve", "reject"}, in.Action) || !text(in.Reason, 2000) {
		return out, ErrInput
	}
	r, e := s.record(ctx, id)
	if e != nil {
		return out, e
	}
	budget := mutationBudget{exemptOps: in.Action == "reject", addJob: in.Action == "approve"}
	e = s.mutationWithBudget(ctx, p, "moderation.execute", r.SchoolID, key, id, in, &budget, func(tx Transaction) (any, error) {
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
		if e = s.applyProposalDecision(tx, r, &v, in, in.Action == "approve"); e != nil {
			return nil, e
		}
		r.Revision++
		if e = tx.SaveRecord(r); e != nil {
			return nil, e
		}
		return ownerView(tx, r, p)
	}, &out)
	return out, e
}
func (s *Service) queue(tx Transaction, kind, resource, version, request, reference string) error {
	id := hash([]string{kind, resource, version, request, reference})
	return s.queueJob(tx, Job{ID: id, Kind: kind, ResourceID: resource, VersionID: version, RequestID: request, ReferenceID: reference})
}
func (s *Service) queueJob(tx Transaction, job Job) error {
	old, e := tx.Job(job.ID)
	if e == nil {
		if old.State == "failed" {
			old.State = "pending"
			old.Attempts = 0
			old.ErrorCode = ""
			old.Owner = ""
			old.LeaseUntil = time.Time{}
			old.AvailableAt = s.now()
			old.Generation++
			return tx.SaveJob(old)
		}
		return nil
	}
	if !errors.Is(e, ErrNotFound) {
		return e
	}
	job.State = "pending"
	job.Generation = 1
	job.AvailableAt = s.now()
	return tx.SaveJob(job)
}
