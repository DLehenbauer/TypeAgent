package builtin

import (
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/provider"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/target"
)

// Services is the single runtime composition seam available to builtins.
type Services struct {
	Providers *provider.Set
	Targets   *target.Registry
}

func (s *Services) Close() {
	if s == nil {
		return
	}
	if s.Providers != nil {
		s.Providers.Close()
	}
	if s.Targets != nil {
		s.Targets.Close()
	}
}
