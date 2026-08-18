package main

import (
	"sort"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/builtin"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/provider"
)

// defaultLimits merges built-in provider caps with platform target-backend caps.
func defaultLimits() provider.Limits {
	limits := provider.DefaultLimits()
	for name, limit := range platformDefaultLimits() {
		limits[name] = limit
	}
	return limits
}

// integrationNames returns configured concurrency-limit names in deterministic order.
func integrationNames() []provider.Name {
	limits := defaultLimits()
	names := make([]provider.Name, 0, len(limits))
	for name := range limits {
		names = append(names, name)
	}
	// Stable ordering keeps generated parallel flags deterministic.
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	return names
}

// composeRuntime wires built-in providers and platform target backends.
func composeRuntime(limits provider.Limits) *builtin.Services {
	return &builtin.Services{
		Providers: provider.Default(limits),
		Targets:   platformTargets(limits),
	}
}
