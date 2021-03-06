package vcs

// This package implements brig's sync algorithm which I called, in a burst of
// modesty, "bright". (Not because it's or I'm very bright, but because it
// starts with brig...)
//
// The sync algorithm tries to handle the following special cases:
// - Propagate moves (most of them, at least)
// - Propagate deletes (configurable?)
// - Also sync empty directories.
//
// Terminology:
// - Destination (short "dst") is used to reference our own storage.
// - Source (short: "src") is used to reference the remote storage.
//
// The sync algorithm can be roughly divided in 4 stages:
// - Stage 1: "Move Marking":
//   Iterate over all ghosts in the tree and check if they were either moved
//   (has sibling) or removed (has no sibling). In case of directories, the
//   second mapping stage is already executed.
//
// - Stage 2: "Mapping":
//   Finding pairs of files that possibly adding, merging or conflict handling.
//   Equal files will already be sorted out at this point. Every already
//   visited node in the remote linker will be marked. The mapping algorithm
//   starts at the root node and uses the attributes of the merkle trees
//   (same hash = same content) to skip over same parts.
//
// - Stage 3: "Resolving":
//   For each file a decision needs to be made. This decison defines the next step
//   and can be one of the following.
//
//   - The file was added on the remote, we should add it to -> Add them.
//   - The file was removed on the remote, we might want to also delete it.
//   - The file was only moved on the remote node, we might want to moev it also.
//   - The file has compatible changes on the both sides. -> Merge them.
//   - The file was incompatible changes on both sides -> Do conflict resolution.
//   - The nodes have differing types (directory vs files). Report them.
//
// - Stage 4: "Handling"
//   Only at this stage "sync" and "diff" differ.
//   Sync will take the the files from Stage 3 and add/remove/merge files.
//   Diff will create a report out of those files and also includes files that
//   are simply missing on the source side (but do not need to be removed).
//
// Everything except Stage 4 is read-only. If a user wants to only show the diff
// between two linkers, he just prints what would be done instead of actually doing it.
// This makes the diff and sync implementation share most of it's code.

import (
	"fmt"

	e "github.com/pkg/errors"
	c "github.com/sahib/brig/catfs/core"
	ie "github.com/sahib/brig/catfs/errors"
	n "github.com/sahib/brig/catfs/nodes"
)

// executor is the interface that executes the actual action
// needed to perform the sync (see "phase 4" on top of this file)
type executor interface {
	handleAdd(src n.ModNode) error
	handleRemove(dst n.ModNode) error
	handleMissing(dst n.ModNode) error
	handleMove(src, dst n.ModNode) error
	handleTypeConflict(src, dst n.ModNode) error
	handleMerge(src, dst n.ModNode, srcMask, dstMask ChangeType) error
	handleConflict(src, dst n.ModNode, srcMask, dstMask ChangeType) error
}

//////////////////////////////////////////////
// IMPLEMENTATION OF ACTUAL DECISION MAKING //
//////////////////////////////////////////////

type resolver struct {
	lkrSrc *c.Linker
	lkrDst *c.Linker

	// What points should be resolved
	dstHead *n.Commit
	srcHead *n.Commit

	// cached attributes:
	dstMergeCmt *n.Commit
	srcMergeCmt *n.Commit

	// actual executor based on the decision
	exec executor
}

func newResolver(lkrSrc, lkrDst *c.Linker, srcHead, dstHead *n.Commit, exec executor) (*resolver, error) {
	var err error
	if srcHead == nil {
		srcHead, err = lkrSrc.Status()
		if err != nil {
			return nil, err
		}
	}

	if dstHead == nil {
		dstHead, err = lkrDst.Status()
		if err != nil {
			return nil, err
		}
	}

	return &resolver{
		lkrSrc:  lkrSrc,
		lkrDst:  lkrDst,
		srcHead: srcHead,
		dstHead: dstHead,
		exec:    exec,
	}, nil
}

func (rv *resolver) resolve() error {
	srcRoot, err := rv.lkrSrc.DirectoryByHash(rv.srcHead.Root())
	if err != nil {
		return err
	}

	if err := rv.cacheLastCommonMerge(); err != nil {
		return e.Wrapf(err, "Error while finding last common merge")
	}

	mapper, err := NewMapper(rv.lkrSrc, rv.lkrDst, rv.srcHead, rv.dstHead, srcRoot)
	if err != nil {
		return err
	}

	mappings := []MapPair{}
	err = mapper.Map(func(pair MapPair) error {
		mappings = append(mappings, pair)
		return nil
	})

	if err != nil {
		return err
	}

	for _, pair := range mappings {
		if err := rv.decide(pair); err != nil {
			return err
		}
	}

	return nil
}

func (rv *resolver) cacheLastCommonMerge() error {
	srcOwner, err := rv.lkrSrc.Owner()
	if err != nil {
		return err
	}

	currHead := rv.dstHead

	for currHead != nil {
		with, srcRef := currHead.MergeMarker()
		if with == srcOwner {
			srcHead, err := rv.lkrSrc.CommitByHash(srcRef)
			if err != nil {
				return err
			}

			rv.dstMergeCmt = currHead
			rv.srcMergeCmt = srcHead
		}

		prevHeadNode, err := currHead.Parent(rv.lkrDst)
		if err != nil {
			return err
		}

		if prevHeadNode == nil {
			break
		}

		newDstHead, ok := prevHeadNode.(*n.Commit)
		if !ok {
			return ie.ErrBadNode
		}

		currHead = newDstHead
	}

	return nil
}

// hasConflicts is always called when two nodes on both sides and they do not
// have the same hash. In the best case, both have compatible changes and can
// be merged, otherwise a user defined conflict strategy has to be applied.
func (rv *resolver) hasConflicts(src, dst n.ModNode) (bool, ChangeType, ChangeType, error) {
	// Nodes with same hashes are no conflicts...
	if src.Hash().Equal(dst.Hash()) {
		return false, 0, 0, nil
	}

	srcHist, err := History(rv.lkrSrc, src, rv.srcHead, rv.srcMergeCmt)
	if err != nil {
		return false, 0, 0, e.Wrapf(err, "history src")
	}

	dstHist, err := History(rv.lkrDst, dst, rv.dstHead, rv.dstMergeCmt)
	if err != nil {
		return false, 0, 0, e.Wrapf(err, "history dst")
	}

	// This loop can be optimized if the need arises:
	commonRootFound := false
	srcRoot, dstRoot := len(srcHist), len(dstHist)

	for srcIdx := 0; srcIdx < len(srcHist) && !commonRootFound; srcIdx++ {
		for dstIdx := 0; dstIdx < len(dstHist) && !commonRootFound; dstIdx++ {
			srcChange, dstChange := srcHist[srcIdx], dstHist[dstIdx]

			if srcChange.Curr.Content().Equal(dstChange.Curr.Content()) {
				srcRoot, dstRoot = srcIdx, dstIdx
				commonRootFound = true
			}
		}
	}

	srcHist = srcHist[:srcRoot]
	dstHist = dstHist[:dstRoot]

	// Compute the combination of all changes:
	var srcMask, dstMask ChangeType
	for _, change := range srcHist {
		srcMask |= change.Mask
	}
	for _, change := range dstHist {
		dstMask |= change.Mask
	}

	if len(srcHist) == 0 && len(dstHist) == 0 {
		return false, 0, 0, nil
	}

	// Handle a few lucky cases:
	if len(srcHist) > 0 && len(dstHist) == 0 {
		// We can "fast forward" our node.
		// There are only remote changes for this file.
		return false, 0, 0, nil

	}

	if len(srcHist) == 0 && len(dstHist) > 0 {
		// Only our side has changes. We can consider this node as merged.
		return false, 0, 0, nil
	}

	// Both sides have changes. Now we need to figure out if they are compatible.
	// We do this simply by OR-ing all changes on both side to an individual mask
	// and check if those can be applied on top of dst's current state.
	// TODO: Define this clearly.
	if !dstMask.IsCompatible(srcMask) {
		// The changes are not compatible.
		// We need to apply a conflict resolution strategy.
		return true, srcMask, dstMask, nil
	}

	// if srcMask&ChangeTypeMove != 0 && dst.Path() != src.Path() {
	// 	fmt.Println("NOTE: File has moved...")
	// }

	// No conflict. We can merge src and dst.
	return false, srcMask, dstMask, nil
}

func (rv *resolver) decide(pair MapPair) error {
	if pair.Src == nil && pair.Dst == nil {
		return fmt.Errorf("Received completely empty mapping; ignoring")
	}

	if pair.SrcWasMoved {
		fmt.Println("Handle move")
		return rv.exec.handleMove(pair.Src, pair.Dst)
	}

	if pair.Src == nil {
		if pair.SrcWasRemoved {
			return rv.exec.handleRemove(pair.Dst)
		} else {
			return rv.exec.handleMissing(pair.Dst)
		}
	}

	if pair.Dst == nil {
		return rv.exec.handleAdd(pair.Src)
	}

	if pair.TypeMismatch {
		fmt.Printf(
			"%s is a %s and %s a %s; ignoring",
			pair.Src.Path(), pair.Src.Type(),
			pair.Dst.Path(), pair.Dst.Type(),
		)
		return rv.exec.handleTypeConflict(pair.Src, pair.Dst)
	}

	hasConflicts, srcMask, dstMask, err := rv.hasConflicts(pair.Src, pair.Dst)
	if err != nil {
		return err
	}

	if hasConflicts {
		return rv.exec.handleConflict(pair.Src, pair.Dst, srcMask, dstMask)
	}

	// handleMerge needs the masks to decide what path / content to choose.
	return rv.exec.handleMerge(pair.Src, pair.Dst, srcMask, dstMask)
}
