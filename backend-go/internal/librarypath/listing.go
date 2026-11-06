package librarypath

import (
	"errors"
	"io"
	"os"
	"sort"
	"strings"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

// Listing bounds. A chooser needs one screenful, not a whole tree: DefaultListEntries
// is what an unconfigured caller gets, MaxListEntries is the ceiling a caller may
// ask for, and listBatch is how many entries one readdir call may consume. The
// answer never carries file contents, so its size is bounded by the entry cap
// times one name.
const (
	DefaultListEntries = 64
	MaxListEntries     = 512
	listBatch          = 32
)

// ErrListingInvalid refuses an entry cap outside the bounds above. It is distinct
// from a path refusal so a caller can tell "your page size is wrong" from "this
// directory is not addressable".
var ErrListingInvalid = errors.New("LIBRARY_PATH_LISTING_INVALID")

// Entry is one depth-1 child a browser may draw. Name is a single component, so
// the page can join it onto the parent it already holds; Size is a value only, no
// bytes. Dir distinguishes a directory the page may descend into from a file it
// may read through a grant.
type Entry struct {
	Name string
	Dir  bool
	Size int64
}

// Listing is the bounded answer for one directory. Relative is the resolved
// identity of the directory itself and Entries holds at most the requested cap;
// Truncated says more entries exist, so the page can show "more" instead of
// silently presenting a partial tree as complete.
type Listing struct {
	Relative  string
	Entries   []Entry
	Truncated bool
}

// ListDirectory answers depth 1 for an already resolved directory target. It
// re-verifies the target before opening it, skips symlinks, hidden names and
// non-regular entries rather than following or failing on them, and reads no file
// contents. maxEntries may be zero for DefaultListEntries.
//
// The order is the one the shipped listing already uses, not readdir order and not
// plain byte order: internal/projectfs/read_tree.go:169-177 sorts a directory's
// entries with the Simplified-Chinese collation, directories first and byte order
// as the tie-break - that is what fs:list-dir answers and what the page draws. The
// cap selects the first maxEntries ENTRIES IN THAT ORDER, so it bounds the answer
// without letting readdir order decide which names survive.
func ListDirectory(target Target, maxEntries int) (Listing, error) {
	if maxEntries == 0 {
		maxEntries = DefaultListEntries
	}
	if maxEntries < 0 || maxEntries > MaxListEntries {
		return Listing{}, ErrListingInvalid
	}
	if !target.Valid() {
		return Listing{}, ErrPathInvalid
	}
	info, exists, err := verifyChain(target.Root, target.Relative)
	if err != nil {
		return Listing{}, err
	}
	if !exists || !info.IsDir() {
		return Listing{}, ErrPathNotDirectory
	}
	root, err := openPinned(target.Path)
	if err != nil {
		return Listing{}, err
	}
	defer func() { _ = root.Close() }()
	directory, err := root.Open(".")
	if err != nil {
		return Listing{}, ErrPathUnavailable
	}
	defer func() { _ = directory.Close() }()

	// One collator per answer, exactly as internal/projectfs/read_tree.go:168
	// builds one per directory it reads: a collator is not safe for concurrent
	// use, and two requests may list two directories at once.
	collator := collate.New(language.SimplifiedChinese)
	// One entry beyond the cap is retained, so a truncated answer and a complete
	// one are distinguishable without a second pass over the directory.
	listing := Listing{Relative: target.Relative, Entries: make([]Entry, 0, maxEntries+1)}
	for {
		batch, readErr := directory.ReadDir(listBatch)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return Listing{}, ErrPathUnavailable
		}
		for _, item := range batch {
			if entry, usable := listingEntry(root, item.Name()); usable {
				listing.Entries = append(listing.Entries, entry)
			}
		}
		// Trim to the best maxEntries+1 in reference order after each batch. The
		// trimming is what makes the cap a bound on memory and on the answer, and
		// it happens after the ordering so the cap never decides which names are
		// dropped: readdir order is not the order the page draws.
		if len(listing.Entries) > maxEntries+1 {
			listing.Entries = sortListing(collator, listing.Entries)[:maxEntries+1]
		}
		if errors.Is(readErr, io.EOF) || len(batch) == 0 {
			break
		}
	}
	listing.Entries = sortListing(collator, listing.Entries)
	if len(listing.Entries) > maxEntries {
		listing.Truncated = true
		listing.Entries = listing.Entries[:maxEntries]
	}
	return listing, nil
}

// sortListing puts entries in the order the shipped listing draws them: the
// comparator is copied from internal/projectfs/read_tree.go:169-177 - directories
// before files, then collate.New(language.SimplifiedChinese), then byte order as
// the tie-break so two names the collation treats as equal still have one stable
// order. It sorts in place and returns the same slice.
func sortListing(collator *collate.Collator, entries []Entry) []Entry {
	sort.SliceStable(entries, func(left, right int) bool {
		if entries[left].Dir != entries[right].Dir {
			return entries[left].Dir
		}
		if comparison := collator.CompareString(entries[left].Name, entries[right].Name); comparison != 0 {
			return comparison < 0
		}
		return entries[left].Name < entries[right].Name
	})
	return entries
}

// listingEntry projects one directory entry, or reports that the browser must not
// see it. A symlink is skipped: it is not a path this package resolved, and a
// listing is how a page learns which names it may then ask for. A hidden name is
// skipped for the same reason cleanRelative refuses one. Any other entry that is
// neither a directory nor a single regular file - a FIFO, a socket, a device - is
// skipped as well, so a chooser can only ever offer things a grant can read.
func listingEntry(root *os.Root, name string) (Entry, bool) {
	if name == "" || strings.HasPrefix(name, ".") {
		return Entry{}, false
	}
	info, err := root.Lstat(name)
	if err != nil {
		// An entry that vanished between readdir and lstat is simply not offered;
		// a listing must not fail because the tree is being written.
		return Entry{}, false
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return Entry{}, false
	}
	if info.IsDir() {
		return Entry{Name: name, Dir: true}, true
	}
	if !info.Mode().IsRegular() {
		return Entry{}, false
	}
	return Entry{Name: name, Size: info.Size()}, true
}
