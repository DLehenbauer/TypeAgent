//go:build windows

package hyperv

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/cache"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/script"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/target"
)

// Kind is the execution-target kind backed by Hyper-V VMs.
const Kind target.Kind = "hyperv"

// DefaultParallel keeps live guest manipulation serialized by default.
// Hyper-V operations are heavy and each leased VM is already exclusive, so a
// conservative default avoids surprising local-machine pressure.
const DefaultParallel = 1

const (
	defaultHyperVBaselineState = "baseline"
	hyperVMLockMaxAge          = time.Hour
)

// Backend implements target.Backend for Hyper-V managed VMs.
type Backend struct {
	runner hyperVRunner
	gate   *gate

	mu        sync.Mutex
	instances map[string]*hyperVInstance
}

type hyperVInstance struct {
	id                string
	opts              hyperVOptions
	lock              hyperVMLock
	logicalState      string
	materializedState string
}

type hyperVOptions struct {
	VMName            string                  `json:"vmName"`
	BaseImage         string                  `json:"baseImage,omitempty"`
	SwitchName        string                  `json:"switchName,omitempty"`
	VMRoot            string                  `json:"vmRoot,omitempty"`
	VHDRoot           string                  `json:"vhdRoot,omitempty"`
	MemoryMB          int                     `json:"memoryMB,omitempty"`
	ProcessorCount    int                     `json:"processorCount,omitempty"`
	Generation        int                     `json:"generation,omitempty"`
	DisableSecureBoot bool                    `json:"disableSecureBoot,omitempty"`
	KeepOnFailure     bool                    `json:"-"`
	BaselineState     string                  `json:"baselineState,omitempty"`
	Guest             hyperVGuestOptions      `json:"guest"`
	Workspace         *hyperVWorkspaceOptions `json:"workspace,omitempty"`
}

type hyperVGuestOptions struct {
	Username             string `json:"username"`
	PasswordEnv          string `json:"passwordEnv"`
	Password             string `json:"password"`
	WinRMPort            int    `json:"winRMPort"`
	ReadinessTimeoutSec  int    `json:"readinessTimeoutSeconds"`
	ConnectionTimeoutSec int    `json:"connectionTimeoutSeconds"`
}

type hyperVWorkspaceOptions struct {
	UNCPath     string `json:"uncPath"`
	Drive       string `json:"drive"`
	Username    string `json:"username"`
	PasswordEnv string `json:"passwordEnv"`
	Password    string `json:"password"`
}

type hyperVAcquireResult struct {
	ID                string `json:"id"`
	BaselineState     string `json:"baselineState"`
	MaterializedState string `json:"materializedState,omitempty"`
}

type hyperVRunResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exitCode"`
	TimedOut bool   `json:"timedOut,omitempty"`
}

type hyperVRunner interface {
	Acquire(ctx context.Context, opts hyperVOptions) (hyperVAcquireResult, error)
	CheckpointExists(ctx context.Context, opts hyperVOptions, id, state string) (bool, error)
	RestoreCheckpoint(ctx context.Context, opts hyperVOptions, id, state string) error
	NewCheckpoint(ctx context.Context, opts hyperVOptions, id, state string) error
	RunGuest(ctx context.Context, opts hyperVOptions, id string, req script.Request) (hyperVRunResult, error)
	RemoveVM(ctx context.Context, opts hyperVOptions, id string) error
	RebootGuest(ctx context.Context, opts hyperVOptions, id string) error
	RemoveCheckpoint(ctx context.Context, opts hyperVOptions, id, state string) error
}

type psHyperVRunner struct {
	command commandRunner
}

// NewBackend returns the real Hyper-V execution-context provider.
func NewBackend(limit int) *Backend {
	return &Backend{
		runner:    &psHyperVRunner{command: execRunner{}},
		gate:      newGate(limit),
		instances: map[string]*hyperVInstance{},
	}
}

// Kind reports the Hyper-V target kind used by this backend.
func (h *Backend) Kind() target.Kind { return Kind }

// Acquire requests a lease for a Hyper-V VM and returns the instance metadata.
func (h *Backend) Acquire(ctx context.Context, req target.AcquireRequest) (target.Instance, error) {
	out, err := h.gate.run(ctx, func(ctx context.Context) (any, error) {
		return h.acquire(ctx, req)
	})
	if err != nil {
		return target.Instance{}, err
	}
	acquired := out.(hyperVAcquireResult)
	return target.Instance{
		ID: acquired.ID, BaselineState: acquired.BaselineState, MaterializedState: acquired.MaterializedState,
	}, nil
}

// acquire takes a VM lease and records the instance state.
func (h *Backend) acquire(ctx context.Context, req target.AcquireRequest) (hyperVAcquireResult, error) {
	opts, err := decodeHyperVOptions(req.Options)
	if err != nil {
		return hyperVAcquireResult{}, err
	}
	opts.KeepOnFailure = req.KeepOnFailure
	stateDir, err := cache.ResolveStateDir()
	if err != nil {
		return hyperVAcquireResult{}, err
	}
	lock, err := acquireHyperVMLock(stateDir, opts.VMName)
	if err != nil {
		return hyperVAcquireResult{}, err
	}
	if !lock.Held() {
		return hyperVAcquireResult{}, fmt.Errorf("hyperv: VM %q is already locked by another taskpilot process", opts.VMName)
	}
	releaseOnError := true
	defer func() {
		if releaseOnError {
			_ = lock.Release()
		}
	}()

	// Reject duplicate leases before invoking Hyper-V so the process-local map stays consistent.
	h.mu.Lock()
	if _, exists := h.instances[opts.VMName]; exists {
		h.mu.Unlock()
		return hyperVAcquireResult{}, fmt.Errorf("hyperv: VM %q is already acquired in this process", opts.VMName)
	}
	h.mu.Unlock()

	acquired, err := h.runner.Acquire(ctx, opts)
	if err != nil {
		return hyperVAcquireResult{}, err
	}
	if acquired.ID == "" {
		acquired.ID = opts.VMName
	}
	if acquired.BaselineState == "" {
		acquired.BaselineState = opts.BaselineState
	}
	if acquired.ID != opts.VMName {
		_ = lock.Release()
		lock, err = acquireHyperVMLock(stateDir, acquired.ID)
		if err != nil {
			return hyperVAcquireResult{}, err
		}
		if !lock.Held() {
			return hyperVAcquireResult{}, fmt.Errorf("hyperv: VM %q is already locked by another taskpilot process", acquired.ID)
		}
		opts.VMName = acquired.ID
	}
	if err := touchHyperVInstance(stateDir, acquired.ID, acquired.BaselineState); err != nil {
		return hyperVAcquireResult{}, err
	}
	h.mu.Lock()
	h.instances[acquired.ID] = &hyperVInstance{
		id:                acquired.ID,
		opts:              opts,
		lock:              lock,
		logicalState:      acquired.BaselineState,
		materializedState: acquired.MaterializedState,
	}
	h.mu.Unlock()
	releaseOnError = false
	return acquired, nil
}

// Release drops the process-local lease and removes the VM when keep is false.
func (h *Backend) Release(ctx context.Context, id string, keep bool) error {
	_, err := h.gate.run(ctx, func(ctx context.Context) (any, error) {
		return nil, h.release(ctx, id, keep)
	})
	return err
}

// release removes the local lease and deletes the VM unless the caller wants to keep it.
func (h *Backend) release(ctx context.Context, id string, keep bool) error {
	inst := h.takeInstance(id)
	if inst == nil {
		return nil
	}
	defer inst.lock.Release()
	if keep {
		return nil
	}
	if err := h.runner.RemoveVM(ctx, inst.opts, id); err != nil {
		return err
	}
	stateDir, err := cache.ResolveStateDir()
	if err != nil {
		return err
	}
	return removeHyperVMetadata(stateDir, id)
}

// Run executes a guest script and returns the decoded result for the request.
func (h *Backend) Run(ctx context.Context, req target.RunRequest) (target.RunOutcome, error) {
	out, err := h.gate.run(ctx, func(ctx context.Context) (any, error) {
		return h.exec(ctx, req)
	})
	if err != nil {
		return target.RunOutcome{}, err
	}
	outcome, ok := out.(target.RunOutcome)
	if !ok {
		return target.RunOutcome{}, fmt.Errorf("hyperv: internal run returned %T", out)
	}
	return outcome, nil
}

// exec runs a script against the acquired guest and persists checkpoint metadata.
func (h *Backend) exec(ctx context.Context, req target.RunRequest) (target.RunOutcome, error) {
	if req.ID == "" {
		return target.RunOutcome{}, fmt.Errorf("hyperv: run requires an id")
	}
	inst, err := h.requireInstance(req.ID)
	if err != nil {
		return target.RunOutcome{}, err
	}
	if err := inst.lock.Touch(); err != nil {
		return target.RunOutcome{}, err
	}

	stateDir, err := cache.ResolveStateDir()
	if err != nil {
		return target.RunOutcome{}, err
	}
	if req.Checkpoint != "" {
		exists, err := h.runner.CheckpointExists(ctx, inst.opts, req.ID, req.Checkpoint)
		if err != nil {
			return target.RunOutcome{}, err
		}
		if exists {
			if committed, ok := readHyperVCheckpointResult(stateDir, req.ID, req.Checkpoint); ok {
				h.setLogicalState(req.ID, req.Checkpoint)
				if err := touchHyperVCheckpoint(stateDir, req.ID, req.Checkpoint, committed); err != nil {
					return target.RunOutcome{}, err
				}
				return target.RunOutcome{Result: committed, Committed: true}, nil
			}
			// A checkpoint without its replay result cannot safely short-circuit:
			// downstream result projections would receive an invented empty value.
			// Remove the incomplete state and rebuild it from its predecessor.
			if err := h.runner.RemoveCheckpoint(ctx, inst.opts, req.ID, req.Checkpoint); err != nil {
				return target.RunOutcome{}, fmt.Errorf("hyperv: checkpoint %q has no result metadata and could not be removed: %w", req.Checkpoint, err)
			}
		}
	}

	if err := h.materialize(ctx, inst, req.State); err != nil {
		return target.RunOutcome{}, err
	}
	run, err := h.runner.RunGuest(ctx, inst.opts, req.ID, req.Script)
	if err != nil {
		h.setDirty(req.ID, req.State)
		return target.RunOutcome{}, err
	}
	result, err := hyperVScriptResult(run)
	if err != nil {
		h.setDirty(req.ID, req.State)
		return target.RunOutcome{}, err
	}
	committed := false
	if req.Checkpoint != "" && run.ExitCode == 0 {
		// Reboot before checkpointing when the guest requested it so the checkpoint reflects the applied state.
		if hyperVWantsReboot(result) {
			if err := h.runner.RebootGuest(ctx, inst.opts, req.ID); err != nil {
				h.setDirty(req.ID, req.State)
				return target.RunOutcome{}, err
			}
		}
		if err := h.runner.NewCheckpoint(ctx, inst.opts, req.ID, req.Checkpoint); err != nil {
			h.setDirty(req.ID, req.State)
			return target.RunOutcome{}, err
		}
		if err := touchHyperVCheckpoint(stateDir, req.ID, req.Checkpoint, result); err != nil {
			h.setDirty(req.ID, req.State)
			return target.RunOutcome{}, fmt.Errorf("hyperv: persist checkpoint result %q: %w", req.Checkpoint, err)
		}
		h.setMaterializedState(req.ID, req.Checkpoint)
		committed = true
	} else {
		// A non-zero exit is an ordinary script result rather than a call failure, so the checkpoint remains uncommitted.
		h.setDirty(req.ID, req.State)
	}
	if err := touchHyperVInstance(stateDir, req.ID, h.logicalStateOf(req.ID)); err != nil {
		return target.RunOutcome{}, err
	}
	return target.RunOutcome{Result: result, Committed: committed}, nil
}

// logicalStateOf returns the current logical checkpoint for an acquired VM.
func (h *Backend) logicalStateOf(id string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if inst, ok := h.instances[id]; ok {
		return inst.logicalState
	}
	return ""
}

// hyperVWantsReboot reports whether a guest body requested a restart.
func hyperVWantsReboot(result script.Result) bool {
	inner, ok := result.Value.(map[string]any)
	if !ok {
		return false
	}
	switch v := inner["rebootRequested"].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(v, "true")
	default:
		return false
	}
}

// Sweep removes stale checkpoints and old VMs that have aged past the cutoff.
func (h *Backend) Sweep(ctx context.Context, maxAge time.Duration) (int, error) {
	out, err := h.gate.run(ctx, func(ctx context.Context) (any, error) {
		return h.sweep(ctx, maxAge)
	})
	if err != nil {
		return 0, err
	}
	return out.(int), nil
}

// sweep removes expired checkpoints and VMs from the host and metadata store.
func (h *Backend) sweep(ctx context.Context, maxAge time.Duration) (int, error) {
	stateDir, err := cache.ResolveStateDir()
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	var firstErr error

	checkpoints, _ := filepath.Glob(filepath.Join(hyperVCheckpointRoot(stateDir), "*", "*.json"))
	for _, path := range checkpoints {
		meta, err := readHyperVCheckpoint(path)
		if err != nil || meta.LastUsed.After(cutoff) || meta.State == defaultHyperVBaselineState {
			continue
		}
		lock, err := acquireHyperVMLock(stateDir, meta.ID)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !lock.Held() {
			continue
		}
		// Hold the VM lock across removal so a freshly acquired lease cannot race with sweep.
		opts := hyperVOptions{VMName: meta.ID}
		if err := h.runner.RemoveCheckpoint(ctx, opts, meta.ID, meta.State); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			_ = lock.Release()
			continue
		}
		_ = os.Remove(path)
		_ = lock.Release()
		removed++
	}

	instances, _ := filepath.Glob(filepath.Join(hyperVInstanceRoot(stateDir), "*.json"))
	for _, path := range instances {
		meta, err := readHyperVInstance(path)
		if err != nil || meta.LastUsed.After(cutoff) {
			continue
		}
		lock, err := acquireHyperVMLock(stateDir, meta.ID)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !lock.Held() {
			continue
		}
		opts := hyperVOptions{VMName: meta.ID}
		if err := h.runner.RemoveVM(ctx, opts, meta.ID); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			_ = lock.Release()
			continue
		}
		_ = removeHyperVMetadata(stateDir, meta.ID)
		_ = lock.Release()
		removed++
	}
	return removed, firstErr
}

// Close tears down all active instances and removes any VM that is not kept on failure.
func (h *Backend) Close() {
	h.mu.Lock()
	instances := h.instances
	h.instances = map[string]*hyperVInstance{}
	h.mu.Unlock()
	for _, inst := range instances {
		if !inst.opts.KeepOnFailure {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			if err := h.runner.RemoveVM(ctx, inst.opts, inst.id); err == nil {
				if stateDir, stateErr := cache.ResolveStateDir(); stateErr == nil {
					_ = removeHyperVMetadata(stateDir, inst.id)
				}
			}
			cancel()
		}
		_ = inst.lock.Release()
	}
}

// requireInstance returns the active VM record or an error when it is not acquired.
func (h *Backend) requireInstance(id string) (*hyperVInstance, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	inst := h.instances[id]
	if inst == nil {
		return nil, fmt.Errorf("hyperv: VM %q is not acquired", id)
	}
	return inst, nil
}

// takeInstance removes and returns an active VM record.
func (h *Backend) takeInstance(id string) *hyperVInstance {
	h.mu.Lock()
	defer h.mu.Unlock()
	inst := h.instances[id]
	delete(h.instances, id)
	return inst
}

// setLogicalState updates the logical checkpoint stored in the process-local instance map.
func (h *Backend) setLogicalState(id, state string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if inst := h.instances[id]; inst != nil {
		inst.logicalState = state
	}
}

// setMaterializedState updates the logical and materialized checkpoints together.
func (h *Backend) setMaterializedState(id, state string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if inst := h.instances[id]; inst != nil {
		inst.logicalState = state
		inst.materializedState = state
	}
}

// setDirty overwrites the logical checkpoint and clears the materialized one.
func (h *Backend) setDirty(id, logicalState string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if inst := h.instances[id]; inst != nil {
		inst.logicalState = logicalState
		inst.materializedState = ""
	}
}

// materialize restores the requested checkpoint before a guest run when needed.
func (h *Backend) materialize(ctx context.Context, inst *hyperVInstance, state string) error {
	if err := inst.lock.Touch(); err != nil {
		return err
	}
	if state == "" {
		return nil
	}
	h.mu.Lock()
	current := inst.materializedState
	h.mu.Unlock()
	if current == state {
		return nil
	}
	if err := h.runner.RestoreCheckpoint(ctx, inst.opts, inst.id, state); err != nil {
		return err
	}
	h.setMaterializedState(inst.id, state)
	stateDir, err := cache.ResolveStateDir()
	if err == nil {
		_ = touchHyperVCheckpoint(stateDir, inst.id, state)
	}
	return nil
}

// decodeHyperVOptions converts the request map into a validated Hyper-V configuration.
func decodeHyperVOptions(input map[string]any) (hyperVOptions, error) {
	stateDir, err := cache.ResolveStateDir()
	if err != nil {
		return hyperVOptions{}, err
	}
	opts := hyperVOptions{
		SwitchName:     "Default Switch",
		BaselineState:  defaultHyperVBaselineState,
		MemoryMB:       4096,
		ProcessorCount: 2,
		Generation:     1,
		Guest: hyperVGuestOptions{
			WinRMPort:            5985,
			ReadinessTimeoutSec:  300,
			ConnectionTimeoutSec: 5,
		},
	}
	guestSeen := false
	for key, v := range input {
		switch key {
		case "baseImage":
			opts.BaseImage, err = hyperVStringOption(key, v)
		case "switchName":
			opts.SwitchName, err = hyperVStringOption(key, v)
		case "vmRoot":
			opts.VMRoot, err = hyperVStringOption(key, v)
		case "vhdRoot":
			opts.VHDRoot, err = hyperVStringOption(key, v)
		case "vmName":
			opts.VMName, err = hyperVStringOption(key, v)
		case "baselineState":
			opts.BaselineState, err = hyperVStringOption(key, v)
		case "memoryMB":
			opts.MemoryMB, err = hyperVIntOption(key, v)
		case "processorCount":
			opts.ProcessorCount, err = hyperVIntOption(key, v)
		case "generation":
			opts.Generation, err = hyperVIntOption(key, v)
		case "disableSecureBoot":
			var ok bool
			opts.DisableSecureBoot, ok = v.(bool)
			if !ok {
				err = fmt.Errorf("hyperv: options.%s must be a boolean", key)
			}
		case "guest":
			guestSeen = true
			opts.Guest, err = decodeHyperVGuestOptions(v, opts.Guest)
		case "workspace":
			opts.Workspace, err = decodeHyperVWorkspaceOptions(v)
		default:
			err = fmt.Errorf("hyperv: unknown option %q", key)
		}
		if err != nil {
			return hyperVOptions{}, err
		}
	}
	if !guestSeen {
		return hyperVOptions{}, fmt.Errorf("hyperv: options.guest is required")
	}
	if opts.VMName == "" {
		opts.VMName = stableHyperVName(opts)
	}
	if opts.BaselineState == "" {
		opts.BaselineState = defaultHyperVBaselineState
	}
	if opts.VMRoot == "" {
		opts.VMRoot = filepath.Join(stateDir, "hyperv", "vms")
	}
	if opts.VHDRoot == "" {
		opts.VHDRoot = filepath.Join(stateDir, "hyperv", "vhds")
	}
	if opts.BaseImage == "" {
		return hyperVOptions{}, fmt.Errorf("hyperv: options.baseImage is required")
	}
	if opts.MemoryMB <= 0 {
		return hyperVOptions{}, fmt.Errorf("hyperv: options.memoryMB must be greater than zero")
	}
	if opts.ProcessorCount <= 0 {
		return hyperVOptions{}, fmt.Errorf("hyperv: options.processorCount must be greater than zero")
	}
	if opts.Generation != 1 && opts.Generation != 2 {
		return hyperVOptions{}, fmt.Errorf("hyperv: options.generation must be 1 or 2")
	}
	return opts, nil
}

// decodeHyperVGuestOptions validates guest connection settings and resolves the password from the environment.
func decodeHyperVGuestOptions(raw any, defaults hyperVGuestOptions) (hyperVGuestOptions, error) {
	input, ok := raw.(map[string]any)
	if !ok {
		return hyperVGuestOptions{}, fmt.Errorf("hyperv: options.guest must be an object")
	}
	opts := defaults
	var err error
	for key, value := range input {
		switch key {
		case "username":
			opts.Username, err = hyperVStringOption("guest.username", value)
		case "passwordEnv":
			opts.PasswordEnv, err = hyperVStringOption("guest.passwordEnv", value)
		case "winRMPort":
			opts.WinRMPort, err = hyperVIntOption("guest.winRMPort", value)
		case "readinessTimeoutSeconds":
			opts.ReadinessTimeoutSec, err = hyperVIntOption("guest.readinessTimeoutSeconds", value)
		case "connectionTimeoutSeconds":
			opts.ConnectionTimeoutSec, err = hyperVIntOption("guest.connectionTimeoutSeconds", value)
		default:
			err = fmt.Errorf("hyperv: unknown option %q", "guest."+key)
		}
		if err != nil {
			return hyperVGuestOptions{}, err
		}
	}
	if opts.Username == "" {
		return hyperVGuestOptions{}, fmt.Errorf("hyperv: options.guest.username is required")
	}
	if opts.PasswordEnv == "" {
		return hyperVGuestOptions{}, fmt.Errorf("hyperv: options.guest.passwordEnv is required")
	}
	if opts.WinRMPort <= 0 || opts.WinRMPort > 65535 {
		return hyperVGuestOptions{}, fmt.Errorf("hyperv: options.guest.winRMPort must be between 1 and 65535")
	}
	if opts.ReadinessTimeoutSec <= 0 {
		return hyperVGuestOptions{}, fmt.Errorf("hyperv: options.guest.readinessTimeoutSeconds must be greater than zero")
	}
	if opts.ConnectionTimeoutSec <= 0 {
		return hyperVGuestOptions{}, fmt.Errorf("hyperv: options.guest.connectionTimeoutSeconds must be greater than zero")
	}
	password, present := os.LookupEnv(opts.PasswordEnv)
	if !present || password == "" {
		return hyperVGuestOptions{}, fmt.Errorf("hyperv: environment variable %s is required", opts.PasswordEnv)
	}
	opts.Password = password
	return opts, nil
}

// decodeHyperVWorkspaceOptions validates UNC workspace credentials and resolves the password from the environment.
func decodeHyperVWorkspaceOptions(raw any) (*hyperVWorkspaceOptions, error) {
	input, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("hyperv: options.workspace must be an object")
	}
	opts := &hyperVWorkspaceOptions{}
	var err error
	for key, value := range input {
		switch key {
		case "uncPath":
			opts.UNCPath, err = hyperVStringOption("workspace.uncPath", value)
		case "drive":
			opts.Drive, err = hyperVStringOption("workspace.drive", value)
		case "username":
			opts.Username, err = hyperVStringOption("workspace.username", value)
		case "passwordEnv":
			opts.PasswordEnv, err = hyperVStringOption("workspace.passwordEnv", value)
		default:
			err = fmt.Errorf("hyperv: unknown option %q", "workspace."+key)
		}
		if err != nil {
			return nil, err
		}
	}
	for name, value := range map[string]string{
		"uncPath": opts.UNCPath, "drive": opts.Drive, "username": opts.Username, "passwordEnv": opts.PasswordEnv,
	} {
		if value == "" {
			return nil, fmt.Errorf("hyperv: options.workspace.%s is required", name)
		}
	}
	password, present := os.LookupEnv(opts.PasswordEnv)
	if !present || password == "" {
		return nil, fmt.Errorf("hyperv: environment variable %s is required", opts.PasswordEnv)
	}
	opts.Password = password
	return opts, nil
}

// hyperVStringOption validates a string option from the request map.
func hyperVStringOption(name string, value any) (string, error) {
	out, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("hyperv: options.%s must be a string", name)
	}
	return out, nil
}

// hyperVIntOption validates an integer option from the request map.
func hyperVIntOption(name string, value any) (int, error) {
	out, ok := intValue(value)
	if !ok {
		return 0, fmt.Errorf("hyperv: options.%s must be an integer", name)
	}
	return out, nil
}

// stableHyperVName derives a deterministic VM name from identity options.
func stableHyperVName(opts hyperVOptions) string {
	type identity struct {
		BaseImage         string `json:"baseImage"`
		SwitchName        string `json:"switchName"`
		VMRoot            string `json:"vmRoot"`
		VHDRoot           string `json:"vhdRoot"`
		MemoryMB          int    `json:"memoryMB"`
		ProcessorCount    int    `json:"processorCount"`
		Generation        int    `json:"generation"`
		DisableSecureBoot bool   `json:"disableSecureBoot"`
	}
	raw, _ := json.Marshal(identity{
		BaseImage:         opts.BaseImage,
		SwitchName:        opts.SwitchName,
		VMRoot:            opts.VMRoot,
		VHDRoot:           opts.VHDRoot,
		MemoryMB:          opts.MemoryMB,
		ProcessorCount:    opts.ProcessorCount,
		Generation:        opts.Generation,
		DisableSecureBoot: opts.DisableSecureBoot,
	})
	sum := sha256.Sum256(raw)
	return "taskpilot-" + hex.EncodeToString(sum[:])[:12]
}

var invalidHyperVName = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// sanitizeHyperVName normalizes a value to the allowed Hyper-V name characters
// and length.
func sanitizeHyperVName(s string) string {
	s = strings.Trim(invalidHyperVName.ReplaceAllString(s, "-"), "-.")
	if s == "" {
		return "default"
	}
	if len(s) > 40 {
		return s[:40]
	}
	return s
}

// hyperVScriptResult converts a PowerShell run result into the common script.Result format.
func hyperVScriptResult(run hyperVRunResult) (script.Result, error) {
	return script.NewResult(run.Stdout, run.Stderr, run.ExitCode, run.TimedOut)
}

func (r *psHyperVRunner) Acquire(ctx context.Context, opts hyperVOptions) (hyperVAcquireResult, error) {
	var out hyperVAcquireResult
	err := r.run(ctx, "acquire", map[string]any{"options": opts}, &out)
	return out, err
}

func (r *psHyperVRunner) CheckpointExists(ctx context.Context, opts hyperVOptions, id, state string) (bool, error) {
	var out struct {
		Exists bool `json:"exists"`
	}
	err := r.run(ctx, "testCheckpoint", map[string]any{"options": opts, "id": id, "state": state}, &out)
	return out.Exists, err
}

func (r *psHyperVRunner) RestoreCheckpoint(ctx context.Context, opts hyperVOptions, id, state string) error {
	return r.run(ctx, "restoreCheckpoint", map[string]any{"options": opts, "id": id, "state": state}, nil)
}

func (r *psHyperVRunner) NewCheckpoint(ctx context.Context, opts hyperVOptions, id, state string) error {
	return r.run(ctx, "newCheckpoint", map[string]any{"options": opts, "id": id, "state": state}, nil)
}

func (r *psHyperVRunner) RunGuest(ctx context.Context, opts hyperVOptions, id string, req script.Request) (hyperVRunResult, error) {
	var out hyperVRunResult
	err := r.run(ctx, "runGuest", map[string]any{"options": opts, "id": id, "script": req.Script, "args": req.Args, "cwd": req.Cwd, "timeoutSeconds": req.TimeoutSeconds}, &out)
	return out, err
}

func (r *psHyperVRunner) RemoveVM(ctx context.Context, opts hyperVOptions, id string) error {
	return r.run(ctx, "removeVM", map[string]any{"options": opts, "id": id}, nil)
}

func (r *psHyperVRunner) RebootGuest(ctx context.Context, opts hyperVOptions, id string) error {
	return r.run(ctx, "rebootGuest", map[string]any{"options": opts, "id": id}, nil)
}

func (r *psHyperVRunner) RemoveCheckpoint(ctx context.Context, opts hyperVOptions, id, state string) error {
	return r.run(ctx, "removeCheckpoint", map[string]any{"options": opts, "id": id, "state": state}, nil)
}

// run invokes the embedded PowerShell driver with a JSON payload and decodes the response.
func (r *psHyperVRunner) run(ctx context.Context, action string, payload map[string]any, out any) error {
	if r.command == nil {
		r.command = execRunner{}
	}
	payload["action"] = action
	stateDir, err := cache.ResolveStateDir()
	if err != nil {
		return err
	}
	tmpRoot := filepath.Join(stateDir, "hyperv", "tmp")
	if err := os.MkdirAll(tmpRoot, 0o700); err != nil {
		return err
	}
	bundleDir, err := os.MkdirTemp(tmpRoot, "bundle-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(bundleDir)
	if err := os.Chmod(bundleDir, 0o700); err != nil {
		return err
	}

	for _, name := range hyperVAssetNames {
		content, err := hyperVAssets.ReadFile(name)
		if err != nil {
			return fmt.Errorf("hyperv: read embedded asset %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(bundleDir, name), content, 0o600); err != nil {
			return fmt.Errorf("hyperv: materialize asset %s: %w", name, err)
		}
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	payloadPath := filepath.Join(bundleDir, "payload.json")
	if err := os.WriteFile(payloadPath, raw, 0o600); err != nil {
		return err
	}

	scriptPath := filepath.Join(bundleDir, "driver.ps1")
	stdout, stderr, exitCode, err := r.command.Run(ctx, []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", scriptPath, payloadPath}, bundleDir)
	if err != nil {
		return err
	}
	if exitCode != 0 {
		return fmt.Errorf("hyperv %s failed with exit code %d: %s", action, exitCode, strings.TrimSpace(stderr))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), out); err != nil {
		return fmt.Errorf("hyperv %s returned invalid JSON: %w; stderr: %s; stdout: %s", action, err, strings.TrimSpace(stderr), strings.TrimSpace(stdout))
	}
	return nil
}

var hyperVAssetNames = []string{"driver.ps1", "host.psm1", "guest.psm1", "retry.ps1"}

//go:embed driver.ps1 host.psm1 guest.psm1 retry.ps1
var hyperVAssets embed.FS

// hyperVMLock records a VM lease-file acquisition shared across processes.
type hyperVMLock struct {
	path string
	info hyperVMLockInfo
	held bool
}

// hyperVMLockInfo stores the owner identity recorded in a VM lock file.
type hyperVMLockInfo struct {
	PID        int       `json:"pid"`
	AcquiredAt time.Time `json:"acquiredAt"`
}

// Held reports whether acquisition was cached as successful.
func (l hyperVMLock) Held() bool { return l.held }

// Release removes the lock file when this process still owns it.
func (l hyperVMLock) Release() error {
	if !l.held {
		return nil
	}
	raw, err := os.ReadFile(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var current hyperVMLockInfo
	if err := json.Unmarshal(raw, &current); err != nil {
		return nil
	}
	if current != l.info {
		return nil
	}
	if err := os.Remove(l.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Touch refreshes the lock mtime and verifies that the file still belongs to this process.
func (l hyperVMLock) Touch() error {
	if !l.held {
		return nil
	}
	raw, err := os.ReadFile(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("hyperv: VM lock %q was lost", l.path)
	}
	if err != nil {
		return err
	}
	var current hyperVMLockInfo
	if err := json.Unmarshal(raw, &current); err != nil {
		return fmt.Errorf("hyperv: VM lock %q is corrupt: %w", l.path, err)
	}
	if current != l.info {
		return fmt.Errorf("hyperv: VM lock %q is owned by another taskpilot process", l.path)
	}
	now := time.Now()
	return os.Chtimes(l.path, now, now)
}

// acquireHyperVMLock creates a lease file for the VM or returns an unheld lock when the existing lock cannot be taken.
func acquireHyperVMLock(stateDir, id string) (hyperVMLock, error) {
	root := hyperVMLockRoot(stateDir)
	if err := os.MkdirAll(root, 0o777); err != nil {
		return hyperVMLock{}, err
	}
	path := hyperVMLockPath(stateDir, id)
	for {
		info := hyperVMLockInfo{PID: os.Getpid(), AcquiredAt: time.Now().UTC()}
		raw, _ := json.MarshalIndent(info, "", "  ")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o666)
		if errors.Is(err, os.ErrExist) {
			stale, err := hyperVMLockStale(path)
			if err != nil {
				return hyperVMLock{}, err
			}
			if stale {
				if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					return hyperVMLock{}, err
				}
				continue
			}
			return hyperVMLock{}, nil
		}
		if err != nil {
			return hyperVMLock{}, err
		}
		if _, err := f.Write(raw); err != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return hyperVMLock{}, err
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(path)
			return hyperVMLock{}, err
		}
		return hyperVMLock{path: path, info: info, held: true}, nil
	}
}

// hyperVMLockStale reports whether a missing, expired, or dead-owner lock can be taken over.
func hyperVMLockStale(path string) (bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if time.Since(info.ModTime()) > hyperVMLockMaxAge {
		return true, nil
	}
	owner, err := readHyperVMLockInfo(path)
	if err != nil {
		// An unreadable lock is not proof the owner is gone; the age rule above is the backstop.
		return false, nil
	}
	if owner.PID <= 0 {
		return false, nil
	}
	return !hyperVProcessAlive(owner.PID), nil
}

// readHyperVMLockInfo loads the owner identity stored in a VM lease file.
func readHyperVMLockInfo(path string) (hyperVMLockInfo, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return hyperVMLockInfo{}, err
	}
	var info hyperVMLockInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return hyperVMLockInfo{}, err
	}
	return info, nil
}

// hyperVProcessAlive is a variable so tests can decide liveness without arranging for a real process.
var hyperVProcessAlive = processAlive

// hyperVInstanceMeta stores the last-used timestamp and baseline state for a VM record.
type hyperVInstanceMeta struct {
	ID            string    `json:"id"`
	BaselineState string    `json:"baselineState"`
	LastUsed      time.Time `json:"lastUsed"`
}

// hyperVCheckpointMeta stores the checkpoint state, timestamp, and persisted result payload.
type hyperVCheckpointMeta struct {
	ID       string         `json:"id"`
	State    string         `json:"state"`
	LastUsed time.Time      `json:"lastUsed"`
	Result   *script.Result `json:"result,omitempty"`
}

// touchHyperVInstance writes or refreshes the VM metadata file for the state store.
func touchHyperVInstance(stateDir, id, baseline string) error {
	path := filepath.Join(hyperVInstanceRoot(stateDir), safeFileName(id)+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(hyperVInstanceMeta{ID: id, BaselineState: baseline, LastUsed: time.Now().UTC()}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o666)
}

// touchHyperVCheckpoint writes checkpoint metadata, preserving the prior result when no new result is supplied.
func touchHyperVCheckpoint(stateDir, id, state string, result ...script.Result) error {
	path := hyperVCheckpointPath(stateDir, id, state)
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	meta := hyperVCheckpointMeta{ID: id, State: state, LastUsed: time.Now().UTC()}
	if len(result) > 0 {
		meta.Result = &result[0]
	} else if existing, err := readHyperVCheckpoint(path); err == nil {
		meta.Result = existing.Result
	}
	raw, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o666)
}

// readHyperVCheckpointResult loads a checkpoint result and normalizes numbers for structured replay.
func readHyperVCheckpointResult(stateDir, id, state string) (script.Result, bool) {
	meta, err := readHyperVCheckpoint(hyperVCheckpointPath(stateDir, id, state))
	if err != nil || meta.Result == nil {
		return script.Result{}, false
	}
	meta.Result.Value = normalizeHyperVCheckpointResult(meta.Result.Value)
	return *meta.Result, true
}

// normalizeHyperVCheckpointResult recursively coerces JSON numbers to the Go values expected by replay.
func normalizeHyperVCheckpointResult(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = normalizeHyperVCheckpointResult(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = normalizeHyperVCheckpointResult(val)
		}
		return out
	case float64:
		if t == float64(int(t)) {
			return int(t)
		}
		return t
	default:
		return v
	}
}

func readHyperVInstance(path string) (hyperVInstanceMeta, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return hyperVInstanceMeta{}, err
	}
	var meta hyperVInstanceMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return hyperVInstanceMeta{}, err
	}
	return meta, nil
}

func readHyperVCheckpoint(path string) (hyperVCheckpointMeta, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return hyperVCheckpointMeta{}, err
	}
	var meta hyperVCheckpointMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return hyperVCheckpointMeta{}, err
	}
	return meta, nil
}

func removeHyperVMetadata(stateDir, id string) error {
	_ = os.Remove(filepath.Join(hyperVInstanceRoot(stateDir), safeFileName(id)+".json"))
	return os.RemoveAll(filepath.Join(hyperVCheckpointRoot(stateDir), safeFileName(id)))
}

func hyperVMLockRoot(stateDir string) string {
	return filepath.Join(stateDir, "hyperv", "locks")
}

func hyperVMLockPath(stateDir, id string) string {
	return filepath.Join(hyperVMLockRoot(stateDir), safeFileName(id)+".lock")
}

func hyperVInstanceRoot(stateDir string) string {
	return filepath.Join(stateDir, "hyperv", "instances")
}

func hyperVCheckpointRoot(stateDir string) string {
	return filepath.Join(stateDir, "hyperv", "checkpoints")
}

func hyperVCheckpointPath(stateDir, id, state string) string {
	return filepath.Join(hyperVCheckpointRoot(stateDir), safeFileName(id), safeFileName(state)+".json")
}

// safeFileName returns a stable metadata key, hashing names outside the narrow
// character and length filter.
func safeFileName(s string) string {
	if s == "" {
		return "_"
	}
	if clean := sanitizeHyperVName(s); clean == s && len(clean) < 80 {
		return clean
	}
	sum := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
