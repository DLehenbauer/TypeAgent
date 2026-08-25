//go:build windows

package hyperv

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/microsoft/TypeAgent/go/taskpilot/internal/script"
	"github.com/microsoft/TypeAgent/go/taskpilot/internal/target"
)

type fakeHyperVRunner struct {
	acquireResult hyperVAcquireResult
	checkpoints   map[string]bool
	runResult     hyperVRunResult

	acquires          []hyperVOptions
	checkpointQueries []string
	restores          []string
	runs              []string
	snapshots         []string
	removedVMs        []string
	removedSnapshots  []string
	rebooted          []string
	removeVMErr       error
	removeStarted     chan struct{}
	removeContinue    <-chan struct{}

	acquireStarted  chan struct{}
	acquireContinue <-chan struct{}
	runStarted      chan struct{}
	runContinue     <-chan struct{}
	runEntry        chan int
	runCount        int
	runMu           sync.Mutex
}

type bundleInspectingRunner struct {
	t         *testing.T
	bundleDir string
}

func (f *bundleInspectingRunner) Run(_ context.Context, args []string, dir string) (string, string, int, error) {
	f.t.Helper()
	f.bundleDir = dir
	for _, name := range append(append([]string{}, hyperVAssetNames...), "payload.json") {
		path := filepath.Join(dir, name)
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			f.t.Fatalf("bundle asset %s is unavailable while the command runs: %v", name, err)
		}
	}
	fileIndex := -1
	for i, arg := range args {
		if arg == "-File" {
			fileIndex = i
			break
		}
	}
	if fileIndex < 0 || fileIndex+2 >= len(args) {
		f.t.Fatalf("PowerShell arguments = %v", args)
	}
	if args[fileIndex+1] != filepath.Join(dir, "driver.ps1") ||
		args[fileIndex+2] != filepath.Join(dir, "payload.json") {
		f.t.Fatalf("PowerShell arguments do not use one bundle: %v", args)
	}
	return "{}", "", 0, nil
}

func (f *fakeHyperVRunner) Acquire(_ context.Context, opts hyperVOptions) (hyperVAcquireResult, error) {
	if f.acquireStarted != nil {
		close(f.acquireStarted)
		<-f.acquireContinue
	}
	f.acquires = append(f.acquires, opts)
	out := f.acquireResult
	if out.ID == "" {
		out.ID = opts.VMName
	}
	if out.BaselineState == "" {
		out.BaselineState = opts.BaselineState
	}
	return out, nil
}

func (f *fakeHyperVRunner) CheckpointExists(_ context.Context, _ hyperVOptions, _ string, state string) (bool, error) {
	f.checkpointQueries = append(f.checkpointQueries, state)
	return f.checkpoints[state], nil
}

func (f *fakeHyperVRunner) RestoreCheckpoint(_ context.Context, _ hyperVOptions, _ string, state string) error {
	f.restores = append(f.restores, state)
	return nil
}

func (f *fakeHyperVRunner) NewCheckpoint(_ context.Context, _ hyperVOptions, _ string, state string) error {
	f.snapshots = append(f.snapshots, state)
	if f.checkpoints == nil {
		f.checkpoints = map[string]bool{}
	}
	f.checkpoints[state] = true
	return nil
}

func (f *fakeHyperVRunner) RunGuest(_ context.Context, _ hyperVOptions, _ string, req script.Request) (hyperVRunResult, error) {
	f.runMu.Lock()
	f.runCount++
	runNumber := f.runCount
	f.runMu.Unlock()
	if f.runEntry != nil {
		f.runEntry <- runNumber
	}
	if f.runStarted != nil {
		close(f.runStarted)
	}
	if f.runContinue != nil {
		<-f.runContinue
	}
	f.runs = append(f.runs, req.Script)
	if f.runResult.Stdout != "" || f.runResult.Stderr != "" || f.runResult.ExitCode != 0 || f.runResult.TimedOut {
		return f.runResult, nil
	}
	return hyperVRunResult{Stdout: `{"ok":true}`, ExitCode: 0}, nil
}

func (f *fakeHyperVRunner) RemoveVM(_ context.Context, _ hyperVOptions, id string) error {
	if f.removeStarted != nil {
		close(f.removeStarted)
		<-f.removeContinue
	}
	f.removedVMs = append(f.removedVMs, id)
	return f.removeVMErr
}

func (f *fakeHyperVRunner) RebootGuest(_ context.Context, _ hyperVOptions, id string) error {
	f.rebooted = append(f.rebooted, id)
	return nil
}

func (f *fakeHyperVRunner) RemoveCheckpoint(_ context.Context, _ hyperVOptions, id, state string) error {
	f.removedSnapshots = append(f.removedSnapshots, id+":"+state)
	return nil
}

func newTestHyperV(t *testing.T, runner *fakeHyperVRunner) *Backend {
	t.Helper()
	t.Setenv("TASKPILOT_STATE_DIR", t.TempDir())
	t.Setenv("HYPERV_TEST_GUEST_PASSWORD", "guest-secret")
	if runner.checkpoints == nil {
		runner.checkpoints = map[string]bool{}
	}
	return newBackend(runner, 0)
}

func acquireTestHyperV(t *testing.T, h *Backend, id string) string {
	t.Helper()
	t.Setenv("HYPERV_TEST_GUEST_PASSWORD", "guest-secret")
	instance, err := h.Acquire(context.Background(), testHyperVAcquireRequest(id))
	if err != nil {
		t.Fatal(err)
	}
	if instance.BaselineState != "base" {
		t.Fatalf("state = %q, want base", instance.BaselineState)
	}
	return instance.ID
}

func testHyperVAcquireRequest(id string) target.AcquireRequest {
	return target.AcquireRequest{
		KeepOnFailure: true,
		Options: map[string]any{
			"vmName":        id,
			"baseImage":     `C:\base.vhdx`,
			"baselineState": "base",
			"guest": map[string]any{
				"username":    "Administrator",
				"passwordEnv": "HYPERV_TEST_GUEST_PASSWORD",
			},
		},
	}
}

func testHyperVOptions() map[string]any {
	return map[string]any{
		"baseImage": `C:\base.vhdx`,
		"guest": map[string]any{
			"username":    "Administrator",
			"passwordEnv": "HYPERV_TEST_GUEST_PASSWORD",
		},
	}
}

func TestHyperVAcquireReturnsIDAndBaseline(t *testing.T) {
	runner := &fakeHyperVRunner{}
	h := newTestHyperV(t, runner)

	id := acquireTestHyperV(t, h, "vm-a")

	if id != "vm-a" {
		t.Fatalf("id = %q, want vm-a", id)
	}
	if len(runner.acquires) != 1 || runner.acquires[0].BaseImage != `C:\base.vhdx` {
		t.Fatalf("acquires = %#v", runner.acquires)
	}
}

func TestHyperVRunnerMaterializesAndCleansOnePrivateBundle(t *testing.T) {
	t.Setenv("TASKPILOT_STATE_DIR", t.TempDir())
	command := &bundleInspectingRunner{t: t}
	runner := &psHyperVRunner{command: command}
	if err := runner.run(context.Background(), "removeVM", map[string]any{
		"options": hyperVOptions{VMName: "vm-a"},
		"id":      "vm-a",
	}, nil); err != nil {
		t.Fatal(err)
	}
	if command.bundleDir == "" {
		t.Fatal("command did not observe a bundle")
	}
	if _, err := os.Stat(command.bundleDir); !os.IsNotExist(err) {
		t.Fatalf("bundle was not cleaned after the command: %v", err)
	}
}

func TestDecodeHyperVOptionsRejectsStaleAndUnknownOptions(t *testing.T) {
	t.Setenv("TASKPILOT_STATE_DIR", t.TempDir())
	t.Setenv("HYPERV_TEST_GUEST_PASSWORD", "secret")
	for _, name := range []string{"hostModule", "guestModule", "secretPath", "password", "profile", "unknown"} {
		t.Run(name, func(t *testing.T) {
			options := testHyperVOptions()
			options[name] = "stale"
			if _, err := decodeHyperVOptions(options); err == nil {
				t.Fatal("expected strict option decoding error")
			}
		})
	}
}

func TestGeneratedVMNameUsesMachineShapingOptions(t *testing.T) {
	base := hyperVOptions{
		BaseImage: `C:\base.vhdx`, SwitchName: "Default Switch",
		VMRoot: `C:\vms`, VHDRoot: `C:\vhds`,
		MemoryMB: 4096, ProcessorCount: 2, Generation: 2,
	}
	name := stableHyperVName(base)
	if !strings.HasPrefix(name, "taskpilot-") || len(name) != len("taskpilot-")+12 {
		t.Fatalf("generated name = %q", name)
	}

	changed := base
	changed.MemoryMB++
	if stableHyperVName(changed) == name {
		t.Fatal("memory change did not affect generated VM name")
	}
	changed = base
	changed.DisableSecureBoot = true
	if stableHyperVName(changed) == name {
		t.Fatal("secure boot change did not affect generated VM name")
	}
}

func TestDecodeHyperVOptionsReadsPasswordFromEnvironment(t *testing.T) {
	t.Setenv("TASKPILOT_STATE_DIR", t.TempDir())
	t.Setenv("HYPERV_TEST_GUEST_PASSWORD", "environment-password")
	opts, err := decodeHyperVOptions(testHyperVOptions())
	if err != nil {
		t.Fatal(err)
	}
	if opts.Guest.Password != "environment-password" {
		t.Fatalf("guest password was not resolved from its named environment variable")
	}
}

func TestDecodeHyperVOptionsReportsMissingEnvironmentNameWithoutSecret(t *testing.T) {
	t.Setenv("TASKPILOT_STATE_DIR", t.TempDir())
	t.Setenv("HYPERV_TEST_MISSING_PASSWORD", "")
	options := testHyperVOptions()
	options["guest"].(map[string]any)["passwordEnv"] = "HYPERV_TEST_MISSING_PASSWORD"
	_, err := decodeHyperVOptions(options)
	if err == nil || !strings.Contains(err.Error(), "HYPERV_TEST_MISSING_PASSWORD") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), "environment-password") {
		t.Fatalf("error disclosed credential material: %v", err)
	}
}

func TestDecodeHyperVOptionsSupportsOptionalWorkspace(t *testing.T) {
	t.Setenv("TASKPILOT_STATE_DIR", t.TempDir())
	t.Setenv("HYPERV_TEST_GUEST_PASSWORD", "guest-secret")
	without, err := decodeHyperVOptions(testHyperVOptions())
	if err != nil {
		t.Fatal(err)
	}
	if without.Workspace != nil {
		t.Fatalf("workspace = %#v, want nil", without.Workspace)
	}

	t.Setenv("HYPERV_TEST_WORKSPACE_PASSWORD", "workspace-secret")
	options := testHyperVOptions()
	options["workspace"] = map[string]any{
		"uncPath": `\\host\share`, "drive": "Z:", "username": `host\worker`,
		"passwordEnv": "HYPERV_TEST_WORKSPACE_PASSWORD",
	}
	with, err := decodeHyperVOptions(options)
	if err != nil {
		t.Fatal(err)
	}
	if with.Workspace == nil || with.Workspace.Password != "workspace-secret" {
		t.Fatalf("workspace = %#v", with.Workspace)
	}
}

func TestHyperVMetadataNeverPersistsGuestPassword(t *testing.T) {
	stateDir := t.TempDir()
	meta := hyperVInstanceMeta{ID: "vm-a", BaselineState: "base", LastUsed: time.Now()}
	if err := touchHyperVInstance(stateDir, meta.ID, meta.BaselineState); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(hyperVInstanceRoot(stateDir), "vm-a.json"))
	if err != nil {
		t.Fatal(err)
	}
	metadata := strings.ToLower(string(raw))
	if strings.Contains(metadata, "password") || strings.Contains(metadata, "username") {
		t.Fatalf("metadata contains credential material: %s", raw)
	}
}

func TestHyperVAcquireFailsBeforeRunnerWhenWorkspacePasswordIsMissing(t *testing.T) {
	t.Setenv("TASKPILOT_STATE_DIR", t.TempDir())
	t.Setenv("HYPERV_TEST_GUEST_PASSWORD", "guest-secret")
	t.Setenv("HYPERV_TEST_WORKSPACE_MISSING", "")
	options := testHyperVOptions()
	options["workspace"] = map[string]any{
		"uncPath": `\\host\share`, "drive": "Z:", "username": `host\worker`,
		"passwordEnv": "HYPERV_TEST_WORKSPACE_MISSING",
	}
	runner := &fakeHyperVRunner{}
	h := newBackend(runner, 0)

	if _, err := h.Acquire(context.Background(), target.AcquireRequest{Options: options}); err == nil {
		t.Fatal("expected missing workspace environment error")
	}
	if len(runner.acquires) != 0 {
		t.Fatalf("runner touched the VM before credential validation: %v", runner.acquires)
	}
}

func TestHyperVEnsureExistingCheckpointShortCircuits(t *testing.T) {
	runner := &fakeHyperVRunner{checkpoints: map[string]bool{"commit": true}}
	h := newTestHyperV(t, runner)
	id := acquireTestHyperV(t, h, "vm-a")
	stateDir := os.Getenv("TASKPILOT_STATE_DIR")
	if err := touchHyperVCheckpoint(stateDir, id, "commit",
		script.Result{Stdout: `{"ok":true}`, ExitCode: 0, Value: map[string]any{"ok": true}}); err != nil {
		t.Fatal(err)
	}

	out, err := h.Run(context.Background(), target.RunRequest{
		ID: id, State: "base", Checkpoint: "commit",
		Script: script.Request{Script: "should-not-run"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if out.Result.ExitCode != 0 {
		t.Fatalf("result = %#v", out.Result)
	}
	// A pre-existing checkpoint counts as committed: the state the successor
	// lease will name demonstrably exists.
	if !out.Committed {
		t.Fatal("short-circuit reported no commit; the checkpoint already exists")
	}
	if len(runner.runs) != 0 || len(runner.restores) != 0 || len(runner.snapshots) != 0 {
		t.Fatalf("short-circuit did guest work: runs=%v restores=%v snapshots=%v", runner.runs, runner.restores, runner.snapshots)
	}
	if h.instances[id].logicalState != "commit" {
		t.Fatalf("logical state = %q, want commit", h.instances[id].logicalState)
	}
}

func TestHyperVCheckpointWithoutResultIsRebuilt(t *testing.T) {
	runner := &fakeHyperVRunner{checkpoints: map[string]bool{"commit": true}}
	h := newTestHyperV(t, runner)
	id := acquireTestHyperV(t, h, "vm-a")

	out, err := h.Run(context.Background(), target.RunRequest{
		ID: id, State: "base", Checkpoint: "commit",
		Script: script.Request{Script: "rebuild"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Committed {
		t.Fatal("rebuilt checkpoint was not committed")
	}
	if !reflect.DeepEqual(runner.removedSnapshots, []string{"vm-a:commit"}) {
		t.Fatalf("removed snapshots = %v", runner.removedSnapshots)
	}
	if !reflect.DeepEqual(runner.runs, []string{"rebuild"}) {
		t.Fatalf("runs = %v", runner.runs)
	}
}

func TestHyperVEnsureShortCircuitReplaysCommittedResult(t *testing.T) {
	runner := &fakeHyperVRunner{runResult: hyperVRunResult{Stdout: `{"installPath":"C:\\app"}`, ExitCode: 0}}
	h := newTestHyperV(t, runner)
	id := acquireTestHyperV(t, h, "vm-a")

	first, err := h.Run(context.Background(), target.RunRequest{
		ID: id, State: "base", Checkpoint: "commit",
		Script: script.Request{Script: "deploy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertInstallPathResult(t, first.Result, `C:\app`)

	second, err := h.Run(context.Background(), target.RunRequest{
		ID: id, State: "base", Checkpoint: "commit",
		Script: script.Request{Script: "should-not-run"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Committed {
		t.Fatal("short-circuit reported no commit; the checkpoint already exists")
	}
	assertInstallPathResult(t, second.Result, `C:\app`)
	if !reflect.DeepEqual(runner.runs, []string{"deploy"}) {
		t.Fatalf("runs = %v, want only the original deploy", runner.runs)
	}
}

func TestHyperVEnsureMissingCheckpointRestoresRunsAndSnapshots(t *testing.T) {
	runner := &fakeHyperVRunner{}
	h := newTestHyperV(t, runner)
	id := acquireTestHyperV(t, h, "vm-a")

	_, err := h.Run(context.Background(), target.RunRequest{
		ID: id, State: "base", Checkpoint: "commit",
		Script: script.Request{Script: "apply"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(runner.restores, []string{"base"}) {
		t.Fatalf("restores = %v, want [base]", runner.restores)
	}
	if !reflect.DeepEqual(runner.runs, []string{"apply"}) {
		t.Fatalf("runs = %v, want [apply]", runner.runs)
	}
	if !reflect.DeepEqual(runner.snapshots, []string{"commit"}) {
		t.Fatalf("snapshots = %v, want [commit]", runner.snapshots)
	}
}

func TestHyperVTransientExecNeverSnapshots(t *testing.T) {
	runner := &fakeHyperVRunner{}
	h := newTestHyperV(t, runner)
	id := acquireTestHyperV(t, h, "vm-a")

	_, err := h.Run(context.Background(), target.RunRequest{
		ID: id, State: "base",
		Script: script.Request{Script: "test"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(runner.runs, []string{"test"}) {
		t.Fatalf("runs = %v, want [test]", runner.runs)
	}
	if len(runner.snapshots) != 0 {
		t.Fatalf("transient exec snapshotted: %v", runner.snapshots)
	}
}

func TestHyperVLazyMaterialization(t *testing.T) {
	runner := &fakeHyperVRunner{checkpoints: map[string]bool{"s1": true, "s2": true}}
	h := newTestHyperV(t, runner)
	id := acquireTestHyperV(t, h, "vm-a")
	stateDir := os.Getenv("TASKPILOT_STATE_DIR")
	for _, state := range []string{"s1", "s2"} {
		if err := touchHyperVCheckpoint(stateDir, id, state,
			script.Result{Stdout: "{}", ExitCode: 0, Value: map[string]any{}}); err != nil {
			t.Fatal(err)
		}
	}

	for _, state := range []string{"s1", "s2"} {
		_, err := h.Run(context.Background(), target.RunRequest{
			ID: id, State: "base", Checkpoint: state,
			Script: script.Request{Script: "ensure"},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(runner.restores) != 0 || len(runner.runs) != 0 {
		t.Fatalf("existing ensure checkpoints did guest work: restores=%v runs=%v", runner.restores, runner.runs)
	}

	_, err := h.Run(context.Background(), target.RunRequest{
		ID: id, State: "s2",
		Script: script.Request{Script: "real-work"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(runner.restores, []string{"s2"}) {
		t.Fatalf("restores = %v, want exactly [s2]", runner.restores)
	}
	if !reflect.DeepEqual(runner.runs, []string{"real-work"}) {
		t.Fatalf("runs = %v, want [real-work]", runner.runs)
	}
}

func TestHyperVReleaseKeepDoesNotTearDown(t *testing.T) {
	runner := &fakeHyperVRunner{}
	h := newTestHyperV(t, runner)
	id := acquireTestHyperV(t, h, "vm-a")

	if err := h.Release(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	if len(runner.removedVMs) != 0 {
		t.Fatalf("keep release removed VM: %v", runner.removedVMs)
	}
	if _, ok := h.instances[id]; ok {
		t.Fatal("release did not drop in-process tracking")
	}
}

func TestHyperVFailedReleaseRetainsOwnershipForRetry(t *testing.T) {
	runner := &fakeHyperVRunner{removeVMErr: errors.New("remove failed")}
	h := newTestHyperV(t, runner)
	id := acquireTestHyperV(t, h, "vm-retry")
	lockPath := hyperVMLockPath(os.Getenv("TASKPILOT_STATE_DIR"), id)

	if err := h.Release(context.Background(), id, false); err == nil {
		t.Fatal("expected release failure")
	}
	if h.instances[id] == nil {
		t.Fatal("failed release discarded the instance")
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("failed release discarded the lock: %v", err)
	}

	runner.removeVMErr = nil
	if err := h.Release(context.Background(), id, false); err != nil {
		t.Fatalf("retry release: %v", err)
	}
	if h.instances[id] != nil {
		t.Fatal("successful retry retained the instance")
	}
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock remains after successful retry: %v", err)
	}
}

func TestHyperVReturnedIDLockFailureDoesNotDeleteForeignVM(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("TASKPILOT_STATE_DIR", stateDir)
	t.Setenv("HYPERV_TEST_GUEST_PASSWORD", "guest-secret")
	existing, err := acquireHyperVMLock(stateDir, "actual-vm")
	if err != nil {
		t.Fatal(err)
	}
	defer existing.Release()

	runner := &fakeHyperVRunner{acquireResult: hyperVAcquireResult{ID: "actual-vm", BaselineState: "base"}}
	h := newBackend(runner, 0)
	if _, err := h.Acquire(context.Background(), testHyperVAcquireRequest("requested-vm")); err == nil {
		t.Fatal("expected returned-ID lock failure")
	}
	if len(runner.removedVMs) != 0 {
		t.Fatalf("foreign locked VM was removed: %v", runner.removedVMs)
	}
	if len(h.instances) != 0 {
		t.Fatalf("failed acquisition retained instances: %v", h.instances)
	}
	if _, err := os.Stat(hyperVMLockPath(stateDir, "requested-vm")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("requested-name lock leaked: %v", err)
	}
}

func TestHyperVPostAcquirePersistenceFailureRollsBackNewVM(t *testing.T) {
	runner := &fakeHyperVRunner{}
	h := newTestHyperV(t, runner)
	h.touch = func(string, string, string) error { return errors.New("persist failed") }

	if _, err := h.Acquire(context.Background(), testHyperVAcquireRequest("new-vm")); err == nil {
		t.Fatal("expected persistence failure")
	}
	if !reflect.DeepEqual(runner.removedVMs, []string{"new-vm"}) {
		t.Fatalf("rollback removed VMs = %v, want [new-vm]", runner.removedVMs)
	}
	if len(h.instances) != 0 {
		t.Fatalf("failed acquisition retained instances: %v", h.instances)
	}
}

func TestHyperVFailedAcquireRollbackRemainsTrackedForClose(t *testing.T) {
	runner := &fakeHyperVRunner{removeVMErr: errors.New("rollback failed")}
	h := newTestHyperV(t, runner)
	h.touch = func(string, string, string) error { return errors.New("persist failed") }

	if _, err := h.Acquire(context.Background(), testHyperVAcquireRequest("rollback-retry")); err == nil {
		t.Fatal("expected acquisition failure")
	}
	if h.instances["rollback-retry"] == nil {
		t.Fatal("failed rollback was not retained for shutdown cleanup")
	}
	runner.removeVMErr = nil
	h.Close()
	if got := runner.removedVMs; !reflect.DeepEqual(got, []string{"rollback-retry", "rollback-retry"}) {
		t.Fatalf("RemoveVM calls = %v, want rollback attempt and close retry", got)
	}
}

func TestHyperVReleaseBlocksConcurrentOperations(t *testing.T) {
	continueRemove := make(chan struct{})
	runner := &fakeHyperVRunner{
		removeStarted:  make(chan struct{}),
		removeContinue: continueRemove,
	}

	h := newTestHyperV(t, runner)
	id := acquireTestHyperV(t, h, "vm-release")

	releaseDone := make(chan error, 1)
	go func() {
		releaseDone <- h.Release(context.Background(), id, false)
	}()
	<-runner.removeStarted

	if _, err := h.Run(context.Background(), target.RunRequest{ID: id}); err == nil || !strings.Contains(err.Error(), "being released") {
		t.Fatalf("Run during release error = %v, want being released", err)
	}
	if err := h.Release(context.Background(), id, false); err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("second Release error = %v, want already in progress", err)
	}

	close(continueRemove)
	if err := <-releaseDone; err != nil {
		t.Fatal(err)
	}
}

func TestHyperVReleaseWaitsForActiveRun(t *testing.T) {
	runContinue := make(chan struct{})
	removeContinue := make(chan struct{})
	close(removeContinue)
	runner := &fakeHyperVRunner{
		runStarted:     make(chan struct{}),
		runContinue:    runContinue,
		removeStarted:  make(chan struct{}),
		removeContinue: removeContinue,
	}

	h := newTestHyperV(t, runner)
	id := acquireTestHyperV(t, h, "vm-active-run")

	runDone := make(chan error, 1)
	go func() {
		_, err := h.Run(context.Background(), target.RunRequest{
			ID: id, State: "base", Script: script.Request{Script: "blocked"},
		})
		runDone <- err
	}()
	<-runner.runStarted

	releaseDone := make(chan error, 1)
	go func() {
		releaseDone <- h.Release(context.Background(), id, false)
	}()
	select {
	case <-runner.removeStarted:
		t.Fatal("Release removed VM while Run was active")
	case <-time.After(20 * time.Millisecond):
	}

	close(runContinue)
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
	<-runner.removeStarted
	if err := <-releaseDone; err != nil {
		t.Fatal(err)
	}
}

func TestHyperVSerializesRunsPerInstance(t *testing.T) {
	runContinue := make(chan struct{})
	runner := &fakeHyperVRunner{
		runContinue: runContinue,
		runEntry:    make(chan int, 2),
	}
	h := newTestHyperV(t, runner)
	id := acquireTestHyperV(t, h, "vm-serialized")

	run := func(scriptText string) <-chan error {
		done := make(chan error, 1)
		go func() {
			_, err := h.Run(context.Background(), target.RunRequest{
				ID: id, State: "base", Script: script.Request{Script: scriptText},
			})
			done <- err
		}()
		return done
	}
	firstDone := run("first")
	if call := <-runner.runEntry; call != 1 {
		t.Fatalf("first entered call = %d", call)
	}
	secondDone := run("second")
	select {
	case call := <-runner.runEntry:
		t.Fatalf("second run entered concurrently as call %d", call)
	case <-time.After(20 * time.Millisecond):
	}

	close(runContinue)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if call := <-runner.runEntry; call != 2 {
		t.Fatalf("second entered call = %d", call)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
}

func TestHyperVReleaseRetriesCleanupWithoutRemovingVMTwice(t *testing.T) {
	runner := &fakeHyperVRunner{}
	h := newTestHyperV(t, runner)
	id := acquireTestHyperV(t, h, "vm-cleanup-retry")
	h.cleanup = func(string, string) error { return errors.New("metadata failed") }

	if err := h.Release(context.Background(), id, false); err == nil {
		t.Fatal("expected metadata cleanup failure")
	}

	if h.instances[id] == nil || !h.instances[id].vmRemoved {
		t.Fatal("cleanup failure did not retain removed-VM release state")
	}

	h.cleanup = func(stateDir, id string) error { return removeHyperVMetadata(stateDir, id) }
	if err := h.Release(context.Background(), id, false); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(runner.removedVMs, []string{id}) {
		t.Fatalf("RemoveVM calls = %v, want one", runner.removedVMs)
	}
}

func TestHyperVCloseResumesPendingMetadataCleanup(t *testing.T) {
	runner := &fakeHyperVRunner{}
	h := newTestHyperV(t, runner)
	id := acquireTestHyperV(t, h, "vm-close-cleanup")
	h.cleanup = func(string, string) error { return errors.New("metadata failed") }
	if err := h.Release(context.Background(), id, false); err == nil {
		t.Fatal("expected metadata cleanup failure")
	}

	cleaned := false
	h.cleanup = func(stateDir, id string) error {
		cleaned = true
		return removeHyperVMetadata(stateDir, id)
	}
	h.Close()
	if !cleaned {
		t.Fatal("Close did not resume pending metadata cleanup")
	}
}

func TestHyperVCloseRemovesTargetWhenKeepOnFailureIsFalse(t *testing.T) {
	runner := &fakeHyperVRunner{}
	h := newTestHyperV(t, runner)
	options := testHyperVOptions()
	options["vmName"] = "vm-cleanup"
	_, err := h.Acquire(context.Background(), target.AcquireRequest{
		KeepOnFailure: false,
		Options:       options,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.Close()
	if !reflect.DeepEqual(runner.removedVMs, []string{"vm-cleanup"}) {
		t.Fatalf("removed VMs = %v", runner.removedVMs)
	}
}

func TestHyperVCloseWaitsForAcquireAndRejectsNewOperations(t *testing.T) {
	acquireContinue := make(chan struct{})
	runner := &fakeHyperVRunner{
		acquireStarted:  make(chan struct{}),
		acquireContinue: acquireContinue,
	}
	h := newTestHyperV(t, runner)

	acquireResult := make(chan error, 1)
	go func() {
		_, err := h.Acquire(context.Background(), testHyperVAcquireRequest("vm-acquire"))
		acquireResult <- err
	}()
	<-runner.acquireStarted

	closeDone := make(chan struct{})
	go func() {
		h.Close()
		close(closeDone)
	}()
	waitForHyperVClosing(t, h)

	select {
	case <-closeDone:
		t.Fatal("Close returned while Acquire was active")
	default:
	}
	if _, err := h.Acquire(context.Background(), testHyperVAcquireRequest("vm-late")); !errors.Is(err, errHyperVBackendClosed) {
		t.Fatalf("Acquire during close error = %v, want %v", err, errHyperVBackendClosed)
	}
	if _, err := h.Run(context.Background(), target.RunRequest{ID: "vm-acquire"}); !errors.Is(err, errHyperVBackendClosed) {
		t.Fatalf("Run during close error = %v, want %v", err, errHyperVBackendClosed)
	}

	close(acquireContinue)
	if err := <-acquireResult; err != nil {
		t.Fatalf("admitted Acquire failed: %v", err)
	}
	<-closeDone

	if _, err := h.Acquire(context.Background(), testHyperVAcquireRequest("vm-after")); !errors.Is(err, errHyperVBackendClosed) {
		t.Fatalf("Acquire after close error = %v, want %v", err, errHyperVBackendClosed)
	}
}

func TestHyperVCloseWaitsForRunBeforeReleasingLease(t *testing.T) {
	runner := &fakeHyperVRunner{}
	h := newTestHyperV(t, runner)
	id := acquireTestHyperV(t, h, "vm-run")
	stateDir := os.Getenv("TASKPILOT_STATE_DIR")

	runContinue := make(chan struct{})
	runner.runStarted = make(chan struct{})
	runner.runContinue = runContinue
	runResult := make(chan error, 1)
	go func() {
		_, err := h.Run(context.Background(), target.RunRequest{
			ID: id, State: "base", Script: script.Request{Script: "blocked"},
		})
		runResult <- err
	}()
	<-runner.runStarted

	closeDone := make(chan struct{})
	go func() {
		h.Close()
		close(closeDone)
	}()
	waitForHyperVClosing(t, h)

	if _, err := os.Stat(hyperVMLockPath(stateDir, id)); err != nil {
		t.Fatalf("active Run lease was released during close: %v", err)
	}
	select {
	case <-closeDone:
		t.Fatal("Close returned while Run was active")
	default:
	}

	close(runContinue)
	if err := <-runResult; err != nil {
		t.Fatalf("admitted Run failed: %v", err)
	}
	<-closeDone
	if _, err := os.Stat(hyperVMLockPath(stateDir, id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lease still exists after close: %v", err)
	}
	if _, err := h.Run(context.Background(), target.RunRequest{ID: id}); !errors.Is(err, errHyperVBackendClosed) {
		t.Fatalf("Run after close error = %v, want %v", err, errHyperVBackendClosed)
	}
}

func TestHyperVLockRefusesLiveAndTakesStale(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("TASKPILOT_STATE_DIR", stateDir)
	first := newBackend(&fakeHyperVRunner{}, 0)
	id := acquireTestHyperV(t, first, "vm-a")

	second := newBackend(&fakeHyperVRunner{}, 0)
	options := testHyperVOptions()
	options["vmName"] = id
	if _, err := second.Acquire(context.Background(), target.AcquireRequest{
		KeepOnFailure: true,
		Options:       options,
	}); err == nil {
		t.Fatal("second acquisition succeeded while live lock was held")
	}

	first.Close()
	lock, err := acquireHyperVMLock(stateDir, "vm-stale")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-cacheStaleAgeForTest())
	if err := os.Chtimes(lock.path, old, old); err != nil {
		t.Fatal(err)
	}
	swapProcessMatches(t, func(int) bool { return false })

	staleRunner := &fakeHyperVRunner{}
	staleOwner := newBackend(staleRunner, 0)
	staleID := acquireTestHyperV(t, staleOwner, "vm-stale")
	if staleID != "vm-stale" || len(staleRunner.acquires) != 1 {
		t.Fatalf("stale lock was not taken over: id=%q acquires=%v", staleID, staleRunner.acquires)
	}
}

func TestHyperVOldLockIsNotStolenFromLiveOwner(t *testing.T) {
	stateDir := t.TempDir()
	lock, err := acquireHyperVMLock(stateDir, "vm-live")
	if err != nil {
		t.Fatal(err)
	}

	old := time.Now().Add(-hyperVMLockMaxAge - time.Minute)
	if err := os.Chtimes(lock.path, old, old); err != nil {
		t.Fatal(err)
	}
	swapProcessMatches(t, func(int) bool { return true })

	if stale, err := hyperVMLockStale(lock.path); err != nil || stale {
		t.Fatalf("old live-owner lock stale = %v, err = %v; want false", stale, err)
	}
	second, err := acquireHyperVMLock(stateDir, "vm-live")
	if err != nil {
		t.Fatal(err)
	}
	if second.Held() {
		t.Fatal("old lock was stolen solely because of its age")
	}
}

func TestHyperVOldLockWithoutUsableOwnerIsStale(t *testing.T) {
	stateDir := t.TempDir()
	path := hyperVMLockPath(stateDir, "vm-corrupt")
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not json"), 0o666); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-hyperVMLockMaxAge - time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	if stale, err := hyperVMLockStale(path); err != nil || !stale {
		t.Fatalf("old ownerless lock stale = %v, err = %v; want true", stale, err)
	}
}

func TestSafeFileNameHashesWindowsReservedDeviceNames(t *testing.T) {
	reserved := []string{
		"CON", "con.txt", "PRN.log", "AUX", "NUL.json",
		"COM1", "com9.ext", "LPT1.data", "lpt9.", "CON.txt.",
	}
	for _, name := range reserved {
		t.Run(name, func(t *testing.T) {
			got := safeFileName(name)
			if got == name {
				t.Fatalf("safeFileName(%q) returned the reserved component unchanged", name)
			}
			if isWindowsReservedPathComponent(got) {
				t.Fatalf("safeFileName(%q) = %q, still reserved", name, got)
			}
		})
	}

	for _, name := range []string{"console", "COM0", "COM10", "LPT0", "LPT10", "NULled", "normal.json"} {
		if got := safeFileName(name); got != name {
			t.Errorf("safeFileName(%q) = %q, want unchanged", name, got)
		}
	}

}

func TestSafeFileNameHashNamespaceCannotCollideWithRawID(t *testing.T) {
	hashed := safeFileName("invalid/name")
	if hashed == safeFileName(hashed) {
		t.Fatalf("hashed key %q collides with the same raw ID", hashed)
	}
	if safeFileName("") == safeFileName("_") {
		t.Fatal("empty ID collides with raw underscore")
	}
}

func TestHyperVLockSupportsReservedDeviceName(t *testing.T) {
	lock, err := acquireHyperVMLock(t.TempDir(), "CON")
	if err != nil {
		t.Fatal(err)
	}

	if !lock.Held() {
		t.Fatal("reserved VM ID lock was not acquired")
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestHyperVHeldLockHeartbeatPreventsStealAndSweep(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("TASKPILOT_STATE_DIR", stateDir)
	runner := &fakeHyperVRunner{}
	h := newBackend(runner, 0)
	id := acquireTestHyperV(t, h, "vm-a")
	lockPath := hyperVMLockPath(stateDir, id)
	old := time.Now().Add(-cacheStaleAgeForTest()).UTC()
	if err := os.Chtimes(lockPath, old, old); err != nil {
		t.Fatal(err)
	}

	if _, err := h.Run(context.Background(), target.RunRequest{
		ID: id, State: "base",
		Script: script.Request{Script: "still-alive"},
	}); err != nil {
		t.Fatal(err)
	}
	if stale, err := hyperVMLockStale(lockPath); err != nil || stale {
		t.Fatalf("lock stale = %v, err = %v; want fresh", stale, err)
	}

	second := newBackend(&fakeHyperVRunner{}, 0)
	options := testHyperVOptions()
	options["vmName"] = id
	if _, err := second.Acquire(context.Background(), target.AcquireRequest{
		KeepOnFailure: true,
		Options:       options,
	}); err == nil {
		t.Fatal("second acquisition succeeded after the live lease heartbeated")
	}

	writeJSON(t, filepath.Join(hyperVInstanceRoot(stateDir), "vm-a.json"), hyperVInstanceMeta{ID: id, BaselineState: "base", LastUsed: old})
	writeJSON(t, hyperVCheckpointPath(stateDir, id, "old-state"), hyperVCheckpointMeta{ID: id, State: "old-state", LastUsed: old})
	sweeperRunner := &fakeHyperVRunner{}
	sweeper := newBackend(sweeperRunner, 0)
	removed, err := sweeper.Sweep(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
	if len(sweeperRunner.removedVMs) != 0 || len(sweeperRunner.removedSnapshots) != 0 {
		t.Fatalf("sweep removed live state: VMs=%v checkpoints=%v", sweeperRunner.removedVMs, sweeperRunner.removedSnapshots)
	}
}

func TestHyperVSweepDoesNotRemoveHeldLockState(t *testing.T) {
	runner := &fakeHyperVRunner{}
	h := newTestHyperV(t, runner)
	stateDir := os.Getenv("TASKPILOT_STATE_DIR")
	lock, err := acquireHyperVMLock(stateDir, "held-vm")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	old := time.Now().Add(-2 * time.Hour).UTC()
	writeJSON(t, filepath.Join(hyperVInstanceRoot(stateDir), "held-vm.json"), hyperVInstanceMeta{ID: "held-vm", BaselineState: "base", LastUsed: old})
	writeJSON(t, hyperVCheckpointPath(stateDir, "held-vm", "old-state"), hyperVCheckpointMeta{ID: "held-vm", State: "old-state", LastUsed: old})

	removed, err := h.Sweep(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
	if len(runner.removedSnapshots) != 0 || len(runner.removedVMs) != 0 {
		t.Fatalf("removed held state: snapshots=%v VMs=%v", runner.removedSnapshots, runner.removedVMs)
	}
}

func TestHyperVSweepRemovesOldState(t *testing.T) {
	runner := &fakeHyperVRunner{}
	h := newTestHyperV(t, runner)
	stateDir := os.Getenv("TASKPILOT_STATE_DIR")
	old := time.Now().Add(-2 * time.Hour).UTC()
	// Unknown fields from older metadata are tolerated by normal JSON decoding,
	// but newly written metadata does not carry runtime asset paths.
	writeJSON(t, filepath.Join(hyperVInstanceRoot(stateDir), "old-vm.json"), map[string]any{
		"id": "old-vm", "baselineState": "base", "hostModule": `C:\old\host.psm1`, "lastUsed": old,
	})
	writeJSON(t, hyperVCheckpointPath(stateDir, "old-vm", "old-state"), map[string]any{
		"id": "old-vm", "state": "old-state", "hostModule": `C:\old\host.psm1`, "lastUsed": old,
	})

	removed, err := h.Sweep(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	if !reflect.DeepEqual(runner.removedSnapshots, []string{"old-vm:old-state"}) {
		t.Fatalf("removed snapshots = %v", runner.removedSnapshots)
	}
	if !reflect.DeepEqual(runner.removedVMs, []string{"old-vm"}) {
		t.Fatalf("removed VMs = %v", runner.removedVMs)
	}
}

func assertInstallPathResult(t *testing.T, result script.Result, want string) {
	t.Helper()
	inner, ok := result.Value.(map[string]any)
	if !ok {
		t.Fatalf("result value = %T, want map", result.Value)
	}
	if got := inner["installPath"]; got != want {
		t.Fatalf("installPath = %q, want %q", got, want)
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o666); err != nil {
		t.Fatal(err)
	}
}

func cacheStaleAgeForTest() time.Duration {
	return hyperVMLockMaxAge + time.Minute
}

func waitForHyperVClosing(t *testing.T, h *Backend) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.mu.Lock()
		closing := h.closing
		h.mu.Unlock()
		if closing {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("Backend.Close did not start")
		}
		runtime.Gosched()
	}
}

// A layer that changes boot-time state (testsigning, driver install) only takes
// effect after a restart. The reboot must happen before the checkpoint, or the
// checkpoint captures state the guest has not actually applied and every later
// restore replays that same un-applied state.
func TestHyperVEnsureRebootsBeforeCheckpointWhenRequested(t *testing.T) {
	runner := &fakeHyperVRunner{
		runResult: hyperVRunResult{Stdout: `{"rebootRequested":true}`, ExitCode: 0},
	}
	h := newTestHyperV(t, runner)
	id := acquireTestHyperV(t, h, "vm-a")

	out, err := h.Run(context.Background(), target.RunRequest{
		ID: id, State: "base", Checkpoint: "layer",
		Script: script.Request{Script: "apply"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Committed {
		t.Fatal("layer did not commit after its reboot")
	}
	if len(runner.rebooted) != 1 {
		t.Fatalf("rebooted %v, want exactly one restart", runner.rebooted)
	}
	if len(runner.snapshots) != 1 {
		t.Fatalf("snapshots %v, want one checkpoint", runner.snapshots)
	}
}

// A layer that does not ask for a restart must not get one: an unnecessary
// reboot costs a minute of guest downtime on every cold layer.
func TestHyperVEnsureDoesNotRebootWhenNotRequested(t *testing.T) {
	runner := &fakeHyperVRunner{
		runResult: hyperVRunResult{Stdout: `{"ok":true}`, ExitCode: 0},
	}
	h := newTestHyperV(t, runner)
	id := acquireTestHyperV(t, h, "vm-a")

	if _, err := h.Run(context.Background(), target.RunRequest{
		ID: id, State: "base", Checkpoint: "layer",
		Script: script.Request{Script: "apply"},
	}); err != nil {
		t.Fatal(err)
	}
	if len(runner.rebooted) != 0 {
		t.Fatalf("rebooted %v, want none", runner.rebooted)
	}
}
