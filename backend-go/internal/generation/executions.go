package generation

import "sync"

// executionDirectory is the process-wide registry of in-flight executions.
// Every HTTP request builds its own Service, so per-instance state cannot
// reach the provider call an earlier request started: a cancel admitted on a
// fresh instance would mark the ledger paused yet leave the original provider
// request running. The directory binds execution control to the run/project
// lifecycle instead of the request that happened to dispatch it.
type executionDirectory struct {
	mu         sync.Mutex
	byAttempt  map[string]*execution
	projectIDs map[*execution]string
}

var processExecutions = &executionDirectory{byAttempt: map[string]*execution{}, projectIDs: map[*execution]string{}}

func newExecutionDirectory() *executionDirectory {
	return &executionDirectory{byAttempt: map[string]*execution{}, projectIDs: map[*execution]string{}}
}

func (d *executionDirectory) register(x *execution, projectID string) {
	if d == nil || x == nil {
		return
	}
	d.mu.Lock()
	d.byAttempt[x.attemptID] = x
	d.projectIDs[x] = projectID
	d.mu.Unlock()
}

func (d *executionDirectory) unregister(attemptID string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	if x, ok := d.byAttempt[attemptID]; ok {
		delete(d.byAttempt, attemptID)
		delete(d.projectIDs, x)
	}
	d.mu.Unlock()
}

// abortMatching marks stopped and cancels the provider context of every
// non-terminal execution the predicate accepts. It deliberately does not
// touch the ledger: the owning Service instance still settles the attempt,
// which is what turns the abort into a durable terminal receipt.
func (d *executionDirectory) abortMatching(match func(*execution, string) bool) {
	if d == nil {
		return
	}
	d.mu.Lock()
	targets := make([]*execution, 0, len(d.byAttempt))
	for _, x := range d.byAttempt {
		if match(x, d.projectIDs[x]) {
			targets = append(targets, x)
		}
	}
	d.mu.Unlock()
	for _, x := range targets {
		x.mu.Lock()
		if !x.terminal {
			x.stopped = true
			x.cancel()
		}
		x.mu.Unlock()
	}
}

func (d *executionDirectory) abortRoot(rootID string) {
	d.abortMatching(func(x *execution, _ string) bool { return x.rootID == rootID })
}

func (d *executionDirectory) abortProject(projectID string) {
	d.abortMatching(func(_ *execution, id string) bool { return id == projectID })
}

// AbortProjectExecutions cancels every in-flight execution admitted for the
// project, whichever Service instance owns it. Project close calls this
// before the captured database goes away so a detached dispatch cannot keep
// dialing the provider against a closing project.
func AbortProjectExecutions(projectID string) {
	processExecutions.abortProject(projectID)
}
