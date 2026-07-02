package lock

import (
	"path/filepath"
	"testing"
)

func TestSecondAcquireFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.lock")

	l1, err := Acquire(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer l1.Release()

	// A second lock on the same path must be refused immediately.
	if l2, err := Acquire(path); err == nil {
		l2.Release()
		t.Fatal("second acquire succeeded — two instances could run concurrently")
	}
}

func TestReacquireAfterRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.lock")

	l1, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l1.Release(); err != nil {
		t.Fatal(err)
	}

	l2, err := Acquire(path)
	if err != nil {
		t.Fatalf("reacquire after release: %v", err)
	}
	l2.Release()
}
