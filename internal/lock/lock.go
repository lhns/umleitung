// Package lock provides the cross-process startup lock that guarantees only
// one Umleiter instance can run against a given state volume.
package lock

import (
	"fmt"

	"github.com/gofrs/flock"
)

// Lock is a held cross-process file lock.
type Lock struct {
	fl *flock.Flock
}

// Acquire takes an exclusive, non-blocking lock on path. If another process
// holds it, Acquire returns an error immediately — the second instance must
// refuse to start rather than wait (two syncers would double-append).
func Acquire(path string) (*Lock, error) {
	fl := flock.New(path)
	ok, err := fl.TryLock()
	if err != nil {
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	if !ok {
		return nil, fmt.Errorf("lock %s: held by another umleiter instance — refusing to start (never run two syncers against the same state)", path)
	}
	return &Lock{fl: fl}, nil
}

// Release releases the lock.
func (l *Lock) Release() error { return l.fl.Unlock() }
