package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// stateLockFileName sits in the machine state directory, beside machine.json.
// One lock for every project is what keeps two projects from claiming the
// same ports or the same emulator: each allocation reads the other projects'
// files while holding it.
const stateLockFileName = "wet.lock"

// stateLockTimeout bounds the wait. Allocation is a few file reads and a
// handful of port probes — a second at most — so a wait this long means a
// crashed holder, not contention, and saying so beats hanging forever.
const stateLockTimeout = 30 * time.Second

// withStateLock runs fn holding the machine-wide allocation lock.
//
// Slots, ports and pool items are claimed by read-modify-write on one JSON
// file. Two `wet setup` runs started at the same moment — the whole point of
// parallel worktrees — both read the same state, both compute the same lowest
// free slot, and both write it: two worktrees on identical ports, holding the
// same phone, and the second stack to boot dies on a port that the first one
// took. The window is small and the symptom is nothing like its cause, which
// is the worst combination.
//
// The lock is advisory and process-scoped (flock), so it costs nothing when
// there is no contention and is released even if wet is killed.
func withStateLock(fn func() error) error {
	return withMachineLock(stateLockFileName, stateLockTimeout,
		"another wet run is allocating slots", fn)
}

// withMachineLock runs fn holding an exclusive flock on name in the machine
// state directory, waiting up to timeout. busy says what a wait is for.
func withMachineLock(name string, timeout time.Duration, busy string, fn func() error) error {
	if err := os.MkdirAll(machineStateDir(), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", machineStateDir(), err)
	}
	path := filepath.Join(machineStateDir(), name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	deadline := time.Now().Add(timeout)
	waited := false
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK {
			return fmt.Errorf("lock %s: %w", path, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for %s: %s, or one died holding it "+
				"(remove the file if nothing else is running)", timeout, path, busy)
		}
		if !waited {
			fmt.Printf("  Waiting: %s…\n", busy)
			waited = true
		}
		time.Sleep(100 * time.Millisecond)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	return fn()
}
