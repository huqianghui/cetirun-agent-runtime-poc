// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cetiadmin

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const exportTempSuffix = ".cetiadmin-tmp"

type exportFile struct {
	source     fs.FS
	sourceName string
	mode       fs.FileMode
	size       int64
	digest     [sha256.Size]byte
}

// ExportAssets exports packaged Markdown, scripts and Helm into an init volume.
// outputDir must be an absolute, clean directory path with an existing parent.
// The caller supplies the canonical assets and packaged Helm, and is the only
// writer to this volume. No environment, credentials or services are consulted.
func ExportAssets(outputDir string, assets fs.FS, helmPath string) error {
	if !filepath.IsAbs(outputDir) || filepath.Clean(outputDir) != outputDir || filepath.Dir(outputDir) == outputDir {
		return fmt.Errorf("export output must be an absolute, clean non-root directory")
	}
	if assets == nil {
		return fmt.Errorf("export assets are required")
	}
	files := map[string]exportFile{}
	directories := map[string]bool{".": true, "bin": true}
	if err := collectExportAssets(assets, ".", files, directories); err != nil {
		return err
	}
	helm, err := os.Lstat(helmPath)
	if err != nil {
		return fmt.Errorf("inspect packaged Helm: %w", err)
	}
	if !helm.Mode().IsRegular() {
		return fmt.Errorf("packaged Helm must be a regular file")
	}
	files["bin/helm"] = exportFile{
		source: os.DirFS(filepath.Dir(helmPath)), sourceName: filepath.Base(helmPath), mode: 0o755,
	}
	for name, file := range files {
		file.digest, file.size, err = exportDigest(file.source, file.sourceName, -1)
		if err != nil {
			return fmt.Errorf("read export source %q: %w", name, err)
		}
		files[name] = file
	}
	root, err := openExportRoot(outputDir)
	if err != nil {
		return err
	}
	defer root.Close()
	completed, pending, err := checkExportContents(root, files, directories)
	if err != nil {
		return err
	}
	for _, name := range pending {
		if err := root.Remove(name); err != nil {
			return fmt.Errorf("remove interrupted export %q: %w", name, err)
		}
	}
	info, err := root.Stat(".")
	if err != nil {
		return err
	}
	// A mounted emptyDir may be root-owned and accessible through the Pod fsGroup.
	// Fix a restrictive newly created root, but keep an accessible mount's mode.
	if info.Mode().Perm()&0o050 != 0o050 && info.Mode().Perm()&0o005 != 0o005 {
		if err := root.Chmod(".", 0o755); err != nil {
			return fmt.Errorf("make export root readable: %w", err)
		}
	}
	dirs := make([]string, 0, len(directories))
	for name := range directories {
		if name != "." {
			dirs = append(dirs, name)
		}
	}
	sort.Strings(dirs)
	for _, name := range dirs {
		if err := root.Mkdir(name, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("create export directory %q: %w", name, err)
		}
		if err := setExportMode(root, name, 0o755); err != nil {
			return err
		}
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		file := files[name]
		if completed[name] {
			err = setExportMode(root, name, file.mode)
		} else {
			err = writeExportFile(root, name, file)
		}
		if err != nil {
			return fmt.Errorf("export %q: %w", name, err)
		}
	}
	return nil
}

func collectExportAssets(assets fs.FS, directory string, files map[string]exportFile, directories map[string]bool) error {
	entries, err := fs.ReadDir(assets, directory)
	if err != nil {
		return fmt.Errorf("read asset directory %q: %w", directory, err)
	}
	for _, entry := range entries {
		base := entry.Name()
		if base == "." || !fs.ValidPath(base) || strings.ContainsAny(base, "/\\\x00") {
			return fmt.Errorf("invalid asset entry name %q", base)
		}
		name := path.Join(directory, base)
		top, _, _ := strings.Cut(name, "/")
		if (top != "skills" && top != "cron" && top != "scripts") || strings.HasPrefix(base, ".") || base == "tests" || base == "__pycache__" {
			return fmt.Errorf("unsupported asset path %q", name)
		}
		if _, exists := files[name]; exists || directories[name] {
			return fmt.Errorf("duplicate asset path %q", name)
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect asset %q: %w", name, err)
		}
		if info.Mode().Type() == fs.ModeDir {
			directories[name] = true
			if err := collectExportAssets(assets, name, files, directories); err != nil {
				return err
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("asset %q must be a regular file or directory", name)
		}
		mode := fs.FileMode(0o644)
		if top == "scripts" {
			if (path.Ext(base) != ".py" && path.Ext(base) != ".sh") || strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py") {
				return fmt.Errorf("unsupported script asset %q", name)
			}
			mode = 0o755
		} else if path.Ext(base) != ".md" {
			return fmt.Errorf("unsupported text asset %q", name)
		}
		files[name] = exportFile{source: assets, sourceName: name, mode: mode}
	}
	return nil
}

func exportDigest(source fs.FS, name string, limit int64) (digest [sha256.Size]byte, size int64, err error) {
	file, err := source.Open(name)
	if err != nil {
		return digest, 0, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return digest, 0, err
	}
	if !info.Mode().IsRegular() {
		return digest, 0, fmt.Errorf("%q must be a regular file", name)
	}
	var reader io.Reader = file
	if limit >= 0 {
		reader = io.LimitReader(file, limit)
	}
	hash := sha256.New()
	size, err = io.Copy(hash, reader)
	copy(digest[:], hash.Sum(nil))
	return digest, size, err
}

func openExportRoot(outputDir string) (*os.Root, error) {
	for name := outputDir; ; name = filepath.Dir(name) {
		info, err := os.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) && name == outputDir {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect export directory: %w", err)
		}
		if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
			return nil, fmt.Errorf("export path %q must be a directory without symlinks", name)
		}
		if filepath.Dir(name) == name {
			break
		}
	}
	if err := os.Mkdir(outputDir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, fmt.Errorf("create export root: %w", err)
	}
	return os.OpenRoot(outputDir)
}

func checkExportContents(root *os.Root, files map[string]exportFile, directories map[string]bool) (map[string]bool, []string, error) {
	completed := map[string]bool{}
	var pending []string
	err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && directories[name] {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("export destination %q is not a regular file", name)
		}
		file, known := files[name]
		partial := false
		if !known && strings.HasSuffix(name, exportTempSuffix) {
			file, known = files[strings.TrimSuffix(name, exportTempSuffix)]
			partial = known
		}
		if !known {
			return fmt.Errorf("foreign export destination %q", name)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() > file.size || (!partial && info.Size() != file.size) {
			return fmt.Errorf("conflicting export destination %q", name)
		}
		expected := file.digest
		if partial {
			var size int64
			expected, size, err = exportDigest(file.source, file.sourceName, info.Size())
			if err != nil {
				return err
			}
			if size != info.Size() {
				return fmt.Errorf("interrupted export source changed for %q", name)
			}
		}
		actual, _, err := exportDigest(root.FS(), name, -1)
		if err != nil {
			return err
		}
		if actual != expected {
			return fmt.Errorf("conflicting export destination %q", name)
		}
		if partial {
			pending = append(pending, name)
		} else {
			completed[name] = true
		}
		return nil
	})
	return completed, pending, err
}

func setExportMode(root *os.Root, name string, mode fs.FileMode) error {
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if info.Mode()&(fs.ModePerm|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) == mode {
		return nil
	}
	return root.Chmod(name, mode)
}

func writeExportFile(root *os.Root, name string, file exportFile) (err error) {
	source, err := file.source.Open(file.sourceName)
	if err != nil {
		return err
	}
	temporary := name + exportTempSuffix
	target, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.Join(err, source.Close())
	}
	defer func() { err = errors.Join(err, root.Remove(temporary)) }()
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(target, hash), io.LimitReader(source, file.size+1))
	err = errors.Join(err, source.Close())
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	if err == nil && (size != file.size || digest != file.digest) {
		err = fmt.Errorf("export source changed during copy")
	}
	if err == nil {
		err = target.Chmod(file.mode)
	}
	if err == nil {
		err = target.Sync()
	}
	err = errors.Join(err, target.Close())
	if err != nil {
		return err
	}
	// Linking publishes a complete file atomically and never replaces an entry.
	// A crash can leave the temporary name; restart accepts only a source prefix.
	return root.Link(temporary, name)
}
