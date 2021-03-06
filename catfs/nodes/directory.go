package nodes

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	ie "github.com/sahib/brig/catfs/errors"
	capnp_model "github.com/sahib/brig/catfs/nodes/capnp"
	h "github.com/sahib/brig/util/hashlib"
	capnp "zombiezen.com/go/capnproto2"
)

// Directory is a typical directory that may contain
// several other directories or files.
type Directory struct {
	Base

	size       uint64
	parentName string
	children   map[string]h.Hash
}

// NewEmptyDirectory creates a new empty directory that does not exist yet.
func NewEmptyDirectory(lkr Linker, parent *Directory, name string, inode uint64) (*Directory, error) {
	absPath := ""
	if parent != nil {
		absPath = path.Join(parent.Path(), name)
	}

	newDir := &Directory{
		Base: Base{
			inode:    inode,
			hash:     h.Sum([]byte(absPath)),
			content:  h.EmptyContent.Clone(),
			name:     name,
			nodeType: NodeTypeDirectory,
			modTime:  time.Now().Truncate(time.Microsecond),
		},
		children: make(map[string]h.Hash),
	}

	if parent != nil {
		// parentName is set by Add:
		if err := parent.Add(lkr, newDir); err != nil {
			return nil, err
		}
	}

	return newDir, nil
}

func (d *Directory) String() string {
	return fmt.Sprintf("<dir %s:%s:%d>", d.Path(), d.Hash(), d.Inode())
}

// ToCapnp converts the directory to an easily serializable capnp message.
func (d *Directory) ToCapnp() (*capnp.Message, error) {
	msg, seg, err := capnp.NewMessage(capnp.SingleSegment(nil))
	if err != nil {
		return nil, err
	}

	capnode, err := capnp_model.NewRootNode(seg)
	if err != nil {
		return nil, err
	}

	if err := d.setBaseAttrsToNode(capnode); err != nil {
		return nil, err
	}

	capdir, err := d.setDirectoryAttrs(seg)
	if err != nil {
		return nil, err
	}

	if err := capnode.SetDirectory(*capdir); err != nil {
		return nil, err
	}

	return msg, nil
}

func (d *Directory) setDirectoryAttrs(seg *capnp.Segment) (*capnp_model.Directory, error) {
	capdir, err := capnp_model.NewDirectory(seg)
	if err != nil {
		return nil, err
	}

	children, err := capnp_model.NewDirEntry_List(seg, int32(len(d.children)))
	if err != nil {
		return nil, err
	}

	entryIdx := 0
	for name, hash := range d.children {
		entry, err := capnp_model.NewDirEntry(seg)
		if err != nil {
			// TODO: Accumulate errors?
			return nil, err
		}

		if err := entry.SetName(name); err != nil {
			return nil, err
		}
		if err := entry.SetHash(hash); err != nil {
			return nil, err
		}
		if err := children.Set(entryIdx, entry); err != nil {
			return nil, err
		}
		entryIdx++
	}

	if err := capdir.SetChildren(children); err != nil {
		return nil, err
	}
	if err := capdir.SetParent(d.parentName); err != nil {
		return nil, err
	}
	capdir.SetSize(d.size)

	return &capdir, nil
}

// FromCapnp will take the result of ToCapnp and set all of it's attributes.
func (d *Directory) FromCapnp(msg *capnp.Message) error {
	capnode, err := capnp_model.ReadRootNode(msg)
	if err != nil {
		return err
	}

	if err := d.parseBaseAttrsFromNode(capnode); err != nil {
		return err
	}

	capdir, err := capnode.Directory()
	if err != nil {
		return err
	}

	return d.readDirectoryAttr(capdir)
}

func (d *Directory) readDirectoryAttr(capdir capnp_model.Directory) error {
	var err error

	d.size = capdir.Size()
	d.parentName, err = capdir.Parent()
	if err != nil {
		return err
	}

	childList, err := capdir.Children()
	if err != nil {
		return err
	}

	d.children = make(map[string]h.Hash)
	for i := 0; i < childList.Len(); i++ {
		entry := childList.At(i)
		name, err := entry.Name()
		if err != nil {
			return err
		}

		hash, err := entry.Hash()
		if err != nil {
			return err
		}

		d.children[name] = hash
	}

	d.nodeType = NodeTypeDirectory
	return nil
}

////////////// NODE INTERFACE /////////////////

// Name returns the dirname of this directory.
func (d *Directory) Name() string {
	return d.name
}

// Size returns the accumulated size of the directory
// (i.e. the sum of a files in it, excluding ghosts)
func (d *Directory) Size() uint64 {
	return d.size
}

// Path returns the full path of this node.
func (d *Directory) Path() string {
	return prefixSlash(path.Join(d.parentName, d.Base.name))
}

// NChildren returns the number of children the directory has.
func (d *Directory) NChildren(lkr Linker) int {
	return len(d.children)
}

// Child returns a specific child with `name` or nil, if it was not found.
func (d *Directory) Child(lkr Linker, name string) (Node, error) {
	childHash, ok := d.children[name]
	if !ok {
		return nil, nil
	}

	return lkr.NodeByHash(childHash)
}

// Parent will return the parent of this directory or nil,
// if this directory is already the root directory.
func (d *Directory) Parent(lkr Linker) (Node, error) {
	if d.parentName == "" {
		return nil, nil
	}

	return lkr.LookupNode(d.parentName)
}

// SetParent will set the parent of this directory to `nd`.
func (d *Directory) SetParent(lkr Linker, nd Node) error {
	if d.Path() == "/" {
		return nil
	}

	if nd == nil {
		d.parentName = ""
	} else {
		d.parentName = nd.Path()
	}

	return nil
}

// ////////////// TREE MOVEMENT /////////////////

// VisitChildren will call `fn` for each of it's direct children.
// Note that the order in which `fn` will be called for each node may
// be in any random order.
func (d *Directory) VisitChildren(lkr Linker, fn func(nd Node) error) error {
	for name, hash := range d.children {
		child, err := lkr.NodeByHash(hash)
		if err != nil {
			return err
		}

		if child == nil {
			return fmt.Errorf("BUG: dead link in tree: %s => %s", name, hash.B58String())
		}

		if err := fn(child); err != nil {
			return err
		}
	}

	return nil
}

// ChildrenSorted returns a list of children node objects,
// sorted lexically by their path.
// Use this whenever you want to have a defined order of nodes,
// but do not really care what order.
func (d *Directory) ChildrenSorted(lkr Linker) ([]Node, error) {
	children := []Node{}

	err := d.VisitChildren(lkr, func(nd Node) error {
		children = append(children, nd)
		return nil
	})

	if err != nil {
		return nil, err
	}

	sort.Slice(children, func(i, j int) bool {
		return children[i].Path() < children[j].Path()
	})

	return children, nil
}

// Up will call `visit` for each node onto the way top to the root node,
// including this directory.
func (d *Directory) Up(lkr Linker, visit func(par *Directory) error) error {
	root, err := lkr.Root()
	if err != nil {
		return err
	}

	elems := strings.Split(d.Path(), "/")
	dirs := []*Directory{root}
	curr := root

	for _, elem := range elems {
		if elem == "" {
			continue
		}

		childHash, ok := curr.children[elem]
		if !ok {
			// This usually means that some link is missing.
			return fmt.Errorf("BUG: Cannot reach self from root in Up()")
		}

		childNode, err := lkr.NodeByHash(childHash)
		if err != nil {
			return err
		}

		child, ok := childNode.(*Directory)
		if !ok {
			return fmt.Errorf("BUG: Non-directory in Up(): %v", childHash)
		}

		dirs = append(dirs, child)
		curr = child
	}

	// Visit the nodes in reverse order, self first, root last:
	for idx := len(dirs) - 1; idx >= 0; idx-- {
		if err := visit(dirs[idx]); err != nil {
			return err
		}
	}

	return nil
}

// IsRoot returns true if this directory is the root directory.
func (d *Directory) IsRoot() bool {
	return d.parentName == ""
}

// Walk calls `visit` for each node below `node`, including `node`.
// If `dfs` is true, depth first search will be used.
// If `dfs` is false, breadth first search will be used.
// It is valid to pass a File to Walk(), then visit will be called exactly once.
func Walk(lkr Linker, node Node, dfs bool, visit func(child Node) error) error {
	if node == nil {
		return nil
	}

	if node.Type() != NodeTypeDirectory {
		return visit(node)
	}

	d, ok := node.(*Directory)
	if !ok {
		return ie.ErrBadNode
	}

	if !dfs {
		if err := visit(node); err != nil {
			return err
		}
	}

	for name, link := range d.children {
		child, err := lkr.NodeByHash(link)
		if err != nil {
			return err
		}

		if child == nil {
			return fmt.Errorf("Walk: could not resolve %s (%s)", name, link.B58String())
		}

		if err := Walk(lkr, child, dfs, visit); err != nil {
			return err
		}
	}

	if dfs {
		if err := visit(node); err != nil {
			return err
		}
	}

	return nil
}

func (d *Directory) xorHash(lkr Linker, hash h.Hash) error {
	oldHash := d.hash.Clone()
	if err := d.hash.Xor(hash); err != nil {
		return err
	}

	if d.IsRoot() {
		lkr.MemSetRoot(d)
	}

	lkr.MemIndexSwap(d, oldHash)
	return nil
}

// Lookup will lookup `repoPath` relative to this directory.
func (d *Directory) Lookup(lkr Linker, repoPath string) (Node, error) {
	repoPath = prefixSlash(path.Clean(repoPath))
	elems := strings.Split(repoPath, "/")

	// Strip off the first empty field:
	elems = elems[1:]

	if len(elems) == 1 && elems[0] == "" {
		return d, nil
	}

	var curr Node = d
	var err error

	for _, elem := range elems {
		curr, err = curr.Child(lkr, elem)
		if err != nil {
			return nil, err
		}

		// TODO: This currently allows to resolve children of ghost dirs.
		//       I'm still not sure if we want this.
		if curr == nil {
			return nil, ie.NoSuchFile(repoPath)
		}
	}

	return curr, nil
}

//////////// STATE ALTERING METHODS //////////////

// SetSize sets the size of this directory.
func (d *Directory) SetSize(size uint64) { d.size = size }

// SetName will set the name of this directory.
func (d *Directory) SetName(name string) {
	d.name = name
}

// SetModTime will set a new mod time to this directory (i.e. "touch" it)
func (d *Directory) SetModTime(modTime time.Time) {
	d.Base.modTime = modTime.Truncate(time.Microsecond)
}

func (d *Directory) Copy(inode uint64) ModNode {
	children := make(map[string]h.Hash)
	for name, hash := range d.children {
		children[name] = hash.Clone()
	}

	return &Directory{
		Base:       d.Base.copyBase(inode),
		size:       d.size,
		parentName: d.parentName,
		children:   children,
	}
}

func dirContentHash(content h.Hash, nchildren int) h.Hash {
	return h.Sum([]byte(fmt.Sprintf("%d:%s", nchildren, content.B58String())))
}

// Add adds `nd` to this directory.
func (d *Directory) Add(lkr Linker, nd Node) error {
	if nd == d {
		return fmt.Errorf("ADD-BUG: attempting to add `%s` to itself", nd.Path())
	}

	if _, ok := d.children[nd.Name()]; ok {
		return ie.ErrExists
	}

	nodeSize := nd.Size()
	nodeHash := nd.Hash()
	nodeContent := dirContentHash(nd.Content(), len(d.children)+1)

	err := d.Up(lkr, func(parent *Directory) error {
		parent.size += nodeSize
		if err := parent.content.Xor(nodeContent); err != nil {
			return err
		}

		return parent.xorHash(lkr, nodeHash)
	})

	if err != nil {
		return err
	}

	// Establish the link between parent and child:
	// (must be done last, because d's hash changed)
	if err := nd.SetParent(lkr, d); err != nil {
		return err
	}

	d.children[nd.Name()] = nodeHash
	return nil
}

func (d *Directory) rehash(lkr Linker, oldPath, newPath string) error {
	oldHash := d.hash.Clone()

	if err := d.hash.Xor(h.Sum([]byte(oldPath))); err != nil {
		return err
	}

	if err := d.hash.Xor(h.Sum([]byte(newPath))); err != nil {
		return err
	}

	lkr.MemIndexSwap(d, oldHash)

	// content hash is not affected.
	return nil
}

// NotifyMove should be called whenever a node is being moved.
func (d *Directory) NotifyMove(lkr Linker, newPath string) error {
	visited := map[string]Node{}

	oldRootPath := d.Path()
	err := Walk(lkr, d, true, func(child Node) error {
		oldChildPath := child.Path() // /another_empty_huhgg
		newChildPath := path.Join(newPath, oldChildPath[len(oldRootPath):])
		visited[newChildPath] = child

		switch child.Type() {
		case NodeTypeDirectory:
			childDir, ok := child.(*Directory)
			if !ok {
				return ie.ErrBadNode
			}

			if err := d.rehash(lkr, oldChildPath, newChildPath); err != nil {
				return err
			}

			dirname, basename := path.Split(newChildPath)
			childDir.parentName = dirname
			childDir.SetName(basename)
			return nil
		case NodeTypeFile:
			childFile, ok := child.(*File)
			if !ok {
				return ie.ErrBadNode
			}

			if err := childFile.NotifyMove(lkr, newChildPath); err != nil {
				return err
			}
		case NodeTypeGhost:
			childGhost, ok := child.(*Ghost)
			if !ok {
				return ie.ErrBadNode
			}

			childGhost.SetGhostPath(newChildPath)
		default:
			return fmt.Errorf("Bad node type in NotifyMove(): %d", child.Type())
		}

		return nil
	})

	if err != nil {
		return err
	}

	for nodePath, node := range visited {
		// Check if the visited nodes also contain the parent directory
		// of this node.
		if parent, ok := visited[path.Dir(nodePath)]; ok {
			parentDir := parent.(*Directory)
			baseName := path.Base(nodePath)
			parentDir.children[baseName] = node.Hash()
		}
	}

	return nil
}

// RemoveChild removes the child named `name` from it's children.
//
// Note that there is no general Remove() function that works on itself.
// It is therefore not possible (or a good idea) to remove the root node.
func (d *Directory) RemoveChild(lkr Linker, nd Node) error {
	name := nd.Name()
	if _, ok := d.children[name]; !ok {
		return ie.NoSuchFile(name)
	}

	// Unset parent from child:
	if err := nd.SetParent(lkr, nil); err != nil {
		return err
	}

	delete(d.children, name)

	nodeSize := nd.Size()
	nodeHash := nd.Hash()
	nodeContent := dirContentHash(nd.Content(), len(d.children))

	return d.Up(lkr, func(parent *Directory) error {
		parent.size -= nodeSize
		if err := parent.content.Xor(nodeContent); err != nil {
			return err
		}

		return parent.xorHash(lkr, nodeHash)
	})
}

// Assert that Directory follows the Node interface:
var _ ModNode = &Directory{}
