package cas

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"sync/atomic"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"

	"github.com/gruntwork-io/terragrunt/internal/git"
	"github.com/gruntwork-io/terragrunt/internal/telemetry"
	"github.com/gruntwork-io/terragrunt/internal/venv"
	"github.com/gruntwork-io/terragrunt/internal/vfs"
	"github.com/gruntwork-io/terragrunt/pkg/log"
)

// unixPermMask isolates the user/group/other rwx bits from a git tree mode.
const unixPermMask = os.FileMode(0o777)

// defaultMaxTreeDepth stops a descent that the repository being materialized
// would otherwise decide the length of, growing the stack and the worker count
// together as every level opens an errgroup of its own.
const defaultMaxTreeDepth = 64

// Git stores the entry type in the high bits of a six-digit octal mode;
// gitTypeMask isolates them so a symlink blob (120000) can be distinguished
// from a regular blob (100644 / 100755) at materialization time.
const (
	gitTypeMask    = uint64(0o170000)
	gitTypeSymlink = uint64(0o120000)
)

// LinkFallback classifies why a tree could not be materialized in the mode it
// was asked for. It travels as the fallback attribute on the cas_link_tree
// span.
type LinkFallback string

const (
	// LinkFallbackNone reports that the requested mode served the whole tree.
	LinkFallbackNone LinkFallback = ""

	// LinkFallbackCloneUnsupported reports that the filesystem holding the
	// target has no copy-on-write clone, so the tree was hard linked or
	// copied instead.
	LinkFallbackCloneUnsupported LinkFallback = "clone_unsupported"

	// LinkFallbackHardlinkUnsupported reports that the target could not be
	// given a second name for every stored blob, because it sits on another
	// filesystem or a blob does not carry the permissions git recorded, so
	// the tree was copied instead.
	LinkFallbackHardlinkUnsupported LinkFallback = "hardlink_unsupported"
)

// linkFallbackByMode names the fallback a tree reports when the mode it was
// asked for did not serve every file in it. [LinkModeCopy] has no entry: it
// is the mode every other one degrades to.
var linkFallbackByMode = map[LinkMode]LinkFallback{
	LinkModeHardlink: LinkFallbackHardlinkUnsupported,
	LinkModeClone:    LinkFallbackCloneUnsupported,
}

// LinkTreeOption configures a LinkTree call.
type LinkTreeOption func(*linkTreeOpts)

type linkTreeOpts struct {
	maxDepth  int
	mode      LinkMode
	forceCopy bool
}

// WithForceCopy tells LinkTree the target directory is going to be edited, so
// blobs must not be materialized as the CAS store's own files. The
// destination tree becomes safe to mutate. A tree asked for in
// [LinkModeHardlink] is cloned instead, so the extra I/O is a copy per file
// only where the filesystem has no copy-on-write clone.
func WithForceCopy() LinkTreeOption {
	return func(o *linkTreeOpts) { o.forceCopy = true }
}

// WithTreeLinkMode selects how blobs reach the target directory. Without it
// LinkTree uses [DefaultLinkMode]; the CAS entry points pass the mode their
// instance was built with.
func WithTreeLinkMode(mode LinkMode) LinkTreeOption {
	return func(o *linkTreeOpts) { o.mode = mode }
}

// WithMaxTreeDepth sets how deep a tree is followed before materialization
// gives up, in place of [defaultMaxTreeDepth].
func WithMaxTreeDepth(depth int) LinkTreeOption {
	return func(o *linkTreeOpts) { o.maxDepth = depth }
}

// maxTreeDepth returns the nesting bound to enforce, falling back to
// [defaultMaxTreeDepth] when the caller leaves it unset.
func (o *linkTreeOpts) maxTreeDepth() int {
	if o.maxDepth > 0 {
		return o.maxDepth
	}

	return defaultMaxTreeDepth
}

// LinkTree writes the tree to a target directory.
// blobStore is used to resolve blob entries, treeStore is used to resolve subtree entries.
//
// The whole tree, subtrees included, is reported as one cas_link_tree span
// carrying the mode that served it and what it cost, so a tree of thousands
// of files stays one span rather than thousands.
func LinkTree(
	ctx context.Context,
	l log.Logger,
	v *venv.Venv,
	blobStore *Store,
	treeStore *Store,
	t *git.Tree,
	targetDir string,
	opts ...LinkTreeOption,
) error {
	o := linkTreeOpts{mode: DefaultLinkMode}
	for _, opt := range opts {
		opt(&o)
	}

	mode := resolveLinkMode(o.mode, o.forceCopy)

	linker := &treeLinker{
		blobStore: blobStore,
		treeStore: treeStore,
		rootDir:   targetDir,
		maxDepth:  o.maxTreeDepth(),
		mode:      mode,
		forceCopy: o.forceCopy,
	}

	return telemetry.TelemeterFromContext(ctx).Collect(ctx, nil, "cas_link_tree", map[string]any{
		"path": targetDir,
		"mode": mode.String(),
	}, func(childCtx context.Context, _ log.Logger) error {
		err := linker.link(childCtx, l, v, t, targetDir, 0)

		linker.report(childCtx)

		return err
	})
}

// The kinds of entry [treeLinker.link] dispatches on.
const (
	itemTypeBlob      = "link"
	itemTypeSymlink   = "symlink"
	itemTypeSubtree   = "subtree"
	itemTypeSubmodule = "submodule"
)

// workItem is one entry of a tree, resolved to the path it materializes at.
type workItem struct {
	itemType string
	entry    git.TreeEntry
	path     string
	dirPath  string
}

// treeLinker materializes one tree and the subtrees under it. Its counters
// are shared across that recursion, and across the workers every level runs,
// so LinkTree can report the tree as a single span.
type treeLinker struct {
	blobStore        *Store
	treeStore        *Store
	rootDir          string
	maxDepth         int
	linked           atomic.Int64
	cloned           atomic.Int64
	copied           atomic.Int64
	bytesCopied      atomic.Int64
	mode             LinkMode
	cloneUnsupported atomic.Bool
	forceCopy        bool
}

// link writes the entries of t into targetDir, recursing into subtrees.
// [treeLinker.rootDir] stays the top-level target the caller asked to
// materialize, which is what lets symlink validation reject a target that
// resolves outside the original tree even when the link sits in a
// subdirectory.
func (tl *treeLinker) link(
	ctx context.Context,
	l log.Logger,
	v *venv.Venv,
	t *git.Tree,
	targetDir string,
	depth int,
) error {
	if depth > tl.maxDepth {
		return &TreeDepthExceededError{MaxDepth: tl.maxDepth, Path: targetDir}
	}

	blobContent := NewContent(tl.blobStore)
	treeContent := NewContent(tl.treeStore)

	dirsToCreate := make(map[string]struct{}, len(t.Entries()))

	workItems := make([]workItem, 0, len(t.Entries()))

	for _, entry := range t.Entries() {
		entryPath := filepath.Join(targetDir, entry.Path)
		dirPath := filepath.Dir(entryPath)

		dirsToCreate[dirPath] = struct{}{}

		// If the parent directory is in dirsToCreate,
		// we can remove it, since it will be created
		// when creating the subtree anyways.
		parentDirPath := filepath.Dir(dirPath)
		delete(dirsToCreate, parentDirPath)

		// Git encodes a symlink as a blob whose body is the link target; the
		// entry mode (120000) is the only signal that distinguishes it from a
		// regular file, so dispatch on the mode rather than the type.
		switch entry.Type {
		case git.EntryTypeBlob:
			itemType := itemTypeBlob
			if gitEntryIsSymlink(entry.Mode) {
				itemType = itemTypeSymlink
			}

			workItems = append(workItems, workItem{
				itemType: itemType,
				entry:    entry,
				path:     entryPath,
				dirPath:  dirPath,
			})
		case git.EntryTypeTree:
			workItems = append(workItems, workItem{
				itemType: itemTypeSubtree,
				entry:    entry,
				path:     entryPath,
				dirPath:  dirPath,
			})
		case git.EntryTypeCommit:
			workItems = append(workItems, workItem{
				itemType: itemTypeSubmodule,
				entry:    entry,
				path:     entryPath,
				dirPath:  dirPath,
			})
		}
	}

	for dirPath := range dirsToCreate {
		if err := v.FS.MkdirAll(dirPath, DefaultDirPerms); err != nil {
			return fmt.Errorf("mkdir %s: %w", dirPath, err)
		}
	}

	if idx := tl.probeIndex(workItems); idx >= 0 {
		if err := tl.linkBlob(l, v, blobContent, &workItems[idx]); err != nil {
			return err
		}

		workItems = slices.Delete(workItems, idx, idx+1)
	}

	g, ctx := errgroup.WithContext(ctx)

	// Use half the available CPUs (at least 1) to avoid saturating I/O during tree materialization.
	scalingFactor := 2
	maxWorkers := max(1, runtime.GOMAXPROCS(0)/scalingFactor)
	g.SetLimit(maxWorkers)

	for _, work := range workItems {
		g.Go(func() error {
			switch work.itemType {
			case itemTypeBlob:
				if err := tl.linkBlob(l, v, blobContent, &work); err != nil {
					return err
				}
			case itemTypeSymlink:
				target, err := blobContent.Read(v, work.entry.Hash)
				if err != nil {
					return fmt.Errorf("read symlink blob %s: %w", work.entry.Hash, err)
				}

				if err := vfs.ValidateSymlinkTarget(
					tl.rootDir,
					work.path,
					string(target),
				); err != nil {
					return err
				}

				if err := v.FS.RemoveAll(work.path); err != nil {
					return fmt.Errorf("clear existing entry before symlink %s: %w", work.path, err)
				}

				if err := vfs.Symlink(v.FS, string(target), work.path); err != nil {
					return fmt.Errorf("symlink %s -> %s: %w", work.path, string(target), err)
				}
			case itemTypeSubtree:
				treeData, err := treeContent.Read(v, work.entry.Hash)
				if err != nil {
					return fmt.Errorf("read tree %s: %w", work.entry.Hash, err)
				}

				subTree, err := git.ParseTree(treeData, work.path)
				if err != nil {
					return fmt.Errorf("parse tree %s: %w", work.entry.Hash, err)
				}

				if err := tl.link(ctx, l, v, subTree, work.path, depth+1); err != nil {
					return fmt.Errorf("link subtree %s: %w", work.path, err)
				}
			case itemTypeSubmodule:
				// A gitlink stands in for the submodule's whole working
				// tree, which ingestion stored keyed by the pinned commit
				// hash. The directory is created up front: a gitlink with
				// no stored tree had no .gitmodules entry to fetch it by,
				// and `git clone` leaves an empty directory there too.
				if err := v.FS.MkdirAll(work.path, DefaultDirPerms); err != nil {
					return fmt.Errorf("mkdir submodule %s: %w", work.path, err)
				}

				if tl.treeStore.NeedsWrite(v, work.entry.Hash) {
					return nil
				}

				treeData, err := treeContent.Read(v, work.entry.Hash)
				if err != nil {
					return fmt.Errorf("read submodule tree %s: %w", work.entry.Hash, err)
				}

				subTree, err := git.ParseTree(treeData, work.path)
				if err != nil {
					return fmt.Errorf("parse submodule tree %s: %w", work.entry.Hash, err)
				}

				if err := tl.link(ctx, l, v, subTree, work.path, depth+1); err != nil {
					return fmt.Errorf("link submodule %s: %w", work.path, err)
				}
			}

			return nil
		})
	}

	return g.Wait()
}

// probeIndex returns the blob to materialize ahead of the others, or -1 when
// nothing is to be learned from doing so. Only a clone has an answer worth
// settling: every other mode either serves every file or is already what a
// clone degrades to.
func (tl *treeLinker) probeIndex(items []workItem) int {
	if tl.mode != LinkModeClone || tl.cloneUnsupported.Load() {
		return -1
	}

	return slices.IndexFunc(items, func(item workItem) bool {
		return item.itemType == itemTypeBlob
	})
}

// linkOptions returns the options one blob is materialized with.
//
// Once a clone has come back as something else, which is how a filesystem
// with no copy-on-write clone answers, the rest of the tree takes the same
// fallback without asking again. A single entry the filesystem cannot clone
// where others clone fine, such as one reached across a mount point, still
// falls back on its own inside [Content.Link].
func (tl *treeLinker) linkOptions() []LinkOption {
	opts := []LinkOption{WithFileLinkMode(tl.mode)}

	if tl.forceCopy {
		opts = append(opts, WithLinkForceCopy())
	}

	if tl.cloneUnsupported.Load() {
		opts = append(opts, WithoutCloneAttempt())
	}

	return opts
}

// linkBlob materializes one blob entry and counts how it arrived.
func (tl *treeLinker) linkBlob(
	l log.Logger,
	v *venv.Venv,
	blobContent *Content,
	work *workItem,
) error {
	outcome, err := blobContent.Link(
		l,
		v,
		work.entry.Hash,
		work.path,
		gitFilePerm(work.entry.Mode),
		tl.linkOptions()...)
	if err != nil {
		return fmt.Errorf("link blob %s: %w", work.path, err)
	}

	if tl.mode == LinkModeClone && outcome.Mode != LinkModeClone {
		tl.cloneUnsupported.Store(true)
	}

	tl.record(outcome)

	return nil
}

// record counts one materialized blob.
func (tl *treeLinker) record(outcome LinkOutcome) {
	switch outcome.Mode {
	case LinkModeHardlink:
		tl.linked.Add(1)
	case LinkModeClone:
		tl.cloned.Add(1)
	case LinkModeCopy:
		tl.copied.Add(1)
		tl.bytesCopied.Add(outcome.BytesCopied)
	}
}

// report stamps what the tree cost onto the span ctx carries. A file that
// arrived in a mode other than the one asked for met a filesystem that could
// not serve the request, which is worth reporting even though every file is
// in place.
func (tl *treeLinker) report(ctx context.Context) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}

	counts := map[LinkMode]int64{
		LinkModeHardlink: tl.linked.Load(),
		LinkModeClone:    tl.cloned.Load(),
		LinkModeCopy:     tl.copied.Load(),
	}

	var total int64
	for _, n := range counts {
		total += n
	}

	fallback := LinkFallbackNone
	if counts[tl.mode] < total {
		fallback = linkFallbackByMode[tl.mode]
	}

	span.SetAttributes(
		attribute.Int64("files_linked", counts[LinkModeHardlink]),
		attribute.Int64("files_cloned", counts[LinkModeClone]),
		attribute.Int64("files_copied", counts[LinkModeCopy]),
		attribute.Int64("bytes_copied", tl.bytesCopied.Load()),
		attribute.String("fallback", string(fallback)),
	)
}

// gitFilePerm extracts the unix permission bits from a git tree entry mode
// string. Git tree modes are six-digit octal: "100644" or "100755" for blobs.
// Returns RegularFilePerms when the mode is missing or unparsable so callers
// always have a sane default.
func gitFilePerm(mode string) os.FileMode {
	if mode == "" {
		return RegularFilePerms
	}

	n, err := strconv.ParseUint(mode, 8, 32)
	if err != nil {
		return RegularFilePerms
	}

	return os.FileMode(n) & unixPermMask
}

// gitEntryIsSymlink reports whether mode encodes the git symlink type
// (120000). The high bits of a six-digit octal mode carry the entry type;
// permission-only inspection cannot distinguish a symlink blob from a regular
// blob.
func gitEntryIsSymlink(mode string) bool {
	if mode == "" {
		return false
	}

	n, err := strconv.ParseUint(mode, 8, 32)
	if err != nil {
		return false
	}

	return n&gitTypeMask == gitTypeSymlink
}
