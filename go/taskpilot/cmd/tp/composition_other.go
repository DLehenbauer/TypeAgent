//go:build !windows

package main

import (
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/provider"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/target"
)

func platformDefaultLimits() provider.Limits {
	return provider.Limits{}
}

func platformTargets(provider.Limits) *target.Registry {
	return target.NewRegistry()
}
