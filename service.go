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
	if repo == nil || deps.Authority == nil || deps.Catalog == nil || deps.Files == nil || deps.Moderation == nil || !validTerms(cfg.Terms) || len(cfg.AllowedTypes) == 0 || len(cfg.AllowedTypes) > 100 || cfg.MaxFileSize < 1 || cfg.MaxFileSize > 1<<30 || cfg.MaxPackageBytes < cfg.MaxFileSize || cfg.MaxPackageBytes > 4<<30 || cfg.MaxPackageFiles < 1 || cfg.MaxPackageFiles > 100 || cfg.MaxPackageParts < 1 || cfg.MaxPackageParts > 100 || cfg.MaxDirectoryDepth < 1 || cfg.MaxDirectoryDepth > 16 || cfg.MaxRecords < 1 || cfg.MaxRecords > 1000000 || cfg.MaxOperations < 1 || cfg.MaxOperations > 10000000 || cfg.MaxJobs < 1 || cfg.MaxJobs > 100000 || cfg.OperationTimeout < time.Second || cfg.OperationTimeout > time.Minute || cfg.PackageTTL < time.Minute || cfg.PackageTTL > time.Hour || cfg.Lease < 2*cfg.OperationTimeout || cfg.Lease > 10*time.Minute || cfg.MaxAttempts < 1 || cfg.MaxAttempts > 10 {
		return nil, ErrInput
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
	return &Service{repo: repo, deps: deps, cfg: cfg, now: func() time.Time { return clock().UTC() }}, nil
}
func (s *Service) Capabilities() Capabilities {
	return Capabilities{Terms: s.cfg.Terms, AllowedTypes: append([]string{}, s.cfg.AllowedTypes...), MaxFileSize: s.cfg.MaxFileSize, MaxPackageBytes: s.cfg.MaxPackageBytes, MaxPackageFiles: s.cfg.MaxPackageFiles, PurchaseAvailable: s.deps.Entitlements != nil}
}
func (s *Service) auth(ctx context.Context, p Principal, action, scope string) error {
	if !p.Valid() && action != "discover" {
		return ErrUnauthenticated
	}
	work, cancel := context.WithTimeout(ctx, s.cfg.OperationTimeout)
	defer cancel()
	err := s.deps.Authority.Check(work, p, action, scope)
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
func (s *Service) mutation(ctx context.Context, p Principal, action, scope, key, id string, input any, apply func(Transaction) (any, error), out any) error {
	if !p.Valid() {
		return ErrUnauthenticated
	}
	if !validID(key) {
		return ErrInput
	}
	if e := s.auth(ctx, p, action, scope); e != nil {
		return e
	}
	operation := hash([]string{p.Key(), action, id, key})
	digest := hash(input)
	return s.repo.Transaction(ctx, func(tx Transaction) error {
		if e := tx.Lock("operation:" + operation); e != nil {
			return e
		}
		prior, e := tx.Replay(operation)
		if e == nil {
			if prior.Digest != digest {
				return ErrConflict
			}
			if json.Unmarshal(prior.Result, out) != nil {
				return ErrUnavailable
			}
			return nil
		}
		if !errors.Is(e, ErrNotFound) {
			return e
		}
		if e = tx.Lock("budget"); e != nil {
			return e
		}
		records, operations, jobs, e := tx.Counts()
		if e != nil {
			return e
		}
		if records > s.cfg.MaxRecords || operations >= s.cfg.MaxOperations || jobs >= s.cfg.MaxJobs {
			return ErrCapacity
		}
		value, e := apply(tx)
		if e != nil {
			return e
		}
		data, e := json.Marshal(value)
		if e != nil {
			return ErrUnavailable
		}
		if e = tx.Event(Event{ID: operation, Type: "resources." + action + ".v1", ResourceID: id, SchoolID: scope, Actor: p.Key(), Time: s.now()}); e != nil {
			return e
		}
		if e = tx.SaveReplay(Replay{Key: operation, Digest: digest, Result: data}); e != nil {
			return e
		}
		if json.Unmarshal(data, out) != nil {
			return ErrUnavailable
		}
		return nil
	})
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
