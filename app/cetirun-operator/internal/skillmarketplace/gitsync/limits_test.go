// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gitsync

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/skillmarketplace/skills"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// commitRepeatedTree commits a tree in which the directory skills holds
// width copies of one directory, each copy holds width copies of the next,
// depth levels deep, and the last level holds one file. Git stores each
// level once: the commit has depth+4 objects, and width^depth paths of the
// file.
func commitRepeatedTree(t *testing.T, dir string, width, depth int) {
	t.Helper()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	store := func(value interface {
		Encode(plumbing.EncodedObject) error
	}, kind plumbing.ObjectType) plumbing.Hash {
		t.Helper()
		encoded := repo.Storer.NewEncodedObject()
		encoded.SetType(kind)
		if err := value.Encode(encoded); err != nil {
			t.Fatal(err)
		}
		hash, err := repo.Storer.SetEncodedObject(encoded)
		if err != nil {
			t.Fatal(err)
		}
		return hash
	}
	blob := repo.Storer.NewEncodedObject()
	blob.SetType(plumbing.BlobObject)
	writer, err := blob.Writer()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = writer.Write([]byte("x"))
	_ = writer.Close()
	file, err := repo.Storer.SetEncodedObject(blob)
	if err != nil {
		t.Fatal(err)
	}
	level := store(&object.Tree{Entries: []object.TreeEntry{{Name: "f", Mode: filemode.Regular, Hash: file}}}, plumbing.TreeObject)
	for i := 0; i < depth; i++ {
		tree := &object.Tree{}
		for j := 0; j < width; j++ {
			tree.Entries = append(tree.Entries, object.TreeEntry{Name: fmt.Sprintf("d%d", j), Mode: filemode.Dir, Hash: level})
		}
		level = store(tree, plumbing.TreeObject)
	}
	root := store(&object.Tree{Entries: []object.TreeEntry{{Name: "skills", Mode: filemode.Dir, Hash: level}}}, plumbing.TreeObject)
	signature := object.Signature{Name: "test", Email: "test@example.invalid", When: time.Now()}
	commit := store(&object.Commit{Author: signature, Committer: signature, Message: "repeated", TreeHash: root}, plumbing.CommitObject)
	if err := repo.Storer.SetReference(plumbing.NewHashReference(plumbing.Master, commit)); err != nil {
		t.Fatal(err)
	}
}

// A commit can hold one directory at many paths. Nine levels of four copies
// are 13 objects, which the fetch limits accept, but 349,524 directories and
// 262,144 files below the repository path. Discover stops at the default
// tree limit, before it keeps more entries. Copies within the limits are
// read as usual.
func TestDiscoverStopsAtTheTreeLimitOfAnExpandedCommit(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		width, depth int
		message      string
	}{
		"expanded": {4, 9, "more than 100000 files and directories"},
		"small":    {2, 3, ""},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			commitRepeatedTree(t, dir, tc.width, tc.depth)
			source := Source{URL: dir, Auth: AuthNone}
			ref, _, err := source.Resolve(ctx)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := source.Fetch(ctx, ref, Limits{MaxBytes: 1 << 20, MaxObjects: 20})
			if err != nil {
				t.Fatalf("the pack holds each directory once: %v", err)
			}
			_, _, err = skills.Discover(snapshot, "skills", skills.DefaultLimits, nil)
			if tc.message == "" {
				if err != nil {
					t.Fatalf("a tree within the limits: %v", err)
				}
			} else if !errors.Is(err, ErrLimit) || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("discover: %v, want %q", err, tc.message)
			}
		})
	}
}

var testLimits = Limits{MaxBytes: 1 << 20, MaxObjects: 1000}

// packObject is one object of a crafted pack.
type packObject struct {
	kind plumbing.ObjectType
	// declared is the size in the object header.
	declared int64
	// base is the base object of a REF_DELTA.
	base plumbing.Hash
	// data is compressed into the pack.
	data []byte
}

// craftPack builds a version 2 pack, with the SHA-1 trailer.
func craftPack(t *testing.T, objects ...packObject) []byte {
	t.Helper()
	var pack bytes.Buffer
	pack.WriteString("PACK")
	_ = binary.Write(&pack, binary.BigEndian, uint32(2))
	_ = binary.Write(&pack, binary.BigEndian, uint32(len(objects)))
	for _, object := range objects {
		size := object.declared
		header := byte(object.kind)<<4 | byte(size&0x0f)
		for size >>= 4; size > 0; size >>= 7 {
			pack.WriteByte(header | 0x80)
			header = byte(size & 0x7f)
		}
		pack.WriteByte(header)
		if object.kind == plumbing.REFDeltaObject {
			pack.Write(object.base[:])
		}
		compressed := zlib.NewWriter(&pack)
		if _, err := compressed.Write(object.data); err != nil {
			t.Fatal(err)
		}
		if err := compressed.Close(); err != nil {
			t.Fatal(err)
		}
	}
	sum := sha1.Sum(pack.Bytes())
	pack.Write(sum[:])
	return pack.Bytes()
}

// delta is the start of a delta: the sizes of its base and of its result.
func delta(baseSize, targetSize uint64) []byte {
	return binary.AppendUvarint(binary.AppendUvarint(nil, baseSize), targetSize)
}

func TestCheckPackCountsTheObjectThatADeltaProduces(t *testing.T) {
	base := []byte("base content")
	baseObject := packObject{kind: plumbing.BlobObject, declared: int64(len(base)), data: base}
	baseHash := plumbing.ComputeHash(plumbing.BlobObject, base)
	// The delta is a few bytes; the object that it produces is 1 GiB.
	huge := delta(uint64(len(base)), 1<<30)
	pack := craftPack(t, baseObject, packObject{kind: plumbing.REFDeltaObject, declared: int64(len(huge)), base: baseHash, data: huge})
	if err := checkPack(bytes.NewReader(pack), testLimits); !errors.Is(err, ErrLimit) {
		t.Fatalf("a delta that produces more than the limit must be refused: %v", err)
	}
	small := delta(uint64(len(base)), 100)
	pack = craftPack(t, baseObject, packObject{kind: plumbing.REFDeltaObject, declared: int64(len(small)), base: baseHash, data: small})
	if err := checkPack(bytes.NewReader(pack), testLimits); err != nil {
		t.Fatalf("a small delta must pass: %v", err)
	}
}

func TestCheckPackRefusesAnObjectByItsHeader(t *testing.T) {
	// The header declares 1 GiB; the check refuses it before reading the object.
	pack := craftPack(t, packObject{kind: plumbing.BlobObject, declared: 1 << 30, data: []byte("x")})
	if err := checkPack(bytes.NewReader(pack), testLimits); !errors.Is(err, ErrLimit) {
		t.Fatalf("a declared size over the limit must be refused: %v", err)
	}
	// A delta that declares 1 GiB of its own, for a small object.
	base := []byte("base content")
	small := delta(uint64(len(base)), 100)
	pack = craftPack(t, packObject{kind: plumbing.BlobObject, declared: int64(len(base)), data: base},
		packObject{kind: plumbing.REFDeltaObject, declared: 1 << 30, base: plumbing.ComputeHash(plumbing.BlobObject, base), data: small})
	if err := checkPack(bytes.NewReader(pack), testLimits); !errors.Is(err, ErrLimit) {
		t.Fatalf("a delta that declares more than the limit must be refused: %v", err)
	}
}

func TestCheckPackAddsTheObjectsUp(t *testing.T) {
	part := make([]byte, 600<<10)
	pack := craftPack(t, packObject{kind: plumbing.BlobObject, declared: int64(len(part)), data: part},
		packObject{kind: plumbing.BlobObject, declared: int64(len(part)) - 1, data: part[1:]})
	if err := checkPack(bytes.NewReader(pack), testLimits); !errors.Is(err, ErrLimit) ||
		!strings.Contains(err.Error(), fmt.Sprintf("hold more than %d bytes", testLimits.MaxBytes)) {
		t.Fatalf("objects that add up to more than the limit must be refused: %v", err)
	}
	if err := checkPack(bytes.NewReader(pack), Limits{MaxBytes: 2 << 20, MaxObjects: 2}); err != nil {
		t.Fatalf("the same objects fit a larger limit: %v", err)
	}
}

func TestCheckPackRefusesTooManyObjects(t *testing.T) {
	var objects []packObject
	for i := 0; i < 3; i++ {
		data := []byte(fmt.Sprintf("object %d", i))
		objects = append(objects, packObject{kind: plumbing.BlobObject, declared: int64(len(data)), data: data})
	}
	pack := craftPack(t, objects...)
	if err := checkPack(bytes.NewReader(pack), Limits{MaxBytes: 1 << 20, MaxObjects: 2}); !errors.Is(err, ErrLimit) {
		t.Fatalf("more objects than the limit must be refused: %v", err)
	}
	if err := checkPack(bytes.NewReader(pack), Limits{MaxBytes: 1 << 20, MaxObjects: 3}); err != nil {
		t.Fatal(err)
	}
}

func TestCheckPackRefusesAnObjectLargerThanItsHeader(t *testing.T) {
	pack := craftPack(t, packObject{kind: plumbing.BlobObject, declared: 4, data: []byte("more than four bytes")})
	if err := checkPack(bytes.NewReader(pack), testLimits); err == nil {
		t.Fatal("an object that inflates past its declared size must be refused")
	}
}

func TestRemoveStalePacks(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{packPrefix + "123", packPrefix + "456", "other-file"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, packPrefix+"dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := RemoveStalePacks(dir); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, entry := range entries {
		left = append(left, entry.Name())
	}
	if strings.Join(left, ",") != packPrefix+"dir,other-file" {
		t.Fatalf("left %v", left)
	}
}

// The service refuses a commit that is too large without decoding it: the
// pack stops at the limit when it does not compress, and its object headers
// are checked when it does. The repository is served over HTTP, as a Git host
// serves it.
func TestFetchRefusesALargeCommitBeforeDecodingIt(t *testing.T) {
	decoded := 0
	saved := decodeHook
	decodeHook = func() { decoded++ }
	defer func() { decodeHook = saved }()
	random := make([]byte, 3<<20)
	rand.New(rand.NewSource(1)).Read(random)
	ctx := context.Background()
	for name, tc := range map[string]struct {
		content string
		limits  Limits
		message string
	}{
		"pack":    {string(random), testLimits, "the Git server sent more than 1048576 bytes"},
		"objects": {strings.Repeat("0", 3<<20), testLimits, "the objects of the commit hold more than 1048576 bytes"},
		"count":   {"small", Limits{MaxBytes: 1 << 20, MaxObjects: 3}, "more than 3"},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			commitFiles(t, filepath.Join(root, "skills"), map[string]string{
				"skills/big/SKILL.md": "---\nname: big\ndescription: Big\n---\n", "skills/big/data.bin": tc.content,
			})
			server, served := gitHTTPServer(t, root, false)
			source := Source{URL: server.URL + "/skills", Auth: AuthNone, AllowInsecureHTTP: true}
			ref, _, err := source.Resolve(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_, err = source.Fetch(ctx, ref, tc.limits)
			if !errors.Is(err, ErrLimit) || !strings.Contains(err.Error(), tc.message) || decoded != 0 || served.Load() != 1 {
				t.Fatalf("fetch: %v (decoded %d times, %d requests), want %q", err, decoded, served.Load(), tc.message)
			}
			// A larger limit loads the same commit.
			snapshot, err := source.Fetch(ctx, ref, Limits{MaxBytes: 8 << 20, MaxObjects: 100})
			if err != nil || decoded != 1 || snapshot.Commit == "" {
				t.Fatalf("fetch with a larger limit: %v (decoded %d times)", err, decoded)
			}
			decoded = 0
		})
	}
}
