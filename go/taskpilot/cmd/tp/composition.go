package main

import (
	"sort"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/builtin"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/provider"
)

func defaultLimits() provider.Limits {
	limits := provider.DefaultLimits()
	for name, limit := range platformDefaultLimits() {
		limits[name] = limit
	}
	return limits
}

func integrationNames() []provider.Name {
	limits := defaultLimits()
	names := make([]provider.Name, 0, len(limits))
	for name := range limits {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	return names
}

func composeRuntime(limits provider.Limits) *builtin.Services {
	return &builtin.Services{
		Providers: provider.Default(limits),
		Targets:   platformTargets(limits),
	}
}
