package resources

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

type Service struct {
	repo Repository
	deps Dependencies
	cfg  Config
	now  func() time.Time
}

func New(repo Repository, deps Dependencies, cfg Config) (*Service, error) {
	if repo == nil || deps.Authority == nil || deps.Catalog == nil || deps.Files == nil || deps.Moderation == nil || (deps.TermsSource == nil && !validTerms(cfg.Terms)) || len(cfg.UploadProfiles) > 100 || len(cfg.AllowedTypes) == 0 || len(cfg.AllowedTypes) > 100 || cfg.MaxFileSize < 1 || cfg.MaxFileSize > 1<<30 || cfg.MaxPackageBytes < cfg.MaxFileSize || cfg.MaxPackageBytes > 4<<30 || cfg.MaxPackageFiles < 1 || cfg.MaxPackageFiles > 100 || cfg.MaxPackageParts < 1 || cfg.MaxPackageParts > 100 || cfg.MaxDirectoryDepth < 1 || cfg.MaxDirectoryDepth > 16 || cfg.MaxRecords < 1 || cfg.MaxRecords > 1000000 || cfg.MaxOperations < 1 || cfg.MaxOperations > 10000000 || cfg.MaxJobs < 1 || cfg.MaxJobs > 100000 || cfg.OperationTimeout < time.Second || cfg.OperationTimeout > time.Minute || cfg.PackageTTL < time.Minute || cfg.PackageTTL > time.Hour || cfg.Lease < 2*cfg.OperationTimeout || cfg.Lease > 10*time.Minute || cfg.MaxAttempts < 1 || cfg.MaxAttempts > 10 {
		return nil, ErrInput
	}
	seenPurpose := map[string]bool{}
	seenBinding := map[string]bool{}
	for _, up := range cfg.UploadProfiles {
		binding := up.SchoolID + ":" + up.Access
		if !optionalID(up.SchoolID) || (up.Access != "public" && up.Access != "restricted") || !validID(up.Purpose) || seenPurpose[up.Purpose] || seenBinding[binding] {
			return nil, ErrInput
		}
		seenPurpose[up.Purpose] = true
		seenBinding[binding] = true
	}
	for _, kind := range cfg.AllowedTypes {
		if kind == "" || !text(kind, 200) {
			return nil, ErrInput
		}
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	if deps.TermsSource != nil {
		cfg.Terms = Terms{}
	}
	return &Service{repo: repo, deps: deps, cfg: cfg, now: func() time.Time { return clock().UTC() }}, nil
}

// OperationTimeout bounds stream preflight and a single stalled transfer operation.
func (s *Service) OperationTimeout() time.Duration { return s.cfg.OperationTimeout }

// Capabilities returns static configuration. Use CurrentCapabilities when a
// managed source provides the original-upload license.
func (s *Service) Capabilities() Capabilities { return s.capabilities(s.cfg.Terms) }

// CurrentCapabilities resolves the license that a new original upload must accept.
func (s *Service) CurrentCapabilities(ctx context.Context) (Capabilities, error) {
	terms, err := s.currentTerms(ctx)
	if err != nil {
		return Capabilities{}, err
	}
	return s.capabilities(terms), nil
}

func (s *Service) capabilities(terms Terms) Capabilities {
	return Capabilities{UploadProfiles: append([]UploadProfile{}, s.cfg.UploadProfiles...), Terms: terms, AllowedTypes: append([]string{}, s.cfg.AllowedTypes...), MaxFileSize: s.cfg.MaxFileSize, MaxPackageBytes: s.cfg.MaxPackageBytes, MaxPackageFiles: s.cfg.MaxPackageFiles, PurchaseAvailable: true}
}
func (s *Service) Ready(ctx context.Context) error {
	port, ok := s.repo.(OperationalReader)
	if !ok {
		return ErrUnavailable
	}
	work, cancel := context.WithTimeout(ctx, s.cfg.OperationTimeout)
	defer cancel()
	return port.Ready(work)
}
func (s *Service) OperationalStats(ctx context.Context) (OperationalStats, error) {
	port, ok := s.repo.(OperationalReader)
	if !ok {
		return OperationalStats{}, ErrUnavailable
	}
	work, cancel := context.WithTimeout(ctx, s.cfg.OperationTimeout)
	defer cancel()
	stats, err := port.OperationalStats(work, s.now())
	if err != nil {
		return OperationalStats{}, err
	}
	stats.MaxRecords = s.cfg.MaxRecords
	stats.MaxOperations = s.cfg.MaxOperations
	stats.MaxJobs = s.cfg.MaxJobs
	if s.cfg.MaxRecords > 0 {
		stats.RecordUtilization = float64(stats.Records) / float64(s.cfg.MaxRecords)
	}
	if s.cfg.MaxOperations > 0 {
		stats.OperationUtilization = float64(stats.Operations) / float64(s.cfg.MaxOperations)
	}
	if s.cfg.MaxJobs > 0 {
		stats.JobUtilization = float64(stats.ActiveJobs) / float64(s.cfg.MaxJobs)
	}
	return stats, nil
}
func (s *Service) auth(ctx context.Context, p Principal, action, scope string) error {
	if !p.Valid() && action != "discover" {
		return ErrUnauthenticated
	}
	work, cancel := context.WithTimeout(ctx, s.cfg.OperationTimeout)
	defer cancel()
	err := s.deps.Authority.Check(work, p, action, scope)
	var detail *AccessFailure
	if errors.As(err, &detail) {
		if detail == nil || !detail.valid() {
			return ErrUnavailable
		}
		return &AccessFailure{Reason: detail.Reason, Missing: append([]string{}, detail.Missing...), Unavailable: append([]string{}, detail.Unavailable...), RuleVersion: detail.RuleVersion}
	}
	if err == nil || errors.Is(err, ErrDenied) || errors.Is(err, ErrUnauthenticated) {
		return err
	}
	return ErrUnavailable
}
func (s *Service) record(ctx context.Context, id string) (Record, error) {
	var out Record
	if !validID(id) {
		return out, ErrInput
	}
	err := s.repo.Transaction(ctx, func(tx Transaction) error { var e error; out, e = tx.Record(id); return e })
	return out, err
}

// Replay uses the original result and the owner's current disclosure state.
func decodeReplay(tx Transaction, data []byte, out any) error {
	resource, ok := out.(*Resource)
	if !ok {
		if json.Unmarshal(data, out) != nil {
			return ErrUnavailable
		}
		return nil
	}
	var saved Resource
	if json.Unmarshal(data, &saved) != nil {
		return ErrUnavailable
	}
	if hasInstructor(&saved) {
		record, err := tx.Record(saved.ID)
		switch {
		case errors.Is(err, ErrNotFound):
			clearInstructor(&saved)
		case err != nil:
			return err
		default:
			if err := redactClosedInstructor(tx, record.Owner, &saved); err != nil {
				return err
			}
		}
	}
	*resource = saved
	return nil
}

func (s *Service) precheckReplay(ctx context.Context, p Principal, action, id, key string, input any, out any) (bool, error) {
	operation := hash([]string{p.Key(), action, id, key})
	digest := hash(input)
	hit := false
	err := s.repo.Transaction(ctx, func(tx Transaction) error {
		prior, e := tx.Replay(operation)
		if errors.Is(e, ErrNotFound) {
			return nil
		}
		if e != nil {
			return e
		}
		if prior.Digest != digest {
			return ErrConflict
		}
		if err := decodeReplay(tx, prior.Result, out); err != nil {
			return err
		}
		hit = true
		return nil
	})
	return hit, err
}

func eventTargetAndRevision(id string, value any) (string, int64) {
	target := id
	var revision int64 = 1
	switch v := value.(type) {
	case Resource:
		if target == "" {
			target = v.ID
		}
		if v.Revision > 0 {
			revision = v.Revision
		}
	case Category:
		if target == "" {
			target = v.ID
		}
		if v.Revision > 0 {
			revision = v.Revision
		}
	case Directory:
		if target == "" {
			target = v.ID
		}
		if v.Revision > 0 {
			revision = v.Revision
		}
	case Grant:
		if target == "" {
			target = v.ID
		}
		if v.Revision > 0 {
			revision = v.Revision
		}
	case Coverage:
		if target == "" {
			target = v.PlanID
		}
		if v.Revision > 0 {
			revision = v.Revision
		}
	case Package:
		if target == "" {
			target = v.ID
		}
	case requestCreation:
		target, revision = v.ResourceID, v.resourceRevision
	case RequestReceipt:
		if target == "" {
			target = v.ID
		}
	}
	return target, revision
}

type mutationBudget struct {
	exemptOps bool
	addRecord bool
	addJob    bool
}

// pending is one before the mutation and zero once its changes are visible
// to this transaction. The final check runs under the budget lock at commit.
func (s *Service) checkBudget(tx Transaction, b mutationBudget, pending int) error {
	if b.exemptOps && !b.addRecord && !b.addJob {
		return nil
	}
	records, operations, jobs, err := tx.Counts()
	if err != nil {
		return err
	}
	if !b.exemptOps && operations > s.cfg.MaxOperations-pending ||
		b.addRecord && records > s.cfg.MaxRecords-pending ||
		b.addJob && jobs > s.cfg.MaxJobs-pending {
		return ErrCapacity
	}
	return nil
}

func (s *Service) mutation(ctx context.Context, p Principal, action, scope, key, id string, input any, apply func(Transaction) (any, error), out any) error {
	return s.mutationWithBudget(ctx, p, action, scope, key, id, input, &mutationBudget{}, apply, out)
}

func (s *Service) mutationWithBudget(ctx context.Context, p Principal, action, scope, key, id string, input any, budget *mutationBudget, apply func(Transaction) (any, error), out any) error {
	_, err := s.mutationWithReplay(ctx, p, action, scope, scope, key, id, input, budget, apply, out)
	return err
}

func (s *Service) mutationWithReplay(ctx context.Context, p Principal, action, scope, auditSchool, key, id string, input any, budget *mutationBudget, apply func(Transaction) (any, error), out any) (bool, error) {
	if !p.Valid() {
		return false, ErrUnauthenticated
	}
	if !validID(key) {
		return false, ErrInput
	}
	if e := s.auth(ctx, p, action, scope); e != nil {
		return false, e
	}
	operation := hash([]string{p.Key(), action, id, key})
	digest := hash(input)
	replayed := false
	err := s.repo.Transaction(ctx, func(tx Transaction) error {
		if e := tx.Lock("operation:" + operation); e != nil {
			return e
		}
		prior, e := tx.Replay(operation)
		if e == nil {
			if prior.Digest != digest {
				return ErrConflict
			}
			if err := decodeReplay(tx, prior.Result, out); err != nil {
				return err
			}
			replayed = true
			return nil
		}
		if !errors.Is(e, ErrNotFound) {
			return e
		}
		if e = s.checkBudget(tx, *budget, 1); e != nil {
			return e
		}
		value, e := apply(tx)
		if e != nil {
			return e
		}
		data, e := json.Marshal(value)
		if e != nil {
			return ErrUnavailable
		}
		auditTarget, revision := eventTargetAndRevision(id, value)
		if e = tx.Event(Event{Reason: auditReason(input), ID: operation, Type: "resources." + action + ".v1", ResourceID: auditTarget, SchoolID: auditSchool, Actor: p.Key(), Revision: revision, Time: s.now()}); e != nil {
			return e
		}
		if e = tx.SaveReplay(Replay{Key: operation, Digest: digest, Result: data}); e != nil {
			return e
		}
		// All business writes have finished. Hold the shared quota's exclusive
		// lock only for the final counter read and commit. READ COMMITTED makes
		// the previous holder's committed counters visible after we acquire it.
		if e = tx.Lock("budget"); e != nil {
			return e
		}
		if e = s.checkBudget(tx, *budget, 0); e != nil {
			return e
		}
		if json.Unmarshal(data, out) != nil {
			return ErrUnavailable
		}
		return nil
	})
	return replayed, err
}
func (s *Service) PutCategory(ctx context.Context, p Principal, id, key string, input CategoryWrite) (Category, error) {
	var out Category
	if !validID(id) {
		return out, ErrInput
	}
	if e := ValidateCategory(input); e != nil {
		return out, e
	}
	e := s.mutation(ctx, p, "taxonomy.manage", "", key, id, input, func(tx Transaction) (any, error) {
		if e := tx.Lock("category:" + id); e != nil {
			return nil, e
		}
		old, e := tx.Category(id, 0)
		if errors.Is(e, ErrNotFound) {
			if input.Revision != 0 {
				return nil, ErrConflict
			}
			all, e := tx.Categories(100, "", true)
			if e != nil {
				return nil, e
			}
			if len(all) >= 100 {
				return nil, ErrCapacity
			}
		} else if e != nil {
			return nil, e
		} else if old.Revision != input.Revision {
			return nil, ErrConflict
		}
		category := Category{ID: id, Revision: input.Revision + 1, Names: input.Names, Enabled: input.Enabled, Order: input.Order, Fields: input.Fields, GroupBy: input.GroupBy}
		if category.Fields == nil {
			category.Fields = []Field{}
		}
		if e = tx.SaveCategory(category); e != nil {
			return nil, e
		}
		return category, nil
	}, &out)
	return out, e
}
func (s *Service) Categories(ctx context.Context, limit int, after string, includeDisabled bool) (CategoryPage, error) {
	out := CategoryPage{Items: []Category{}}
	if limit < 1 || limit > 100 || !optionalID(after) {
		return out, ErrInput
	}
	e := s.repo.Transaction(ctx, func(tx Transaction) error {
		var e error
		out.Items, e = tx.Categories(limit, after, includeDisabled)
		return e
	})
	if len(out.Items) == limit {
		out.NextCursor = out.Items[len(out.Items)-1].ID
	}
	return out, e
}
func currentCategory(tx Transaction, m Metadata) (Category, error) {
	c, e := tx.Category(m.CategoryID, 0)
	if e != nil {
		return c, e
	}
	if !c.Enabled || c.Revision != m.CategoryRevision {
		return c, ErrConflict
	}
	return c, ValidateAttributes(c, m.Attributes, false)
}

// ReadCategories is the user-facing taxonomy read boundary. Scoped configuration
// access remains independent of campus qualification.
func (s *Service) ReadCategories(ctx context.Context, p Principal, limit int, after string, disabled bool) (CategoryPage, error) {
	if !p.Valid() {
		return CategoryPage{}, ErrUnauthenticated
	}
	if e := s.auth(ctx, p, "taxonomy.manage", ""); e != nil {
		if !denied(e) {
			return CategoryPage{}, e
		}
		if disabled {
			return CategoryPage{}, ErrDenied
		}
		if e = s.auth(ctx, p, "discover", ""); e != nil {
			return CategoryPage{}, e
		}
	}
	return s.Categories(ctx, limit, after, disabled)
}

// Only explicit operation reasons enter audit; never serialize arbitrary input.
func auditReason(input any) string {
	switch v := input.(type) {
	case StateChange:
		return v.Reason
	case Decision:
		return v.Reason
	case Request:
		return ""
	}
	return ""
}
