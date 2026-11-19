// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package memory

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path"
	"sort"
	"syscall"
)

type assetFile struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
	Mode   uint32 `json:"mode"`
}
type assetManifest struct {
	ContractVersion string      `json:"contractVersion"`
	HelperABI       int         `json:"helperABI"`
	AssetRevision   string      `json:"assetRevision"`
	Files           []assetFile `json:"files"`
	AdapterVersion  string      `json:"adapterVersion"`
	ReadableSchemas []string    `json:"readableSchemas"`
	WritableSchemas []string    `json:"writableSchemas"`
	Profiles        []string    `json:"profiles"`
}

// InstallAssets writes only the compiled package into the dedicated init volume.
// It never opens HOME or executes material from the Agent image.
func InstallAssets(destination string, source fs.FS, executable []byte) error {
	files := map[string][]byte{"bin/cetirun-memory": executable}
	if err := fs.WalkDir(source, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		b, err := fs.ReadFile(source, name)
		if err != nil {
			return err
		}
		files[name] = b
		return nil
	}); err != nil {
		return err
	}
	if len(files) > 512 {
		return problem(5, "LimitReached")
	}
	total := 0
	names := []string{}
	for name, b := range files {
		if !relative(name) {
			return problem(2, "InvalidPath")
		}
		names = append(names, name)
		total += len(b)
	}
	if total > 32<<20 {
		return problem(5, "LimitReached")
	}
	sort.Strings(names)
	manifest := assetManifest{ContractVersion: ContractVersion, HelperABI: HelperABI, Files: []assetFile{}, AdapterVersion: AdapterVersion, ReadableSchemas: []string{SchemaVersion}, WritableSchemas: []string{SchemaVersion}, Profiles: []string{RetrievalProfile, ArtifactProfile}}
	for _, name := range names {
		mode := uint32(0444)
		if name == "bin/cetirun-memory" {
			mode = 0555
		}
		manifest.Files = append(manifest.Files, assetFile{name, digest(files[name]), mode})
	}
	body, _ := encode(manifest)
	manifest.AssetRevision = digest(body)
	body, _ = encode(manifest)
	root, err := openRoot(destination, false)
	if err != nil {
		return err
	}
	defer root.close()
	existing, _, err := root.inventory()
	if err != nil {
		return err
	}
	for _, file := range existing {
		if _, ok := files[file.Name]; !ok && file.Name != "manifest.json" {
			return problem(6, "AssetConflict")
		}
	}
	for _, file := range manifest.Files {
		if err := root.create(file.Path, files[file.Path]); err != nil {
			if !errors.Is(err, syscall.EEXIST) {
				return err
			}
			old, err := root.read(file.Path, 32<<20)
			if err != nil || !bytes.Equal(old, files[file.Path]) {
				return problem(6, "AssetConflict")
			}
		}
		f, err := root.open(file.Path, syscall.O_RDONLY, false)
		if err != nil {
			return err
		}
		err = f.Chmod(os.FileMode(file.Mode))
		f.Close()
		if err != nil {
			return err
		}
		for parent := path.Dir(file.Path); parent != "."; parent = path.Dir(parent) {
			dir, _, err := root.parent(parent+"/unused", false)
			if err != nil {
				return err
			}
			err = dir.Chmod(0755)
			dir.Close()
			if err != nil {
				return err
			}
		}
	}
	rootInfo, err := root.root.Stat()
	if err != nil {
		return err
	}
	// A Kubernetes emptyDir root is normally owned by root with fsGroup
	// access. Its non-root initializer need not, and often cannot, chmod it.
	if rootInfo.Mode().Perm()&0055 != 0055 {
		if err := root.root.Chmod(rootInfo.Mode().Perm() | 0055); err != nil {
			return err
		}
	}
	if err := root.create("manifest.json", body); err != nil {
		old, readErr := root.read("manifest.json", MaxManifest)
		if readErr != nil || !bytes.Equal(old, body) {
			return problem(6, "AssetConflict")
		}
	}
	f, err := root.open("manifest.json", syscall.O_RDONLY, false)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Chmod(0444)
}
