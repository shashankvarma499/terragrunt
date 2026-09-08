package cas

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/gruntwork-io/terragrunt/internal/venv"
	"github.com/gruntwork-io/terragrunt/internal/vfs"
	"github.com/gruntwork-io/terragrunt/pkg/log"
)

const (
	// DefaultDirPerms represents standard directory permissions (rwxr-xr-x).
	DefaultDirPerms = os.FileMode(0755)
	// StoredFilePerms represents read-only file permissions (r--r--r--).
	StoredFilePerms = os.FileMode(0444)
	// RegularFilePerms represents standard file permissions (rw-r--r--).
	RegularFilePerms = os.FileMode(0644)
	// WriteBitMask covers all owner/group/other write bits.
	WriteBitMask = os.FileMode(0o222)
	// WindowsOS is the name of the Windows operating system.
	WindowsOS = "windows"
)

// opWriteTarget names the operation in the errors every path that writes a
// materialized file reports, so a caller reading one cannot tell which mode
// produced it apart from the path it names.
const opWriteTarget = "write_target"

// Content manages git object storage and linking.
type Content struct {
	store *Store
}

// NewContent creates a new Content instance bound to store.
func NewContent(store *Store) *Content {
	return &Content{store: store}
}

// LinkOption configures a single [Content.Link] call.
type LinkOption func(*linkOpts)

type linkOpts struct {
	mode      LinkMode
	forceCopy bool
	skipClone bool
}

// WithFileLinkMode selects how the stored blob reaches the target path.
// Without it, Link uses [DefaultLinkMode].
func WithFileLinkMode(mode LinkMode) LinkOption {
	return func(o *linkOpts) { o.mode = mode }
}

// WithLinkForceCopy tells Link the destination is going to be edited, so it
// must not share the stored blob's file. [LinkModeHardlink] is served by
// [LinkModeClone], which falls back to a copy where the filesystem has no
// copy-on-write clone.
func WithLinkForceCopy() LinkOption {
	return func(o *linkOpts) { o.forceCopy = true }
}

// WithoutCloneAttempt tells Link the filesystem holding the target has
// already refused a copy-on-write clone, so [LinkModeClone] takes the
// fallback it would have reached anyway without paying for the syscall that
// says so. It changes what a clone costs, never what it leaves behind.
func WithoutCloneAttempt() LinkOption {
	return func(o *linkOpts) { o.skipClone = true }
}

// LinkOutcome reports how one blob reached its destination.
type LinkOutcome struct {
	// BytesCopied is the size of the file written when Mode is
	// [LinkModeCopy], and zero under every other mode.
	BytesCopied int64
	// Mode is the mode that produced the file, which is not the requested
	// mode when the filesystem could not honour that one.
	Mode LinkMode
}

// Link materializes a stored blob at targetPath under gitPerm and reports how
// it got there.
//
// The mode chosen with [WithFileLinkMode] decides how much work that takes
// and what the destination is allowed to be: see [LinkMode] for what each one
// promises. A mode the filesystem cannot serve degrades rather than failing,
// so every mode ends at a copy in the worst case.
func (c *Content) Link(
	v *venv.Venv,
	hash, targetPath string,
	gitPerm os.FileMode,
	opts ...LinkOption,
) (LinkOutcome, error) {
	o := linkOpts{mode: DefaultLinkMode}
	for _, opt := range opts {
		opt(&o)
	}

	mode := resolveLinkMode(o.mode, o.forceCopy)

	sourcePath := c.getPath(hash)
	perm := destPerm(mode, gitPerm)

	switch mode {
	case LinkModeHardlink:
		return c.hardlinkBlob(v, sourcePath, targetPath, perm)
	case LinkModeClone:
		return c.cloneBlob(v, sourcePath, targetPath, perm, o)
	case LinkModeCopy:
		return c.copyBlob(v, sourcePath, targetPath, perm)
	}

	return LinkOutcome{}, &InvalidLinkModeError{Value: mode.String()}
}

// Store stores a single content item. This is typically used for trees,
// as blobs are written directly from git cat-file stdout.
func (c *Content) Store(l log.Logger, v *venv.Venv, hash string, data []byte) error {
	unlock := c.store.Lock(hash)
	defer unlock()

	if err := v.FS.MkdirAll(c.store.Path(), DefaultDirPerms); err != nil {
		return fmt.Errorf("create store dir %s: %w", c.store.Path(), ErrCreateDir)
	}

	partitionDir := c.getPartition(hash)
	if err := v.FS.MkdirAll(partitionDir, DefaultDirPerms); err != nil {
		return fmt.Errorf("create partition dir %s: %w", partitionDir, ErrCreateDir)
	}

	return c.writeContentToFile(l, v, hash, data)
}

// Ensure ensures that a content item exists in the store.
func (c *Content) Ensure(l log.Logger, v *venv.Venv, hash string, data []byte) error {
	path := c.getPath(hash)
	if c.store.hasContent(v, path) {
		return nil
	}

	return c.Store(l, v, hash, data)
}

// EnsureWithWait ensures that a content item exists in the store, with optimization
// to wait for concurrent writes instead of doing redundant work.
func (c *Content) EnsureWithWait(l log.Logger, v *venv.Venv, hash string, data []byte) error {
	needsWrite, unlock := c.store.EnsureWithWait(v, hash)
	defer unlock()

	if !needsWrite {
		return nil
	}

	if err := v.FS.MkdirAll(c.store.Path(), DefaultDirPerms); err != nil {
		return fmt.Errorf("create store dir %s: %w", c.store.Path(), ErrCreateDir)
	}

	partitionDir := c.getPartition(hash)
	if err := v.FS.MkdirAll(partitionDir, DefaultDirPerms); err != nil {
		return fmt.Errorf("create partition dir %s: %w", partitionDir, ErrCreateDir)
	}

	return c.writeContentToFile(l, v, hash, data)
}

// EnsureCopy ensures that a content item exists in the store by copying from a file.
// The stored blob is chmodded to the source file's perms with the write bits cleared,
// so the default-link path can hardlink the blob directly without losing its
// executable-ness or risking writes back into the shared store.
func (c *Content) EnsureCopy(l log.Logger, v *venv.Venv, hash, src string) (err error) {
	path := c.getPath(hash)
	if c.store.hasContent(v, path) {
		return nil
	}

	srcInfo, err := v.FS.Stat(src)
	if err != nil {
		return fmt.Errorf("stat source %s: %w", src, err)
	}

	unlock := c.store.Lock(hash)
	defer unlock()

	// Re-check under the lock: another worker may have raced ahead and
	// stored the blob between the lock-free hasContent check and Lock,
	// and skipping the copy is the point of waiting.
	if c.store.hasContent(v, path) {
		return nil
	}

	partitionDir := c.getPartition(hash)
	if err = v.FS.MkdirAll(partitionDir, DefaultDirPerms); err != nil {
		return fmt.Errorf("create partition dir %s: %w", partitionDir, ErrCreateDir)
	}

	f, err := c.createTemp(v, hash)
	if err != nil {
		return err
	}

	tempPath := f.Name()

	// renamed flips after the publish step so the deferred cleanup
	// removes a stale tempPath only on the error path.
	renamed := false

	defer func() {
		if renamed {
			return
		}

		if rmErr := v.FS.Remove(tempPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			err = errors.Join(err, rmErr)
		}
	}()

	r, err := v.FS.Open(src)
	if err != nil {
		err = errors.Join(err, f.Close())
		return fmt.Errorf("open source %s: %w", src, err)
	}

	defer func() {
		err = errors.Join(err, r.Close())
	}()

	if _, err := io.Copy(f, r); err != nil {
		closeErr := f.Close()
		return fmt.Errorf("copy from %s: %w", src, errors.Join(err, closeErr))
	}

	// Close the writer before rename so platforms that disallow
	// renaming an open file (Windows) can complete the publish.
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tempPath, err)
	}

	if err := v.FS.Chmod(tempPath, srcInfo.Mode().Perm()&^WriteBitMask); err != nil {
		return fmt.Errorf("chmod %s: %w", tempPath, err)
	}

	if err := c.publish(v, tempPath, hash); err != nil {
		return fmt.Errorf("finalize %s: %w", path, err)
	}

	renamed = true

	return nil
}

// GetTmpHandle returns a handle to a fresh temporary file in the
// partition of hash, for callers that stream the object's content
// themselves before renaming it into place.
func (c *Content) GetTmpHandle(v *venv.Venv, hash string) (vfs.File, error) {
	partitionDir := c.getPartition(hash)
	if err := v.FS.MkdirAll(partitionDir, DefaultDirPerms); err != nil {
		return nil, fmt.Errorf("create partition dir %s: %w", partitionDir, ErrCreateDir)
	}

	return c.createTemp(v, hash)
}

// Read retrieves content from the store by hash.
func (c *Content) Read(v *venv.Venv, hash string) ([]byte, error) {
	path := c.getPath(hash)
	return vfs.ReadFile(v.FS, path)
}

// hardlinkBlob gives targetPath a second name for the stored blob.
//
// The link is only made when the stored blob already carries perm: the two
// names share one inode, so a chmod on the destination would change the
// store, and a destination that has to end up under a different mode is
// copied instead.
func (c *Content) hardlinkBlob(
	v *venv.Venv,
	sourcePath, targetPath string,
	perm os.FileMode,
) (LinkOutcome, error) {
	if c.storedPermIs(v, sourcePath, perm) {
		if err := vfs.Link(v.FS, sourcePath, targetPath); err == nil {
			return LinkOutcome{Mode: LinkModeHardlink}, nil
		}
		// A failed link leaves the copy below to produce the file. An
		// existing targetPath is one way it fails, and the temp-file rename
		// there replaces stale content atomically.
	}

	return c.copyBlob(v, sourcePath, targetPath, perm)
}

// cloneBlob makes targetPath a copy-on-write clone of the stored blob.
//
// A filesystem with no reflink support falls back to a hard link. That link
// is the store's own file, so it arrives without its write bits and only
// serves a destination nobody is going to edit whose stored blob already
// carries the permissions to hand out. Everything else is copied.
func (c *Content) cloneBlob(
	v *venv.Venv,
	sourcePath, targetPath string,
	perm os.FileMode,
	o linkOpts,
) (LinkOutcome, error) {
	if !o.skipClone {
		err := c.cloneInto(v, sourcePath, targetPath, perm)
		if err == nil {
			return LinkOutcome{Mode: LinkModeClone}, nil
		}

		if !errors.Is(err, vfs.ErrNoCloneFile) {
			return LinkOutcome{}, err
		}
	}

	if !o.forceCopy {
		linkPerm := perm &^ WriteBitMask
		if c.storedPermIs(v, sourcePath, linkPerm) {
			if err := vfs.Link(v.FS, sourcePath, targetPath); err == nil {
				return LinkOutcome{Mode: LinkModeHardlink}, nil
			}
		}
	}

	return c.copyBlob(v, sourcePath, targetPath, perm)
}

// copyBlob writes an independent copy of the stored blob at targetPath under
// perm.
func (c *Content) copyBlob(
	v *venv.Venv,
	sourcePath, targetPath string,
	perm os.FileMode,
) (LinkOutcome, error) {
	data, readErr := vfs.ReadFile(v.FS, sourcePath)
	if readErr != nil {
		return LinkOutcome{}, &WrappedError{
			Op:   "read_source",
			Path: sourcePath,
			Err:  ErrReadFile,
		}
	}

	targetDir := filepath.Dir(targetPath)
	if err := v.FS.MkdirAll(targetDir, DefaultDirPerms); err != nil {
		return LinkOutcome{}, &WrappedError{
			Op:   opWriteTarget,
			Path: targetDir,
			Err:  err,
		}
	}

	// A unique, freshly-writable temp avoids a fixed "<target>.tmp":
	// reopening that name fails with EACCES once a prior write (an
	// interrupted run, or a concurrent Link to the same target) left it at
	// the read-only `perm` mode.
	tmp, err := vfs.CreateTemp(v.FS, targetDir, filepath.Base(targetPath)+".tmp")
	if err != nil {
		return LinkOutcome{}, &WrappedError{
			Op:   opWriteTarget,
			Path: targetDir,
			Err:  err,
		}
	}

	tempPath := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		return LinkOutcome{}, &WrappedError{
			Op:   opWriteTarget,
			Path: tempPath,
			Err:  errors.Join(err, tmp.Close(), v.FS.Remove(tempPath)),
		}
	}

	if err := tmp.Close(); err != nil {
		return LinkOutcome{}, &WrappedError{
			Op:   opWriteTarget,
			Path: tempPath,
			Err:  errors.Join(err, v.FS.Remove(tempPath)),
		}
	}

	// CreateTemp opens at 0o600, so set the requested mode before publishing.
	if err := v.FS.Chmod(tempPath, perm); err != nil {
		return LinkOutcome{}, &WrappedError{
			Op:   "chmod_target",
			Path: tempPath,
			Err:  errors.Join(err, v.FS.Remove(tempPath)),
		}
	}

	if err := v.FS.Rename(tempPath, targetPath); err != nil {
		return LinkOutcome{}, &WrappedError{
			Op:   "rename_target",
			Path: tempPath,
			Err:  errors.Join(err, v.FS.Remove(tempPath)),
		}
	}

	return LinkOutcome{Mode: LinkModeCopy, BytesCopied: int64(len(data))}, nil
}

// cloneInto clones the stored blob to a unique name beside targetPath and
// renames it into place. The clone reaches the filesystem through a syscall
// that creates the destination itself and refuses to replace a file, so the
// name it is handed has to be free, and the rename is what makes replacing an
// existing target atomic.
func (c *Content) cloneInto(
	v *venv.Venv,
	sourcePath, targetPath string,
	perm os.FileMode,
) error {
	targetDir := filepath.Dir(targetPath)
	if err := v.FS.MkdirAll(targetDir, DefaultDirPerms); err != nil {
		return &WrappedError{
			Op:   opWriteTarget,
			Path: targetDir,
			Err:  err,
		}
	}

	tempPath, err := reserveTempPath(v, targetDir, filepath.Base(targetPath)+".tmp")
	if err != nil {
		return &WrappedError{
			Op:   opWriteTarget,
			Path: targetDir,
			Err:  err,
		}
	}

	// Returned unwrapped so the caller can still recognize a filesystem that
	// cannot clone and pick another mode.
	if err := vfs.CloneFile(v.FS, sourcePath, tempPath); err != nil {
		return err
	}

	if err := v.FS.Chmod(tempPath, perm); err != nil {
		return &WrappedError{
			Op:   "chmod_target",
			Path: tempPath,
			Err:  errors.Join(err, v.FS.Remove(tempPath)),
		}
	}

	if err := v.FS.Rename(tempPath, targetPath); err != nil {
		return &WrappedError{
			Op:   "rename_target",
			Path: tempPath,
			Err:  errors.Join(err, v.FS.Remove(tempPath)),
		}
	}

	return nil
}

// storedPermIs reports whether the stored blob at sourcePath already carries
// perm, which is what decides whether the destination can share that file.
func (c *Content) storedPermIs(v *venv.Venv, sourcePath string, perm os.FileMode) bool {
	info, err := v.FS.Stat(sourcePath)

	return err == nil && info.Mode().Perm() == perm
}

// reserveTempPath returns a free path in dir. Taking the name from a temp
// file that is created and immediately removed keeps two concurrent links to
// the same target from choosing the same one.
func reserveTempPath(v *venv.Venv, dir, pattern string) (string, error) {
	f, err := vfs.CreateTemp(v.FS, dir, pattern)
	if err != nil {
		return "", err
	}

	path := f.Name()

	if err := errors.Join(f.Close(), v.FS.Remove(path)); err != nil {
		return "", err
	}

	return path, nil
}

// writeContentToFile writes data to a temporary file, sets appropriate
// permissions, and performs an atomic rename.
func (c *Content) writeContentToFile(l log.Logger, v *venv.Venv, hash string, data []byte) error {
	v.RequireGOOS()

	path := c.getPath(hash)

	f, err := c.createTemp(v, hash)
	if err != nil {
		return err
	}

	tempPath := f.Name()

	buf := bufio.NewWriter(f)

	if _, err := buf.Write(data); err != nil {
		if closeErr := f.Close(); closeErr != nil {
			l.Warnf("failed to close temp file %s: %v", tempPath, closeErr)
		}

		if removeErr := v.FS.Remove(tempPath); removeErr != nil {
			l.Warnf("failed to remove temp file %s: %v", tempPath, removeErr)
		}

		return fmt.Errorf("write to %s: %w", tempPath, err)
	}

	if err := buf.Flush(); err != nil {
		if closeErr := f.Close(); closeErr != nil {
			l.Warnf("failed to close temp file %s: %v", tempPath, closeErr)
		}

		if removeErr := v.FS.Remove(tempPath); removeErr != nil {
			l.Warnf("failed to remove temp file %s: %v", tempPath, removeErr)
		}

		return fmt.Errorf("flush %s: %w", tempPath, err)
	}

	if err := f.Close(); err != nil {
		if removeErr := v.FS.Remove(tempPath); removeErr != nil {
			l.Warnf("failed to remove temp file %s: %v", tempPath, removeErr)
		}

		return fmt.Errorf("close %s: %w", tempPath, err)
	}

	if err := v.FS.Chmod(tempPath, StoredFilePerms); err != nil {
		if removeErr := v.FS.Remove(tempPath); removeErr != nil {
			l.Warnf("failed to remove temp file %s: %v", tempPath, removeErr)
		}

		return fmt.Errorf("chmod temp %s: %w", tempPath, err)
	}

	if v.Platform.GOOS == WindowsOS {
		if _, err := v.FS.Stat(path); err == nil {
			if err := v.FS.Chmod(path, RegularFilePerms); err != nil {
				l.Warnf("failed to make destination file writable %s: %v", path, err)
			}
		}
	}

	if err := c.publish(v, tempPath, hash); err != nil {
		if removeErr := v.FS.Remove(tempPath); removeErr != nil {
			l.Warnf("failed to remove temp file %s: %v", tempPath, removeErr)
		}

		return fmt.Errorf("finalize %s: %w", path, err)
	}

	if v.Platform.GOOS == WindowsOS {
		if err := v.FS.Chmod(path, StoredFilePerms); err != nil {
			return fmt.Errorf("chmod %s: %w", path, err)
		}
	}

	return nil
}

// createTemp opens a uniquely named temp file beside the object path of
// hash. A fixed "<hash>.tmp" would let two processes writing the same
// object truncate each other's partial file, since nothing serializes
// them any more; distinct names make the final rename the only step
// they share.
func (c *Content) createTemp(v *venv.Venv, hash string) (vfs.File, error) {
	f, err := vfs.CreateTemp(v.FS, c.getPartition(hash), hash+".tmp")
	if err != nil {
		return nil, fmt.Errorf("create temp file for %s: %w", hash, err)
	}

	return f, nil
}

// publish renames the closed temp file at tempPath onto the object path
// of hash.
//
// Another process may have published the same object first. Filesystems
// that refuse to replace an existing read-only file (Windows) report
// that as a rename error; the object is then present with the same
// content, so publish discards tempPath and reports success.
func (c *Content) publish(v *venv.Venv, tempPath, hash string) error {
	path := c.getPath(hash)

	renameErr := v.FS.Rename(tempPath, path)
	if renameErr == nil {
		return nil
	}

	if !c.store.hasContent(v, path) {
		return renameErr
	}

	if err := v.FS.Remove(tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.Join(renameErr, err)
	}

	return nil
}

// getPartition returns the partition path for a given hash.
func (c *Content) getPartition(hash string) string {
	return filepath.Join(c.store.Path(), hash[:2])
}

// getPath returns the full path for a given hash.
func (c *Content) getPath(hash string) string {
	return filepath.Join(c.getPartition(hash), hash)
}
