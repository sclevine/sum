package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/jessevdk/go-flags"
	"golang.org/x/sync/semaphore"
)

type Options struct {
	Algorithm string `short:"a" long:"algorithm" default:"sha256" description:"Use hashing algorithm"`
	Check     bool   `short:"c" long:"check" description:"Validate checksums"`
	Mask      string `short:"m" long:"mask" default:"0000" description:"Apply mask"`
	Args      struct {
		Paths []string `required:"1"`
	} `positional-args:"yes"`
}

func main() {
	log.SetFlags(0)

	var opts Options
	parser := flags.NewParser(&opts, flags.HelpFlag|flags.PassAfterNonOption|flags.PassDoubleDash)
	rest, err := parser.Parse()
	if err != nil {
		log.Fatalf("Invalid arguments: %s", err)
	}
	if len(rest) != 0 {
		log.Fatalf("Unparsable arguments: %s", strings.Join(rest, ", "))
	}

	mask := NewMaskString(opts.Mask)
	hf := ParseHash(opts.Algorithm)
	sum := Sum{Func: hf, Mask: mask}
	if hf == nil {
		log.Fatalf("Invalid algorithm `%s'", opts.Algorithm)
	}
	next := sum.Sum(opts.Args.Paths)
	for n, err := next(); err != ErrEmpty; n, err = next() {
		if err != nil {
			log.Printf("xsum: %s", err)
			continue
		}
		if n.Mode&os.ModeDir != 0 {
			fmt.Printf("%x:%s  %s\n", n.Sum, mask, filepath.ToSlash(n.Path))
		} else {
			fmt.Printf("%x  %s\n", n.Sum, filepath.ToSlash(n.Path))
		}
	}
}

var Lock = semaphore.NewWeighted(int64(runtime.NumCPU()))

func lock()    { Lock.Acquire(context.Background(), 1) }
func release() { Lock.Release(1) }

var ErrSpecialFile = errors.New("special file")

type Node struct {
	Path string
	Sum  []byte
	Mode os.FileMode
	Sys  *SysProps
}

type Sum struct {
	Func HashFunc
	Mask Mask
}

func (s Sum) Sum(paths []string) func() (*Node, error) {
	queue := newPQ(len(paths))
	for i, path := range paths {
		i, path := i, path
		go func() {
			n, err := s.walk(filepath.Clean(path), false)
			queue.add(i, n, err)
		}()
	}
	return queue.next
}

func (s Sum) walk(path string, subdir bool) (*Node, error) {
	lock()
	rs := runSwitch(true)
	defer rs.Do(release)

	fi, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, pathNewErr("does not exist", path, subdir)
	}
	if err != nil {
		return nil, pathErr("stat", path, subdir, err)
	}
	switch {
	case fi.IsDir():
		names, err := readDirUnordered(path)
		if err != nil {
			return nil, pathErr("read dir", path, subdir, err)
		}
		release()
		rs.Set(false)
		nodes, err := s.dir(path, names)
		if err != nil {
			if subdir {
				return nil, err
			}
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		lock()
		rs.Set(true)
		sum, err := s.merkle(nodes)
		if err != nil {
			return nil, pathErr("hash", path, subdir, err)
		}
		return &Node{path, sum, fi.Mode(), getSysProps(fi)}, nil

	case fi.Mode().IsRegular() || (!subdir && fi.Mode()&os.ModeSymlink != 0):
		f, err := os.Open(path)
		if err != nil {
			return nil, pathErr("open", path, subdir, err)
		}
		defer f.Close()
		sum, err := s.hashReader(f)
		if err != nil {
			return nil, pathErr("hash", path, subdir, err)
		}
		return &Node{path, sum, fi.Mode(), getSysProps(fi)}, nil

	case fi.Mode()&os.ModeSymlink != 0:
		link, err := os.Readlink(path)
		if err != nil {
			return nil, pathErr("read link", path, subdir, err)
		}
		sum, err := s.hash([]byte(link))
		if err != nil {
			return nil, pathErr("hash", path, subdir, err)
		}
		return &Node{path, sum, fi.Mode(), getSysProps(fi)}, nil
	}
	return nil, pathErr("hash", path, subdir, ErrSpecialFile)
}

func (s Sum) dir(path string, names []string) ([]*Node, error) {
	var wg sync.WaitGroup
	wg.Add(len(names))
	errC := make(chan error)
	go func() {
		wg.Wait()
		close(errC) // safe, no more errors sent
	}()
	nodes := make([]*Node, len(names))
	for i, name := range names {
		i, path := i, filepath.Join(path, name)
		go func() {
			var err error
			nodes[i], err = s.walk(path, true)
			if err != nil {
				errC <- err
			}
			wg.Done()
		}()
	}
	for err := range errC {
		// error from walk has adequate context
		return nil, err
	}
	return nodes, nil
}

func (s Sum) merkle(nodes []*Node) ([]byte, error) {
	blocks := make([][]byte, 0, len(nodes))
	for _, n := range nodes {
		nameSum, err := s.hash([]byte(filepath.Base(n.Path)))
		if err != nil {
			return nil, err
		}
		permSum, err := s.sysattrHash(n)
		if err != nil {
			return nil, err
		}
		xattrSum, err := s.xattrHash(n)
		if err != nil {
			return nil, err
		}
		buf := bytes.NewBuffer(make([]byte, 0, len(n.Sum)*4))
		buf.Write(nameSum)
		buf.Write(n.Sum)
		buf.Write(permSum)
		buf.Write(xattrSum)
		blocks = append(blocks, buf.Bytes())
	}
	sort.Slice(blocks, func(i, j int) bool {
		return bytes.Compare(blocks[i], blocks[j]) < 0
	})
	h := s.Func()
	for _, block := range blocks {
		if _, err := h.Write(block); err != nil {
			return nil, err
		}
	}
	return h.Sum(nil), nil
}

func (s Sum) hash(b []byte) ([]byte, error) {
	h := s.Func()
	if _, err := h.Write(b); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

func (s Sum) hashReader(r io.Reader) ([]byte, error) {
	h := s.Func()
	if _, err := io.Copy(h, r); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

const (
	sModeSetuid = 04000
	sModeSetgid = 02000
	sModeSticky = 01000
)

func (s Sum) sysattrHash(n *Node) ([]byte, error) {
	var out [52]byte
	var specialMask os.FileMode
	if s.Mask.Mode&sModeSetuid != 0 {
		specialMask |= os.ModeSetuid
	}
	if s.Mask.Mode&sModeSetgid != 0 {
		specialMask |= os.ModeSetgid
	}
	if s.Mask.Mode&sModeSticky != 0 {
		specialMask |= os.ModeSticky
	}
	permMask := os.FileMode(s.Mask.Mode) & os.ModePerm
	mode := n.Mode & (os.ModeType | permMask | specialMask)
	binary.LittleEndian.PutUint32(out[:4], uint32(mode))

	if s.Mask.Attr&AttrUID != 0 {
		binary.LittleEndian.PutUint32(out[4:8], n.Sys.UID)
	}
	if s.Mask.Attr&AttrGID != 0 {
		binary.LittleEndian.PutUint32(out[8:12], n.Sys.GID)
	}
	if s.Mask.Attr&AttrSpecial != 0 && n.Mode & (os.ModeDevice | os.ModeCharDevice) != 0  {
		binary.LittleEndian.PutUint32(out[12:20], uint32(n.Sys.Device))
	}
	if s.Mask.Attr&AttrMtime != 0 {
		binary.LittleEndian.PutUint64(out[20:28], uint64(n.Sys.Mtime.Sec))
		binary.LittleEndian.PutUint64(out[28:36], uint64(n.Sys.Mtime.Nsec))
	}
	if s.Mask.Attr&AttrCtime != 0 {
		binary.LittleEndian.PutUint64(out[36:44], uint64(n.Sys.Ctime.Sec))
		binary.LittleEndian.PutUint64(out[44:52], uint64(n.Sys.Ctime.Nsec))
	}

	// out[52:68] - reserve for btime?

	return s.hash(out[:])
}

func (s Sum) xattrHash(n *Node) ([]byte, error) {
	if s.Mask.Attr & AttrX != 0 {
		xattr, err := getXattr(n.Path)
		if err != nil {
			return nil, err
		}
		return s.hash(xattr)
	}
	return nil, nil
}

type runSwitch bool

func (rs *runSwitch) Do(f func()) {
	if *rs {
		f()
	}
}

func (rs *runSwitch) Set(v bool) {
	*rs = runSwitch(v)
}

func pathErr(verb, path string, subdir bool, err error) error {
	var msg string
	pErr := &os.PathError{}
	if !subdir {
		msg = "%[2]s: failed to %[1]s: %[3]w"
	} else if errors.As(err, &pErr) {
		msg = "failed to %[1]s: %[3]w"
	} else {
		msg = "failed to %s `%s': %w"
	}
	return fmt.Errorf(msg, verb, path, err)
}

func pathNewErr(state, path string, subdir bool) error {
	var msg string
	if subdir {
		msg = "`%s' %s"
	} else {
		msg = "%s: %s"
	}
	return fmt.Errorf(msg, path, state)
}
