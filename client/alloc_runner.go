package client

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/nomad/client/allocdir"
	"github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/client/driver"
	"github.com/hashicorp/nomad/nomad/structs"
)

const (
	// allocSyncRetryIntv is the interval on which we retry updating
	// the status of the allocation
	allocSyncRetryIntv = 15 * time.Second
)

// taskStatus is used to track the status of a task
type taskStatus struct {
	Status      string
	Description string
}

// AllocStateUpdater is used to update the status of an allocation
type AllocStateUpdater func(alloc *structs.Allocation) error

// AllocRunner is used to wrap an allocation and provide the execution context.
type AllocRunner struct {
	config        *config.Config
	updater       AllocStateUpdater
	logger        *log.Logger
	consulService *ConsulService

	alloc *structs.Allocation

	dirtyCh chan struct{}

	ctx           *driver.ExecContext
	tasks         map[string]*TaskRunner
	restored      map[string]struct{}
	RestartPolicy *structs.RestartPolicy
	taskLock      sync.RWMutex

	taskStatusLock sync.RWMutex

	updateCh chan *structs.Allocation

	destroy     bool
	destroyCh   chan struct{}
	destroyLock sync.Mutex
	waitCh      chan struct{}
}

// allocRunnerState is used to snapshot the state of the alloc runner
type allocRunnerState struct {
	Alloc         *structs.Allocation
	RestartPolicy *structs.RestartPolicy
	TaskStatus    map[string]taskStatus
	Context       *driver.ExecContext
}

// NewAllocRunner is used to create a new allocation context
func NewAllocRunner(logger *log.Logger, config *config.Config, updater AllocStateUpdater,
	alloc *structs.Allocation, consulService *ConsulService) *AllocRunner {
	ar := &AllocRunner{
		config:        config,
		updater:       updater,
		logger:        logger,
		alloc:         alloc,
		consulService: consulService,
		dirtyCh:       make(chan struct{}, 1),
		tasks:         make(map[string]*TaskRunner),
		restored:      make(map[string]struct{}),
		updateCh:      make(chan *structs.Allocation, 8),
		destroyCh:     make(chan struct{}),
		waitCh:        make(chan struct{}),
	}
	return ar
}

// stateFilePath returns the path to our state file
func (r *AllocRunner) stateFilePath() string {
	return filepath.Join(r.config.StateDir, "alloc", r.alloc.ID, "state.json")
}

// RestoreState is used to restore the state of the alloc runner
func (r *AllocRunner) RestoreState() error {
	// Load the snapshot
	var snap allocRunnerState
	if err := restoreState(r.stateFilePath(), &snap); err != nil {
		return err
	}

	// Restore fields
	r.alloc = snap.Alloc
	r.RestartPolicy = snap.RestartPolicy
	r.ctx = snap.Context

	// Restore the task runners
	var mErr multierror.Error
	for name, state := range r.alloc.TaskStates {
		// Mark the task as restored.
		r.restored[name] = struct{}{}

		task := &structs.Task{Name: name}
		restartTracker := newRestartTracker(r.alloc.Job.Type, r.RestartPolicy)
		tr := NewTaskRunner(r.logger, r.config, r.setTaskState, r.ctx,
			r.alloc.ID, task, r.alloc.TaskStates[task.Name], restartTracker,
			r.consulService)
		r.tasks[name] = tr

		// Skip tasks in terminal states.
		if state.State == structs.TaskStateDead {
			continue
		}

		if err := tr.RestoreState(); err != nil {
			r.logger.Printf("[ERR] client: failed to restore state for alloc %s task '%s': %v", r.alloc.ID, name, err)
			mErr.Errors = append(mErr.Errors, err)
		} else {
			go tr.Run()
		}
	}
	return mErr.ErrorOrNil()
}

// SaveState is used to snapshot the state of the alloc runner
// if the fullSync is marked as false only the state of the Alloc Runner
// is snapshotted. If fullSync is marked as true, we snapshot
// all the Task Runners associated with the Alloc
func (r *AllocRunner) SaveState() error {
	if err := r.saveAllocRunnerState(); err != nil {
		return err
	}

	// Save state for each task
	r.taskLock.RLock()
	defer r.taskLock.RUnlock()
	var mErr multierror.Error
	for _, tr := range r.tasks {
		if err := r.saveTaskRunnerState(tr); err != nil {
			mErr.Errors = append(mErr.Errors, err)
		}
	}
	return mErr.ErrorOrNil()
}

func (r *AllocRunner) saveAllocRunnerState() error {
	r.taskStatusLock.RLock()
	defer r.taskStatusLock.RUnlock()
	snap := allocRunnerState{
		Alloc:         r.alloc,
		RestartPolicy: r.RestartPolicy,
		Context:       r.ctx,
	}
	return persistState(r.stateFilePath(), &snap)
}

func (r *AllocRunner) saveTaskRunnerState(tr *TaskRunner) error {
	var err error
	if err = tr.SaveState(); err != nil {
		r.logger.Printf("[ERR] client: failed to save state for alloc %s task '%s': %v",
			r.alloc.ID, tr.task.Name, err)
	}
	return err
}

// DestroyState is used to cleanup after ourselves
func (r *AllocRunner) DestroyState() error {
	return os.RemoveAll(filepath.Dir(r.stateFilePath()))
}

// DestroyContext is used to destroy the context
func (r *AllocRunner) DestroyContext() error {
	return r.ctx.AllocDir.Destroy()
}

// Alloc returns the associated allocation
func (r *AllocRunner) Alloc() *structs.Allocation {
	return r.alloc
}

// dirtySyncState is used to watch for state being marked dirty to sync
func (r *AllocRunner) dirtySyncState() {
	for {
		select {
		case <-r.dirtyCh:
			r.retrySyncState(r.destroyCh)
		case <-r.destroyCh:
			return
		}
	}
}

// retrySyncState is used to retry the state sync until success
func (r *AllocRunner) retrySyncState(stopCh chan struct{}) {
	for {
		if err := r.syncStatus(); err == nil {
			// The Alloc State might have been re-computed so we are
			// snapshoting only the alloc runner
			r.saveAllocRunnerState()
			return
		}
		select {
		case <-time.After(allocSyncRetryIntv + randomStagger(allocSyncRetryIntv)):
		case <-stopCh:
			return
		}
	}
}

// syncStatus is used to run and sync the status when it changes
func (r *AllocRunner) syncStatus() error {
	// Scan the task states to determine the status of the alloc
	var pending, running, dead, failed bool
	r.taskStatusLock.RLock()
	for _, state := range r.alloc.TaskStates {
		switch state.State {
		case structs.TaskStateRunning:
			running = true
		case structs.TaskStatePending:
			pending = true
		case structs.TaskStateDead:
			last := len(state.Events) - 1
			if state.Events[last].Type == structs.TaskDriverFailure {
				failed = true
			} else {
				dead = true
			}
		}
	}
	if len(r.alloc.TaskStates) > 0 {
		taskDesc, _ := json.Marshal(r.alloc.TaskStates)
		r.alloc.ClientDescription = string(taskDesc)
	}
	r.taskStatusLock.RUnlock()

	// Determine the alloc status
	if failed {
		r.alloc.ClientStatus = structs.AllocClientStatusFailed
	} else if running {
		r.alloc.ClientStatus = structs.AllocClientStatusRunning
	} else if dead && !pending {
		r.alloc.ClientStatus = structs.AllocClientStatusDead
	}

	// Attempt to update the status
	if err := r.updater(r.alloc); err != nil {
		r.logger.Printf("[ERR] client: failed to update alloc '%s' status to %s: %s",
			r.alloc.ID, r.alloc.ClientStatus, err)
		return err
	}
	return nil
}

// setStatus is used to update the allocation status
func (r *AllocRunner) setStatus(status, desc string) {
	r.alloc.ClientStatus = status
	r.alloc.ClientDescription = desc
	select {
	case r.dirtyCh <- struct{}{}:
	default:
	}
}

// setTaskState is used to set the status of a task
func (r *AllocRunner) setTaskState(taskName string) {
	select {
	case r.dirtyCh <- struct{}{}:
	default:
	}
}

// Run is a long running goroutine used to manage an allocation
func (r *AllocRunner) Run() {
	defer close(r.waitCh)
	go r.dirtySyncState()

	// Check if the allocation is in a terminal status
	alloc := r.alloc
	if alloc.TerminalStatus() {
		r.logger.Printf("[DEBUG] client: aborting runner for alloc '%s', terminal status", r.alloc.ID)
		return
	}
	r.logger.Printf("[DEBUG] client: starting runner for alloc '%s'", r.alloc.ID)

	// Find the task group to run in the allocation
	tg := alloc.Job.LookupTaskGroup(alloc.TaskGroup)
	if tg == nil {
		r.logger.Printf("[ERR] client: alloc '%s' for missing task group '%s'", alloc.ID, alloc.TaskGroup)
		r.setStatus(structs.AllocClientStatusFailed, fmt.Sprintf("missing task group '%s'", alloc.TaskGroup))
		return
	}

	// Extract the RestartPolicy from the TG and set it on the alloc
	r.RestartPolicy = tg.RestartPolicy

	// Create the execution context
	if r.ctx == nil {
		allocDir := allocdir.NewAllocDir(filepath.Join(r.config.AllocDir, r.alloc.ID))
		if err := allocDir.Build(tg.Tasks); err != nil {
			r.logger.Printf("[WARN] client: failed to build task directories: %v", err)
			r.setStatus(structs.AllocClientStatusFailed, fmt.Sprintf("failed to build task dirs for '%s'", alloc.TaskGroup))
			return
		}
		r.ctx = driver.NewExecContext(allocDir, r.alloc.ID)
	}

	// Start the task runners
	r.taskLock.Lock()
	for _, task := range tg.Tasks {
		if _, ok := r.restored[task.Name]; ok {
			continue
		}

		// Merge in the task resources
		task.Resources = alloc.TaskResources[task.Name]
		restartTracker := newRestartTracker(r.alloc.Job.Type, r.RestartPolicy)
		tr := NewTaskRunner(r.logger, r.config, r.setTaskState, r.ctx,
			r.alloc.ID, task, r.alloc.TaskStates[task.Name], restartTracker,
			r.consulService)
		r.tasks[task.Name] = tr
		go tr.Run()
	}
	r.taskLock.Unlock()

OUTER:
	// Wait for updates
	for {
		select {
		case update := <-r.updateCh:
			// Check if we're in a terminal status
			if update.TerminalStatus() {
				break OUTER
			}

			// Update the task groups
			r.taskLock.RLock()
			for _, task := range tg.Tasks {
				tr := r.tasks[task.Name]

				// Merge in the task resources
				task.Resources = update.TaskResources[task.Name]
			FOUND:
				for _, updateGroup := range update.Job.TaskGroups {
					if tg.Name != updateGroup.Name {
						continue
					}
					for _, updateTask := range updateGroup.Tasks {
						if updateTask.Name != task.Name {
							continue
						}
						task.Services = updateTask.Services
						break FOUND
					}
				}
				tr.Update(task)
			}
			r.taskLock.RUnlock()

		case <-r.destroyCh:
			break OUTER
		}
	}

	// Destroy each sub-task
	r.taskLock.RLock()
	defer r.taskLock.RUnlock()
	for _, tr := range r.tasks {
		tr.Destroy()
	}

	// Wait for termination of the task runners
	for _, tr := range r.tasks {
		<-tr.WaitCh()
	}

	// Final state sync
	r.retrySyncState(nil)

	// Check if we should destroy our state
	if r.destroy {
		if err := r.DestroyContext(); err != nil {
			r.logger.Printf("[ERR] client: failed to destroy context for alloc '%s': %v",
				r.alloc.ID, err)
		}
		if err := r.DestroyState(); err != nil {
			r.logger.Printf("[ERR] client: failed to destroy state for alloc '%s': %v",
				r.alloc.ID, err)
		}
	}
	r.logger.Printf("[DEBUG] client: terminating runner for alloc '%s'", r.alloc.ID)
}

// Update is used to update the allocation of the context
func (r *AllocRunner) Update(update *structs.Allocation) {
	select {
	case r.updateCh <- update:
	default:
		r.logger.Printf("[ERR] client: dropping update to alloc '%s'", update.ID)
	}
}

// Destroy is used to indicate that the allocation context should be destroyed
func (r *AllocRunner) Destroy() {
	r.destroyLock.Lock()
	defer r.destroyLock.Unlock()

	if r.destroy {
		return
	}
	r.destroy = true
	close(r.destroyCh)
}

// WaitCh returns a channel to wait for termination
func (r *AllocRunner) WaitCh() <-chan struct{} {
	return r.waitCh
}
