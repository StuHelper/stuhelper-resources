package postgres

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	resources "github.com/StuHelper/stuhelper-resources"
)

//go:embed default-categories.json
var initialCategories []byte

// Migrate uses a separate migration identity. Initial categories are ordinary
// persisted settings; repeated migrations never overwrite operator changes.
func Migrate(ctx context.Context, db *sql.DB) error {
	tx, e := db.BeginTx(ctx, nil)
	if e != nil {
		return safe(e)
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, Schema); e != nil {
		return safe(e)
	}
	var categories []resources.Category
	if json.Unmarshal(initialCategories, &categories) != nil {
		return resources.ErrInput
	}
	for _, category := range categories {
		if e = resources.ValidateCategory(resources.CategoryWrite{Names: category.Names, Enabled: category.Enabled, Order: category.Order, Fields: category.Fields, GroupBy: category.GroupBy}); e != nil {
			return e
		}
		data, e := json.Marshal(category)
		if e != nil {
			return resources.ErrInput
		}
		result, e := tx.ExecContext(ctx, `INSERT INTO resources.categories(id,revision,enabled,data) VALUES($1,1,true,$2) ON CONFLICT(id) DO NOTHING`, category.ID, string(data))
		if e != nil {
			return safe(e)
		}
		n, e := result.RowsAffected()
		if e != nil {
			return safe(e)
		}
		if n == 1 {
			if _, e = tx.ExecContext(ctx, `INSERT INTO resources.category_revisions(id,revision,data) VALUES($1,1,$2)`, category.ID, string(data)); e != nil {
				return safe(e)
			}
		}
	}
	return safe(tx.Commit())
}
