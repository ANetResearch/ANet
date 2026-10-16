package anetlink

import (
	"reflect"
	"testing"

	"github.com/ANetResearch/ANet/provider"
)

// CORE-K14: a capability reached over C1 takes no files. The ANetLink
// provider must not implement provider.FileReceiver, so the daemon refuses
// to start a configuration that declares files for one of its capabilities.
func TestC1ProviderTakesNoFiles(t *testing.T) {
	recv := reflect.TypeOf((*provider.FileReceiver)(nil)).Elem()
	for _, typ := range []reflect.Type{reflect.TypeOf(&Provider{}), reflect.TypeOf(Provider{})} {
		if typ.Implements(recv) {
			t.Errorf("%s implements provider.FileReceiver", typ)
		}
		if _, ok := typ.MethodByName("AcceptsFiles"); ok {
			t.Errorf("%s has an AcceptsFiles method", typ)
		}
	}
}
