// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package gitsync reads one commit of a remote Git repository into memory.
// Nothing is checked out to disk and no repository content is executed.
package gitsync

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/skillmarketplace/skills"
	"github.com/go-git/go-billy/v5/osfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/idxfile"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-git/go-git/v5/storage/memory"
	"golang.org/x/crypto/ssh/knownhosts"
)

func init() {
	// Azure DevOps refuses an upload-pack request that does not ask for
	// multi_ack or multi_ack_detailed (HTTP 400). go-git leaves both out by
	// default, because its negotiation does not implement them for a fetch
	// into a repository that already has objects. Every fetch of this
	// package is a new clone into empty storage: it sends no "have" lines, so
	// these capabilities do not change the negotiation. Thin packs stay off.
	transport.UnsupportedCapabilities = []capability.Capability{capability.ThinPack}
}

// Auth selects how the repository is accessed.
type Auth string

const (
	AuthNone  Auth = "none"
	AuthToken Auth = "token"
	AuthSSH   Auth = "ssh"
)

// Source describes the repository and its credentials.
type Source struct {
	URL               string
	Ref               string
	Auth              Auth
	TokenUsername     string
	CredentialsDir    string
	AllowInsecureHTTP bool
}

var scpLike = regexp.MustCompile(`^([A-Za-z0-9._-]+)@([A-Za-z0-9.-]+):([^/].*)$`)

// Validate checks the URL and authentication before any network use. The
// worker logs the URL and lists it in the catalog, so the URL must not carry
// credentials, a query or a fragment.
func (s Source) Validate() error {
	switch s.Auth {
	case AuthNone, AuthToken:
		parsed, err := url.Parse(s.URL)
		if err != nil || parsed.Host == "" {
			return fmt.Errorf("the repository URL must be an absolute https:// URL")
		}
		if parsed.User != nil {
			return fmt.Errorf("the repository URL must not contain credentials; use the credentials Secret")
		}
		if parsed.Scheme != "https" && !(parsed.Scheme == "http" && s.AllowInsecureHTTP) {
			return fmt.Errorf("the repository URL must use https://")
		}
		if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(s.URL, "#") {
			return fmt.Errorf("the repository URL must not contain a query or a fragment; keep credentials in the credentials Secret")
		}
	case AuthSSH:
		if strings.HasPrefix(s.URL, "ssh://") {
			parsed, err := url.Parse(s.URL)
			if err != nil || parsed.Host == "" {
				return fmt.Errorf("invalid ssh:// repository URL")
			}
			if _, hasPassword := parsed.User.Password(); hasPassword {
				return fmt.Errorf("the repository URL must not contain a password")
			}
			if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(s.URL, "#") {
				return fmt.Errorf("the repository URL must not contain a query or a fragment")
			}
		} else if !scpLike.MatchString(s.URL) {
			return fmt.Errorf("SSH repositories need an ssh:// URL or the user@host:path form")
		}
	default:
		return fmt.Errorf("auth must be none, token or ssh")
	}
	if s.Ref != "" && (strings.ContainsAny(s.Ref, " ~^:?*[\\") || strings.Contains(s.Ref, "..")) {
		return fmt.Errorf("invalid ref %q", s.Ref)
	}
	return nil
}

// Redacted is the URL without user information, query or fragment, for logs
// and the catalog.
func Redacted(raw string) string {
	if match := scpLike.FindStringSubmatch(raw); match != nil {
		return match[2] + ":" + match[3]
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "<invalid URL>"
	}
	parsed.User, parsed.RawQuery, parsed.ForceQuery, parsed.Fragment, parsed.RawFragment = nil, "", false, "", ""
	return parsed.String()
}

func (s Source) auth() (transport.AuthMethod, error) {
	switch s.Auth {
	case AuthToken:
		token, err := readSecret(filepath.Join(s.CredentialsDir, "token"))
		if err != nil {
			return nil, err
		}
		username := s.TokenUsername
		if username == "" {
			username = "x-access-token"
		}
		return &githttp.BasicAuth{Username: username, Password: token}, nil
	case AuthSSH:
		key, err := os.ReadFile(filepath.Join(s.CredentialsDir, "ssh-privatekey"))
		if err != nil {
			return nil, fmt.Errorf("read the SSH private key: %w", err)
		}
		user := "git"
		if match := scpLike.FindStringSubmatch(s.URL); match != nil {
			user = match[1]
		} else if parsed, err := url.Parse(s.URL); err == nil && parsed.User != nil && parsed.User.Username() != "" {
			user = parsed.User.Username()
		}
		auth, err := gitssh.NewPublicKeys(user, key, "")
		if err != nil {
			return nil, fmt.Errorf("parse the SSH private key: %w", err)
		}
		callback, err := knownhosts.New(filepath.Join(s.CredentialsDir, "known_hosts"))
		if err != nil {
			return nil, fmt.Errorf("read known_hosts (required for SSH): %w", err)
		}
		auth.HostKeyCallback = callback
		return auth, nil
	}
	return nil, nil
}

func readSecret(name string) (string, error) {
	raw, err := os.ReadFile(name)
	if err != nil {
		return "", fmt.Errorf("read the repository token: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("the repository token is empty")
	}
	return token, nil
}

// peeledSuffix names the commit of an annotated tag in a ref advertisement.
const peeledSuffix = "^{}"

// Resolve returns the commit that the configured ref names on the remote.
// For an annotated tag, the remote advertises the tag object and, under the
// name with "^{}", its commit: Resolve returns the commit, which a fetch of
// the ref loads.
func (s Source) Resolve(ctx context.Context) (plumbing.ReferenceName, plumbing.Hash, error) {
	auth, err := s.auth()
	if err != nil {
		return "", plumbing.ZeroHash, err
	}
	remote := git.NewRemote(memory.NewStorage(), &config.RemoteConfig{Name: "origin", URLs: []string{s.URL}})
	refs, err := remote.ListContext(ctx, &git.ListOptions{Auth: auth, PeelingOption: git.AppendPeeled})
	if err != nil {
		return "", plumbing.ZeroHash, s.sanitize(err)
	}
	byName := map[plumbing.ReferenceName]*plumbing.Reference{}
	for _, ref := range refs {
		byName[ref.Name()] = ref
	}
	var candidates []plumbing.ReferenceName
	if s.Ref == "" {
		head, ok := byName[plumbing.HEAD]
		if !ok {
			return "", plumbing.ZeroHash, fmt.Errorf("the repository has no HEAD; set repository.ref")
		}
		if head.Type() == plumbing.SymbolicReference {
			candidates = []plumbing.ReferenceName{head.Target()}
		} else {
			for name, ref := range byName {
				if name.IsBranch() && ref.Hash() == head.Hash() {
					candidates = append(candidates, name)
				}
			}
			if len(candidates) == 0 {
				return "", plumbing.ZeroHash, fmt.Errorf("the repository HEAD is not a branch; set repository.ref")
			}
		}
	} else if strings.HasPrefix(s.Ref, "refs/") {
		candidates = []plumbing.ReferenceName{plumbing.ReferenceName(s.Ref)}
	} else {
		candidates = []plumbing.ReferenceName{plumbing.NewBranchReferenceName(s.Ref), plumbing.NewTagReferenceName(s.Ref)}
	}
	for _, name := range candidates {
		if ref, ok := byName[name]; ok {
			if peeled, ok := byName[name+peeledSuffix]; ok {
				return name, peeled.Hash(), nil
			}
			return name, ref.Hash(), nil
		}
	}
	return "", plumbing.ZeroHash, fmt.Errorf("the ref %q is not in the repository", s.Ref)
}

// Snapshot is one commit's tree, held in memory.
type Snapshot struct {
	// Commit is the hash of the commit.
	Commit string
	// Ref is the hash that the ref named when the fetch read it: the commit,
	// or the tag object of an annotated tag. The ref can move between Resolve
	// and Fetch, so only this hash names what the fetch read.
	Ref   string
	tree  *object.Tree
	store *memory.Storage
}

// Limits bound the memory that one fetch may use. The service checks them
// while the pack arrives and before it decodes any object, so a commit that
// is too large never reaches memory.
type Limits struct {
	// MaxBytes bounds the pack that the Git server sends, and the decoded
	// objects of the commit together.
	MaxBytes int64
	// MaxObjects bounds the number of objects in the pack.
	MaxObjects int64
	// TempDir holds the pack until it is checked and decoded. Empty uses the
	// default directory for temporary files.
	TempDir string
}

// ErrLimit reports a commit that exceeds the fetch limits. It is the error
// that skills.Discover reports for a tree that exceeds its limits too.
var ErrLimit = skills.ErrLimit

// Fetch reads the commit that ref names into memory (depth 1).
func (s Source) Fetch(ctx context.Context, ref plumbing.ReferenceName, limits Limits) (*Snapshot, error) {
	if limits.MaxBytes <= 0 || limits.MaxObjects <= 0 {
		return nil, fmt.Errorf("the fetch limits must be positive")
	}
	auth, err := s.auth()
	if err != nil {
		return nil, err
	}
	storage := &boundedStorage{Storage: memory.NewStorage(), limits: limits}
	repo, err := git.CloneContext(ctx, storage, nil, &git.CloneOptions{
		URL: s.URL, Auth: auth, ReferenceName: ref, SingleBranch: true, Depth: 1,
		Tags: git.NoTags, NoCheckout: true, RecurseSubmodules: git.NoRecurseSubmodules,
	})
	if err != nil {
		return nil, s.sanitize(err)
	}
	head, err := repo.Reference(ref, true)
	if err != nil {
		head, err = repo.Head()
		if err != nil {
			return nil, err
		}
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		// An annotated tag points at a tag object.
		tag, tagErr := repo.TagObject(head.Hash())
		if tagErr != nil {
			return nil, err
		}
		if commit, err = tag.Commit(); err != nil {
			return nil, err
		}
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, err
	}
	return &Snapshot{Commit: commit.Hash.String(), Ref: head.Hash().String(), tree: tree, store: storage.Storage}, nil
}

// boundedStorage keeps the objects of one fetch in memory. go-git hands the
// pack of a fetch to a storage that implements storer.PackfileWriter as a
// stream, before it decodes anything: the stream goes to a temporary file,
// which stops growing at the size limit, and the object headers of the
// complete pack are checked before the pack is decoded into memory.
type boundedStorage struct {
	*memory.Storage
	limits Limits
}

// packPrefix names the temporary pack files of fetches.
const packPrefix = "cetirun-skills-pack-"

// RemoveStalePacks removes the pack files that fetches of an earlier process
// left in dir, for example after the container ran out of memory. Call it
// before the first fetch.
func RemoveStalePacks(dir string) error {
	if dir == "" {
		dir = os.TempDir()
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasPrefix(entry.Name(), packPrefix) {
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// PackfileWriter implements storer.PackfileWriter.
func (s *boundedStorage) PackfileWriter() (io.WriteCloser, error) {
	file, err := os.CreateTemp(s.limits.TempDir, packPrefix)
	if err != nil {
		return nil, fmt.Errorf("create a temporary file for the pack: %w", err)
	}
	return &packWriter{file: file, storage: s}, nil
}

type packWriter struct {
	file    *os.File
	storage *boundedStorage
	written int64
	failed  error
}

func (w *packWriter) Write(data []byte) (int, error) {
	if w.failed != nil {
		return 0, w.failed
	}
	if int64(len(data)) > w.storage.limits.MaxBytes-w.written {
		w.failed = fmt.Errorf("%w: the Git server sent more than %d bytes for the commit", ErrLimit, w.storage.limits.MaxBytes)
		return 0, w.failed
	}
	n, err := w.file.Write(data)
	w.written += int64(n)
	if err != nil {
		w.failed = err
	}
	return n, err
}

// Close checks the complete pack and decodes it into the storage. go-git
// also calls Close after a failed transfer; then nothing is decoded.
func (w *packWriter) Close() error {
	defer func() {
		_ = w.file.Close()
		_ = os.Remove(w.file.Name())
	}()
	if w.failed != nil {
		return w.failed
	}
	if _, err := w.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := checkPack(w.file, w.storage.limits); err != nil {
		return err
	}
	if _, err := w.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	decodeHook()
	return loadPack(w.file, w.storage.Storage)
}

// decodeHook runs before a checked pack is decoded (tests replace it).
var decodeHook = func() {}

// streamedObjectSize is the size from which an object is read from the pack
// file as a stream when it is copied into memory.
const streamedObjectSize = 1 << 20

// deltaBaseCache bounds the objects that the copy keeps to resolve deltas,
// instead of go-git's default of 96 MiB.
const deltaBaseCache = 16 * cache.MiByte

// loadPack copies the objects of a checked pack into the storage. go-git's
// parser holds every object in memory, and grows its buffer of the largest
// object to twice its size: decoding into a storage needs about four times
// the largest object. Here the parser only indexes the pack, which needs the
// largest object once, and each object is then copied into a buffer of its
// exact size.
func loadPack(file *os.File, storage *memory.Storage) error {
	index := new(idxfile.Writer)
	// A seekable source: the parser reads deltas from the file, not memory.
	parser, err := packfile.NewParser(packfile.NewScanner(file), index)
	if err != nil {
		return err
	}
	if _, err := parser.Parse(); err != nil {
		return err
	}
	entries, err := index.Index()
	if err != nil {
		return err
	}
	fs := osfs.New(filepath.Dir(file.Name()))
	packFile, err := fs.Open(filepath.Base(file.Name()))
	if err != nil {
		return err
	}
	pack := packfile.NewPackfileWithCache(entries, fs, packFile, cache.NewObjectLRU(deltaBaseCache), streamedObjectSize)
	defer func() { _ = pack.Close() }()
	objects, err := pack.GetAll()
	if err != nil {
		return err
	}
	defer objects.Close()
	return objects.ForEach(func(object plumbing.EncodedObject) error {
		content := make([]byte, object.Size())
		reader, err := object.Reader()
		if err != nil {
			return err
		}
		_, err = io.ReadFull(reader, content)
		if closeErr := reader.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return fmt.Errorf("read object %s of the pack: %w", object.Hash(), err)
		}
		_, err = storage.SetEncodedObject(&exactObject{hash: object.Hash(), kind: object.Type(), content: content})
		return err
	})
}

// exactObject holds the content of an object in a slice of its exact size.
type exactObject struct {
	hash    plumbing.Hash
	kind    plumbing.ObjectType
	content []byte
}

func (o *exactObject) Hash() plumbing.Hash           { return o.hash }
func (o *exactObject) Type() plumbing.ObjectType     { return o.kind }
func (o *exactObject) SetType(t plumbing.ObjectType) { o.kind = t }
func (o *exactObject) Size() int64                   { return int64(len(o.content)) }
func (o *exactObject) SetSize(int64)                 {}
func (o *exactObject) Reader() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(o.content)), nil
}
func (o *exactObject) Writer() (io.WriteCloser, error) {
	return nil, errors.New("a fetched object cannot be changed")
}

// checkPack reads the object headers of a pack without keeping any object,
// and refuses a pack whose objects together are larger than the limits. The
// size of a delta object is the size of the object that it produces, which
// the start of the delta records.
func checkPack(pack io.Reader, limits Limits) error {
	scanner := packfile.NewScanner(pack)
	_, count, err := scanner.Header()
	if err != nil {
		return err
	}
	if int64(count) > limits.MaxObjects {
		return fmt.Errorf("%w: the commit has %d objects, more than %d", ErrLimit, count, limits.MaxObjects)
	}
	tooLarge := fmt.Errorf("%w: the objects of the commit hold more than %d bytes", ErrLimit, limits.MaxBytes)
	var total int64
	for i := uint32(0); i < count; i++ {
		header, err := scanner.NextObjectHeader()
		if err != nil {
			return err
		}
		size := header.Length
		if header.Type == plumbing.OFSDeltaObject || header.Type == plumbing.REFDeltaObject {
			// Decoding holds the delta itself too.
			if size < 0 || size > limits.MaxBytes {
				return tooLarge
			}
			prefix := &deltaPrefix{}
			if _, _, err := scanner.NextObject(prefix); err != nil {
				return err
			}
			if size, err = prefix.targetSize(); err != nil {
				return err
			}
		}
		if size < 0 || size > limits.MaxBytes-total {
			return tooLarge
		}
		total += size
	}
	_, err = scanner.Checksum()
	return err
}

// deltaPrefix keeps the start of a delta: the sizes of its base and of the
// object that it produces, each a little-endian base-128 number.
type deltaPrefix struct {
	data []byte
}

func (d *deltaPrefix) Write(p []byte) (int, error) {
	if room := 2*binary.MaxVarintLen64 - len(d.data); room > 0 {
		d.data = append(d.data, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

func (d *deltaPrefix) targetSize() (int64, error) {
	_, n := binary.Uvarint(d.data)
	if n <= 0 {
		return 0, fmt.Errorf("%w: a delta has an invalid base size", packfile.ErrMalformedPackFile)
	}
	target, m := binary.Uvarint(d.data[n:])
	if m <= 0 || target > math.MaxInt64 {
		return 0, fmt.Errorf("%w: a delta has an invalid target size", packfile.ErrMalformedPackFile)
	}
	return int64(target), nil
}

// walkTree visits every entry of tree, parents first. Unlike go-git's
// checkout walks, it does not stop at a name that is unsafe to write to a
// file system: the skills package rejects the skill that holds it, and the
// service writes no other entry.
func walkTree(tree *object.Tree, fn func(name string, entry object.TreeEntry) error) error {
	walker := object.NewTreeWalker(tree, true, nil)
	defer walker.Close()
	walker.SkipPathValidation()
	for {
		name, entry, err := walker.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := fn(name, entry); err != nil {
			return err
		}
	}
}

func (s Source) sanitize(err error) error {
	message := err.Error()
	if s.URL != "" {
		message = strings.ReplaceAll(message, s.URL, Redacted(s.URL))
	}
	if errors.Is(err, transport.ErrAuthenticationRequired) || errors.Is(err, transport.ErrAuthorizationFailed) {
		return fmt.Errorf("repository access was refused: check the credentials Secret (%s)", message)
	}
	return &sanitizedError{message: message, err: err}
}

// sanitizedError replaces the message of an error, and keeps the error for
// errors.Is.
type sanitizedError struct {
	message string
	err     error
}

func (e *sanitizedError) Error() string { return e.message }
func (e *sanitizedError) Unwrap() error { return e.err }

// Walk implements skills.Tree.
func (s *Snapshot) Walk(root string, fn func(skills.Entry) error) error {
	tree := s.tree
	root = strings.Trim(root, "/")
	if root != "" {
		sub, err := s.tree.Tree(root)
		if err != nil {
			return fmt.Errorf("the repository path %q does not exist in the commit", root)
		}
		tree = sub
	}
	return walkTree(tree, func(name string, entry object.TreeEntry) error {
		full := name
		if root != "" {
			full = path.Join(root, name)
		}
		item := skills.Entry{Path: full}
		switch entry.Mode {
		case filemode.Dir:
			item.Mode = skills.Dir
		case filemode.Symlink:
			item.Mode = skills.Symlink
		case filemode.Submodule:
			item.Mode = skills.Submodule
		case filemode.Executable:
			item.Mode = skills.Executable
		default:
			item.Mode = skills.Regular
		}
		if item.Mode == skills.Regular || item.Mode == skills.Executable {
			blob, err := object.GetBlob(s.store, entry.Hash)
			if err != nil {
				return err
			}
			item.Size = blob.Size
			item.Open = func() (io.ReadCloser, error) { return blob.Reader() }
		}
		return fn(item)
	})
}

// TreeHash implements skills.Tree.
func (s *Snapshot) TreeHash(dir string) (string, error) {
	dir = strings.Trim(dir, "/")
	if dir == "" {
		return s.tree.Hash.String(), nil
	}
	sub, err := s.tree.Tree(dir)
	if err != nil {
		return "", err
	}
	return sub.Hash.String(), nil
}
