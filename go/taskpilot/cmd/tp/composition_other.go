//go:build !windows

package main

import (
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/provider"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/target"
)

// platformDefaultLimits returns no platform target-backend caps on non-Windows builds.
func platformDefaultLimits() provider.Limits {
	return provider.Limits{}
}

// platformTargets returns an empty target registry on non-Windows builds.
func platformTargets(provider.Limits) *target.Registry {
	return target.NewRegistry()
}
