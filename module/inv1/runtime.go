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
// It walks the static type graph AND, where a type cannot answer, the actual values: everything this
// daemon publishes travels as map[string]any, so a type-only walk reached `interface{}`, found no
// marker, and passed. A runtime tripwire blind to runtime values is not a tripwire.
//
// A nil value passes. The value walk is bounded by depth and by a visited-pointer set, so a
// self-referential structure terminates.
func GuardCommonsPublish(v any) error {
	if v == nil {
		return nil
	}
	marker := reflect.TypeOf((*OrgScoped)(nil)).Elem()
	if typeContainsOrgScoped(reflect.TypeOf(v), marker, map[reflect.Type]bool{}) {
		return ErrOrgScopedOnCommons
	}
	// Then the values, because the type walk alone could not see the only
	// boundary this repository has.
	//
	// The static walk cannot look through an interface field: the type is
	// `any` and what it holds is a runtime fact. Everything published
	// here travels as map[string]any, so the walk reached `interface{}`,
	// found no marker, and passed — a runtime tripwire that could not see
	// runtime values, guarding the one path it was finally attached to
	// and proving nothing about it.
	//
	// Values are walked only where types cannot answer: an interface, and
	// the containers that can hold one. Depth is bounded by the same
	// visited-pointer discipline the type walk uses, so a self-
	// referential structure terminates.
	if valueContainsOrgScoped(reflect.ValueOf(v), marker, 0, map[uintptr]bool{}) {
		return ErrOrgScopedOnCommons
	}
	return nil
}

// maxValueDepth bounds the value walk. Published bodies are shallow maps;
// anything deeper than this is not a publication shape and the guard
// should not spend the stack on it.
const maxValueDepth = 24

// valueContainsOrgScoped walks actual values to find an org-scoped object
// hiding behind an interface, which the type walk is blind to.
func valueContainsOrgScoped(v reflect.Value, marker reflect.Type, depth int, seen map[uintptr]bool) bool {
	if !v.IsValid() || depth > maxValueDepth {
		return false
	}
	t := v.Type()
	if t.Implements(marker) || (v.CanAddr() && reflect.PtrTo(t).Implements(marker)) {
		return true
	}
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return false
		}
		return valueContainsOrgScoped(v.Elem(), marker, depth+1, seen)
	case reflect.Ptr:
		if v.IsNil() {
			return false
		}
		// One visit per address: a cycle would otherwise not terminate.
		if p := v.Pointer(); seen[p] {
			return false
		} else {
			seen[p] = true
		}
		if reflect.PtrTo(v.Elem().Type()).Implements(marker) {
			return true
		}
		return valueContainsOrgScoped(v.Elem(), marker, depth+1, seen)
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if valueContainsOrgScoped(v.Index(i), marker, depth+1, seen) {
				return true
			}
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			if valueContainsOrgScoped(k, marker, depth+1, seen) ||
				valueContainsOrgScoped(v.MapIndex(k), marker, depth+1, seen) {
				return true
			}
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			// Unexported fields cannot be read through reflection, and the
			// type walk already covered their declared types.
			if !t.Field(i).IsExported() {
				continue
			}
			if valueContainsOrgScoped(v.Field(i), marker, depth+1, seen) {
				return true
			}
		}
	}
	return false
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
