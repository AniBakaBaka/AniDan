// SPDX-License-Identifier: AGPL-3.0-only

// Package logrotate provides a bounded, record-oriented application log writer.
// Its directory must be private to the service: advisory locking excludes other
// cooperating Writers, not unrelated programs modifying, linking or appending
// files. Each Write is one record; oversized records are rejected, never split.
// It does not bound stderr, filesystem metadata, or unrelated files.
package logrotate

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
)

const (
	DefaultMaxBytes int64 = 10 << 20
	DefaultBackups        = 3
	filename              = "app.log"
	lockname              = ".app.log.lock"
)

var (
	ErrRecordTooLarge = errors.New("log record exceeds per-file limit")
	ErrClosed         = errors.New("log writer is closed")
)

// Options sets explicit limits. MaxBytes must be positive and Backups must be
// between 0 and 100. OnFailure, if non-nil, is called once after a latched I/O or
// safety failure, outside the writer lock. It must not panic. A rejected oversized
// record is not a latched failure. Use DefaultMaxBytes and DefaultBackups for the
// application defaults: at most 40 MiB across app.log and app.log.1 through .3.
// Older numeric archives outside this range are preserved but not managed.
type Options struct {
	MaxBytes  int64
	Backups   int
	OnFailure func()
}

// Writer is safe for concurrent Write and Close calls. A rotation or I/O failure
// permanently disables further file writes; it never falls back to unbounded
// appending. The caller can continue logging to an independently managed stderr.
type Writer struct {
	mu       sync.Mutex
	root     *os.Root
	file     *os.File
	lock     *os.File
	opts     Options
	size     int64
	failure  error
	closed   bool
	closeErr error
	// An instance-local seam for deterministic rotation failure tests.
	rename func(string, string) error
}

// Open uses an existing directory and preserves existing in-limit regular logs.
// It rejects symlinks, nonregular/hardlinked files, oversized existing logs, and
// unexpected app.log.* entries. Numeric archives outside the retention limit
// belong to the previous logger and are left untouched, outside our size bound.
// Unix directories and managed files must not be group- or world-writable;
// Windows directory/file ACLs remain the operator's responsibility.
// Such legacy files require operator review/removal before startup; Open never
// truncates, archives, or deletes them to force them into the new limits.
// A persistent zero-byte .app.log.lock is held until Close. After opening, all
// accesses are relative to the pinned directory, even if it is renamed.
func Open(directory string, opts Options) (*Writer, error) {
	if opts.MaxBytes <= 0 || opts.Backups < 0 || opts.Backups > 100 {
		return nil, errors.New("log rotation requires positive MaxBytes and 0..100 Backups")
	}
	before, err := os.Lstat(directory)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("log directory must be a real directory, not a symlink")
	}
	if err := checkDirectory(before); err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	w := &Writer{root: r, opts: opts, rename: r.Rename}
	ok := false
	defer func() {
		if !ok {
			w.Close()
		}
	}()
	after, err := r.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		return nil, errors.New("log directory changed while opening")
	}
	if err := checkDirectory(after); err != nil {
		return nil, err
	}
	w.lock, err = w.openRegular(lockname, os.O_RDWR, true)
	if err != nil {
		return nil, fmt.Errorf("open log lock: %w", err)
	}
	if err = lockExclusive(w.lock); err != nil {
		return nil, fmt.Errorf("lock log directory (another writer may be active): %w", err)
	}
	if err = w.checkLock(); err != nil {
		return nil, err
	}
	if _, err = w.preflight(); err != nil {
		return nil, err
	}
	w.file, err = w.openRegular(filename, os.O_WRONLY|os.O_APPEND, true)
	if err != nil {
		return nil, err
	}
	info, err := w.file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > opts.MaxBytes {
		return nil, fmt.Errorf("%s exceeds log size limit; operator cleanup required", filename)
	}
	w.size = info.Size()
	ok = true
	return w, nil
}

// openRegular never truncates and never follows a final symlink on Unix. The
// nonblocking flag prevents a concurrent FIFO replacement from blocking open.
// Descriptor identity and type are checked before any write, on every platform.
func (w *Writer) openRegular(name string, flags int, create bool) (*os.File, error) {
	before, err := w.root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) && create {
		flags |= os.O_CREATE | os.O_EXCL
	} else if err != nil {
		return nil, err
	} else if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular file", name)
	}
	f, err := openAnchored(w.root, name, flags, 0600)
	if err != nil {
		return nil, err
	}
	info, err := w.checkFile(name, f)
	if err == nil && before != nil && !os.SameFile(before, info) {
		err = fmt.Errorf("%s changed while opening", name)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func (w *Writer) checkFile(name string, f *os.File) (os.FileInfo, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular file", name)
	}
	if err := checkSingleLink(f, info); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	pathInfo, err := w.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !pathInfo.Mode().IsRegular() || !os.SameFile(info, pathInfo) {
		return nil, fmt.Errorf("%s was replaced", name)
	}
	return info, nil
}

func (w *Writer) checkLock() error {
	info, err := w.checkFile(lockname, w.lock)
	if err == nil && info.Size() != 0 {
		err = errors.New("log lock must be an empty regular file")
	}
	return err
}

// preflight checks every managed entry before rotation changes any of them.
// ReadDir is batched so unrelated directory entries cannot cause a large slice.
func (w *Writer) preflight() (map[string]bool, error) {
	d, err := w.root.Open(".")
	if err != nil {
		return nil, err
	}
	defer d.Close()
	present := make(map[string]bool, w.opts.Backups+1)
	for {
		entries, readErr := d.ReadDir(128)
		for _, entry := range entries {
			name := entry.Name()
			if name != filename && !strings.HasPrefix(name, filename+".") {
				continue
			}
			if name != filename {
				n, err := strconv.Atoi(strings.TrimPrefix(name, filename+"."))
				if err != nil || n < 1 || name != archive(n) {
					return nil, fmt.Errorf("unexpected log archive %q; operator cleanup required", name)
				}
				if n > w.opts.Backups {
					continue
				}
			}
			f, err := w.openRegular(name, os.O_RDONLY, false)
			if err != nil {
				return nil, err
			}
			info, statErr := f.Stat()
			closeErr := f.Close()
			if err := errors.Join(statErr, closeErr); err != nil {
				return nil, err
			}
			if info.Size() > w.opts.MaxBytes {
				return nil, fmt.Errorf("%s exceeds log size limit; operator cleanup required", name)
			}
			present[name] = true
		}
		if errors.Is(readErr, io.EOF) {
			return present, nil
		}
		if readErr != nil {
			return nil, readErr
		}
	}
}

func archive(n int) string { return filename + "." + strconv.Itoa(n) }

func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	wasFailed := w.failure != nil
	n, err := w.writeLocked(p)
	notify := !wasFailed && w.failure != nil && w.opts.OnFailure != nil
	w.mu.Unlock()
	if notify {
		w.opts.OnFailure()
	}
	return n, err
}

func (w *Writer) writeLocked(p []byte) (int, error) {
	if w.closed {
		return 0, ErrClosed
	}
	if w.failure != nil {
		return 0, w.failure
	}
	if int64(len(p)) > w.opts.MaxBytes {
		return 0, ErrRecordTooLarge
	}
	if len(p) == 0 {
		return 0, nil
	}
	if err := w.checkLock(); err != nil {
		return 0, w.fail(err)
	}
	info, err := w.checkFile(filename, w.file)
	if err != nil {
		return 0, w.fail(err)
	}
	if info.Size() != w.size {
		return 0, w.fail(errors.New("app.log size changed outside this writer"))
	}
	if int64(len(p)) > w.opts.MaxBytes-w.size {
		if err := w.rotate(); err != nil {
			return 0, w.fail(err)
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		err = w.fail(err)
	}
	return n, err
}

func (w *Writer) fail(err error) error {
	w.failure = fmt.Errorf("app.log file logging disabled: %w", err)
	return w.failure
}

func (w *Writer) rotate() error {
	present, err := w.preflight()
	if err != nil {
		return err
	}
	if err = w.file.Sync(); err != nil {
		return err
	}
	err = w.file.Close()
	w.file = nil
	if err != nil {
		return err
	}
	if w.opts.Backups == 0 {
		if err = w.root.Remove(filename); err != nil {
			return err
		}
	} else {
		if present[archive(w.opts.Backups)] {
			if err = w.root.Remove(archive(w.opts.Backups)); err != nil {
				return err
			}
		}
		for n := w.opts.Backups - 1; n >= 1; n-- {
			if present[archive(n)] {
				if err = w.rename(archive(n), archive(n+1)); err != nil {
					return err
				}
			}
		}
		if err = w.rename(filename, archive(1)); err != nil {
			return err
		}
	}
	w.file, err = w.openRegular(filename, os.O_WRONLY|os.O_APPEND|os.O_CREATE|os.O_EXCL, true)
	if err != nil {
		return err
	}
	w.size = 0
	return nil
}

// Close waits for current writes, syncs/closes the file, and releases the lock.
// It is idempotent; writes after Close return ErrClosed. Rotation is not a
// transactional journal: a crash/failure may leave gaps in archive numbering or
// lose the oldest archive already evicted, but never enables unbounded growth.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return w.closeErr
	}
	w.closed = true
	if w.file != nil {
		w.closeErr = errors.Join(w.file.Sync(), w.file.Close())
		w.file = nil
	}
	if w.lock != nil {
		w.closeErr = errors.Join(w.closeErr, w.lock.Close())
		w.lock = nil
	}
	if w.root != nil {
		w.closeErr = errors.Join(w.closeErr, w.root.Close())
		w.root = nil
	}
	return w.closeErr
}
