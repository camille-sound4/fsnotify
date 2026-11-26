//go:build windows
// +build windows

package fsnotify

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRemoveState(t *testing.T) {
	// TODO: the Windows backend is too confusing; needs some serious attention.
	return

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
		if len(w.watches) != want {
			var d []string
			for k, v := range w.watches {
				d = append(d, fmt.Sprintf("%#v = %#v", k, v))
			}
			t.Errorf("unexpected number of entries in w.watches (have %d, want %d):\n%v",
				len(w.watches), want, strings.Join(d, "\n"))
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

func TestWindowsRemWatch(t *testing.T) {
	tmp := t.TempDir()

	touch(t, tmp, "file")

	w := newWatcher(t)
	defer w.Close()

	addWatch(t, w, tmp)
	if err := w.Remove(tmp); err != nil {
		t.Fatalf("Could not remove the watch: %v\n", err)
	}
	if err := w.remWatch(tmp); err == nil {
		t.Fatal("Should be fail with closed handle\n")
	}
}

// Remove() while events are coming in: the completion of the read in flight
// arrives after the handle is closed, and must not be processed (it closed the
// handle a second time, which could close an unrelated handle of the process).
func TestRemoveWhileWriting(t *testing.T) {
	tmp := t.TempDir()
	w, err := NewWatcher()
	if err != nil {
		t.Fatal(err)
	}

	errs := make(chan error, 100)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case _, ok := <-w.Events:
				if !ok {
					return
				}
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				select {
				case errs <- err:
				default:
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
				os.WriteFile(join(tmp, fmt.Sprintf("f%d", j%20)), []byte("x"), 0o644)
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
	close(errs)
	for err := range errs {
		if s := err.Error(); strings.Contains(s, "CloseHandle") || strings.Contains(s, "CancelIo") {
			t.Errorf("handle used after being closed: %v", err)
		}
	}
}

// Renaming "in" must not rewrite the path of a watch on a sibling "in2" that
// only shares its prefix.
func TestRenameSiblingPrefix(t *testing.T) {
	tmp := t.TempDir()
	sibling := join(tmp, "in2")
	mkdir(t, sibling)
	touch(t, join(tmp, "in"))

	w, err := NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.Add(tmp); err != nil {
		t.Fatal(err)
	}
	if err := w.Add(sibling); err != nil {
		t.Fatal(err)
	}

	if err := os.Rename(join(tmp, "in"), join(tmp, "x")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	want := join(sibling, "f")
	touch(t, want)

	timeout := time.After(5 * time.Second)
	for {
		select {
		case e := <-w.Events:
			if strings.HasSuffix(e.Name, `\f`) {
				if e.Name != want {
					t.Fatalf("event for %q, want %q", e.Name, want)
				}
				return
			}
		case err := <-w.Errors:
			t.Fatal(err)
		case <-timeout:
			t.Fatalf("no event for %q", want)
		}
	}
}
