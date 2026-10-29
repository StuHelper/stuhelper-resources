package resources

import (
	"context"
	"errors"
	"io"
)

func (s *Service) PutDirectory(ctx context.Context, p Principal, id, key string, in DirectoryWrite) (Directory, error) {
	var out Directory
	if !validID(id) || !validID(in.SchoolID) || !optionalID(in.CourseID) || !optionalID(in.ParentID) || in.ParentID == id || in.Revision < 0 || !names(in.Names) {
		return out, ErrInput
	}
	if e := s.auth(ctx, p, "directories.manage", in.SchoolID); e != nil {
		return out, e
	}
	work, cancel := context.WithTimeout(ctx, s.cfg.OperationTimeout)
	defer cancel()
	if e := s.deps.Catalog.Validate(work, Associations{SchoolID: in.SchoolID, CourseID: in.CourseID}); e != nil {
		return out, e
	}
	e := s.mutation(ctx, p, "directories.manage", in.SchoolID, key, id, in, func(tx Transaction) (any, error) {
		if e := tx.Lock("directories:" + in.SchoolID); e != nil {
			return nil, e
		}
		old, e := tx.Directory(id)
		if errors.Is(e, ErrNotFound) {
			if in.Revision != 0 {
				return nil, ErrConflict
			}
		} else if e != nil {
			return nil, e
		} else if old.Revision != in.Revision || old.SchoolID != in.SchoolID || old.CourseID != in.CourseID {
			return nil, ErrConflict
		}
		depth := 1
		seen := map[string]bool{id: true}
		parent := in.ParentID
		for parent != "" {
			if seen[parent] {
				return nil, ErrInput
			}
			seen[parent] = true
			depth++
			if depth > s.cfg.MaxDirectoryDepth {
				return nil, ErrInput
			}
			d, e := tx.Directory(parent)
			if e != nil {
				return nil, e
			}
			if !d.Enabled || d.SchoolID != in.SchoolID || d.CourseID != "" && d.CourseID != in.CourseID {
				return nil, ErrInput
			}
			parent = d.ParentID
		}
		// A moved subtree must fit too, so moving a parent cannot bypass depth limits.
		budget := 1000
		subDepth, e := directoryDepth(tx, in.SchoolID, id, map[string]bool{}, &budget)
		if e != nil {
			return nil, e
		}
		if depth+subDepth-1 > s.cfg.MaxDirectoryDepth {
			return nil, ErrInput
		}
		out = Directory{ID: id, Revision: in.Revision + 1, SchoolID: in.SchoolID, CourseID: in.CourseID, ParentID: in.ParentID, Names: in.Names, Enabled: in.Enabled}
		if e = tx.SaveDirectory(out); e != nil {
			return nil, e
		}
		return out, nil
	}, &out)
	return out, e
}
func directoryDepth(tx Transaction, school, id string, seen map[string]bool, budget *int) (int, error) {
	if seen[id] {
		return 0, ErrConflict
	}
	seen[id] = true
	*budget--
	if *budget < 0 {
		return 0, ErrCapacity
	}
	children, e := tx.Directories(school, "", id, 101, "")
	if e != nil {
		return 0, e
	}
	if len(children) > 100 {
		return 0, ErrCapacity
	}
	deepest := 1
	for _, child := range children {
		depth, e := directoryDepth(tx, school, child.ID, seen, budget)
		if e != nil {
			return 0, e
		}
		if depth+1 > deepest {
			deepest = depth + 1
		}
	}
	return deepest, nil
}
func (s *Service) Directories(ctx context.Context, p Principal, school, course, parent string, n int, after string) (DirectoryPage, error) {
	out := DirectoryPage{Items: []Directory{}}
	if !validID(school) || !optionalID(course) || !optionalID(parent) || !optionalID(after) || n < 1 || n > 100 {
		return out, ErrInput
	}
	if e := s.auth(ctx, p, "discover", school); e != nil {
		return out, e
	}
	e := s.repo.Transaction(ctx, func(tx Transaction) error {
		var e error
		out.Items, e = tx.Directories(school, course, parent, n, after)
		return e
	})
	if len(out.Items) == n {
		out.NextCursor = out.Items[n-1].ID
	}
	return out, e
}
func (s *Service) directoryResources(ctx context.Context, id string) ([]string, error) {
	ids := []string{}
	e := s.repo.Transaction(ctx, func(tx Transaction) error {
		root, e := tx.Directory(id)
		if e != nil {
			return e
		}
		if !root.Enabled {
			return ErrNotFound
		}
		queue := []string{id}
		seen := map[string]bool{}
		maxFiles := s.cfg.MaxPackageFiles * s.cfg.MaxPackageParts
		for len(queue) > 0 {
			dir := queue[0]
			queue = queue[1:]
			if seen[dir] {
				return ErrConflict
			}
			seen[dir] = true
			if len(seen) > 1000 {
				return ErrCapacity
			}
			children, e := tx.Directories(root.SchoolID, "", dir, 101, "")
			if e != nil {
				return e
			}
			if len(children) > 100 {
				return ErrCapacity
			}
			for _, child := range children {
				if child.Enabled {
					queue = append(queue, child.ID)
				}
			}
			after := ""
			for {
				records, e := tx.List(Filter{SchoolID: root.SchoolID, DirectoryID: dir, After: after, Limit: 100})
				if e != nil {
					return e
				}
				for _, r := range records {
					ids = append(ids, r.ID)
					if len(ids) > maxFiles {
						return ErrCapacity
					}
				}
				if len(records) < 100 {
					break
				}
				after = records[len(records)-1].ID
			}
		}
		return nil
	})
	return ids, e
}
func (s *Service) CreatePackage(ctx context.Context, p Principal, key string, in PackageRequest) (Package, error) {
	var out Package
	if !p.Valid() {
		return out, ErrUnauthenticated
	}
	if !validID(key) || in.DirectoryID == "" && len(in.ResourceIDs) == 0 || in.DirectoryID != "" && len(in.ResourceIDs) > 0 || !optionalID(in.DirectoryID) || len(in.ResourceIDs) > s.cfg.MaxPackageFiles*s.cfg.MaxPackageParts {
		return out, ErrInput
	}
	if e := s.auth(ctx, p, "package", ""); e != nil {
		return out, e
	}
	ids := in.ResourceIDs
	if in.DirectoryID != "" {
		var e error
		ids, e = s.directoryResources(ctx, in.DirectoryID)
		if e != nil {
			return out, e
		}
	}
	selected := [][]Selection{}
	parts := []PackagePart{}
	seen := map[string]bool{}
	for _, id := range ids {
		if !validID(id) || seen[id] {
			return out, ErrInput
		}
		seen[id] = true
		file, e := s.selection(ctx, p, id)
		if denied(e) || errors.Is(e, ErrNotFound) {
			continue
		}
		if e != nil {
			return out, e
		}
		if file.File.Size > s.cfg.MaxPackageBytes {
			return out, ErrCapacity
		}
		if len(parts) == 0 || len(parts[len(parts)-1].Files) >= s.cfg.MaxPackageFiles || parts[len(parts)-1].Bytes+file.File.Size > s.cfg.MaxPackageBytes {
			if len(parts) >= s.cfg.MaxPackageParts {
				return out, ErrCapacity
			}
			parts = append(parts, PackagePart{Number: len(parts) + 1, Files: []File{}})
			selected = append(selected, []Selection{})
		}
		n := len(parts) - 1
		parts[n].Files = append(parts[n].Files, file.File)
		parts[n].Bytes += file.File.Size
		selected[n] = append(selected[n], file)
	}
	if len(parts) == 0 {
		return out, ErrNotFound
	}
	e := s.mutation(ctx, p, "package", "", key, "", in, func(tx Transaction) (any, error) {
		out = Package{ID: newID(), Parts: parts, ExpiresAt: s.now().Add(s.cfg.PackageTTL)}
		if e := tx.SavePackage(PackageRecord{Package: out, Owner: p, Selections: selected}); e != nil {
			return nil, e
		}
		return out, nil
	}, &out)
	if e != nil {
		return Package{}, e
	}
	checked, e := s.GetPackage(ctx, p, out.ID)
	if e != nil {
		return Package{}, e
	}
	return checked.Package, nil
}
func (s *Service) GetPackage(ctx context.Context, p Principal, id string) (PackageRecord, error) {
	var out PackageRecord
	if !p.Valid() {
		return out, ErrUnauthenticated
	}
	if !validID(id) {
		return out, ErrInput
	}
	e := s.repo.Transaction(ctx, func(tx Transaction) error { var e error; out, e = tx.Package(id); return e })
	if e != nil {
		return PackageRecord{}, e
	}
	if out.Owner != p {
		return PackageRecord{}, ErrNotFound
	}
	if !s.now().Before(out.Package.ExpiresAt) {
		return PackageRecord{}, ErrConflict
	}
	// Recheck before exposing the manifest as well as before delivering bytes.
	for _, part := range out.Selections {
		for _, old := range part {
			current, e := s.selection(ctx, p, old.ResourceID)
			if e != nil {
				return PackageRecord{}, e
			}
			if current.VersionID != old.VersionID || current.ReferenceID != old.ReferenceID {
				return PackageRecord{}, ErrConflict
			}
		}
	}
	return out, nil
}
func (s *Service) DownloadPackage(ctx context.Context, p Principal, id string, part int, w io.Writer) error {
	bundle, e := s.GetPackage(ctx, p, id)
	if e != nil {
		return e
	}
	if part < 1 || part > len(bundle.Selections) {
		return ErrInput
	}
	work, cancel := context.WithTimeout(ctx, s.cfg.OperationTimeout)
	defer cancel()
	return s.deps.Files.Archive(work, p, bundle.Selections[part-1], w)
}
