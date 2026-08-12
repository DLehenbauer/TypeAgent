//go:build windows

package main

import (
	"github.com/microsoft/TypeAgent/go/taskpilot/extensions/windows/hyperv"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/provider"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/target"
)

const hyperVLimitName provider.Name = "hyperv"

func platformDefaultLimits() provider.Limits {
	return provider.Limits{hyperVLimitName: hyperv.DefaultParallel}
}

func platformTargets(limits provider.Limits) *target.Registry {
	return target.NewRegistry(hyperv.NewBackend(limits[hyperVLimitName]))
}
