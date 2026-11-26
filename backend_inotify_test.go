//go:build linux
// +build linux

package fsnotify

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Above this number of events, TestInotifyOverflow is skipped: creating the
// files would take too long.
const maxOverflowEvents = 2 * 1024 * 1024

// maxQueuedEvents returns the fs.inotify.max_queued_events sysctl setting, or
// the kernel default if it cannot be read.
func maxQueuedEvents(t *testing.T) int {
	const kernelDefault = 16384
	b, err := os.ReadFile("/proc/sys/fs/inotify/max_queued_events")
	if err != nil {
		t.Logf("can not read fs.inotify.max_queued_events, assuming %d: %v", kernelDefault, err)
		return kernelDefault
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n <= 0 {
		t.Logf("bad fs.inotify.max_queued_events %q, assuming %d", b, kernelDefault)
		return kernelDefault
	}
	return n
}

// Ensure that the correct error is returned on overflows.
func TestInotifyOverflow(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	w := newWatcher(t)
	defer w.Close()

	// We need to generate many more events than the
	// fs.inotify.max_queued_events sysctl setting.
	numDirs, numFiles := 128, 1024
	limit := maxQueuedEvents(t)
	if n := 2*limit/numFiles + 1; n > numDirs {
		numDirs = n
	}
	if numDirs*numFiles > maxOverflowEvents {
		t.Skipf("fs.inotify.max_queued_events is %d: too high to trigger an overflow with at most %d events",
			limit, maxOverflowEvents)
	}

	// All events need to be in the inotify queue before pulling events off it
	// to trigger this error.
	var wg sync.WaitGroup
	for i := 0; i < numDirs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			dir := join(tmp, strconv.Itoa(i))
			mkdir(t, dir, noWait)
			addWatch(t, w, dir)

			createFiles(t, dir, "", numFiles, 10*time.Second)
		}(i)
	}
	wg.Wait()

	var (
		creates   = 0
		overflows = 0
	)
	for overflows == 0 && creates < numDirs*numFiles {
		select {
		case <-time.After(10 * time.Second):
			t.Fatalf("Not done")
		case err := <-w.Errors:
			if !errors.Is(err, ErrEventOverflow) {
				t.Fatalf("unexpected error from watcher: %v", err)
			}
			overflows++
		case e := <-w.Events:
			if !strings.HasPrefix(e.Name, tmp) {
				t.Fatalf("Event for unknown file: %s", e.Name)
			}
			if e.Op == Create {
				creates++
			}
		}
	}

	if creates == numDirs*numFiles {
		t.Fatalf("could not trigger overflow")
	}
	if overflows == 0 {
		t.Fatalf("no overflow and not enough CREATE events (expected %d, got %d)",
			numDirs*numFiles, creates)
	}
}

// Test inotify's "we don't send REMOVE until all file descriptors are removed"
// behaviour.
func TestInotifyDeleteOpenFile(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	file := join(tmp, "file")

	touch(t, file)
	fp, err := os.Open(file)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	defer fp.Close()

	w := newCollector(t, file)
	w.collect(t)

	rm(t, file)
	waitForEvents()
	e := w.events(t)
	cmpEvents(t, tmp, e, newEvents(t, `chmod /file`))

	fp.Close()
	e = w.stop(t)
	cmpEvents(t, tmp, e, newEvents(t, `remove /file`))
}

func TestRemoveState(t *testing.T) {
	var (
		tmp  = t.TempDir()
		dir  = join(tmp, "dir")
		file = join(dir, "file")
	)
	mkdir(t, dir)
	touch(t, file)

	w := newWatcher(t, tmp)
	addWatch(t, w, tmp)
	addWatch(t, w, file)

	check := func(want int) {
		t.Helper()
		if w.watches.len() != want {
			t.Error(w.watches)
		}
	}

	check(2)

	if err := w.Remove(file); err != nil {
		t.Fatal(err)
	}
	check(1)

	if err := w.Remove(tmp); err != nil {
		t.Fatal(err)
	}
	check(0)
}

// Events still queued for a watch when it is removed must not be reported with
// a wrong path ("/name"), and the reader must not panic.
func TestInotifyRemoveWhileWriting(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	w, err := NewWatcher()
	if err != nil {
		t.Fatal(err)
	}

	bad := make(chan string, 100)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case e, ok := <-w.Events:
				if !ok {
					return
				}
				if !strings.HasPrefix(e.Name, tmp) {
					select {
					case bad <- e.String():
					default:
					}
				}
			case _, ok := <-w.Errors:
				if !ok {
					return
				}
			}
		}
	}()

	for i := 0; i < 300; i++ {
		if err := w.Add(tmp); err != nil {
			t.Fatal(err)
		}
		stop := make(chan struct{})
		written := make(chan struct{})
		go func() {
			defer close(written)
			for j := 0; ; j++ {
				select {
				case <-stop:
					return
				default:
				}
				os.WriteFile(join(tmp, strconv.Itoa(j%20)), []byte("x"), 0o644)
			}
		}()
		time.Sleep(time.Millisecond)
		if err := w.Remove(tmp); err != nil {
			t.Fatal(err)
		}
		close(stop)
		<-written
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	<-done
	close(bad)
	for e := range bad {
		t.Errorf("event outside of the watched directory: %s", e)
	}
}
