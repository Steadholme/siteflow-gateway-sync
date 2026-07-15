// Package sfsync reconciles the set of publicly reachable SiteFlow deployment
// hosts into the Steadholme gateway (Sluice) `routes` table, so a deployed site
// becomes reachable from the public internet through the gateway.
//
// The whole design turns on one invariant: this service owns exactly the routes
// whose name is in the "sfsite-" namespace and never reads, writes, or deletes
// any other row. The estate core routes (relay-*, cistern-*, ...) are sacred; a
// bug here must not be able to touch them. Every write path re-asserts the
// namespace at both the application layer (this package) and the SQL layer
// (postgres.go WHERE clauses).
package sfsync

import (
	"context"
	"crypto/sha1" //nolint:gosec // sha1 is used only to derive a stable route name, not for security
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

const (
	// RoutePrefix is the namespace this service owns in the routes table. Only
	// rows whose name starts with it are ever upserted or deleted.
	RoutePrefix = "sfsite-"

	// ConsoleHost is the SiteFlow control-plane host (SSO-protected). It must
	// never be published as a public route, so it is always filtered out.
	ConsoleHost = "siteflow.w33d.xyz"

	// InternalSuffix marks internal-only hosts that must stay off the public
	// gateway; artifact hosts with this suffix are dropped.
	InternalSuffix = ".holdfast.internal"

	// DefaultUpstream is the gateway upstream every SiteFlow site route points at
	// unless overridden by SITEFLOW_UPSTREAM.
	DefaultUpstream = "http://siteflow-api:9360"
)

// Route is a single row this service manages in the Steadholme routes table. It
// intentionally omits the waf column so the table's own DEFAULT applies and this
// service never overrides an operator's WAF setting.
type Route struct {
	Name         string
	Host         string
	PathPrefix   string
	Upstream     string
	Protected    bool
	Auth         string
	RequireGroup string
}

// Source reads the public-host inputs from the (read-only) SiteFlow database.
type Source interface {
	// ArtifactHosts returns every siteflow_artifact_routes.host (internal hosts
	// included; they are filtered out downstream).
	ArtifactHosts(ctx context.Context) ([]string, error)
	// VerifiedDomains returns siteflow_project_domains.hostname for verified=true
	// rows only. The verified filter lives in the SQL so an unverified custom
	// domain can never enter the pipeline.
	VerifiedDomains(ctx context.Context) ([]string, error)
}

// Store reads and writes only the sfsite- namespace of the Steadholme routes table.
type Store interface {
	// ManagedNames lists the names of existing sfsite- rows (for prune diffing).
	ManagedNames(ctx context.Context) ([]string, error)
	// Upsert inserts or updates one sfsite- route.
	Upsert(ctx context.Context, r Route) error
	// Delete removes one sfsite- route by name.
	Delete(ctx context.Context, name string) error
}

// RouteName derives the deterministic, namespaced route name for a host:
// "sfsite-" + first 12 hex chars of sha1(host). Stable across rounds so the same
// host always upserts the same row.
func RouteName(host string) string {
	sum := sha1.Sum([]byte(host)) //nolint:gosec // stable identifier, not a security hash
	return RoutePrefix + hex.EncodeToString(sum[:])[:12]
}

// assertManaged is the application-layer namespace guard: it refuses any name
// outside the sfsite- namespace. Every write method calls it before touching the
// database, on top of the SQL WHERE guard, so a foreign row can never be mutated.
func assertManaged(name string) error {
	if !strings.HasPrefix(name, RoutePrefix) {
		return fmt.Errorf("route %q is outside the %q namespace; refusing to write", name, RoutePrefix)
	}
	return nil
}

// normalizeHost lowercases and trims a raw host for stable comparison/keying.
func normalizeHost(h string) string {
	return strings.ToLower(strings.TrimSpace(h))
}

// validPublicHost reports whether a normalized host is safe to expose publicly:
// non-empty, contains a dot, no whitespace, no wildcard, not the SSO console, and
// not an internal-only host. Anything else is dropped so we never route an
// arbitrary or user-controlled value (open-proxy / certificate-abuse guard).
func validPublicHost(h string) bool {
	switch {
	case h == "":
		return false
	case !strings.Contains(h, "."):
		return false
	case strings.ContainsAny(h, " \t\r\n"):
		return false
	case strings.Contains(h, "*"):
		return false
	case h == ConsoleHost:
		return false
	case strings.HasSuffix(h, InternalSuffix):
		return false
	default:
		return true
	}
}

// CollectHosts merges the two SiteFlow host sources into the deduped, sorted set
// of hosts that should be publicly routable. Internal, wildcard, malformed, and
// console hosts are dropped. Verified domains are already verified=true at the
// SQL layer; both inputs still pass validPublicHost here as defense in depth.
func CollectHosts(artifactHosts, verifiedDomains []string) []string {
	set := make(map[string]struct{})
	add := func(list []string) {
		for _, h := range list {
			h = normalizeHost(h)
			if !validPublicHost(h) {
				continue
			}
			set[h] = struct{}{}
		}
	}
	add(artifactHosts)
	add(verifiedDomains)

	out := make([]string, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// DesiredRoutes turns the raw SiteFlow host inputs into the target sfsite- route
// rows. Each route is a public, unprotected proxy to the SiteFlow upstream. A
// pure function: no I/O, so the filtering and row shape are unit-testable.
func DesiredRoutes(artifactHosts, verifiedDomains []string, upstream string) []Route {
	if upstream == "" {
		upstream = DefaultUpstream
	}
	hosts := CollectHosts(artifactHosts, verifiedDomains)
	routes := make([]Route, 0, len(hosts))
	for _, h := range hosts {
		routes = append(routes, Route{
			Name:         RouteName(h),
			Host:         h,
			PathPrefix:   "/",
			Upstream:     upstream,
			Protected:    false,
			Auth:         "public",
			RequireGroup: "",
		})
	}
	return routes
}

// PruneList returns the sfsite- route names to delete: those present in the store
// but absent from keep. It NEVER returns a name outside the sfsite- namespace,
// even if the store returns a foreign name, so estate core routes can never be
// selected for deletion.
func PruneList(existing []string, keep map[string]bool) []string {
	var del []string
	for _, name := range existing {
		if !strings.HasPrefix(name, RoutePrefix) {
			continue
		}
		if keep[name] {
			continue
		}
		del = append(del, name)
	}
	sort.Strings(del)
	return del
}

// Stats summarizes one reconcile round for logging.
type Stats struct {
	Hosts        int
	Upserted     int
	Pruned       int
	PruneSkipped bool
}

// Syncer performs one reconcile round per RunOnce call.
type Syncer struct {
	src      Source
	store    Store
	upstream string
	log      *slog.Logger
}

// NewSyncer builds a Syncer. An empty upstream falls back to DefaultUpstream and
// a nil logger falls back to slog.Default.
func NewSyncer(src Source, store Store, upstream string, log *slog.Logger) *Syncer {
	if upstream == "" {
		upstream = DefaultUpstream
	}
	if log == nil {
		log = slog.Default()
	}
	return &Syncer{src: src, store: store, upstream: upstream, log: log}
}

// RunOnce executes a single reconcile pass:
//  1. Read the SiteFlow snapshot (artifact hosts + verified domains).
//  2. Upsert every desired sfsite- route.
//  3. Prune sfsite- routes no longer backed by a live host.
//
// Fail-safe guarantees:
//   - A read error aborts the whole round before any write, leaving all existing
//     routes intact (we would rather keep stale routes than take sites down on a
//     transient SiteFlow outage).
//   - An empty snapshot upserts nothing and SKIPS prune, so a momentarily empty
//     read can never delete every deployed site's route.
//   - Prune only ever runs after a positive snapshot and only over sfsite- names.
func (s *Syncer) RunOnce(ctx context.Context) (Stats, error) {
	artifactHosts, err := s.src.ArtifactHosts(ctx)
	if err != nil {
		return Stats{}, fmt.Errorf("read siteflow artifact hosts: %w", err)
	}
	verifiedDomains, err := s.src.VerifiedDomains(ctx)
	if err != nil {
		return Stats{}, fmt.Errorf("read siteflow verified domains: %w", err)
	}

	desired := DesiredRoutes(artifactHosts, verifiedDomains, s.upstream)

	for _, r := range desired {
		if err := s.store.Upsert(ctx, r); err != nil {
			return Stats{}, fmt.Errorf("upsert route %s (%s): %w", r.Name, r.Host, err)
		}
	}

	st := Stats{Hosts: len(desired), Upserted: len(desired)}

	// Fail-safe prune: an empty desired set is treated as suspicious and never
	// triggers mass deletion. Prune only runs on a positive snapshot.
	if len(desired) == 0 {
		s.log.Warn("siteflow snapshot yielded zero public hosts; skipping prune (fail-safe)")
		st.PruneSkipped = true
		return st, nil
	}

	keep := make(map[string]bool, len(desired))
	for _, r := range desired {
		keep[r.Name] = true
	}

	existing, err := s.store.ManagedNames(ctx)
	if err != nil {
		return st, fmt.Errorf("list managed routes: %w", err)
	}
	for _, name := range PruneList(existing, keep) {
		if err := s.store.Delete(ctx, name); err != nil {
			return st, fmt.Errorf("prune route %s: %w", name, err)
		}
		st.Pruned++
	}
	return st, nil
}
