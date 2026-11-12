package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Recipe is a saved standalone build.
type Recipe struct {
	Name    string
	Spec    string // JSON, owned by the server's build form
	Updated time.Time
}

// SaveRecipe writes or replaces a recipe.
func (s *Store) SaveRecipe(ctx context.Context, r Recipe) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO build_recipes (name, spec, updated) VALUES (?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET spec = excluded.spec, updated = excluded.updated`,
		r.Name, r.Spec, r.Updated.UnixMilli())
	return err
}

// GetRecipe returns one recipe. ok is false when there is none by that name.
func (s *Store) GetRecipe(ctx context.Context, name string) (r Recipe, ok bool, err error) {
	var upd int64
	err = s.db.QueryRowContext(ctx, "SELECT name, spec, updated FROM build_recipes WHERE name = ?", name).Scan(&r.Name, &r.Spec, &upd)
	if errors.Is(err, sql.ErrNoRows) {
		return Recipe{}, false, nil
	}
	if err != nil {
		return Recipe{}, false, err
	}
	r.Updated = time.UnixMilli(upd)
	return r, true, nil
}

// ListRecipes returns every recipe by name.
func (s *Store) ListRecipes(ctx context.Context) ([]Recipe, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name, spec, updated FROM build_recipes ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Recipe
	for rows.Next() {
		var r Recipe
		var upd int64
		if err := rows.Scan(&r.Name, &r.Spec, &upd); err != nil {
			return nil, err
		}
		r.Updated = time.UnixMilli(upd)
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteRecipe removes a recipe.
func (s *Store) DeleteRecipe(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM build_recipes WHERE name = ?", name)
	return err
}
