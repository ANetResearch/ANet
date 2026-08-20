package inv1

// runtime.go is the INV-1 RUNTIME type-tag assertion that complements the static import scan
// (inv1_test.go). The static scan proves org-runtime packages cannot even import the commons gossip/
// DHT layer; this adds a defense-in-depth runtime tripwire on the publish boundary: an object whose
// TYPE is marked org-scoped must never be handed to a commons publish, even if a future refactor
// introduced a code path that the static scan didn't anticipate (e.g. a generic publish helper).

import (
	"errors"
	"reflect"
)

// ErrOrgScopedOnCommons is returned by GuardCommonsPublish when an org-scoped object reaches a commons
// (global gossip/DHT) publish path — a programming error: org data is centralized and must never enter
// the p2p fabric (INV-1).
var ErrOrgScopedOnCommons = errors.New("inv1: org-scoped object on a commons publish path (INV-1 violation)")

// OrgScoped is the marker an org-scoped runtime type carries to declare it must never be published to
// the commons. It is a no-op method (a type tag), implemented by the canonical org-secret carriers
// (e.g. org.Credential, blackboard.CogUnit). New org-scoped types SHOULD implement it.
type OrgScoped interface {
	OrgScopedObject()
}

// GuardCommonsPublish asserts v is NOT org-scoped before it is published to the commons, returning
// ErrOrgScopedOnCommons if an OrgScoped type appears ANYWHERE in v's static type graph — v itself, or
// reachable through a pointer / slice / array / map element-or-key / struct field (bounded by a
// visited set). Call it at every commons publish boundary (discovery.Announce, the commons boards) as
// the runtime INV-1 tripwire that complements the static import scan.
//
// LIMIT: it inspects the STATIC type graph, so it cannot see through an interface field (e.g. an
// `any` holding an org object at runtime) — those dynamic types are invisible to a type walk. At the
// current call sites the published types (anrp.NameRecord, adp.AgentCard) carry no OrgScoped type, so
// the guard passes; its value is catching a FUTURE refactor that routes an org-scoped object (directly
// or embedded as a field) to a commons publish. A nil value passes.
func GuardCommonsPublish(v any) error {
	if v == nil {
		return nil
	}
	marker := reflect.TypeOf((*OrgScoped)(nil)).Elem()
	if typeContainsOrgScoped(reflect.TypeOf(v), marker, map[reflect.Type]bool{}) {
		return ErrOrgScopedOnCommons
	}
	return nil
}

// typeContainsOrgScoped reports whether t — or any type reachable from it — implements OrgScoped. The
// visited set bounds self-referential types.
func typeContainsOrgScoped(t, marker reflect.Type, seen map[reflect.Type]bool) bool {
	if t == nil || seen[t] {
		return false
	}
	seen[t] = true
	if t.Implements(marker) || reflect.PtrTo(t).Implements(marker) {
		return true
	}
	switch t.Kind() {
	case reflect.Ptr, reflect.Slice, reflect.Array:
		return typeContainsOrgScoped(t.Elem(), marker, seen)
	case reflect.Map:
		return typeContainsOrgScoped(t.Key(), marker, seen) || typeContainsOrgScoped(t.Elem(), marker, seen)
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			if typeContainsOrgScoped(t.Field(i).Type, marker, seen) {
				return true
			}
		}
	}
	return false
}
