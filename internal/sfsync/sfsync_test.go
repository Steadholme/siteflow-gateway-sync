package sfsync

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeSource is an in-memory Source; no database required.
type fakeSource struct {
	artifact    []string
	domains     []string
	artifactErr error
	domainsErr  error
}

func (f *fakeSource) ArtifactHosts(context.Context) ([]string, error) {
	return f.artifact, f.artifactErr
}
func (f *fakeSource) VerifiedDomains(context.Context) ([]string, error) {
	return f.domains, f.domainsErr
}

// fakeStore mirrors the real Store's namespace guards: it holds every row
// (including non-sfsite estate routes) and only ever reports/deletes sfsite-
// rows, exactly like the production SQL WHERE clauses. It fails loudly if asked
// to write outside the namespace so a regression is caught in tests.
type fakeStore struct {
	rows    map[string]Route
	upserts int
	deleted []string
}

func newFakeStore(rows map[string]Route) *fakeStore {
	if rows == nil {
		rows = map[string]Route{}
	}
	return &fakeStore{rows: rows}
}

func (f *fakeStore) ManagedNames(context.Context) ([]string, error) {
	var out []string
	for name := range f.rows {
		if strings.HasPrefix(name, RoutePrefix) {
			out = append(out, name)
		}
	}
	return out, nil
}

func (f *fakeStore) Upsert(_ context.Context, r Route) error {
	if !strings.HasPrefix(r.Name, RoutePrefix) {
		return errors.New("fakeStore: refused non-sfsite upsert " + r.Name)
	}
	f.rows[r.Name] = r
	f.upserts++
	return nil
}

func (f *fakeStore) Delete(_ context.Context, name string) error {
	if !strings.HasPrefix(name, RoutePrefix) {
		return errors.New("fakeStore: refused non-sfsite delete " + name)
	}
	delete(f.rows, name)
	f.deleted = append(f.deleted, name)
	return nil
}

// (a) internal .holdfast.internal hosts are excluded.
func TestCollectHostsExcludesInternal(t *testing.T) {
	got := CollectHosts(
		[]string{
			"app.siteflow.w33d.xyz",
			"buildbox.holdfast.internal",
			"preview-x.siteflow.w33d.xyz",
			"api.holdfast.internal",
		},
		nil,
	)
	want := []string{"app.siteflow.w33d.xyz", "preview-x.siteflow.w33d.xyz"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CollectHosts = %v, want %v", got, want)
	}
	for _, h := range got {
		if strings.HasSuffix(h, InternalSuffix) {
			t.Fatalf("internal host %q leaked into public set", h)
		}
	}
}

// (b) unverified custom domains are excluded — enforced by the SQL filter, so the
// query must be verified-only.
func TestVerifiedDomainsQueryFiltersVerified(t *testing.T) {
	if !strings.Contains(selectVerifiedDomainsSQL, "siteflow_project_domains") {
		t.Fatalf("verified-domains query does not target siteflow_project_domains: %q", selectVerifiedDomainsSQL)
	}
	if !strings.Contains(selectVerifiedDomainsSQL, "verified = true") {
		t.Fatalf("verified-domains query is not verified-only (missing `verified = true`): %q", selectVerifiedDomainsSQL)
	}
}

// (c) generated route rows have the correct fields.
func TestDesiredRoutesFields(t *testing.T) {
	// Mixed case input proves hosts are normalized to lower case.
	routes := DesiredRoutes([]string{"App.Siteflow.W33d.xyz"}, nil, "")
	if len(routes) != 1 {
		t.Fatalf("got %d routes, want 1", len(routes))
	}
	r := routes[0]

	const host = "app.siteflow.w33d.xyz"
	if r.Host != host {
		t.Fatalf("host = %q, want %q (normalized)", r.Host, host)
	}
	if r.Name != RouteName(host) {
		t.Fatalf("name = %q, want %q", r.Name, RouteName(host))
	}
	if !strings.HasPrefix(r.Name, RoutePrefix) {
		t.Fatalf("name %q missing %q prefix", r.Name, RoutePrefix)
	}
	if len(r.Name) != len(RoutePrefix)+12 {
		t.Fatalf("name %q length = %d, want %d", r.Name, len(r.Name), len(RoutePrefix)+12)
	}
	if r.PathPrefix != "/" {
		t.Fatalf("path_prefix = %q, want /", r.PathPrefix)
	}
	if r.Upstream != DefaultUpstream {
		t.Fatalf("upstream = %q, want default %q", r.Upstream, DefaultUpstream)
	}
	if r.Auth != "public" {
		t.Fatalf("auth = %q, want public", r.Auth)
	}
	if r.Protected {
		t.Fatalf("protected = true, want false")
	}
	if r.RequireGroup != "" {
		t.Fatalf("require_group = %q, want empty", r.RequireGroup)
	}
}

func TestDesiredRoutesUpstreamOverride(t *testing.T) {
	routes := DesiredRoutes([]string{"app.siteflow.w33d.xyz"}, nil, "http://siteflow-api:1234")
	if len(routes) != 1 || routes[0].Upstream != "http://siteflow-api:1234" {
		t.Fatalf("upstream override not applied: %+v", routes)
	}
}

// Custom verified domains are included alongside SiteFlow hosts.
func TestDesiredRoutesIncludesVerifiedDomain(t *testing.T) {
	routes := DesiredRoutes(
		[]string{"app.siteflow.w33d.xyz"},
		[]string{"www.customer.com"},
		"",
	)
	hosts := map[string]bool{}
	for _, r := range routes {
		hosts[r.Host] = true
	}
	if !hosts["www.customer.com"] {
		t.Fatalf("verified custom domain not routed: %+v", routes)
	}
}

// Safety red line: the SSO console host is never routed, from either source.
func TestConsoleHostNeverRouted(t *testing.T) {
	got := CollectHosts([]string{ConsoleHost, "app.siteflow.w33d.xyz"}, []string{ConsoleHost})
	for _, h := range got {
		if h == ConsoleHost {
			t.Fatalf("console host %q was exposed as a public route", ConsoleHost)
		}
	}
}

// Malformed / wildcard / empty hosts are dropped.
func TestCollectHostsRejectsMalformed(t *testing.T) {
	got := CollectHosts([]string{
		"",                       // empty
		"nodot",                  // no dot
		"*.siteflow.w33d.xyz",    // wildcard
		"has space.w33d.xyz",     // whitespace
		"good.siteflow.w33d.xyz", // the only valid one
	}, nil)
	if !reflect.DeepEqual(got, []string{"good.siteflow.w33d.xyz"}) {
		t.Fatalf("CollectHosts = %v, want [good.siteflow.w33d.xyz]", got)
	}
}

// (d) PruneList only selects stale sfsite- rows and NEVER a core estate route,
// even if the store hands back foreign names.
func TestPruneListNeverTouchesCoreRoutes(t *testing.T) {
	existing := []string{
		"sfsite-aaaaaaaaaaaa",
		"sfsite-bbbbbbbbbbbb",
		"relay-v1",     // estate core route
		"cistern-dash", // estate core route
	}
	keep := map[string]bool{"sfsite-aaaaaaaaaaaa": true}

	got := PruneList(existing, keep)
	if !reflect.DeepEqual(got, []string{"sfsite-bbbbbbbbbbbb"}) {
		t.Fatalf("PruneList = %v, want [sfsite-bbbbbbbbbbbb]", got)
	}
	for _, n := range got {
		if !strings.HasPrefix(n, RoutePrefix) {
			t.Fatalf("PruneList selected non-sfsite name %q", n)
		}
		if n == "relay-v1" || n == "cistern-dash" {
			t.Fatalf("PruneList selected estate core route %q for deletion", n)
		}
	}
}

// (d) full-round prune: a stale sfsite- route is removed while estate core routes
// survive untouched.
func TestRunOncePrunesStaleKeepsCore(t *testing.T) {
	live := "live.siteflow.w33d.xyz"
	liveName := RouteName(live)
	staleName := "sfsite-deadbeef0000"

	store := newFakeStore(map[string]Route{
		staleName:      {Name: staleName, Host: "gone.siteflow.w33d.xyz"},
		"relay-v1":     {Name: "relay-v1", Host: "relay.w33d.xyz"},
		"cistern-dash": {Name: "cistern-dash", Host: "cistern.w33d.xyz"},
	})
	src := &fakeSource{artifact: []string{live}}

	st, err := NewSyncer(src, store, "", testLogger()).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if _, ok := store.rows[liveName]; !ok {
		t.Fatalf("live route %q was not upserted", liveName)
	}
	if _, ok := store.rows[staleName]; ok {
		t.Fatalf("stale sfsite route %q was not pruned", staleName)
	}
	if _, ok := store.rows["relay-v1"]; !ok {
		t.Fatal("estate core route relay-v1 was deleted")
	}
	if _, ok := store.rows["cistern-dash"]; !ok {
		t.Fatal("estate core route cistern-dash was deleted")
	}
	for _, n := range store.deleted {
		if !strings.HasPrefix(n, RoutePrefix) {
			t.Fatalf("deleted a non-sfsite route %q", n)
		}
	}
	if st.Pruned != 1 {
		t.Fatalf("pruned = %d, want 1", st.Pruned)
	}
}

// (e) a SiteFlow read failure aborts the round before any write: no upsert, no
// prune, all existing rows intact.
func TestRunOnceSkipsPruneOnReadError(t *testing.T) {
	store := newFakeStore(map[string]Route{
		"sfsite-deadbeef0000": {Name: "sfsite-deadbeef0000"},
		"relay-v1":            {Name: "relay-v1"},
	})
	src := &fakeSource{artifactErr: errors.New("siteflow db down")}

	_, err := NewSyncer(src, store, "", testLogger()).RunOnce(context.Background())
	if err == nil {
		t.Fatal("expected error on read failure, got nil")
	}
	if len(store.deleted) != 0 {
		t.Fatalf("prune ran despite read failure: deleted %v", store.deleted)
	}
	if store.upserts != 0 {
		t.Fatalf("upsert ran despite read failure: %d upserts", store.upserts)
	}
	if len(store.rows) != 2 {
		t.Fatalf("rows changed on failed round: %v", store.rows)
	}
}

// Verified-domains read failure is also fail-safe (aborts before prune).
func TestRunOnceSkipsPruneOnDomainReadError(t *testing.T) {
	store := newFakeStore(map[string]Route{"sfsite-deadbeef0000": {Name: "sfsite-deadbeef0000"}})
	src := &fakeSource{artifact: []string{"app.siteflow.w33d.xyz"}, domainsErr: errors.New("boom")}

	if _, err := NewSyncer(src, store, "", testLogger()).RunOnce(context.Background()); err == nil {
		t.Fatal("expected error on domain read failure")
	}
	if len(store.deleted) != 0 || store.upserts != 0 {
		t.Fatalf("writes happened despite domain read failure: deleted=%v upserts=%d", store.deleted, store.upserts)
	}
}

// An empty (but successful) snapshot upserts nothing and skips prune, so a
// transient empty read cannot take every deployed site offline.
func TestRunOnceSkipsPruneOnEmptySnapshot(t *testing.T) {
	staleName := "sfsite-deadbeef0000"
	store := newFakeStore(map[string]Route{staleName: {Name: staleName}})
	src := &fakeSource{} // both queries succeed but return nothing

	st, err := NewSyncer(src, store, "", testLogger()).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !st.PruneSkipped {
		t.Fatal("expected PruneSkipped on empty snapshot")
	}
	if len(store.deleted) != 0 {
		t.Fatalf("prune ran on empty snapshot: deleted %v", store.deleted)
	}
	if _, ok := store.rows[staleName]; !ok {
		t.Fatalf("stale route %q deleted on empty snapshot", staleName)
	}
}

// assertManaged rejects any name outside the sfsite- namespace.
func TestAssertManaged(t *testing.T) {
	if err := assertManaged("sfsite-abc123"); err != nil {
		t.Fatalf("assertManaged rejected a valid sfsite name: %v", err)
	}
	for _, bad := range []string{"relay-v1", "cistern-dash", "", "sfsit-x", "SFSITE-x"} {
		if err := assertManaged(bad); err == nil {
			t.Fatalf("assertManaged accepted foreign name %q", bad)
		}
	}
}

// RouteName is deterministic and correctly namespaced.
func TestRouteNameDeterministic(t *testing.T) {
	a := RouteName("app.siteflow.w33d.xyz")
	b := RouteName("app.siteflow.w33d.xyz")
	if a != b {
		t.Fatalf("RouteName not deterministic: %q != %q", a, b)
	}
	if RouteName("a.example.com") == RouteName("b.example.com") {
		t.Fatal("RouteName collided for different hosts")
	}
	if !strings.HasPrefix(a, RoutePrefix) || len(a) != len(RoutePrefix)+12 {
		t.Fatalf("RouteName %q malformed", a)
	}
}
