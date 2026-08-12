package main

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestRunAndGCShareTargetComposition(t *testing.T) {
	runServices := composeRuntime(defaultLimits())
	defer runServices.Close()
	gcServices := composeRuntime(defaultLimits())
	defer gcServices.Close()

	split := func(kinds string) []string {
		if kinds == "none" {
			return nil
		}
		out := strings.Split(kinds, ", ")
		sort.Strings(out)
		return out
	}
	runKinds := split(runServices.Targets.Kinds())
	gcKinds := split(gcServices.Targets.Kinds())
	if !reflect.DeepEqual(runKinds, gcKinds) {
		t.Fatalf("run targets=%v gc targets=%v", runKinds, gcKinds)
	}
}
