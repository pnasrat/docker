// The client binary owns the filesystem and runs on the host machine
// (which may be Mac, Windows, Linux, etc) and responds to the dockerd
// server (which is running FUSE, and always Linux).
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/dotcloud/docker/vfuse"
	"github.com/dotcloud/docker/vfuse/pb"

	"code.google.com/p/goprotobuf/proto"
)

var (
	root    = flag.String("root", ".", "Directory to share.")
	rw      = flag.Bool("writable", true, "whether -root is writable")
	addr    = flag.String("addr", "localhost:4321", "dockerfs service address")
	verbose = flag.Bool("verbose", false, "verbose debugging mode")
)

type Volume struct {
	Root     string
	Writable bool

	mu      sync.Mutex
	handles uint64
	files   map[uint64]*os.File
}

func NewVolume(root string, writable bool) *Volume {
	return &Volume{
		Root:     root,
		Writable: writable,
	}
}

func (v *Volume) newHandle(f *os.File) uint64 {
	defer v.mu.Unlock()
	v.mu.Lock()
	if v.files == nil {
		v.files = make(map[uint64]*os.File)
	}
	v.handles++
	v.files[v.handles] = f
	return v.handles
}

// Server is the server that runs on the client (filesystem host)
// server.  It accepts FUSE proxy requests from the peer (the Linux
// dockerd running a real FUSE filesystem) and responds with the FUSE
// answers from vol.
type Server struct {
	// TODO(bradfitz): perhaps rename some things here to avoid both
	// the words "server" and "client".
	vol *Volume
	c   *vfuse.Client
}

var (
	errRO      = &pb.Error{ReadOnly: proto.Bool(true)}
	errNotDir  = &pb.Error{NotDir: proto.Bool(true)}
	errBadPath = &pb.Error{Other: proto.String("Path must be relative and clean")}
)

func vlogf(format string, args ...interface{}) {
	if !*verbose {
		return
	}
	log.Printf("client: "+format, args...)
}

func main() {
	flag.Parse()

	conn, err := net.Dial("tcp", *addr)
	if err != nil {
		log.Panic(err)
	}

	srv := &Server{
		vol: NewVolume(".", *rw),
		c:   vfuse.NewClient(conn),
	}
	srv.Run()
}

func (s *Server) Run() {
	for {
		vlogf("reading packet...")
		p, err := s.c.ReadPacket()
		vlogf("read packet %#v, %v", p, err)
		if err != nil {
			log.Fatalf("ReadPacket error: %v", err)
		}

		var res proto.Message
		switch m := p.Body.(type) {
		case *pb.AttrRequest:
			res, err = s.handleAttrRequest(m)
		case *pb.ChmodRequest:
			res, err = s.handleChmodRequest(m)
		case *pb.CloseRequest:
			res, err = s.handleCloseRequest(m)
		case *pb.MkdirRequest:
			res, err = s.handleMkdirRequest(m)
		case *pb.OpenRequest:
			res, err = s.handleOpenRequest(m)
		case *pb.ReadRequest:
			res, err = s.handleReadRequest(m)
		case *pb.ReaddirRequest:
			res, err = s.handleReaddirRequest(m)
		case *pb.ReadlinkRequest:
			res, err = s.handleReadlinkRequest(m)
		case *pb.RenameRequest:
			res, err = s.handleRenameRequest(m)
		case *pb.RmdirRequest:
			res, err = s.handleRmdirRequest(m)
		case *pb.SymlinkRequest:
			res, err = s.handleSymlinkRequest(m)
		case *pb.UtimeRequest:
			res, err = s.handleUtimeRequest(m)
		case *pb.UnlinkRequest:
			res, err = s.handleUnlinkRequest(m)
		case *pb.TruncateRequest:
			res, err = s.handleTruncateRequest(m)
		case *pb.MknodRequest:
			res, err = s.handleMknodRequest(m)
		case *pb.ChownRequest:
			res, err = s.handleChownRequest(m)
		default:
			log.Fatalf("unhandled request type %T", p.Body)
		}

		if err != nil {
			log.Fatalf("Error handling %T: %v", p.Body, err)
		}
		err = s.c.WriteReply(p.ID, res)
		if err != nil {
			log.Fatal(err)
		}
	}
}

func mapError(err error) *pb.Error {
	if err == nil {
		return nil
	}
	if os.IsNotExist(err) {
		return &pb.Error{NotExist: proto.Bool(true)}
	}
	// TODO: more specific types
	return &pb.Error{Other: proto.String(err.Error())}
}

func validPath(p string) bool {
	if p == "" || filepath.IsAbs(p) || p != path.Clean(p) {
		return false
	}
	return true
}

// mapMode maps from a Go os.FileMode to a Linux FUSE uint32 mode.
func mapMode(m os.FileMode) uint32 {
	mode := uint32(m & 0777)
	const (
		S_IFBLK  = 0x6000
		S_IFCHR  = 0x2000
		S_IFDIR  = 0x4000
		S_IFIFO  = 0x1000
		S_IFLNK  = 0xa000
		S_IFREG  = 0x8000
		S_IFSOCK = 0xc000
	)
	if m.IsDir() {
		mode |= S_IFDIR
	} else if m.IsRegular() {
		mode |= S_IFREG
	} else if m&os.ModeSymlink != 0 {
		mode |= S_IFLNK
	} else if m&(os.ModeDevice|os.ModeCharDevice) == (os.ModeDevice | os.ModeCharDevice) {
		mode |= S_IFCHR
	} else if m&os.ModeDevice != 0 {
		mode |= S_IFBLK
	} else if m&os.ModeNamedPipe != 0 {
		mode |= S_IFIFO
	} else if m&os.ModeSocket != 0 {
		mode |= S_IFSOCK
	}
	return mode
}

func (s *Server) handleAttrRequest(req *pb.AttrRequest) (proto.Message, error) {
	res := new(pb.AttrResponse)
	if !validPath(req.GetName()) {
		res.Err = errBadPath
		return res, nil
	}
	fi, err := os.Lstat(filepath.Join(s.vol.Root, filepath.FromSlash(req.GetName())))
	if err != nil {
		res.Err = mapError(err)
		return res, nil
	}
	res.Attr = &pb.Attr{
		Size: proto.Uint64(uint64(fi.Size())),
		Mode: proto.Uint32(mapMode(fi.Mode())),
		// TODO: more
	}
	return res, nil
}

func (s *Server) handleChmodRequest(req *pb.ChmodRequest) (proto.Message, error) {
	if !s.vol.Writable {
		return &pb.ChmodResponse{Err: errRO}, nil
	}
	if !validPath(req.GetName()) {
		return &pb.ChmodResponse{Err: errBadPath}, nil
	}

	err := os.Chmod(filepath.Join(s.vol.Root, filepath.FromSlash(req.GetName())), os.FileMode(req.GetMode()))
	return &pb.ChmodResponse{
		Err: mapError(err),
	}, nil
}

func (s *Server) handleCloseRequest(req *pb.CloseRequest) (proto.Message, error) {
	res := &pb.CloseResponse{}
	h := req.GetHandle()

	s.vol.mu.Lock()
	_, ok := s.vol.files[h]
	if ok {
		delete(s.vol.files, h)
	}
	s.vol.mu.Unlock()

	if !ok {
		err := fmt.Sprintf("Close of bogus non-open handle %d", h)
		vlogf("Close: %s", err)
		res.Err = &pb.Error{Other: proto.String(err)}
	}
	return res, nil
}

func (s *Server) handleMkdirRequest(req *pb.MkdirRequest) (proto.Message, error) {
	if !s.vol.Writable {
		return &pb.MkdirResponse{Err: errRO}, nil
	}
	if !validPath(req.GetName()) {
		return &pb.MkdirResponse{Err: errBadPath}, nil
	}
	err := os.Mkdir(filepath.Join(s.vol.Root, filepath.FromSlash(req.GetName())), os.FileMode(req.GetMode()))
	return &pb.MkdirResponse{
		Err: mapError(err),
	}, nil
}

func (s *Server) handleOpenRequest(req *pb.OpenRequest) (proto.Message, error) {
	if !validPath(req.GetName()) {
		return &pb.OpenResponse{Err: errBadPath}, nil
	}
	// TODO: look at flags and return errRO earlier, instead of at the write later.
	var f *os.File
	var err error
	flags := int(req.GetFlags())
	if flags != 0 {
		// TODO: May be wrong perm usage
		f, err = os.OpenFile(req.GetName(), flags, 0)
	} else {
		f, err = os.Open(req.GetName())
	}
	if err != nil {
		return &pb.OpenResponse{Err: mapError(err)}, nil
	}
	return &pb.OpenResponse{Handle: proto.Uint64(s.vol.newHandle(f))}, nil
}

func (s *Server) handleReadRequest(req *pb.ReadRequest) (proto.Message, error) {
	vlogf("ReadRequest: %v", req)
	res := &pb.ReadResponse{}
	h := req.GetHandle()

	s.vol.mu.Lock()
	f, ok := s.vol.files[h]
	s.vol.mu.Unlock()

	if ok {
		buf := make([]byte, req.GetSize())
		n, err := f.ReadAt(buf, int64(req.GetOffset()))
		vlogf("ReadRequest = %d, %v", n, err)
		if n > 0 {
			res.Data = buf[:n]
		} else {
			res.Err = mapError(err)
		}
	} else {
		err := fmt.Sprintf("Read of bogus non-open handle %d", h)
		vlogf("Read: %s", err)
		res.Err = &pb.Error{Other: proto.String(err)}
	}
	return res, nil
}

func (s *Server) handleReaddirRequest(req *pb.ReaddirRequest) (proto.Message, error) {
	res := new(pb.ReaddirResponse)
	if !validPath(req.GetName()) {
		res.Err = errBadPath
		return res, nil
	}
	f, err := os.Open(filepath.Join(s.vol.Root, filepath.FromSlash(req.GetName())))
	if err != nil {
		res.Err = mapError(err)
		return res, nil
	}
	defer f.Close()
	all, err := f.Readdir(-1)
	if err != nil {
		res.Err = mapError(err)
		return res, nil
	}
	res.Entry = make([]*pb.DirEntry, 0, len(all))
	for _, fi := range all {
		res.Entry = append(res.Entry, &pb.DirEntry{
			Name: proto.String(fi.Name()),
			Mode: proto.Uint32(mapMode(fi.Mode())),
		})
	}
	return res, nil
}

func (s *Server) handleReadlinkRequest(req *pb.ReadlinkRequest) (proto.Message, error) {
	res := new(pb.ReadlinkResponse)
	if !validPath(req.GetName()) {
		res.Err = errBadPath
		return res, nil
	}
	target, err := os.Readlink(filepath.Join(s.vol.Root, filepath.FromSlash(req.GetName())))
	if err != nil {
		res.Err = mapError(err)
		return res, nil
	}
	res.Target = &target
	return res, nil
}

func (s *Server) handleRenameRequest(req *pb.RenameRequest) (proto.Message, error) {
	res := new(pb.RenameResponse)
	if !s.vol.Writable {
		return &pb.RenameResponse{Err: errRO}, nil
	}
	if !validPath(req.GetName()) {
		res.Err = errBadPath
		return res, nil
	}
	err := os.Rename(filepath.Join(s.vol.Root, filepath.FromSlash(req.GetName())),
		filepath.Join(s.vol.Root, filepath.FromSlash(req.GetTarget())))
	res.Err = mapError(err)
	return res, nil
}

func (s *Server) handleRmdirRequest(req *pb.RmdirRequest) (proto.Message, error) {
	if !s.vol.Writable {
		return &pb.RmdirResponse{Err: errRO}, nil
	}
	if !validPath(req.GetName()) {
		return &pb.RmdirResponse{Err: errBadPath}, nil
	}
	path := filepath.Join(s.vol.Root, filepath.FromSlash(req.GetName()))
	fi, err := os.Lstat(path)
	if err != nil {
		return &pb.RmdirResponse{Err: mapError(err)}, nil
	}
	if !fi.IsDir() {
		return &pb.RmdirResponse{Err: errNotDir}, nil
	}

	err = os.Remove(path)
	return &pb.RmdirResponse{Err: mapError(err)}, nil
}

func (s *Server) handleSymlinkRequest(req *pb.SymlinkRequest) (proto.Message, error) {
	if !s.vol.Writable {
		return &pb.SymlinkResponse{Err: errRO}, nil
	}
	if !validPath(req.GetName()) {
		return &pb.SymlinkResponse{Err: errBadPath}, nil
	}
	err := os.Symlink(req.GetValue(), filepath.Join(s.vol.Root, filepath.FromSlash(req.GetName())))
	return &pb.SymlinkResponse{
		Err: mapError(err),
	}, nil
}

func (s *Server) handleUtimeRequest(req *pb.UtimeRequest) (proto.Message, error) {
	if !s.vol.Writable {
		return &pb.UtimeResponse{Err: errRO}, nil
	}
	if !validPath(req.GetName()) {
		return &pb.UtimeResponse{Err: errBadPath}, nil
	}
	at := req.GetAtime()
	mt := req.GetMtime()
	atime := time.Unix(at.GetSec(), int64(at.GetNsec()))
	mtime := time.Unix(mt.GetSec(), int64(mt.GetNsec()))
	err := os.Chtimes(filepath.Join(s.vol.Root, filepath.FromSlash(req.GetName())), atime, mtime)
	return &pb.UtimeResponse{
		Err: mapError(err),
	}, nil
}

func (s *Server) handleUnlinkRequest(req *pb.UnlinkRequest) (proto.Message, error) {
	if !s.vol.Writable {
		return &pb.UnlinkResponse{Err: errRO}, nil
	}
	if !validPath(req.GetName()) {
		return &pb.UnlinkResponse{Err: errBadPath}, nil
	}
	err := os.Remove(filepath.Join(s.vol.Root, filepath.FromSlash(req.GetName())))
	return &pb.UnlinkResponse{
		Err: mapError(err),
	}, nil
}

func (s *Server) handleTruncateRequest(req *pb.TruncateRequest) (proto.Message, error) {
	if !s.vol.Writable {
		return &pb.TruncateResponse{Err: errRO}, nil
	}
	h := req.GetHandle()
	if h != 0 {
		s.vol.mu.Lock()
		f, ok := s.vol.files[h]
		s.vol.mu.Unlock()

		size := req.GetSize()
		if ok {
			err := f.Truncate(int64(size))
			return &pb.TruncateResponse{
				Err: mapError(err),
			}, nil
		}
		err := fmt.Sprintf("Truncating non-open handle %d", h)
		vlogf("Truncate(%d, %d): %s", h, size, err)
		return &pb.TruncateResponse{
			Err: &pb.Error{Other: proto.String(err)},
		}, nil
	}
	name := req.GetName()
	if !validPath(name) {
		return &pb.TruncateResponse{Err: errBadPath}, nil
	}
	err := os.Truncate(filepath.Join(s.vol.Root, filepath.FromSlash(name)), int64(req.GetSize()))
	return &pb.TruncateResponse{
		Err: mapError(err),
	}, nil
}

func (s *Server) handleMknodRequest(req *pb.MknodRequest) (proto.Message, error) {
	if !s.vol.Writable {
		return &pb.MknodResponse{Err: errRO}, nil
	}
	if !validPath(req.GetName()) {
		return &pb.MknodResponse{Err: errBadPath}, nil
	}
	err := syscall.Mknod(filepath.Join(s.vol.Root, filepath.FromSlash(req.GetName())), req.GetMode(), int(req.GetDev()))
	if err != nil {
		vlogf("Mknod(%q): %v", req.GetName(), err)
	}
	return &pb.MknodResponse{Err: mapError(err)}, nil
}

func (s *Server) handleChownRequest(req *pb.ChownRequest) (proto.Message, error) {
	if !s.vol.Writable {
		return &pb.ChownResponse{Err: errRO}, nil
	}
	name := req.GetName()
	if !validPath(name) {
		return &pb.ChownResponse{Err: errBadPath}, nil
	}
	uid := int(req.GetUid())
	gid := int(req.GetGid())
	err := syscall.Chown(filepath.Join(s.vol.Root, filepath.FromSlash(name)), uid, gid)
	if err != nil {
		vlogf("Chown(%q, %d, %d): %v", name, uid, gid, err)
	}
	return &pb.ChownResponse{Err: mapError(err)}, nil
}
