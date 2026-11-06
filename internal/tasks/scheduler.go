// SPDX-License-Identifier: GPL-2.0-only

package tasks

import (
	"context"
	"errors"
	"fmt"
	"log"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
)

// Runnable is the execution seam a registered task provides. Run is called
// on the scheduler's worker goroutine; progress reports a 0–100 percentage
// and may be called at any frequency — the scheduler clamps and throttles.
type Runnable interface {
	Run(ctx context.Context, progress func(float64)) error
}

// Definition describes one scheduled task for registration.
type Definition struct {
	ID          string
	Key         string
	Name        string
	Description string
	Category    string
	Hidden      bool
	// HiddenFunc, when set, decides IsHidden on every snapshot instead of
	// Hidden (upstream IsHidden is a property read on each listing).
	HiddenFunc      func() bool
	DefaultTriggers []TriggerInfo
}

// Result status values, pinned from openapi TaskCompletionStatus.
const (
	StatusCompleted = "Completed"
	StatusFailed    = "Failed"
	StatusCancelled = "Cancelled"
	StatusAborted   = "Aborted"
)

// drainYieldLimit bounds the cooperative yield loop in Close. Each
// iteration yields this goroutine so timer goroutines that observed
// the done signal can run and call doneWorker. The bound is high
// enough to drain every armed timer goroutine under a single-threaded
// scheduler, and low enough that a blocked worker exits the loop
// promptly. This is an iteration count, not a duration: it does not
// advance the task clock and does not add a wall-clock grace delay.
const drainYieldLimit = 200

var (
	// ErrSchedulerClosed is returned by Register/Run/UpdateTriggers after Close.
	ErrSchedulerClosed = errors.New("tasks: scheduler is closed")
	// ErrSchedulerStarted is returned by Register after Start. The
	// Register-before-Start contract is documented on Register.
	ErrSchedulerStarted = errors.New("tasks: scheduler is started")
	// ErrTaskNotFound is returned when an unknown task ID is addressed.
	ErrTaskNotFound = errors.New("tasks: task not found")
)

// task is the scheduler's internal record for one registered task.
type task struct {
	def  Definition
	run  Runnable
	info Info

	mu             sync.Mutex
	pending        bool         // a trigger or Run arrived while running
	pendingTrigger *TriggerInfo // the firing trigger of a coalesced triggered run; nil when the pending run is manual (no max runtime)
	running        bool         // execution goroutine is active
	cancelFn       context.CancelFunc
	timer          clock.Timer
	armCancel      chan struct{} // closed once to release the armed timer worker; nil when no timer is armed
	armTime        time.Time     // when the current trigger timer was armed
	result         Result
	hasResult      bool
	startupFired   bool         // a StartupTrigger has fired; never rearms (StartupTrigger.cs)
	firingTrigger  *TriggerInfo // the trigger whose timer is currently armed; nil for manual runs
}

// Scheduler owns task registration, timers, worker goroutines, queue/coalescing,
// persistence, and shutdown. Every state change is published through the
// existing Catalog.Upsert.
type Scheduler struct {
	clock   clock.Clock
	loc     *time.Location
	catalog *Catalog
	persist *persist

	mu           sync.Mutex
	closed       bool
	started      bool
	done         chan struct{} // closed once at shutdown; never blocks
	tasks        map[string]*task
	completed    func(context.Context, Result) // set before Start; runs after each execution
	workers      int32                         // active execution + timer goroutines
	drained      chan struct{}                 // closed once when workers reaches zero during shutdown
	closeDrained sync.Once                     // guards the single close(drained)
}

// NewScheduler constructs a task scheduler. configDir and dataDir are the
// trigger-override and last-result persistence directories. It creates both
// directories with MkdirAll. It returns nil and an error when the creation of
// either persistence directory fails.
func NewScheduler(c clock.Clock, loc *time.Location, catalog *Catalog, configDir, dataDir string) (*Scheduler, error) {
	if c == nil {
		panic("tasks: clock is required")
	}
	if catalog == nil {
		panic("tasks: catalog is required")
	}
	if loc == nil {
		loc = time.UTC
	}
	p := newPersist(configDir, dataDir)
	if err := p.ensureDirs(); err != nil {
		return nil, err
	}
	return &Scheduler{
		clock:   c,
		loc:     loc,
		catalog: catalog,
		persist: p,
		done:    make(chan struct{}),
		drained: make(chan struct{}),
		tasks:   make(map[string]*task),
	}, nil
}

// OnCompleted sets the function that receives each execution result after the
// scheduler records it (upstream TaskCompletionEventArgs consumers). Set it
// before Start. The function runs on the execution goroutine.
func (s *Scheduler) OnCompleted(observer func(context.Context, Result)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrSchedulerClosed
	}
	if s.started {
		return ErrSchedulerStarted
	}
	s.completed = observer
	return nil
}

// Register adds a task definition before Start. A duplicate ID is rejected.
func (s *Scheduler) Register(def Definition, run Runnable) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrSchedulerClosed
	}
	if s.started {
		return ErrSchedulerStarted
	}
	lower := lowerID(def.ID)
	if _, exists := s.tasks[lower]; exists {
		return fmt.Errorf("tasks: duplicate task ID %q", def.ID)
	}

	// Load persisted trigger overrides or fall back to defaults.
	triggers := def.DefaultTriggers
	if persisted, err := s.persist.loadTriggers(def.ID); err != nil {
		return fmt.Errorf("tasks: load triggers for %s: %w", def.ID, err)
	} else if persisted != nil {
		triggers = persisted
	}

	// Load last result.
	var lastResult *Result
	if lr, err := s.persist.loadResult(def.ID); err != nil {
		return fmt.Errorf("tasks: load result for %s: %w", def.ID, err)
	} else {
		lastResult = lr
	}

	info := Info{
		Name:        def.Name,
		State:       StateIdle,
		ID:          def.ID,
		Triggers:    triggers,
		Description: def.Description,
		Category:    def.Category,
		IsHidden:    def.Hidden,
		Key:         def.Key,
	}
	if lastResult != nil {
		info.LastExecutionResult = lastResult
	}

	t := &task{
		def:       def,
		run:       run,
		info:      info,
		result:    Result{},
		hasResult: lastResult != nil,
	}
	if lastResult != nil {
		t.result = *lastResult
	}

	s.tasks[lower] = t

	_ = s.catalog.Upsert(context.Background(), t.snapshot())
	return nil
}

// Start arms every task's triggers. Startup triggers fire 3 seconds later.
func (s *Scheduler) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrSchedulerClosed
	}
	s.started = true
	for _, t := range s.tasks {
		t.mu.Lock()
		s.armLocked(t)
		t.mu.Unlock()
	}
	return nil
}

// List returns all registered tasks, hidden included.
func (s *Scheduler) List() []Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]Info, 0, len(s.tasks))
	for _, t := range s.tasks {
		t.mu.Lock()
		result = append(result, t.snapshot())
		t.mu.Unlock()
	}
	return result
}

// Get returns the task info by case-insensitive ID match.
func (s *Scheduler) Get(id string) (Info, bool) {
	s.mu.Lock()
	t, ok := s.tasks[lowerID(id)]
	s.mu.Unlock()
	if !ok {
		return Info{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshot(), true
}

// Run starts a task manually; a Run while Running coalesces (sets pending).
func (s *Scheduler) Run(id string) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrSchedulerClosed
	}
	t, ok := s.tasks[lowerID(id)]
	s.mu.Unlock()
	if !ok {
		return ErrTaskNotFound
	}
	return s.startOrCoalesce(t, nil)
}

// Cancel cancels a running task; no-op when idle.
func (s *Scheduler) Cancel(id string) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrSchedulerClosed
	}
	t, ok := s.tasks[lowerID(id)]
	s.mu.Unlock()
	if !ok {
		return ErrTaskNotFound
	}
	t.mu.Lock()
	if !t.running {
		t.mu.Unlock()
		return nil
	}
	if t.cancelFn != nil {
		t.info.State = StateCancelling
		t.cancelFn()
	}
	snap := t.snapshot()
	t.mu.Unlock()
	_ = s.catalog.Upsert(context.Background(), snap)
	return nil
}

// UpdateTriggers replaces a task's trigger overrides, persists them, and
// rearms immediately. A running execution is left untouched.
func (s *Scheduler) UpdateTriggers(id string, triggers []TriggerInfo) error {
	for _, tr := range triggers {
		if err := validateTrigger(tr); err != nil {
			return err
		}
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrSchedulerClosed
	}
	t, ok := s.tasks[lowerID(id)]
	s.mu.Unlock()
	if !ok {
		return ErrTaskNotFound
	}
	// Persist under the registered ID, the name loadTriggers reads at
	// startup, not the request casing.
	if err := s.persist.saveTriggers(t.def.ID, triggers); err != nil {
		return err
	}
	t.mu.Lock()
	t.info.Triggers = triggers
	s.armLocked(t)
	snap := t.snapshot()
	t.mu.Unlock()
	_ = s.catalog.Upsert(context.Background(), snap)
	return nil
}

// IsRunning reports whether a task is currently executing.
func (s *Scheduler) IsRunning(id string) bool {
	s.mu.Lock()
	t, ok := s.tasks[lowerID(id)]
	s.mu.Unlock()
	if !ok {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.running
}

// Close rejects new starts, stops all timers, cancels running executions,
// and waits for drain until ctx expires. Re-callable to finish draining.
// This mirrors TranscodeManager.Close's re-callable drain pattern: a completed
// drain is observable state that takes deterministic precedence over a caller's
// canceled context.
func (s *Scheduler) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return s.waitForDrain(ctx)
	}
	s.closed = true
	close(s.done)
	for _, t := range s.tasks {
		t.mu.Lock()
		s.cancelArmLocked(t)
		if t.running && t.cancelFn != nil {
			t.info.State = StateCancelling
			t.cancelFn()
		}
		t.mu.Unlock()
	}
	// If no goroutines are active, the drain is already complete.
	if atomic.LoadInt32(&s.workers) == 0 {
		s.closeDrained.Do(func() { close(s.drained) })
	}
	s.mu.Unlock()

	// Give timer goroutines a bounded cooperative window to observe the
	// done signal, exit, and call doneWorker before waitForDrain races a
	// caller's canceled context. close(s.done) above wakes every timer
	// goroutine, but the goroutines need a scheduling turn to run.
	// runtime.Gosched yields this goroutine so the runtime can schedule
	// them. The bound is an iteration count, not a clock deadline, so a
	// fixed fake clock cannot keep the loop active indefinitely. A worker
	// that does not respect its context (a blocked execution goroutine)
	// keeps the count above zero, so the bound prevents an infinite spin;
	// waitForDrain then returns the caller's context error. This is a
	// cooperative yield, not a production wall-clock grace delay.
	for i := 0; i < drainYieldLimit && atomic.LoadInt32(&s.workers) > 0; i++ {
		runtime.Gosched()
	}
	if atomic.LoadInt32(&s.workers) == 0 {
		s.closeDrained.Do(func() { close(s.drained) })
	}

	return s.waitForDrain(ctx)
}

func (s *Scheduler) waitForDrain(ctx context.Context) error {
	// A completed drain takes precedence over a canceled context (matching
	// TranscodeManager.Close). The non-blocking check handles the case where
	// all goroutines have already exited before the caller's context expired.
	select {
	case <-s.drained:
		return nil
	default:
	}
	select {
	case <-s.drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// startOrCoalesce starts a task execution if idle, or sets the pending flag
// if it's already running (coalescing — upstream TaskManager.QueueScheduledTask).
// firingTrigger is the trigger whose timer fired (nil for manual starts); when
// the task is already running, a non-nil firingTrigger is retained as the
// pending run's trigger so the coalesced execution inherits that trigger's
// MaxRuntimeTicks. A manual Run while running clears any pending trigger so
// the coalesced re-execution has no max runtime (spec §1: "Manual starts have
// no max runtime").
func (s *Scheduler) startOrCoalesce(t *task, firingTrigger *TriggerInfo) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrSchedulerClosed
	}

	t.mu.Lock()
	ctx, snap, started := s.startLocked(t, firingTrigger)
	t.mu.Unlock()
	if !started {
		s.mu.Unlock()
		return nil
	}

	// Increment the worker counter while still holding s.mu so Close cannot
	// observe zero workers in the gap between the closed check and the
	// launch. Close sets closed and then re-checks workers under s.mu, so a
	// start linearized before Close is counted and one after is rejected.
	s.addWorker()
	s.mu.Unlock()

	_ = s.catalog.Upsert(context.Background(), snap)
	go s.execute(ctx, t)
	return nil
}

// startLocked commits the state mutation for a manual or timer-triggered
// start. The caller must hold s.mu and t.mu. It returns a non-nil context
// and snapshot when the execution starts, or started=false when the task is
// already running (coalescing — sets pending and retains the firing trigger).
func (s *Scheduler) startLocked(t *task, firingTrigger *TriggerInfo) (ctx context.Context, snap Info, started bool) {
	if t.running {
		t.pending = true
		t.pendingTrigger = firingTrigger
		return nil, Info{}, false
	}
	t.running = true
	t.pending = false
	t.pendingTrigger = nil
	t.firingTrigger = firingTrigger
	t.info.State = StateRunning
	t.info.CurrentProgressPercentage = zeroPtr()
	cctx, cancel := context.WithCancel(context.Background())
	t.cancelFn = cancel
	return cctx, t.snapshot(), true
}

// execute runs the task body, handles progress throttling, result recording,
// and re-arming triggers on completion.
func (s *Scheduler) execute(ctx context.Context, t *task) {
	defer s.doneWorker()

	start := s.clock.Now()

	// MaxRuntimeTicks comes from the firing trigger only (upstream
	// ScheduledTaskWorker applies it per trigger). Manual starts have no
	// firing trigger and therefore no max runtime.
	maxRuntime := s.firingMaxRuntime(t)
	var maxTimer clock.Timer
	if maxRuntime > 0 {
		maxTimer = s.clock.NewTimer(maxRuntime)
		defer maxTimer.Stop()
	}

	// Throttled progress reporting.
	var lastProgressTime time.Time
	var lastWholePercent float64
	progress := func(p float64) {
		if p < 0 {
			p = 0
		}
		if p > 100 {
			p = 100
		}
		now := s.clock.Now()
		whole := float64(int(p))
		t.mu.Lock()
		t.info.CurrentProgressPercentage = &p
		shouldPublish := now.Sub(lastProgressTime) >= time.Second || whole != lastWholePercent
		if shouldPublish {
			lastProgressTime = now
			lastWholePercent = whole
		}
		snap := t.snapshot()
		t.mu.Unlock()
		if shouldPublish {
			_ = s.catalog.Upsert(context.Background(), snap)
		}
	}

	var runErr error
	maxRuntimeFired := false
	if maxTimer != nil {
		done := make(chan struct{})
		go func() {
			runErr = t.run.Run(ctx, progress)
			close(done)
		}()
		select {
		case <-done:
		case <-maxTimer.C():
			maxRuntimeFired = true
			t.mu.Lock()
			if t.cancelFn != nil {
				t.cancelFn()
			}
			t.mu.Unlock()
			<-done
		}
	} else {
		runErr = t.run.Run(ctx, progress)
	}

	end := s.clock.Now()

	// Determine result status.
	status := StatusCompleted
	errMsg := ""
	longErrMsg := ""
	if maxRuntimeFired {
		// Max-runtime expiry cancels the execution context and records Aborted
		// (spec §1: "expiry cancels the execution context and records Aborted").
		status = StatusAborted
	} else if ctx.Err() != nil {
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if closed {
			// Close during a run cancels and records Aborted.
			status = StatusAborted
		} else {
			// User Cancel records Cancelled.
			status = StatusCancelled
		}
	} else if runErr != nil {
		status = StatusFailed
		errMsg = runErr.Error()
		longErrMsg = runErr.Error()
	}

	result := newResult(t.def.ID, t.def.Key, t.def.Name, start, end, status, errMsg, longErrMsg)
	if err := s.persist.saveResult(t.def.ID, result); err != nil {
		log.Printf("tasks: persist result for %s: %v", t.def.ID, err)
	}

	t.mu.Lock()
	pending := t.pending
	pendingTrigger := t.pendingTrigger
	t.running = false
	t.pending = false
	t.pendingTrigger = nil
	t.cancelFn = nil
	t.result = result
	t.hasResult = true
	t.info.LastExecutionResult = &result
	t.info.CurrentProgressPercentage = nil
	t.info.State = StateIdle
	if !s.closed {
		s.armLocked(t)
	}
	snap := t.snapshot()
	t.mu.Unlock()
	_ = s.catalog.Upsert(context.Background(), snap)
	if s.completed != nil {
		s.completed(context.Background(), result)
	}

	// Coalesced re-execution: if a trigger or Run arrived during execution,
	// re-execute once with the retained firing trigger so the coalesced run
	// inherits that trigger's MaxRuntimeTicks (nil for a manual coalesce → no
	// max runtime, per spec §1).
	if pending && !s.isClosed() {
		_ = s.startOrCoalesce(t, pendingTrigger)
	}
}

// firingMaxRuntime returns the MaxRuntimeTicks of the trigger that fired
// (t.firingTrigger), or 0 if none. Manual starts have no firing trigger and
// therefore no max runtime (spec §1: "Manual starts have no max runtime").
func (s *Scheduler) firingMaxRuntime(t *task) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.firingTrigger == nil {
		return 0
	}
	if t.firingTrigger.MaxRuntimeTicks != nil && *t.firingTrigger.MaxRuntimeTicks > 0 {
		return ticksToDuration(*t.firingTrigger.MaxRuntimeTicks)
	}
	return 0
}

// cancelArmLocked stops the armed timer, closes the arm-cancellation channel
// once, and clears both fields. The caller must hold t.mu. A second call is a
// no-op because both fields are nil after the first call.
func (s *Scheduler) cancelArmLocked(t *task) {
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
	if t.armCancel != nil {
		close(t.armCancel)
		t.armCancel = nil
	}
}

// armLocked arms (or rearms) the timer for the next trigger firing. The caller
// must hold t.mu.
func (s *Scheduler) armLocked(t *task) {
	s.cancelArmLocked(t)
	t.firingTrigger = nil
	if s.closed {
		return
	}

	bestDelay := time.Duration(0)
	var bestTrigger *TriggerInfo
	for i := range t.info.Triggers {
		tr := &t.info.Triggers[i]
		// Startup triggers fire once and never rearm (StartupTrigger.cs — no
		// timer, no OnTriggered re-entry).
		if tr.Type == TriggerStartup && t.startupFired {
			continue
		}
		var lastEnd time.Time
		if t.hasResult {
			lastEnd = t.result.EndTimeUTC.Time
		}
		delay := nextFireDelay(s.clock, s.loc, *tr, lastEnd, t.armTime)
		if bestTrigger == nil || delay < bestDelay {
			bestDelay = delay
			bestTrigger = tr
		}
	}
	if bestTrigger == nil {
		return
	}

	t.armTime = s.clock.Now()
	t.firingTrigger = bestTrigger
	t.timer = s.clock.NewTimer(bestDelay)
	t.armCancel = make(chan struct{})

	s.addWorker()
	go s.armWorker(t, t.timer, t.armCancel, bestTrigger)
}

// armWorker waits for timer delivery, cancellation, or shutdown. It owns one
// addWorker/doneWorker pair. On timer delivery it acquires s.mu and t.mu in
// that order, verifies the timer is still current, clears arm ownership, and
// starts or coalesces the execution.
func (s *Scheduler) armWorker(t *task, timer clock.Timer, cancel chan struct{}, tr *TriggerInfo) {
	defer s.doneWorker()
	select {
	case <-cancel:
		return
	case <-s.done:
		return
	case <-timer.C():
	}

	s.mu.Lock()
	t.mu.Lock()
	// Reject shutdown and stale arms (a rearm replaced the timer or closed
	// the channel). The timer and channel must both match this arm.
	if s.closed || t.timer != timer || t.armCancel != cancel {
		t.mu.Unlock()
		s.mu.Unlock()
		return
	}
	// Clear arm ownership: the timer fired and this worker commits the start.
	t.timer = nil
	t.armCancel = nil
	// Set startupFired only after the identity check passes.
	if tr != nil && tr.Type == TriggerStartup {
		t.startupFired = true
	}
	ctx, snap, started := s.startLocked(t, tr)
	t.mu.Unlock()
	if !started {
		s.mu.Unlock()
		return
	}
	s.addWorker()
	s.mu.Unlock()
	_ = s.catalog.Upsert(context.Background(), snap)
	go s.execute(ctx, t)
}

func (s *Scheduler) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// addWorker increments the active goroutine counter. Called before starting
// an execution or timer goroutine.
func (s *Scheduler) addWorker() {
	atomic.AddInt32(&s.workers, 1)
}

// doneWorker decrements the active goroutine counter. If it reaches zero
// during shutdown, the drained channel is closed — this is deterministic
// because it happens on the goroutine that observed the last decrement, not
// on a separate watcher goroutine racing a canceled context.
func (s *Scheduler) doneWorker() {
	if atomic.AddInt32(&s.workers, -1) == 0 {
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if closed {
			s.closeDrained.Do(func() { close(s.drained) })
		}
	}
}

// snapshot returns a defensive copy of the task's current Info.
func (t *task) snapshot() Info {
	clone := t.info
	if t.def.HiddenFunc != nil {
		clone.IsHidden = t.def.HiddenFunc()
	}
	if clone.CurrentProgressPercentage != nil {
		v := *clone.CurrentProgressPercentage
		clone.CurrentProgressPercentage = &v
	}
	if clone.LastExecutionResult != nil {
		v := *clone.LastExecutionResult
		clone.LastExecutionResult = &v
	}
	if clone.Triggers != nil {
		clone.Triggers = append([]TriggerInfo(nil), clone.Triggers...)
	}
	return clone
}

func lowerID(id string) string {
	b := make([]byte, len(id))
	for i := 0; i < len(id); i++ {
		c := id[i]
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		} else {
			b[i] = c
		}
	}
	return string(b)
}

func zeroPtr() *float64 {
	v := 0.0
	return &v
}
