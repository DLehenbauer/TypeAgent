//go:build windows

package main

import (
	"github.com/microsoft/TypeAgent/go/taskpilot/extensions/windows/hyperv"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/provider"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/target"
)

const hyperVLimitName provider.Name = "hyperv"

// platformDefaultLimits returns the Windows Hyper-V target-backend cap.
func platformDefaultLimits() provider.Limits {
	return provider.Limits{hyperVLimitName: hyperv.DefaultParallel}
}

// platformTargets registers the Windows Hyper-V target backend.
func platformTargets(limits provider.Limits) *target.Registry {
	return target.NewRegistry(hyperv.NewBackend(limits[hyperVLimitName]))
}
