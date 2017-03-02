package fsutil

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/docker/containerd/fs"
	"github.com/pkg/errors"
	"github.com/stevvooe/continuity/sysx"
	"golang.org/x/sys/unix"
)

type writeToFunc func(context.Context, string, io.WriteCloser) error

type DiskWriter struct {
	asyncDataFunc writeToFunc
	syncDataFunc  writeToFunc
	dest          string

	wg     sync.WaitGroup
	mu     sync.RWMutex
	err    error
	ctx    context.Context
	cancel func()
}

func (dw *DiskWriter) Wait() error {
	dw.wg.Wait()
	dw.mu.RLock()
	defer dw.mu.RUnlock()
	return dw.err
}

func (dw *DiskWriter) HandleChange(kind fs.ChangeKind, p string, fi os.FileInfo, err error) (retErr error) {
	if err != nil {
		return err
	}

	if dw.ctx == nil {
		ctx, cancel := context.WithCancel(context.Background())
		dw.ctx = ctx
		dw.cancel = cancel
	}

	defer func() {
		if retErr != nil {
			dw.mu.Lock()
			if dw.err == nil {
				dw.err = err
			}
			dw.mu.Unlock()
			dw.cancel()
		}
	}()

	destPath := filepath.Join(dw.dest, p)

	if kind == fs.ChangeKindDelete {
		// todo: no need to validate if diff is trusted but is it always?
		if err := os.RemoveAll(destPath); err != nil {
			return errors.Wrapf(err, "failed to remove: %s", destPath)
		}
		return nil
	}

	stat, ok := fi.Sys().(*Stat)
	if !ok {
		return errors.Errorf("%s invalid change without stat information", p)
	}

	rename := true
	oldFi, err := os.Lstat(destPath)
	if err != nil {
		if os.IsNotExist(err) {
			if kind != fs.ChangeKindAdd {
				return errors.Wrapf(err, "invalid addition: %s", destPath)
			}
			rename = false
		} else {
			return errors.Wrapf(err, "failed to stat %s", destPath)
		}
	}

	if oldFi != nil && fi.IsDir() && oldFi.IsDir() {
		if err := rewriteMetadata(destPath, stat); err != nil {
			return errors.Wrapf(err, "error setting dir metadata for %s", destPath)
		}
		return nil
	}

	newPath := destPath
	if rename {
		newPath = filepath.Join(filepath.Dir(destPath), ".tmp."+nextSuffix())
	}

	// todo: combine with hardlink validation

	asyncRequestFileData := false

	switch {
	case fi.IsDir():
		if err := os.Mkdir(newPath, fi.Mode()); err != nil {
			return errors.Wrapf(err, "failed to create dir %s", newPath)
		}
	case fi.Mode()&os.ModeDevice != 0 || fi.Mode()&os.ModeNamedPipe != 0:
		if err := handleTarTypeBlockCharFifo(newPath, stat); err != nil {
			return errors.Wrapf(err, "failed to create device %s", newPath)
		}
	case fi.Mode()&os.ModeSymlink != 0:
		if err := os.Symlink(stat.Linkname, newPath); err != nil {
			return errors.Wrapf(err, "failed to symlink %s", newPath)
		}
	case stat.Linkname != "":
		if err := os.Link(filepath.Join(dw.dest, stat.Linkname), newPath); err != nil {
			return errors.Wrapf(err, "failed to link %s to %s", newPath, stat.Linkname)
		}
	default:
		file, err := os.OpenFile(newPath, os.O_CREATE|os.O_WRONLY, fi.Mode()) //todo: windows
		if err != nil {
			return errors.Wrapf(err, "failed to create %s", newPath)
		}
		if dw.syncDataFunc != nil {
			if err := dw.syncDataFunc(dw.ctx, p, file); err != nil {
				return errors.Wrapf(err, "failed to write %s", newPath)
			}
			break
		} else if dw.asyncDataFunc != nil {
			asyncRequestFileData = true
		}
		if err := file.Close(); err != nil {
			return errors.Wrapf(err, "failed to close %s", newPath)
		}
	}

	if err := rewriteMetadata(newPath, stat); err != nil {
		return errors.Wrapf(err, "error setting metadata for %s", newPath)
	}

	if rename {
		if err := os.Rename(newPath, destPath); err != nil {
			return errors.Wrapf(err, "failed to rename %s to %s", newPath, destPath)
		}
	}

	if asyncRequestFileData {
		dw.requestAsyncFileData(p, destPath, stat)
	}

	return nil
}

func (dw *DiskWriter) requestAsyncFileData(p, dest string, stat *Stat) {
	dw.wg.Add(1)
	go func() {
		if err := dw.asyncDataFunc(dw.ctx, p, &lazyFileWriter{
			dest: dest,
		}); err != nil {
			dw.mu.Lock()
			if dw.err == nil {
				dw.err = err
				dw.cancel()
			}
			dw.mu.Unlock()
		}
		if err := chtimes(dest, stat.ModTime); err != nil { // TODO: check parent dirs
			dw.mu.Lock()
			if dw.err == nil {
				dw.err = err
				dw.cancel()
			}
			dw.mu.Unlock()
		}
		dw.wg.Done()
	}()
}

type lazyFileWriter struct {
	dest string
	ctx  context.Context
	f    *os.File
}

func (lfw *lazyFileWriter) Write(dt []byte) (int, error) {
	if lfw.f == nil {
		file, err := os.OpenFile(lfw.dest, os.O_WRONLY, 0) //todo: windows
		if err != nil {
			return 0, errors.Wrapf(err, "failed to open %s", lfw.dest)
		}
		lfw.f = file
	}
	return lfw.f.Write(dt)
}

func (lfw *lazyFileWriter) Close() error {
	if lfw.f != nil {
		return lfw.f.Close()
	}
	return nil
}

func rewriteMetadata(p string, stat *Stat) error {
	for key, value := range stat.Xattrs {
		sysx.Setxattr(p, key, value, 0)
	}

	if err := os.Lchown(p, int(stat.Uid), int(stat.Gid)); err != nil {
		return errors.Wrapf(err, "failed to lchown %s", p)
	}

	if os.FileMode(stat.Mode)&os.ModeSymlink == 0 {
		if err := os.Chmod(p, os.FileMode(stat.Mode)); err != nil {
			return errors.Wrapf(err, "failed to chown %s", p)
		}
	}

	if err := chtimes(p, stat.ModTime); err != nil {
		return errors.Wrapf(err, "failed to chtimes %s", p)
	}

	return nil
}

func chtimes(path string, un int64) error {
	var utimes [2]unix.Timespec
	utimes[0] = unix.NsecToTimespec(un)
	utimes[1] = utimes[0]

	if err := unix.UtimesNanoAt(unix.AT_FDCWD, path, utimes[0:], unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return errors.Wrap(err, "failed call to UtimesNanoAt")
	}

	return nil
}

// handleTarTypeBlockCharFifo is an OS-specific helper function used by
// createTarFile to handle the following types of header: Block; Char; Fifo
func handleTarTypeBlockCharFifo(path string, stat *Stat) error {
	mode := uint32(stat.Mode & 07777)
	if os.FileMode(stat.Mode)&os.ModeCharDevice != 0 {
		mode |= syscall.S_IFCHR
	} else if os.FileMode(stat.Mode)&os.ModeNamedPipe != 0 {
		mode |= syscall.S_IFIFO
	} else {
		mode |= syscall.S_IFBLK
	}

	if err := syscall.Mknod(path, mode, int(mkdev(stat.Devmajor, stat.Devminor))); err != nil {
		return err
	}
	return nil
}

func mkdev(major int64, minor int64) uint32 {
	return uint32(((minor & 0xfff00) << 12) | ((major & 0xfff) << 8) | (minor & 0xff))
}

// Random number state.
// We generate random temporary file names so that there's a good
// chance the file doesn't exist yet - keeps the number of tries in
// TempFile to a minimum.
var rand uint32
var randmu sync.Mutex

func reseed() uint32 {
	return uint32(time.Now().UnixNano() + int64(os.Getpid()))
}

func nextSuffix() string {
	randmu.Lock()
	r := rand
	if r == 0 {
		r = reseed()
	}
	r = r*1664525 + 1013904223 // constants from Numerical Recipes
	rand = r
	randmu.Unlock()
	return strconv.Itoa(int(1e9 + r%1e9))[1:]
}
