// Package authz implements authorization: wildcard permission matching, role-based
// access control with inheritance, and resource policies.
//
// Permissions are "resource:action" strings with optional wildcards:
// "orders:read", "orders:*", "*:read", "*". Matching is segment-based, so "orders:*"
// grants "orders:refund" but not "ordersarchive:read".
package authz

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/werhani-houssemeddine/go-archkit/errs"
	"github.com/werhani-houssemeddine/go-archkit/execctx"
)

// Matches reports whether a granted permission covers the required one.
func Matches(granted, required string) bool {
	if granted == "*" || granted == required {
		return true
	}
	g := strings.Split(granted, ":")
	r := strings.Split(required, ":")
	if len(g) != len(r) {
		// "orders:*" also covers deeper permissions like "orders:items:write"
		if g[len(g)-1] == "*" && len(g) < len(r) {
			for i, seg := range g[:len(g)-1] {
				if seg != "*" && seg != r[i] {
					return false
				}
			}
			return true
		}
		return false
	}
	for i := range g {
		if g[i] != "*" && g[i] != r[i] {
			return false
		}
	}
	return true
}

// HasAll reports whether granted covers every required permission.
func HasAll(granted []string, required ...string) bool {
	for _, req := range required {
		if !slices.ContainsFunc(granted, func(g string) bool { return Matches(g, req) }) {
			return false
		}
	}
	return true
}

// HasAny reports whether granted covers at least one required permission.
func HasAny(granted []string, required ...string) bool {
	for _, req := range required {
		if slices.ContainsFunc(granted, func(g string) bool { return Matches(g, req) }) {
			return true
		}
	}
	return false
}

// Role defines permissions and inherited roles.
type Role struct {
	Permissions []string
	Inherits    []string
}

// RBAC resolves roles (with inheritance) to permissions. Immutable after New; safe for
// concurrent use.
type RBAC struct {
	resolved map[string][]string
}

// NewRBAC validates the role graph (unknown or circular inheritance is an error).
func NewRBAC(roles map[string]Role) (*RBAC, error) {
	r := &RBAC{resolved: map[string][]string{}}
	var resolve func(name string, chain []string) ([]string, error)
	resolve = func(name string, chain []string) ([]string, error) {
		if slices.Contains(chain, name) {
			return nil, fmt.Errorf("authz: circular role inheritance %s", strings.Join(append(chain, name), " -> "))
		}
		def, ok := roles[name]
		if !ok {
			return nil, fmt.Errorf("authz: unknown role %q", name)
		}
		var perms []string
		for _, parent := range def.Inherits {
			p, err := resolve(parent, append(chain, name))
			if err != nil {
				return nil, err
			}
			perms = append(perms, p...)
		}
		perms = append(perms, def.Permissions...)
		slices.Sort(perms)
		return slices.Compact(perms), nil
	}
	for name := range roles {
		p, err := resolve(name, nil)
		if err != nil {
			return nil, err
		}
		r.resolved[name] = p
	}
	return r, nil
}

// MustRBAC is NewRBAC that panics on configuration errors (for package-level setup).
func MustRBAC(roles map[string]Role) *RBAC {
	r, err := NewRBAC(roles)
	if err != nil {
		panic(err)
	}
	return r
}

// PermissionsFor returns the flattened permissions of roles.
func (r *RBAC) PermissionsFor(roles ...string) []string {
	var out []string
	for _, role := range roles {
		out = append(out, r.resolved[role]...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Effective returns role permissions plus permissions granted directly to the actor.
func (r *RBAC) Effective(a *execctx.Actor) []string {
	if a == nil {
		return nil
	}
	out := append(r.PermissionsFor(a.Roles...), a.Permissions...)
	slices.Sort(out)
	return slices.Compact(out)
}

// Can reports whether the actor holds every required permission.
func (r *RBAC) Can(a *execctx.Actor, required ...string) bool {
	return a != nil && HasAll(r.Effective(a), required...)
}

// Expand returns a copy of the actor with role permissions merged into Permissions,
// so downstream checks (Require) only need the actor.
func (r *RBAC) Expand(a *execctx.Actor) *execctx.Actor {
	if a == nil {
		return nil
	}
	c := *a
	c.Permissions = r.Effective(a)
	return &c
}

// Require checks the context actor's permissions: errs Unauthenticated when anonymous,
// Forbidden (INSUFFICIENT_PERMISSIONS) when a permission is missing.
func Require(ctx context.Context, required ...string) error {
	a := execctx.ActorFrom(ctx)
	if a == nil {
		return errs.New(errs.KindUnauthenticated, "", "authentication required")
	}
	if !HasAll(a.Permissions, required...) {
		return errs.MissingPermissions(required...)
	}
	return nil
}

// RequireRole checks that the context actor has at least one of the roles.
func RequireRole(ctx context.Context, roles ...string) error {
	a := execctx.ActorFrom(ctx)
	if a == nil {
		return errs.New(errs.KindUnauthenticated, "", "authentication required")
	}
	if !a.HasRole(roles...) {
		e := errs.New(errs.KindForbidden, "INSUFFICIENT_ROLE", "you do not have the required role")
		return e.WithDetails(map[string]any{"required": roles})
	}
	return nil
}

/* ------------------------------------------------------------------ */
/* Resource policies                                                   */
/* ------------------------------------------------------------------ */

// Policy decides whether an actor may act on a resource ("only the author can edit a draft").
type Policy[R any] func(a *execctx.Actor, resource R) bool

// Authorize evaluates the policy against the context actor.
func Authorize[R any](ctx context.Context, p Policy[R], resource R) error {
	a := execctx.ActorFrom(ctx)
	if a == nil {
		return errs.New(errs.KindUnauthenticated, "", "authentication required")
	}
	if !p(a, resource) {
		return errs.New(errs.KindForbidden, "", "you do not have permission to access this resource")
	}
	return nil
}

// All requires every policy.
func All[R any](ps ...Policy[R]) Policy[R] {
	return func(a *execctx.Actor, r R) bool {
		for _, p := range ps {
			if !p(a, r) {
				return false
			}
		}
		return true
	}
}

// Any requires at least one policy.
func Any[R any](ps ...Policy[R]) Policy[R] {
	return func(a *execctx.Actor, r R) bool {
		for _, p := range ps {
			if p(a, r) {
				return true
			}
		}
		return false
	}
}

// Owner allows the actor whose id equals ownerID(resource).
func Owner[R any](ownerID func(R) string) Policy[R] {
	return func(a *execctx.Actor, r R) bool { return a.ID != "" && a.ID == ownerID(r) }
}

// SameTenant allows actors of the resource's tenant.
func SameTenant[R any](tenantID func(R) string) Policy[R] {
	return func(a *execctx.Actor, r R) bool { return a.TenantID != "" && a.TenantID == tenantID(r) }
}

// HasRole allows actors with any of the roles.
func HasRole[R any](roles ...string) Policy[R] {
	return func(a *execctx.Actor, _ R) bool { return a.HasRole(roles...) }
}

// HasPermission allows actors holding all permissions.
func HasPermission[R any](perms ...string) Policy[R] {
	return func(a *execctx.Actor, _ R) bool { return HasAll(a.Permissions, perms...) }
}
