// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxfence

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type journalStore struct {
	directory *os.File
	lock      *os.File
}

func openJournal(directory string, create bool) (*journalStore, error) {
	return openJournalWithParentSync(directory, create, syncJournalParent)
}

func syncJournalParent(parentDirectory string) error {
	parent, err := os.Open(parentDirectory)
	if err != nil {
		return fmt.Errorf("open journal parent directory: %w", err)
	}
	return errors.Join(parent.Sync(), parent.Close())
}

func openJournalWithParentSync(directory string, create bool, syncParent func(string) error) (*journalStore, error) {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return nil, errors.New("journal directory must be a clean absolute path")
	}
	if syncParent == nil {
		return nil, errors.New("durable parent-directory synchronization is unavailable")
	}
	if create {
		if err := os.Mkdir(directory, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create private journal directory: %w", err)
		}
	}
	if err := syncParent(filepath.Dir(directory)); err != nil {
		return nil, fmt.Errorf("persist journal directory creation: %w", err)
	}
	fd, err := unix.Open(directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open private journal directory: %w", err)
	}
	root := os.NewFile(uintptr(fd), directory)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&0777 != 0700 || stat.Uid != uint32(os.Geteuid()) {
		root.Close()
		return nil, errors.New("journal directory must be owned by this user with mode 0700")
	}
	lockFD, err := unix.Openat(fd, "lock", unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		root.Close()
		return nil, fmt.Errorf("open journal lock: %w", err)
	}
	lock := os.NewFile(uintptr(lockFD), "lock")
	if err := unix.Fstat(lockFD, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG ||
		stat.Mode&0777 != 0600 || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) {
		lock.Close()
		root.Close()
		return nil, errors.New("journal lock is not a private regular file")
	}
	if err := unix.Flock(lockFD, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		root.Close()
		return nil, errors.New("journal is already in use; no concurrent fencing action is permitted")
	}
	return &journalStore{directory: root, lock: lock}, nil
}

func (s *journalStore) close() error {
	return errors.Join(s.lock.Close(), s.directory.Close())
}

func (s *journalStore) load() (*Journal, error) {
	fd, err := unix.Openat(int(s.directory.Fd()), "journal.json", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "journal.json")
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG ||
		stat.Mode&0777 != 0600 || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) || stat.Size > MaxJournalBytes {
		return nil, errors.New("journal must be a bounded private regular file owned by this user")
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxJournalBytes+1))
	if err != nil || len(data) > MaxJournalBytes {
		return nil, errors.New("journal could not be read within its size bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var journal Journal
	if err := decoder.Decode(&journal); err != nil {
		return nil, errors.New("journal contains invalid or unknown fields")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("journal contains trailing data")
	}
	if err := journal.validate(); err != nil {
		return nil, err
	}
	return &journal, nil
}

func (s *journalStore) save(journal *Journal) (returnErr error) {
	if err := journal.validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(journal, "", "  ")
	if err != nil || len(data)+1 > MaxJournalBytes {
		return errors.New("journal serialization exceeded its bound")
	}
	name := "journal-" + newRequestID() + ".tmp"
	fd, err := unix.Openat(int(s.directory.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return fmt.Errorf("create journal transaction: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	defer func() {
		if err := unix.Unlinkat(int(s.directory.Fd()), name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			returnErr = errors.Join(returnErr, fmt.Errorf("remove incomplete journal transaction: %w", err))
		}
	}()
	if _, err := file.Write(append(data, '\n')); err != nil {
		file.Close()
		return fmt.Errorf("write journal transaction: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync journal transaction: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close journal transaction: %w", err)
	}
	if err := unix.Renameat(int(s.directory.Fd()), name, int(s.directory.Fd()), "journal.json"); err != nil {
		return fmt.Errorf("replace journal: %w", err)
	}
	if err := s.directory.Sync(); err != nil {
		return fmt.Errorf("sync journal directory: %w", err)
	}
	return nil
}
