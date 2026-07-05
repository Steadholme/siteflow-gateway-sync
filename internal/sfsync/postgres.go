package sfsync

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SiteflowDB reads the public-host sources from the SiteFlow database. The pool
// is opened read-only (default_transaction_read_only=on) in main, so this side
// can never mutate SiteFlow even by mistake; it also only ever issues SELECTs.
type SiteflowDB struct {
	pool *pgxpool.Pool
}

// NewSiteflowDB wraps a read-only SiteFlow connection pool.
func NewSiteflowDB(pool *pgxpool.Pool) *SiteflowDB { return &SiteflowDB{pool: pool} }

const (
	selectArtifactHostsSQL = `SELECT host FROM siteflow_artifact_routes`
	// The verified filter is enforced here in SQL: an unverified custom domain is
	// never returned, so it can never become a public route.
	selectVerifiedDomainsSQL = `SELECT hostname FROM siteflow_project_domains WHERE verified = true`
)

// ArtifactHosts returns every siteflow_artifact_routes.host.
func (s *SiteflowDB) ArtifactHosts(ctx context.Context) ([]string, error) {
	return queryStrings(ctx, s.pool, selectArtifactHostsSQL)
}

// VerifiedDomains returns siteflow_project_domains.hostname for verified rows.
func (s *SiteflowDB) VerifiedDomains(ctx context.Context) ([]string, error) {
	return queryStrings(ctx, s.pool, selectVerifiedDomainsSQL)
}

// RoutesDB reads and writes only the sfsite- namespace of the Holdfast routes
// table. Every write re-asserts the namespace at the SQL layer so an estate core
// route can never be mutated.
type RoutesDB struct {
	pool *pgxpool.Pool
}

// NewRoutesDB wraps the read-write Holdfast routes connection pool.
func NewRoutesDB(pool *pgxpool.Pool) *RoutesDB { return &RoutesDB{pool: pool} }

const (
	// upsertRouteSQL writes a single sfsite- route. The waf column is deliberately
	// omitted from the INSERT so the routes table DEFAULT (FALSE) applies and it is
	// never touched on UPDATE, honoring any operator WAF setting. The
	// `WHERE routes.name LIKE 'sfsite-%'` on DO UPDATE is a hard namespace guard:
	// even on a name collision, only an sfsite- row can be updated.
	upsertRouteSQL = `
INSERT INTO routes (name, host, path_prefix, upstream, protected, auth, require_group)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (name) DO UPDATE SET
    host          = EXCLUDED.host,
    path_prefix   = EXCLUDED.path_prefix,
    upstream      = EXCLUDED.upstream,
    protected     = EXCLUDED.protected,
    auth          = EXCLUDED.auth,
    require_group = EXCLUDED.require_group
WHERE routes.name LIKE 'sfsite-%'`

	// selectManagedNamesSQL lists only the routes this service owns.
	selectManagedNamesSQL = `SELECT name FROM routes WHERE name LIKE 'sfsite-%'`

	// deleteRouteSQL removes one route, re-asserting the sfsite- namespace so a
	// non-sfsite name can never be deleted even if one were passed by mistake.
	deleteRouteSQL = `DELETE FROM routes WHERE name = $1 AND name LIKE 'sfsite-%'`
)

// ManagedNames lists existing sfsite- route names.
func (r *RoutesDB) ManagedNames(ctx context.Context) ([]string, error) {
	return queryStrings(ctx, r.pool, selectManagedNamesSQL)
}

// Upsert inserts or updates one sfsite- route. It refuses any name outside the
// namespace before touching the database.
func (r *RoutesDB) Upsert(ctx context.Context, rt Route) error {
	if err := assertManaged(rt.Name); err != nil {
		return err
	}
	if _, err := r.pool.Exec(ctx, upsertRouteSQL,
		rt.Name, rt.Host, rt.PathPrefix, rt.Upstream, rt.Protected, rt.Auth, rt.RequireGroup); err != nil {
		return fmt.Errorf("upsert route %s: %w", rt.Name, err)
	}
	return nil
}

// Delete removes one sfsite- route by name. It refuses any name outside the
// namespace before touching the database.
func (r *RoutesDB) Delete(ctx context.Context, name string) error {
	if err := assertManaged(name); err != nil {
		return err
	}
	if _, err := r.pool.Exec(ctx, deleteRouteSQL, name); err != nil {
		return fmt.Errorf("delete route %s: %w", name, err)
	}
	return nil
}

// queryStrings runs a single-column text query and collects the results.
func queryStrings(ctx context.Context, pool *pgxpool.Pool, sql string) ([]string, error) {
	rows, err := pool.Query(ctx, sql)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate: %w", err)
	}
	return out, nil
}
