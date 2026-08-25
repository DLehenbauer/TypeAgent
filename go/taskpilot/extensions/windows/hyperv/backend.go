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

var errHyperVBackendClosed = errors.New("hyperv: backend is closed")

// Backend implements target.Backend for Hyper-V managed VMs.
// touch and cleanup are the metadata side of a lease. They are fields rather
// than direct calls so tests can inject I/O failures at the exact points where
// acquire has to roll back and release has to resume.
type Backend struct {
	runner  hyperVRunner
	gate    *gate
	touch   func(stateDir, id, baseline string) error
	cleanup func(stateDir, id string) error

	mu         sync.Mutex
	instances  map[string]*hyperVInstance
	closing    bool
	closeDone  chan struct{}
	operations sync.WaitGroup
}

func (h *Backend) beginInstanceOperation(id string) (*hyperVInstance, func(), error) {
	h.mu.Lock()
	inst := h.instances[id]
	blocked := inst != nil && (inst.releasing || inst.vmRemoved)
	h.mu.Unlock()
	if inst == nil {
		return nil, nil, fmt.Errorf("hyperv: VM %q is not acquired", id)
	}
	if blocked {
		return nil, nil, fmt.Errorf("hyperv: VM %q is being released", id)
	}

	inst.operation.Lock()
	h.mu.Lock()
	valid := h.instances[id] == inst && !inst.releasing && !inst.vmRemoved
	h.mu.Unlock()
	if !valid {
		inst.operation.Unlock()
		return nil, nil, fmt.Errorf("hyperv: VM %q is being released", id)
	}
	return inst, inst.operation.Unlock, nil
}

type hyperVInstance struct {
	operation         sync.Mutex
	id                string
	opts              hyperVOptions
	lock              hyperVMLock
	logicalState      string
	materializedState string
	releasing         bool
	vmRemoved         bool
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
	Reused            bool   `json:"reused,omitempty"`
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
	return newBackend(&psHyperVRunner{command: execRunner{}}, limit)
}

// newBackend wires a backend around a runner so production and tests share one
// set of defaults for the metadata seams.
func newBackend(runner hyperVRunner, limit int) *Backend {
	return &Backend{
		runner:    runner,
		gate:      newGate(limit),
		instances: map[string]*hyperVInstance{},
		touch:     touchHyperVInstance,
		cleanup:   removeHyperVMetadata,
	}
}

// Kind reports the Hyper-V target kind used by this backend.
func (h *Backend) Kind() target.Kind { return Kind }

// Acquire requests a lease for a Hyper-V VM and returns the instance metadata.
func (h *Backend) Acquire(ctx context.Context, req target.AcquireRequest) (target.Instance, error) {
	if err := h.beginOperation(); err != nil {
		return target.Instance{}, err
	}
	defer h.operations.Done()

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
		// The runner named a different VM than we locked, so the lease has to
		// move. A lock conflict here means that name belongs to another
		// taskpilot process, so we fail without rolling back: leaking a VM is
		// recoverable by Sweep, deleting someone else's VM is not.
		nextLock, err := acquireHyperVMLock(stateDir, acquired.ID)
		if err != nil {
			return hyperVAcquireResult{}, err
		}
		if !nextLock.Held() {
			return hyperVAcquireResult{}, fmt.Errorf("hyperv: VM %q is already locked by another taskpilot process", acquired.ID)
		}
		// Past this point the VM is provably ours, so a failure rolls it back.
		// A failed release of the old name leaves that lock file behind until
		// this process exits; that is reported rather than swallowed, and it
		// strands only a name we no longer use.
		previousLock := lock
		lock = nextLock
		opts.VMName = acquired.ID
		if err := previousLock.Release(); err != nil {
			retained, rollbackErr := h.rollbackAcquire(opts, acquired, stateDir, lock, err)
			if retained {
				releaseOnError = false
			}
			return hyperVAcquireResult{}, rollbackErr
		}
	}
	if err := h.touch(stateDir, acquired.ID, acquired.BaselineState); err != nil {
		retained, rollbackErr := h.rollbackAcquire(opts, acquired, stateDir, lock, err)
		if retained {
			releaseOnError = false
		}
		return hyperVAcquireResult{}, rollbackErr
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

func (h *Backend) rollbackAcquire(opts hyperVOptions, acquired hyperVAcquireResult, stateDir string, lock hyperVMLock, cause error) (bool, error) {
	if acquired.Reused {
		return false, cause
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	opts.VMName = acquired.ID
	opts.KeepOnFailure = false
	if err := h.runner.RemoveVM(cleanupCtx, opts, acquired.ID); err != nil {
		h.retainFailedAcquire(opts, acquired, lock, false)
		return true, errors.Join(cause, fmt.Errorf("hyperv: rollback acquired VM %q: %w", acquired.ID, err))
	}
	if err := h.cleanup(stateDir, acquired.ID); err != nil {
		h.retainFailedAcquire(opts, acquired, lock, true)
		return true, errors.Join(cause, fmt.Errorf("hyperv: remove rollback metadata for VM %q: %w", acquired.ID, err))
	}
	return false, cause
}

func (h *Backend) retainFailedAcquire(opts hyperVOptions, acquired hyperVAcquireResult, lock hyperVMLock, vmRemoved bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.instances[acquired.ID] = &hyperVInstance{
		id:                acquired.ID,
		opts:              opts,
		lock:              lock,
		logicalState:      acquired.BaselineState,
		materializedState: acquired.MaterializedState,
		vmRemoved:         vmRemoved,
	}
}

// Release drops the process-local lease and removes the VM when keep is false.
func (h *Backend) Release(ctx context.Context, id string, keep bool) error {
	if err := h.beginOperation(); err != nil {
		return err
	}
	defer h.operations.Done()

	_, err := h.gate.run(ctx, func(ctx context.Context) (any, error) {
		return nil, h.release(ctx, id, keep)
	})
	return err
}

// release removes the local lease and deletes the VM unless the caller wants to keep it.
func (h *Backend) release(ctx context.Context, id string, keep bool) error {
	inst, err := h.beginInstanceRelease(id)
	if err != nil {
		return err
	}
	if inst == nil {
		return nil
	}
	defer inst.operation.Unlock()
	committed := false
	defer func() {
		if !committed {
			h.cancelInstanceRelease(id, inst)
		}
	}()
	if keep && !inst.vmRemoved {
		if err := inst.lock.Release(); err != nil {
			return err
		}
		committed = h.takeInstanceIf(id, inst)
		if !committed {
			return fmt.Errorf("hyperv: VM %q ownership changed during release", id)
		}
		return nil
	}
	if !inst.vmRemoved {
		if err := h.runner.RemoveVM(ctx, inst.opts, id); err != nil {
			return err
		}
		h.markInstanceRemoved(id, inst)
	}
	stateDir, stateErr := cache.ResolveStateDir()
	if stateErr != nil {
		return stateErr
	}
	if err := h.cleanup(stateDir, id); err != nil {
		return err
	}
	if err := inst.lock.Release(); err != nil {
		return err
	}
	committed = h.takeInstanceIf(id, inst)
	if !committed {
		return fmt.Errorf("hyperv: VM %q ownership changed during release", id)
	}
	return nil
}

// Run executes a guest script and returns the decoded result for the request.
func (h *Backend) Run(ctx context.Context, req target.RunRequest) (target.RunOutcome, error) {
	if err := h.beginOperation(); err != nil {
		return target.RunOutcome{}, err
	}
	defer h.operations.Done()

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
	inst, endOperation, err := h.beginInstanceOperation(req.ID)
	if err != nil {
		return target.RunOutcome{}, err
	}
	defer endOperation()
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
	if err := h.beginOperation(); err != nil {
		return 0, err
	}
	defer h.operations.Done()

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

// Close waits for admitted operations, rejects new work, then tears down active instances.
func (h *Backend) Close() {
	h.mu.Lock()
	if h.closing {
		done := h.closeDone
		h.mu.Unlock()
		<-done
		return
	}
	h.closing = true
	h.closeDone = make(chan struct{})
	done := h.closeDone
	h.mu.Unlock()

	h.operations.Wait()

	h.mu.Lock()
	instances := h.instances
	h.instances = map[string]*hyperVInstance{}
	h.mu.Unlock()
	for _, inst := range instances {
		if inst.vmRemoved {
			if stateDir, stateErr := cache.ResolveStateDir(); stateErr == nil {
				_ = h.cleanup(stateDir, inst.id)
			}
		} else if !inst.opts.KeepOnFailure {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			if err := h.runner.RemoveVM(ctx, inst.opts, inst.id); err == nil {
				if stateDir, stateErr := cache.ResolveStateDir(); stateErr == nil {
					_ = h.cleanup(stateDir, inst.id)
				}
			}
			cancel()
		}
		_ = inst.lock.Release()
	}

	close(done)
}

// beginOperation admits work that arrived before shutdown and rejects later calls.
func (h *Backend) beginOperation() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing {
		return errHyperVBackendClosed
	}
	h.operations.Add(1)
	return nil
}

// beginInstanceRelease marks an instance as releasing and takes its operation
// lock, so an in-flight Run finishes before teardown starts and no new one
// begins. It returns (nil, nil) when the VM was never acquired, which callers
// treat as an already-released no-op. The caller owns inst.operation on success.
func (h *Backend) beginInstanceRelease(id string) (*hyperVInstance, error) {
	h.mu.Lock()
	inst := h.instances[id]
	switch {
	case inst == nil:
		h.mu.Unlock()
		return nil, nil
	case inst.releasing:
		h.mu.Unlock()
		return nil, fmt.Errorf("hyperv: VM %q release is already in progress", id)
	}
	// Publish the intent before blocking so concurrent operations reject
	// immediately instead of queueing behind the teardown.
	inst.releasing = true
	h.mu.Unlock()

	inst.operation.Lock()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.instances[id] != inst {
		inst.operation.Unlock()
		return nil, fmt.Errorf("hyperv: VM %q ownership changed during release", id)
	}
	return inst, nil
}

func (h *Backend) cancelInstanceRelease(id string, expected *hyperVInstance) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.instances[id] == expected {
		expected.releasing = false
	}
}

func (h *Backend) markInstanceRemoved(id string, expected *hyperVInstance) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.instances[id] == expected {
		expected.vmRemoved = true
	}
}

func (h *Backend) takeInstanceIf(id string, expected *hyperVInstance) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.instances[id] != expected {
		return false
	}
	delete(h.instances, id)
	return true
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
	PID          int       `json:"pid"`
	ProcessStart uint64    `json:"processStart,omitempty"`
	AcquiredAt   time.Time `json:"acquiredAt"`
}

// Held reports whether acquisition was cached as successful.
func (l hyperVMLock) Held() bool { return l.held }

// Release removes the lock file when this process still owns it.
func (l hyperVMLock) Release() error {
	if !l.held {
		return nil
	}
	return withHyperVMLockGuard(l.path, func() error {
		raw, err := os.ReadFile(l.path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		var current hyperVMLockInfo
		if err := json.Unmarshal(raw, &current); err != nil || current != l.info {
			return nil
		}
		if err := os.Remove(l.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	})
}

// Touch refreshes the lock mtime and verifies that the file still belongs to this process.
func (l hyperVMLock) Touch() error {
	if !l.held {
		return nil
	}
	return withHyperVMLockGuard(l.path, func() error {
		now := time.Now()
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
		return os.Chtimes(l.path, now, now)
	})
}

// acquireHyperVMLock creates a lease file for the VM or returns an unheld lock when the existing lock cannot be taken.
func acquireHyperVMLock(stateDir, id string) (hyperVMLock, error) {
	root := hyperVMLockRoot(stateDir)
	if err := os.MkdirAll(root, 0o777); err != nil {
		return hyperVMLock{}, err
	}
	path := hyperVMLockPath(stateDir, id)
	var acquired hyperVMLock
	err := withHyperVMLockGuard(path, func() error {
		stale, err := hyperVMLockStale(path)
		if err != nil {
			return err
		}
		if !stale {
			return nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		started, alive := hyperVProcessStart(os.Getpid())
		if !alive || started == 0 {
			// A lock that cannot name its owner is unprobeable: every later
			// acquirer has to wait out the age backstop before it may touch this
			// VM. Refuse to create one rather than silently downgrading the lease
			// to a timeout.
			return fmt.Errorf("hyperv: cannot establish owner identity for VM lock %q: the current process (pid %d) has no readable start time", path, os.Getpid())
		}
		info := hyperVMLockInfo{PID: os.Getpid(), ProcessStart: started, AcquiredAt: time.Now().UTC()}
		raw, _ := json.MarshalIndent(info, "", "  ")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o666)
		if err != nil {
			return err
		}
		if _, err := f.Write(raw); err != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return err
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(path)
			return err
		}
		acquired = hyperVMLock{path: path, info: info, held: true}
		return nil
	})
	if err != nil {
		return hyperVMLock{}, err
	}
	return acquired, nil
}

// hyperVMLockStale reports whether a missing, dead-owner, or ownerless expired lock can be taken over.
func hyperVMLockStale(path string) (bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	owner, err := readHyperVMLockInfo(path)
	if err != nil {
		// Corrupt locks have no owner to probe, so age is their recovery backstop.
		return time.Since(info.ModTime()) > hyperVMLockMaxAge, nil
	}
	if owner.PID <= 0 || owner.ProcessStart == 0 {
		// Acquisition refuses to write a lock without an owner identity, so an
		// unidentifiable owner here came from a foreign or older writer. There is
		// nothing to probe, which leaves age as the only safe recovery.
		return time.Since(info.ModTime()) > hyperVMLockMaxAge, nil
	}
	return !hyperVProcessMatches(owner.PID, owner.ProcessStart), nil
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

// hyperVProcessMatches is a variable so tests can decide lock ownership without
// arranging for a real process with a known start time.
var hyperVProcessMatches = processMatches

// hyperVProcessStart is a variable so tests can simulate a host that will not
// report the current process's creation time.
var hyperVProcessStart = processStartToken

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
	if err != nil || meta.Result == nil || meta.ID != id || meta.State != state {
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
	var errs []error
	key := safeFileName(id)
	if err := os.Remove(filepath.Join(hyperVInstanceRoot(stateDir), key+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	if err := os.RemoveAll(filepath.Join(hyperVCheckpointRoot(stateDir), key)); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
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
	const hashPrefix = "sha256-"
	if clean := sanitizeHyperVName(s); s != "" && clean == s && len(clean) < 80 &&
		!strings.HasPrefix(clean, hashPrefix) && !isWindowsReservedPathComponent(clean) {
		return clean
	}
	sum := sha256.Sum256([]byte(s))
	return hashPrefix + base64.RawURLEncoding.EncodeToString(sum[:])
}

// isWindowsReservedPathComponent reports device names that Windows resolves
// even when they have an extension or trailing dots/spaces.
func isWindowsReservedPathComponent(s string) bool {
	s = strings.TrimRight(s, ". ")
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = s[:i]
	}
	switch strings.ToUpper(s) {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(s) == 4 {
		prefix := strings.ToUpper(s[:3])
		return (prefix == "COM" || prefix == "LPT") && s[3] >= '1' && s[3] <= '9'
	}
	return false
}
