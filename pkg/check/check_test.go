package check

import (
	"reflect"
	"testing"

	"github.com/github/gh-actions-pin/pkg/findings"
	"github.com/github/gh-actions-pin/pkg/lockfile"
)

// TestReachabilityStringsAreFrozen pins the ReachabilityStatus string
// vocabulary. These values appear in serialized outputs and any
// downstream consumer's switch statements; renaming one is a breaking
// change to the public schema.
func TestReachabilityStringsAreFrozen(t *testing.T) {
	cases := []struct {
		got  ReachabilityStatus
		want string
	}{
		{Reachable, "reachable"},
		{Unreachable, "unreachable"},
		{ReachabilityUnknown, "unknown"},
	}
	for _, c := range cases {
		if string(c.got) != c.want {
			t.Errorf("ReachabilityStatus renamed: got %q, want %q (this is a breaking change to the public schema)", string(c.got), c.want)
		}
	}
}

// requireField asserts that struct value v has an exported field named
// fieldName whose type matches wantType (compared via reflect.Type
// equality). The check is additive: extra fields on v are fine, so
// future facts can be added without churning this test.
func requireField(t *testing.T, v any, fieldName string, wantType reflect.Type) {
	t.Helper()
	rt := reflect.TypeOf(v)
	if rt.Kind() == reflect.Pointer {
		rt = rt.Elem()
	}
	f, ok := rt.FieldByName(fieldName)
	if !ok {
		t.Errorf("%s: missing field %q (renaming or removing a public field is a breaking change)", rt.Name(), fieldName)
		return
	}
	if f.Type != wantType {
		t.Errorf("%s.%s: type changed: got %s, want %s (changing a public field's type is a breaking change)", rt.Name(), fieldName, f.Type, wantType)
	}
}

// TestInputShapeIsFrozen pins the Input bundle's required fields and
// types. New fields may be added; the existing ones may not be renamed
// or retyped without breaking downstream callers.
func TestInputShapeIsFrozen(t *testing.T) {
	requireField(t, Input{}, "Workflows", reflect.TypeOf([]WorkflowFacts(nil)))
	requireField(t, Input{}, "Lockfile", reflect.TypeOf((*lockfile.File)(nil)))
	requireField(t, Input{}, "Resolutions", reflect.TypeOf(ResolutionFacts{}))
	requireField(t, Input{}, "Options", reflect.TypeOf(Options{}))
}

// TestFactShapesAreFrozen pins the per-fact-type required fields and
// types. Additive-only: extra fields on any of these structs are fine,
// renames or type changes are not.
func TestFactShapesAreFrozen(t *testing.T) {
	t.Run("WorkflowFacts", func(t *testing.T) {
		requireField(t, WorkflowFacts{}, "Path", reflect.TypeOf(""))
		requireField(t, WorkflowFacts{}, "ActionRefs", reflect.TypeOf([]ActionRefFact(nil)))
	})

	t.Run("ActionRefFact", func(t *testing.T) {
		requireField(t, ActionRefFact{}, "Ref", reflect.TypeOf(lockfile.ActionRef{}))
		requireField(t, ActionRefFact{}, "Location", reflect.TypeOf(findings.Location{}))
	})

	t.Run("ResolutionFacts", func(t *testing.T) {
		requireField(t, ResolutionFacts{}, "ResolvedRefs", reflect.TypeOf([]ResolvedRef(nil)))
		requireField(t, ResolutionFacts{}, "Reachability", reflect.TypeOf([]Reachability(nil)))
		requireField(t, ResolutionFacts{}, "Metas", reflect.TypeOf(map[string]lockfile.ActionMeta(nil)))
	})

	t.Run("ResolvedRef", func(t *testing.T) {
		requireField(t, ResolvedRef{}, "NWO", reflect.TypeOf(""))
		requireField(t, ResolvedRef{}, "Path", reflect.TypeOf(""))
		requireField(t, ResolvedRef{}, "Ref", reflect.TypeOf(""))
		requireField(t, ResolvedRef{}, "SHA", reflect.TypeOf(""))
		requireField(t, ResolvedRef{}, "HashAlgo", reflect.TypeOf(""))
		requireField(t, ResolvedRef{}, "Tag", reflect.TypeOf(""))
		requireField(t, ResolvedRef{}, "Branch", reflect.TypeOf(""))
		requireField(t, ResolvedRef{}, "OwnerID", reflect.TypeOf(int64(0)))
		requireField(t, ResolvedRef{}, "RepoID", reflect.TypeOf(int64(0)))
	})

	t.Run("Reachability", func(t *testing.T) {
		requireField(t, Reachability{}, "NWO", reflect.TypeOf(""))
		requireField(t, Reachability{}, "Ref", reflect.TypeOf(""))
		requireField(t, Reachability{}, "SHA", reflect.TypeOf(""))
		requireField(t, Reachability{}, "Status", reflect.TypeOf(ReachabilityStatus("")))
		requireField(t, Reachability{}, "Detail", reflect.TypeOf(""))
		requireField(t, Reachability{}, "FullScanUsed", reflect.TypeOf(false))
	})
}

// noopCheck is a compile-time witness that the Check interface can be
// satisfied with the documented method signatures. If Check's method
// set drifts, this assignment will fail to compile and the change is
// caught at build time, before TestCheckInterfaceIsFrozen's reflection
// check runs.
type noopCheck struct{}

func (noopCheck) Name() string                            { return "noop" }
func (noopCheck) Evaluate(input Input) []findings.Finding { return nil }

var _ Check = noopCheck{}

// TestCheckInterfaceIsFrozen pins the Check interface's method set:
// method names, argument types, and return types. The Check interface
// is the contract every external rule implementation will satisfy;
// renaming a method or changing its signature is a breaking change.
func TestCheckInterfaceIsFrozen(t *testing.T) {
	checkType := reflect.TypeOf((*Check)(nil)).Elem()

	wantMethods := map[string]struct {
		args    []reflect.Type
		returns []reflect.Type
	}{
		"Name": {
			args:    nil,
			returns: []reflect.Type{reflect.TypeOf("")},
		},
		"Evaluate": {
			args:    []reflect.Type{reflect.TypeOf(Input{})},
			returns: []reflect.Type{reflect.TypeOf([]findings.Finding(nil))},
		},
	}

	for name, want := range wantMethods {
		m, ok := checkType.MethodByName(name)
		if !ok {
			t.Errorf("Check: missing method %q (the interface method set is frozen)", name)
			continue
		}
		mt := m.Type
		if mt.NumIn() != len(want.args) {
			t.Errorf("Check.%s: arg count changed: got %d, want %d", name, mt.NumIn(), len(want.args))
			continue
		}
		for i, wantArg := range want.args {
			if mt.In(i) != wantArg {
				t.Errorf("Check.%s arg %d: got %s, want %s", name, i, mt.In(i), wantArg)
			}
		}
		if mt.NumOut() != len(want.returns) {
			t.Errorf("Check.%s: return count changed: got %d, want %d", name, mt.NumOut(), len(want.returns))
			continue
		}
		for i, wantRet := range want.returns {
			if mt.Out(i) != wantRet {
				t.Errorf("Check.%s return %d: got %s, want %s", name, i, mt.Out(i), wantRet)
			}
		}
	}
}
