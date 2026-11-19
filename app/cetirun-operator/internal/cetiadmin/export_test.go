// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cetiadmin

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
	"time"
)

var exportTestAssets = map[string]string{
	"skills/cetiadmin/SKILL.md":                              "# CetiAdmin\nSynthetic skill.\n",
	"skills/cetiadmin/references/crd-reference.md":           "# API reference\n",
	"skills/cetiadmin/references/instance-lifecycle.md":      "# Instance lifecycle\n",
	"skills/cetiadmin/references/operational-constraints.md": "# Operational constraints\n",
	"skills/cetiadmin/references/remediation-playbooks.md":   "# Remediation\n",
	"cron/cetirun-monitor-cron.md":                           "# Monitor\n",
	"scripts/cron_sync.py":                                   "#!/usr/bin/env python3\nprint('fixture')\n",
	"scripts/rotate_native_auth.py":                          "print('fixture')\n",
	"scripts/repair_helm_release.py":                         "print('fixture')\n",
}

const exportTestHelm = "#!/bin/sh\nprintf 'synthetic Helm\\n'\n"

func exportTestFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	assets := filepath.Join(root, "assets")
	for name, data := range exportTestAssets {
		exportTestWrite(t, filepath.Join(assets, filepath.FromSlash(name)), data, 0o600)
	}
	helm := filepath.Join(root, "packaged-helm")
	exportTestWrite(t, helm, exportTestHelm, 0o600)
	return assets, helm
}

func exportTestWrite(t *testing.T, name, data string, mode fs.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
}

func exportTestContents(t *testing.T, root string) map[string]string {
	t.Helper()
	contents := map[string]string{}
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(name)
			contents[filepath.ToSlash(relative)] = "symlink:" + target
			return err
		}
		data, err := os.ReadFile(name)
		contents[filepath.ToSlash(relative)] = string(data)
		return err
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return contents
}

func exportTestWant() map[string]string {
	want := map[string]string{"bin/helm": exportTestHelm}
	for name, data := range exportTestAssets {
		want[name] = data
	}
	return want
}

func TestExportAssetsLayoutAndPermissions(t *testing.T) {
	previous := syscall.Umask(0o077)
	defer syscall.Umask(previous)
	assets, helm := exportTestFixture(t)
	output := filepath.Join(t.TempDir(), "output")
	if err := os.Chmod(filepath.Join(assets, "scripts/cron_sync.py"), 0o777|fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if err := ExportAssets(output, os.DirFS(assets), helm); err != nil {
		t.Fatal(err)
	}
	if got := exportTestContents(t, output); !reflect.DeepEqual(got, exportTestWant()) {
		t.Fatalf("exported contents = %v", got)
	}
	err := filepath.WalkDir(output, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		want := fs.FileMode(0o644)
		if entry.IsDir() || strings.Contains(name, "/scripts/") || name == filepath.Join(output, "bin/helm") {
			want = 0o755
		}
		if got := info.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky); got != want {
			t.Errorf("%s mode = %v, want %v", name, got, want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(filepath.Join(output, "bin/helm"))
	command.Env = []string{"PATH=/usr/bin:/bin"}
	if data, err := command.CombinedOutput(); err != nil || string(data) != "synthetic Helm\n" {
		t.Fatalf("synthetic Helm: output %q, error %v", data, err)
	}
}

func TestExportAssetsRerunPreservesCompletedFiles(t *testing.T) {
	assets, helm := exportTestFixture(t)
	output := filepath.Join(t.TempDir(), "output")
	if err := ExportAssets(output, os.DirFS(assets), helm); err != nil {
		t.Fatal(err)
	}
	before := map[string]fs.FileInfo{}
	stamp := time.Unix(1234567890, 0)
	for name := range exportTestWant() {
		file := filepath.Join(output, filepath.FromSlash(name))
		if err := os.Chtimes(file, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		before[name] = info
	}
	if err := ExportAssets(output, os.DirFS(assets), helm); err != nil {
		t.Fatal(err)
	}
	for name, info := range before {
		after, err := os.Stat(filepath.Join(output, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(info, after) || !after.ModTime().Equal(info.ModTime()) {
			t.Errorf("matching completed file was replaced or rewritten: %s", name)
		}
	}
}

func TestExportAssetsPartialRestart(t *testing.T) {
	assets, helm := exportTestFixture(t)
	output := filepath.Join(t.TempDir(), "output")
	completed := filepath.Join(output, "skills/cetiadmin/SKILL.md")
	exportTestWrite(t, completed, exportTestAssets["skills/cetiadmin/SKILL.md"], 0o644)
	before, err := os.Stat(completed)
	if err != nil {
		t.Fatal(err)
	}
	if err := ExportAssets(output, os.DirFS(assets), helm); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(completed)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("partial restart replaced a completed file")
	}
	if got := exportTestContents(t, output); !reflect.DeepEqual(got, exportTestWant()) {
		t.Fatalf("partial restart contents = %v", got)
	}
}

func TestExportAssetsRecoversRecognizableTemporaryFiles(t *testing.T) {
	assets, helm := exportTestFixture(t)
	name := "skills/cetiadmin/SKILL.md"
	for _, stage := range []string{"empty", "partial", "complete", "published"} {
		t.Run(stage, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "output")
			data := exportTestAssets[name]
			if stage == "empty" {
				data = ""
			} else if stage == "partial" {
				data = data[:7]
			}
			temporary := filepath.Join(output, name+exportTempSuffix)
			exportTestWrite(t, temporary, data, 0o600)
			before, err := os.Stat(temporary)
			if err != nil {
				t.Fatal(err)
			}
			if stage == "published" {
				if err := os.Link(temporary, filepath.Join(output, name)); err != nil {
					t.Fatal(err)
				}
			}
			if err := ExportAssets(output, os.DirFS(assets), helm); err != nil {
				t.Fatal(err)
			}
			if got := exportTestContents(t, output); !reflect.DeepEqual(got, exportTestWant()) {
				t.Fatalf("restart contents = %v", got)
			}
			if stage == "published" {
				after, err := os.Stat(filepath.Join(output, name))
				if err != nil || !os.SameFile(before, after) {
					t.Fatalf("restart replaced published file: %v", err)
				}
			}
		})
	}
}

func TestExportAssetsRejectsForeignTemporaryFiles(t *testing.T) {
	assets, helm := exportTestFixture(t)
	for _, kind := range []string{"wrong-prefix", "oversized", "unknown", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "output")
			name := "skills/cetiadmin/SKILL.md" + exportTempSuffix
			data := "foreign"
			if kind == "unknown" {
				name = "unknown" + exportTempSuffix
				data = ""
			} else if kind == "oversized" {
				data = exportTestAssets["skills/cetiadmin/SKILL.md"] + "excess"
			}
			file := filepath.Join(output, name)
			if kind == "symlink" {
				if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(helm, file); err != nil {
					t.Fatal(err)
				}
			} else {
				exportTestWrite(t, file, data, 0o600)
			}
			before := exportTestContents(t, output)
			if err := ExportAssets(output, os.DirFS(assets), helm); err == nil {
				t.Fatal("accepted foreign temporary file")
			}
			if got := exportTestContents(t, output); !reflect.DeepEqual(got, before) {
				t.Fatalf("removed or changed conflicting temporary file: %v", got)
			}
		})
	}
}

func TestExportAssetsPreservesVolumeRootGroupMode(t *testing.T) {
	assets, helm := exportTestFixture(t)
	output := t.TempDir()
	mode := fs.FileMode(0o770 | fs.ModeSetgid)
	if err := os.Chmod(output, mode); err != nil {
		t.Fatal(err)
	}
	if err := ExportAssets(output, os.DirFS(assets), helm); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode() & (fs.ModePerm | fs.ModeSetgid); got != mode {
		t.Fatalf("volume root mode = %v, want %v", got, mode)
	}
}

func TestExportAssetsUnwritableRootDoesNotLeaveFiles(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires an unprivileged process to exercise directory permissions")
	}
	assets, helm := exportTestFixture(t)
	output := t.TempDir()
	if err := os.Chmod(output, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(output, 0o755); err != nil {
			t.Error(err)
		}
	})
	if err := ExportAssets(output, os.DirFS(assets), helm); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("unwritable root error = %v", err)
	}
	if got := exportTestContents(t, output); len(got) != 0 {
		t.Fatalf("unwritable root left files: %v", got)
	}
}

func TestExportAssetsRejectsConflictsBeforeChangingOutput(t *testing.T) {
	assets, helm := exportTestFixture(t)
	for _, name := range []string{
		"bin/helm", "skills/cetiadmin/SKILL.md", "unrelated", ".kube/config",
		"skills/cetiadmin/references/foreign.md", "skills",
	} {
		t.Run(name, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "output")
			exportTestWrite(t, filepath.Join(output, filepath.FromSlash(name)), "foreign contents", 0o600)
			before := exportTestContents(t, output)
			if err := ExportAssets(output, os.DirFS(assets), helm); err == nil {
				t.Fatal("accepted conflicting output")
			}
			if after := exportTestContents(t, output); !reflect.DeepEqual(after, before) {
				t.Fatalf("conflict changed existing output: %v", after)
			}
		})
	}
	for _, name := range []string{"bin/helm", "unknown-directory", "skills/cetiadmin/unknown-directory"} {
		t.Run("directory/"+name, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "output")
			if err := os.MkdirAll(filepath.Join(output, filepath.FromSlash(name)), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := ExportAssets(output, os.DirFS(assets), helm); err == nil {
				t.Fatal("accepted conflicting directory")
			}
			if got := exportTestContents(t, output); len(got) != 0 {
				t.Fatalf("conflict wrote files: %v", got)
			}
		})
	}
}

func TestExportAssetsRejectsOutputRootAndAncestorSymlinks(t *testing.T) {
	assets, helm := exportTestFixture(t)
	for _, child := range []string{"", "output", "../output"} {
		t.Run(child, func(t *testing.T) {
			root := t.TempDir()
			outside := t.TempDir()
			link := filepath.Join(root, "link")
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			output := link
			if child != "" {
				output += "/" + child
			}
			if err := ExportAssets(output, os.DirFS(assets), helm); err == nil {
				t.Fatal("accepted output symlink")
			}
			if got := exportTestContents(t, outside); len(got) != 0 {
				t.Fatalf("wrote outside destination: %v", got)
			}
			if _, err := os.Stat(filepath.Join(root, "output")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("unclean destination was used: %v", err)
			}
		})
	}
}

func TestExportAssetsRejectsChildSymlinks(t *testing.T) {
	assets, helm := exportTestFixture(t)
	for _, name := range []string{"skills", "skills/cetiadmin", "skills/cetiadmin/SKILL.md", "bin", "bin/helm"} {
		for _, targetKind := range []string{"outside", "inside", "dangling"} {
			t.Run(name+"/"+targetKind, func(t *testing.T) {
				output := filepath.Join(t.TempDir(), "output")
				outside := t.TempDir()
				exportTestWrite(t, filepath.Join(outside, "sentinel"), "keep", 0o600)
				file := filepath.Join(output, filepath.FromSlash(name))
				if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
					t.Fatal(err)
				}
				target := outside
				if targetKind == "inside" {
					target = output
				} else if targetKind == "dangling" {
					target = filepath.Join(outside, "missing")
				}
				if err := os.Symlink(target, file); err != nil {
					t.Fatal(err)
				}
				before := exportTestContents(t, output)
				if err := ExportAssets(output, os.DirFS(assets), helm); err == nil {
					t.Fatal("accepted destination symlink")
				}
				if got := exportTestContents(t, output); !reflect.DeepEqual(got, before) {
					t.Fatalf("changed symlinked output: %v", got)
				}
				if got := exportTestContents(t, outside); !reflect.DeepEqual(got, map[string]string{"sentinel": "keep"}) {
					t.Fatalf("changed outside files: %v", got)
				}
			})
		}
	}
}

type exportTestNamedEntry struct {
	fs.DirEntry
	name string
}

func (entry exportTestNamedEntry) Name() string { return entry.name }

type exportTestTraversalFS struct {
	fs.FS
	directory string
	name      string
}

func (assets exportTestTraversalFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(assets.FS, name)
	if err == nil && name == assets.directory {
		entries = append(entries, exportTestNamedEntry{DirEntry: entries[0], name: assets.name})
	}
	return entries, err
}

func TestExportAssetsRejectsTraversalBeforeJoiningSourcePaths(t *testing.T) {
	assets, helm := exportTestFixture(t)
	for _, directory := range []string{".", "skills/cetiadmin"} {
		for _, name := range []string{"..", ".", "../escape", "/escape", "a/../../escape", "nested/file", `..\escape`, "", "nul\x00name"} {
			t.Run(directory+"/"+name, func(t *testing.T) {
				output := filepath.Join(t.TempDir(), "output")
				invalid := exportTestTraversalFS{FS: os.DirFS(assets), directory: directory, name: name}
				if err := ExportAssets(output, invalid, helm); err == nil {
					t.Fatal("accepted invalid source entry name")
				}
				if _, err := os.Stat(output); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("invalid source created output: %v", err)
				}
			})
		}
	}
}

func TestExportAssetsRejectsNonAssetsAndSpecialSources(t *testing.T) {
	_, helm := exportTestFixture(t)
	for _, name := range []string{
		"export.go", "export_test.go", "manager", "bin/helm", ".kube/config",
		"skills/cetiadmin/config.go", "scripts/test_helper.py", "scripts/tests/helper.py",
		"scripts/token", "skills/cetiadmin/config.json",
	} {
		t.Run(name, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "output")
			assets := fstest.MapFS{name: &fstest.MapFile{Data: []byte("not an asset"), Mode: 0o600}}
			if err := ExportAssets(output, assets, helm); err == nil {
				t.Fatal("accepted non-asset source")
			}
			if _, err := os.Stat(output); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("invalid source created output: %v", err)
			}
		})
	}
	for _, mode := range []fs.FileMode{fs.ModeSymlink, fs.ModeNamedPipe, fs.ModeDevice, fs.ModeSocket} {
		t.Run(mode.String(), func(t *testing.T) {
			assets := fstest.MapFS{"scripts/helper.py": &fstest.MapFile{Mode: mode | 0o600}}
			if err := ExportAssets(filepath.Join(t.TempDir(), "output"), assets, helm); err == nil {
				t.Fatal("accepted special asset file")
			}
		})
	}
}

func TestExportAssetsRejectsInvalidInputs(t *testing.T) {
	assets, helm := exportTestFixture(t)
	for _, output := range []string{"", ".", "/", "relative/output"} {
		if err := ExportAssets(output, os.DirFS(assets), helm); err == nil {
			t.Errorf("accepted output %q", output)
		}
	}
	if err := ExportAssets(filepath.Join(t.TempDir(), "output"), nil, helm); err == nil {
		t.Error("accepted nil assets")
	}
	for _, kind := range []string{"missing", "directory", "symlink", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			invalid := filepath.Join(root, "helm")
			var err error
			switch kind {
			case "directory":
				err = os.Mkdir(invalid, 0o700)
			case "symlink":
				err = os.Symlink(helm, invalid)
			case "fifo":
				err = syscall.Mkfifo(invalid, 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(root, "output")
			if err := ExportAssets(output, os.DirFS(assets), invalid); err == nil {
				t.Fatal("accepted invalid Helm source")
			}
			if _, err := os.Stat(output); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("invalid Helm source created output: %v", err)
			}
		})
	}
}

type exportTestCopyFS struct {
	fs.FS
	opens int
	wrap  func(fs.File) fs.File
}

func (assets *exportTestCopyFS) Open(name string) (fs.File, error) {
	file, err := assets.FS.Open(name)
	if err == nil && name == "skills/cetiadmin/SKILL.md" {
		assets.opens++
		if assets.opens == 2 {
			return assets.wrap(file), nil
		}
	}
	return file, err
}

func (assets *exportTestCopyFS) ReadDir(name string) ([]fs.DirEntry, error) {
	return fs.ReadDir(assets.FS, name)
}

type exportTestInterruptedFile struct {
	fs.File
	read      bool
	interrupt func() error
}

func (file *exportTestInterruptedFile) Read(data []byte) (int, error) {
	if file.read {
		return 0, file.interrupt()
	}
	file.read = true
	return file.File.Read(data[:min(len(data), 7)])
}

func TestExportAssetsCopyFailureCleansPartialFile(t *testing.T) {
	assets, helm := exportTestFixture(t)
	output := filepath.Join(t.TempDir(), "output")
	failed := &exportTestCopyFS{
		FS: os.DirFS(assets),
		wrap: func(file fs.File) fs.File {
			return &exportTestInterruptedFile{File: file, interrupt: func() error {
				if _, err := os.Stat(filepath.Join(output, "skills/cetiadmin/SKILL.md")); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("incomplete file was published: %v", err)
				}
				return io.ErrUnexpectedEOF
			}}
		},
	}
	if err := ExportAssets(output, failed, helm); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("copy error = %v, want unexpected EOF", err)
	}
	for name, data := range exportTestContents(t, output) {
		if want, ok := exportTestWant()[name]; !ok || want != data {
			t.Errorf("failed copy left unfinished output: %s", name)
		}
	}
	if err := ExportAssets(output, os.DirFS(assets), helm); err != nil {
		t.Fatal(err)
	}
	if got := exportTestContents(t, output); !reflect.DeepEqual(got, exportTestWant()) {
		t.Fatalf("retry contents = %v", got)
	}
}

type exportTestCloseErrorFile struct{ fs.File }

func (file exportTestCloseErrorFile) Close() error {
	return errors.Join(file.File.Close(), io.ErrClosedPipe)
}

func TestExportAssetsSourceCloseFailureCleansUnpublishedFile(t *testing.T) {
	assets, helm := exportTestFixture(t)
	output := filepath.Join(t.TempDir(), "output")
	failed := &exportTestCopyFS{
		FS:   os.DirFS(assets),
		wrap: func(file fs.File) fs.File { return exportTestCloseErrorFile{File: file} },
	}
	if err := ExportAssets(output, failed, helm); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("source close error = %v", err)
	}
	for name, data := range exportTestContents(t, output) {
		if want, ok := exportTestWant()[name]; !ok || want != data || name == "skills/cetiadmin/SKILL.md" {
			t.Errorf("close failure left unpublished output: %s", name)
		}
	}
	if err := ExportAssets(output, os.DirFS(assets), helm); err != nil {
		t.Fatal(err)
	}
}

func TestExportAssetsAtomicPublicationDoesNotReplaceExistingFile(t *testing.T) {
	assets, helm := exportTestFixture(t)
	output := filepath.Join(t.TempDir(), "output")
	name := "skills/cetiadmin/SKILL.md"
	source := &exportTestCopyFS{
		FS: os.DirFS(assets),
		wrap: func(file fs.File) fs.File {
			exportTestWrite(t, filepath.Join(output, name), "foreign", 0o600)
			return file
		},
	}
	if err := ExportAssets(output, source, helm); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("publication conflict error = %v", err)
	}
	got := exportTestContents(t, output)
	if got[name] != "foreign" {
		t.Fatalf("publication overwrote existing file: %q", got[name])
	}
	for name, data := range got {
		if _, ok := exportTestWant()[name]; !ok || data == "" {
			t.Errorf("publication failure left temporary output: %s", name)
		}
	}
}

func TestExportAssetsInterruptedInitRestart(t *testing.T) {
	if os.Getenv("CETIADMIN_TEST_INTERRUPT") == "1" {
		assets := &exportTestCopyFS{
			FS: os.DirFS(os.Getenv("CETIADMIN_TEST_ASSETS")),
			wrap: func(file fs.File) fs.File {
				return &exportTestInterruptedFile{File: file, interrupt: func() error {
					os.Exit(23)
					return nil
				}}
			},
		}
		if err := ExportAssets(os.Getenv("CETIADMIN_TEST_OUTPUT"), assets, os.Getenv("CETIADMIN_TEST_HELM")); err != nil {
			t.Fatal(err)
		}
		t.Fatal("copy did not reach the interruption")
	}
	assets, helm := exportTestFixture(t)
	output := filepath.Join(t.TempDir(), "output")
	child := exec.Command(os.Args[0], "-test.run=^TestExportAssetsInterruptedInitRestart$")
	child.Env = []string{
		"CETIADMIN_TEST_INTERRUPT=1", "CETIADMIN_TEST_ASSETS=" + assets,
		"CETIADMIN_TEST_HELM=" + helm, "CETIADMIN_TEST_OUTPUT=" + output,
	}
	data, err := child.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatalf("interrupted init: error %v, output %q", err, data)
	}
	if _, err := os.Stat(filepath.Join(output, "skills/cetiadmin/SKILL.md")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("interruption published incomplete file: %v", err)
	}
	if err := ExportAssets(output, os.DirFS(assets), helm); err != nil {
		t.Fatalf("restart after abrupt exit: %v", err)
	}
	if got := exportTestContents(t, output); !reflect.DeepEqual(got, exportTestWant()) {
		t.Fatalf("restarted init contents = %v", got)
	}
}
