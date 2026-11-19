// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package memory

import (
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

type safeFS struct {
	root   *os.File
	fault  func(string) error
	unlock func()
}

func openRoot(name string, create bool) (*safeFS, error) {
	if !filepath.IsAbs(name) || filepath.Clean(name) != name || name == "/" {
		return nil, problem(6, "RootInvalid")
	}
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, component := range strings.Split(strings.TrimPrefix(name, "/"), "/") {
		if create {
			err = syscall.Mkdirat(fd, component, 0700)
			if err == nil {
				err = syscall.Fsync(fd)
			}
			if err != nil && !errors.Is(err, syscall.EEXIST) {
				syscall.Close(fd)
				return nil, err
			}
		}
		next, err := syscall.Openat(fd, component, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		syscall.Close(fd)
		if err != nil {
			return nil, err
		}
		fd = next
	}
	return &safeFS{root: os.NewFile(uintptr(fd), name)}, nil
}

func (s *safeFS) close() error {
	if s.unlock != nil {
		s.unlock()
		s.unlock = nil
	}
	return s.root.Close()
}
func relative(name string) bool {
	return name != "" && !strings.ContainsAny(name, "\\\x00") && !strings.HasPrefix(name, "/") && name != ".." && !strings.HasPrefix(name, "../") && path.Clean(name) == name
}

func (s *safeFS) parent(name string, create bool) (*os.File, string, error) {
	if !relative(name) {
		return nil, "", problem(2, "InvalidPath")
	}
	fd, err := syscall.Dup(int(s.root.Fd()))
	if err != nil {
		return nil, "", err
	}
	parts := strings.Split(name, "/")
	for _, part := range parts[:len(parts)-1] {
		if create {
			err = syscall.Mkdirat(fd, part, 0700)
			if err == nil {
				err = syscall.Fsync(fd)
			}
			if err != nil && !errors.Is(err, syscall.EEXIST) {
				syscall.Close(fd)
				return nil, "", err
			}
		}
		next, err := syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		syscall.Close(fd)
		if err != nil {
			return nil, "", err
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), path.Dir(name)), parts[len(parts)-1], nil
}

func (s *safeFS) open(name string, flags int, createParents bool) (*os.File, error) {
	dir, base, err := s.parent(name, createParents)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	fd, err := syscall.Openat(int(dir.Fd()), base, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || stat.Nlink != 1 {
		f.Close()
		return nil, problem(6, "UnsafeFile")
	}
	return f, nil
}

func (s *safeFS) read(name string, limit int) ([]byte, error) {
	f, err := s.open(name, syscall.O_RDONLY, false)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, problem(5, "LimitReached")
	}
	return data, nil
}

func (s *safeFS) create(name string, data []byte) error {
	f, err := s.open(name, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL, true)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	dir, _, err := s.parent(name, false)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *safeFS) atomic(name string, data []byte) error {
	temp := path.Join(path.Dir(name), ".tmp-"+newID("tx"))
	if err := s.create(temp, data); err != nil {
		if !errors.Is(err, syscall.EEXIST) {
			_ = s.remove(temp)
		}
		return err
	}
	defer s.remove(temp)
	if s.fault != nil {
		if err := s.fault("atomic-temp"); err != nil {
			return err
		}
	}
	source, sourceName, err := s.parent(temp, false)
	if err != nil {
		return err
	}
	defer source.Close()
	target, targetName, err := s.parent(name, false)
	if err != nil {
		return err
	}
	defer target.Close()
	if err := syscall.Renameat(int(source.Fd()), sourceName, int(target.Fd()), targetName); err != nil {
		return err
	}
	return target.Sync()
}

func (s *safeFS) remove(name string) error {
	dir, base, err := s.parent(name, false)
	if errors.Is(err, syscall.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer dir.Close()
	f, err := s.open(name, syscall.O_RDONLY, false)
	if errors.Is(err, syscall.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	f.Close()
	if err := syscall.Unlinkat(int(dir.Fd()), base); err != nil && !errors.Is(err, syscall.ENOENT) {
		return err
	}
	return dir.Sync()
}

type diskFile struct {
	Name string
	Size int64
}

// names lists up to 1024 entries of a directory below the root.
func (s *safeFS) names(name string) ([]string, error) {
	dir, base, err := s.parent(name, false)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	fd, err := syscall.Openat(int(dir.Fd()), base, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	names, err := f.Readdirnames(1024)
	if errors.Is(err, io.EOF) {
		return names, nil
	}
	return names, err
}

func (s *safeFS) removeDirectory(name string) error {
	dir, base, err := s.parent(name, false)
	if err != nil {
		return err
	}
	defer dir.Close()
	pointer, err := syscall.BytePtrFromString(base)
	if err != nil {
		return err
	}
	const atRemoveDir = 0x200
	_, _, errno := syscall.Syscall(syscall.SYS_UNLINKAT, dir.Fd(), uintptr(unsafe.Pointer(pointer)), atRemoveDir)
	if errno != 0 && errno != syscall.ENOENT && errno != syscall.ENOTEMPTY {
		return errno
	}
	return dir.Sync()
}

func (s *safeFS) inventory() ([]diskFile, int64, error) {
	files := []diskFile{}
	var size int64
	entries := 0
	var walk func(*os.File, string, int) error
	walk = func(dir *os.File, prefix string, depth int) error {
		if depth > 12 {
			return problem(5, "LimitReached")
		}
		for {
			batch, err := dir.ReadDir(256)
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			for _, entry := range batch {
				entries++
				if entries > 16384 {
					return problem(5, "LimitReached")
				}
				name := path.Join(prefix, entry.Name())
				if entry.IsDir() {
					fd, err := syscall.Openat(int(dir.Fd()), entry.Name(), syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
					if err != nil {
						return err
					}
					sub := os.NewFile(uintptr(fd), name)
					err = walk(sub, name, depth+1)
					sub.Close()
					if err != nil {
						return err
					}
				} else {
					f, err := s.open(name, syscall.O_RDONLY, false)
					if err != nil {
						return err
					}
					info, err := f.Stat()
					f.Close()
					if err != nil {
						return err
					}
					files = append(files, diskFile{name, info.Size()})
					size += info.Size()
					if size > MaxRootBytes {
						return problem(5, "LimitReached")
					}
				}
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
		}
	}
	fd, err := syscall.Openat(int(s.root.Fd()), ".", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, 0, err
	}
	dir := os.NewFile(uintptr(fd), ".")
	defer dir.Close()
	err = walk(dir, "", 0)
	return files, size, err
}

func (s *safeFS) lock(ctx context.Context) (func(), error) {
	return s.lockMode(ctx, syscall.LOCK_EX)
}

func (s *safeFS) lockMode(ctx context.Context, mode int) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	flags := syscall.O_RDONLY
	if mode == syscall.LOCK_EX {
		flags = syscall.O_RDWR | syscall.O_CREAT
	}
	f, err := s.open("state/write.lock", flags, mode == syscall.LOCK_EX)
	if err != nil {
		return nil, err
	}
	return lockFile(ctx, f, mode)
}

func (s *safeFS) lockRoot(ctx context.Context) (func(), error) {
	fd, err := syscall.Openat(int(s.root.Fd()), ".", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return lockFile(ctx, os.NewFile(uintptr(fd), "."), syscall.LOCK_EX)
}

func lockFile(ctx context.Context, f *os.File, operation int) (func(), error) {
	if err := ctx.Err(); err != nil {
		f.Close()
		return nil, err
	}
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		err := syscall.Flock(int(f.Fd()), operation|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-deadline.C:
			f.Close()
			return nil, problem(5, "LockBusy")
		case <-tick.C:
		}
	}
}
