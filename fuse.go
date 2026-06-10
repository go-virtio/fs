// go-virtio/fs — the FUSE request path and the read-write mount op
// closure. Each FUSE request is one descriptor chain on the request
// virtqueue (Virtio 1.2 §5.11.6 "Device Operation"):
//
//	readable descriptors : [ struct fuse_in_header | op-specific in-args ]
//	writable descriptors : [ struct fuse_out_header | op-specific out-args ]
//
// The driver builds the readable bytes (header + in-args) into one DMA
// buffer, the writable bytes (out-header + out-args, sized to the
// expected reply) into another, rings the doorbell, busy-polls the used
// ring, then reads fuse_out_header.error and the op-specific reply.
//
// FUSE_WRITE is the single exception: its request inserts an extra
// readable data descriptor between the in-args and the writable reply (a
// three-region chain, built by writeRequest).
//
// References:
//
//   - Linux include/uapi/linux/fuse.h — every struct + opcode below.
//   - Virtio 1.2 §5.11.6 "Device Operation" — the in/out descriptor
//     split.

package fs

import (
	"encoding/binary"

	"github.com/go-virtio/common"
)

var le = binary.LittleEndian

// fuseRequest performs one FUSE round-trip. `op` is the fuse_opcode,
// `nodeid` the target node, `inArgs` the op-specific in-args (appended
// after the 40-byte fuse_in_header), and `outArgsLen` the number of
// bytes of op-specific out-args the device will write (after the 16-byte
// fuse_out_header). It returns the out-args bytes (len == the bytes the
// device actually wrote, capped at outArgsLen) and the FUSE error code
// from fuse_out_header.error (0 == success).
//
// Memory: one page-group holds the readable request (in-header +
// inArgs); a second holds the writable reply (out-header + out-args).
// Both are device-visible DMA from the transport's PageAllocator.
func (f *VirtioFS) fuseRequest(op uint32, nodeid uint64, inArgs []byte, outArgsLen int) ([]byte, int32, error) {
	inLen := fuseInHeaderSize + len(inArgs)
	outLen := fuseOutHeaderSize + outArgsLen

	// Readable buffer: fuse_in_header followed by inArgs.
	inPhys, inMem, err := f.allocBytes(inLen)
	if err != nil {
		return nil, 0, err
	}
	unique := f.nextUnique()
	// fuse_in_header (Linux fuse.h): len, opcode, unique, nodeid, uid,
	// gid, pid, total_extlen, padding. len is the TOTAL request length
	// the device should read (header + in-args).
	le.PutUint32(inMem[0:4], uint32(inLen))
	le.PutUint32(inMem[4:8], op)
	le.PutUint64(inMem[8:16], unique)
	le.PutUint64(inMem[16:24], nodeid)
	le.PutUint32(inMem[24:28], 0) // uid
	le.PutUint32(inMem[28:32], 0) // gid
	le.PutUint32(inMem[32:36], 0) // pid
	le.PutUint16(inMem[36:38], 0) // total_extlen
	le.PutUint16(inMem[38:40], 0) // padding
	copy(inMem[fuseInHeaderSize:inLen], inArgs)

	// Writable buffer: fuse_out_header followed by outArgsLen out-args.
	outPhys, outMem, err := f.allocBytes(outLen)
	if err != nil {
		return nil, 0, err
	}

	chain := []common.ChainBuffer{
		{Addr: uintptr(inPhys), Phys: inPhys, Len: uint32(inLen), Writable: false},
		{Addr: uintptr(outPhys), Phys: outPhys, Len: uint32(outLen), Writable: true},
	}

	head, err := f.rq.AddChain(chain)
	if err != nil {
		return nil, 0, err
	}
	if err := f.Cfg.NotifyQueue(RequestQueueIdx, f.rq.NotifyOff); err != nil {
		return nil, 0, err
	}

	for spin := 0; spin < TxPollIterations; spin++ {
		gotIdx, _, ok := f.rq.PollUsed()
		if !ok {
			continue
		}
		_ = f.rq.ReclaimChain(gotIdx)
		// fuse_out_header (Linux fuse.h): len (le32), error (s32),
		// unique (le64). error is a NEGATIVE errno on failure, 0 on ok.
		replyLen := le.Uint32(outMem[0:4])
		ferr := int32(le.Uint32(outMem[4:8]))
		if ferr != 0 {
			return nil, ferr, ErrFuseError
		}
		// Bytes the device actually wrote past the out-header, capped at
		// the buffer we provided (a well-behaved device never exceeds it).
		got := int(replyLen) - fuseOutHeaderSize
		if got < 0 {
			got = 0
		}
		if got > outArgsLen {
			got = outArgsLen
		}
		out := make([]byte, got)
		copy(out, outMem[fuseOutHeaderSize:fuseOutHeaderSize+got])
		return out, 0, nil
	}
	_ = f.rq.ReclaimChain(head)
	return nil, 0, ErrRequestTimeout
}

// allocBytes allocates a DMA page-group large enough for n bytes and
// returns its physical address + host view (the first n bytes are the
// caller's; the rest is zeroed slack).
func (f *VirtioFS) allocBytes(n int) (uint64, []byte, error) {
	// n is always >= fuseInHeaderSize / fuseOutHeaderSize at every call
	// site (the headers are unconditional), so n is strictly positive
	// and the page count is at least 1.
	pages := (n + int(common.PageSize) - 1) / int(common.PageSize)
	phys, mem, err := f.transport.AllocatePages(pages)
	if err != nil {
		return 0, nil, err
	}
	if phys == 0 {
		return 0, nil, common.ErrAllocReturnedZero
	}
	return phys, mem, nil
}

// Init performs the FUSE_INIT handshake (Linux fuse.h, opcode 26). It
// sends fuse_init_in{major=7, minor, max_readahead, flags} on nodeid 0
// and parses fuse_init_out{major, minor, ...}, recording the negotiated
// version. Returns ErrFuseVersion if the device reports a major other
// than 7 (the only major this driver speaks).
func (f *VirtioFS) Init() error {
	in := make([]byte, fuseInitInSize)
	le.PutUint32(in[0:4], FuseKernelVersion)      // major
	le.PutUint32(in[4:8], FuseKernelMinorVersion) // minor
	le.PutUint32(in[8:12], 0)                     // max_readahead
	le.PutUint32(in[12:16], FuseInitFlags)        // flags (write-relevant)
	// flags2 + unused[11] stay zero.

	out, _, err := f.fuseRequest(FuseOpInit, 0, in, fuseInitOutSize)
	if err != nil {
		return err
	}
	if len(out) < 8 {
		return ErrShortReply
	}
	major := le.Uint32(out[0:4])
	minor := le.Uint32(out[4:8])
	if major != FuseKernelVersion {
		return ErrFuseVersion
	}
	f.FuseMajor = major
	f.FuseMinor = minor
	// fuse_init_out.flags is at offset 12 (after major,minor,max_readahead).
	// The negotiated set is what we proposed AND the device offered. A
	// device that returns a truncated reply (no flags field) negotiates
	// nothing.
	if len(out) >= 16 {
		f.FuseFlags = FuseInitFlags & le.Uint32(out[12:16])
	}
	return nil
}

// Lookup performs FUSE_LOOKUP (Linux fuse.h, opcode 1): resolve `name`
// within directory node `parent`. The in-args are the NUL-terminated
// name; the reply is fuse_entry_out{nodeid, ..., attr}. A nodeid of 0
// means a negative lookup (name does not exist) — returned as ErrNoEntry.
func (f *VirtioFS) Lookup(parent uint64, name string) (Entry, error) {
	in := append([]byte(name), 0) // NUL-terminated (Linux fuse.h convention)
	out, _, err := f.fuseRequest(FuseOpLookup, parent, in, fuseEntryOutSize)
	if err != nil {
		return Entry{}, err
	}
	if len(out) < fuseEntryOutSize {
		return Entry{}, ErrShortReply
	}
	nodeid := le.Uint64(out[0:8])
	if nodeid == 0 {
		return Entry{}, ErrNoEntry
	}
	// fuse_entry_out: nodeid(0), generation(8), entry_valid(16),
	// attr_valid(24), entry_valid_nsec(32), attr_valid_nsec(36),
	// then fuse_attr at offset 40.
	attr := ParseAttr(out[40 : 40+fuseAttrSize])
	return Entry{NodeID: nodeid, Attr: attr}, nil
}

// GetAttr performs FUSE_GETATTR (Linux fuse.h, opcode 3) on `nodeid`.
// in-args are fuse_getattr_in{getattr_flags=0, dummy=0, fh=0} (a
// path-based getattr, no open handle). The reply is
// fuse_attr_out{attr_valid, attr_valid_nsec, dummy, attr}.
func (f *VirtioFS) GetAttr(nodeid uint64) (Attr, error) {
	in := make([]byte, fuseGetattrInSize) // all-zero: no GETATTR_FH flag
	out, _, err := f.fuseRequest(FuseOpGetattr, nodeid, in, fuseAttrOutSize)
	if err != nil {
		return Attr{}, err
	}
	if len(out) < fuseAttrOutSize {
		return Attr{}, ErrShortReply
	}
	// fuse_attr_out: attr_valid(0), attr_valid_nsec(8), dummy(12),
	// then fuse_attr at offset 16.
	return ParseAttr(out[16 : 16+fuseAttrSize]), nil
}

// Open performs FUSE_OPEN (Linux fuse.h, opcode 14) on `nodeid`,
// read-only. It is unchanged from the read-only closure (flags =
// O_RDONLY); the returned fh is used in Read/Release.
func (f *VirtioFS) Open(nodeid uint64) (uint64, error) {
	return f.OpenRW(nodeid, OpenReadOnly)
}

// OpenRW performs FUSE_OPEN (Linux fuse.h, opcode 14) on `nodeid` with an
// explicit access mode in `flags` (OpenReadOnly / OpenWriteOnly /
// OpenReadWrite, optionally OR-ed with other open(2) flags such as
// O_TRUNC). The in-args are fuse_open_in{flags, open_flags}; the reply is
// fuse_open_out{fh, open_flags, backing_id}; the returned fh is used in
// Read/Write/Fsync/Flush/Release.
func (f *VirtioFS) OpenRW(nodeid uint64, flags uint32) (uint64, error) {
	in := make([]byte, fuseOpenInSize)
	le.PutUint32(in[0:4], flags) // fuse_open_in.flags (O_RDONLY/WRONLY/RDWR)
	le.PutUint32(in[4:8], 0)     // open_flags
	out, _, err := f.fuseRequest(FuseOpOpen, nodeid, in, fuseOpenOutSize)
	if err != nil {
		return 0, err
	}
	if len(out) < 8 {
		return 0, ErrShortReply
	}
	fh := le.Uint64(out[0:8]) // fuse_open_out.fh
	return fh, nil
}

// Read performs FUSE_READ (Linux fuse.h, opcode 15) on (nodeid, fh),
// reading up to `size` bytes at byte offset `off`. The in-args are
// fuse_read_in{fh, offset, size, read_flags, lock_owner, flags,
// padding}; the reply is raw file bytes (no out-args struct — the
// device writes the data directly after fuse_out_header). The returned
// slice length is min(size, bytes-available) — a short read at EOF.
func (f *VirtioFS) Read(nodeid, fh, off uint64, size uint32) ([]byte, error) {
	if size == 0 {
		return nil, ErrZeroSize
	}
	in := make([]byte, fuseReadInSize)
	le.PutUint64(in[0:8], fh)     // fh
	le.PutUint64(in[8:16], off)   // offset
	le.PutUint32(in[16:20], size) // size
	le.PutUint32(in[20:24], 0)    // read_flags
	le.PutUint64(in[24:32], 0)    // lock_owner
	le.PutUint32(in[32:36], 0)    // flags
	le.PutUint32(in[36:40], 0)    // padding
	out, _, err := f.fuseRequest(FuseOpRead, nodeid, in, int(size))
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Release performs FUSE_RELEASE (Linux fuse.h, opcode 18) on
// (nodeid, fh), closing the open handle. in-args are fuse_release_in{fh,
// flags, release_flags, lock_owner}; there are no out-args.
func (f *VirtioFS) Release(nodeid, fh uint64) error {
	in := make([]byte, fuseReleaseInSize)
	le.PutUint64(in[0:8], fh) // fh; flags/release_flags/lock_owner = 0
	_, _, err := f.fuseRequest(FuseOpRelease, nodeid, in, 0)
	return err
}

// Write performs FUSE_WRITE (Linux fuse.h, opcode 16) on (nodeid, fh),
// writing `data` at byte offset `off`. The request is a THREE-region
// chain (Virtio 1.2 §5.11.6): readable [fuse_in_header | fuse_write_in],
// readable [data], writable [fuse_out_header | fuse_write_out]. The reply
// fuse_write_out{size, padding} reports the bytes the device accepted;
// Write returns that count and ErrShortWrite if it is less than
// len(data).
//
// fuse_write_in (Linux fuse.h): fh, offset, size, write_flags,
// lock_owner, flags, padding.
func (f *VirtioFS) Write(nodeid, fh, off uint64, data []byte) (int, error) {
	if len(data) == 0 {
		return 0, ErrZeroSize
	}
	wi := make([]byte, fuseWriteInSize)
	le.PutUint64(wi[0:8], fh)                  // fh
	le.PutUint64(wi[8:16], off)                // offset
	le.PutUint32(wi[16:20], uint32(len(data))) // size
	le.PutUint32(wi[20:24], 0)                 // write_flags
	le.PutUint64(wi[24:32], 0)                 // lock_owner
	le.PutUint32(wi[32:36], 0)                 // flags
	le.PutUint32(wi[36:40], 0)                 // padding

	out, _, err := f.writeRequest(nodeid, wi, data, fuseWriteOutSize)
	if err != nil {
		return 0, err
	}
	if len(out) < fuseWriteOutSize {
		return 0, ErrShortReply
	}
	n := int(le.Uint32(out[0:4])) // fuse_write_out.size
	if n < len(data) {
		return n, ErrShortWrite
	}
	return n, nil
}

// writeRequest performs one FUSE round-trip whose request carries an
// extra readable data region between the in-header+in-args and the
// writable reply (the FUSE_WRITE shape). `inArgs` is the op-specific
// in-args (fuse_write_in) appended after the fuse_in_header; `data` is
// the payload region; `outArgsLen` sizes the writable out-args. The
// fuse_in_header.len covers header + inArgs + data (the device reads the
// data as part of the request).
func (f *VirtioFS) writeRequest(nodeid uint64, inArgs, data []byte, outArgsLen int) ([]byte, int32, error) {
	inLen := fuseInHeaderSize + len(inArgs)
	outLen := fuseOutHeaderSize + outArgsLen

	inPhys, inMem, err := f.allocBytes(inLen)
	if err != nil {
		return nil, 0, err
	}
	unique := f.nextUnique()
	// fuse_in_header.len is the TOTAL request length the device reads:
	// header + in-args + the data region (Linux fuse.h).
	le.PutUint32(inMem[0:4], uint32(inLen+len(data)))
	le.PutUint32(inMem[4:8], FuseOpWrite)
	le.PutUint64(inMem[8:16], unique)
	le.PutUint64(inMem[16:24], nodeid)
	le.PutUint32(inMem[24:28], 0) // uid
	le.PutUint32(inMem[28:32], 0) // gid
	le.PutUint32(inMem[32:36], 0) // pid
	le.PutUint16(inMem[36:38], 0) // total_extlen
	le.PutUint16(inMem[38:40], 0) // padding
	copy(inMem[fuseInHeaderSize:inLen], inArgs)

	dataPhys, dataMem, err := f.allocBytes(len(data))
	if err != nil {
		return nil, 0, err
	}
	copy(dataMem[:len(data)], data)

	outPhys, outMem, err := f.allocBytes(outLen)
	if err != nil {
		return nil, 0, err
	}

	chain := []common.ChainBuffer{
		{Addr: uintptr(inPhys), Phys: inPhys, Len: uint32(inLen), Writable: false},
		{Addr: uintptr(dataPhys), Phys: dataPhys, Len: uint32(len(data)), Writable: false},
		{Addr: uintptr(outPhys), Phys: outPhys, Len: uint32(outLen), Writable: true},
	}

	head, err := f.rq.AddChain(chain)
	if err != nil {
		return nil, 0, err
	}
	if err := f.Cfg.NotifyQueue(RequestQueueIdx, f.rq.NotifyOff); err != nil {
		return nil, 0, err
	}

	for spin := 0; spin < TxPollIterations; spin++ {
		gotIdx, _, ok := f.rq.PollUsed()
		if !ok {
			continue
		}
		_ = f.rq.ReclaimChain(gotIdx)
		replyLen := le.Uint32(outMem[0:4])
		ferr := int32(le.Uint32(outMem[4:8]))
		if ferr != 0 {
			return nil, ferr, ErrFuseError
		}
		got := int(replyLen) - fuseOutHeaderSize
		if got < 0 {
			got = 0
		}
		if got > outArgsLen {
			got = outArgsLen
		}
		o := make([]byte, got)
		copy(o, outMem[fuseOutHeaderSize:fuseOutHeaderSize+got])
		return o, 0, nil
	}
	_ = f.rq.ReclaimChain(head)
	return nil, 0, ErrRequestTimeout
}

// Create performs FUSE_CREATE (Linux fuse.h, opcode 35) in directory
// `parent`: an atomic create+open of `name` with `mode` (st_mode incl.
// type/perm bits) and access `flags` (OpenWriteOnly/OpenReadWrite, OR-ed
// with create-time flags such as O_EXCL). The in-args are
// fuse_create_in{flags, mode, umask, open_flags} followed by the
// NUL-terminated name; the reply is fuse_entry_out immediately followed
// by fuse_open_out. Returns the new Entry and the open file handle.
func (f *VirtioFS) Create(parent uint64, name string, mode, flags uint32) (Entry, uint64, error) {
	ci := make([]byte, fuseCreateInSize)
	le.PutUint32(ci[0:4], flags) // flags (open access mode)
	le.PutUint32(ci[4:8], mode)  // mode
	le.PutUint32(ci[8:12], 0)    // umask
	le.PutUint32(ci[12:16], 0)   // open_flags
	in := append(ci, append([]byte(name), 0)...)
	out, _, err := f.fuseRequest(FuseOpCreate, parent, in, fuseEntryOutSize+fuseOpenOutSize)
	if err != nil {
		return Entry{}, 0, err
	}
	if len(out) < fuseEntryOutSize+fuseOpenOutSize {
		return Entry{}, 0, ErrShortReply
	}
	nodeid := le.Uint64(out[0:8])
	attr := ParseAttr(out[40 : 40+fuseAttrSize])
	// fuse_open_out follows the 132-byte fuse_entry_out; its fh is at +0.
	fh := le.Uint64(out[fuseEntryOutSize : fuseEntryOutSize+8])
	return Entry{NodeID: nodeid, Attr: attr}, fh, nil
}

// Mkdir performs FUSE_MKDIR (Linux fuse.h, opcode 9) in directory
// `parent`, creating subdirectory `name` with permission bits `mode`.
// The in-args are fuse_mkdir_in{mode, umask} followed by the
// NUL-terminated name; the reply is fuse_entry_out for the new directory.
func (f *VirtioFS) Mkdir(parent uint64, name string, mode uint32) (Entry, error) {
	mi := make([]byte, fuseMkdirInSize)
	le.PutUint32(mi[0:4], mode) // mode
	le.PutUint32(mi[4:8], 0)    // umask
	in := append(mi, append([]byte(name), 0)...)
	out, _, err := f.fuseRequest(FuseOpMkdir, parent, in, fuseEntryOutSize)
	if err != nil {
		return Entry{}, err
	}
	if len(out) < fuseEntryOutSize {
		return Entry{}, ErrShortReply
	}
	nodeid := le.Uint64(out[0:8])
	if nodeid == 0 {
		return Entry{}, ErrNoEntry
	}
	attr := ParseAttr(out[40 : 40+fuseAttrSize])
	return Entry{NodeID: nodeid, Attr: attr}, nil
}

// Mknod performs FUSE_MKNOD (Linux fuse.h, opcode 8) in directory
// `parent`, creating a non-directory node `name` with st_mode `mode`
// (type bits select regular file / fifo / socket / device) and device
// number `rdev` (0 for non-device nodes). The in-args are
// fuse_mknod_in{mode, rdev, umask, padding} followed by the
// NUL-terminated name; the reply is fuse_entry_out.
func (f *VirtioFS) Mknod(parent uint64, name string, mode, rdev uint32) (Entry, error) {
	mi := make([]byte, fuseMknodInSize)
	le.PutUint32(mi[0:4], mode) // mode
	le.PutUint32(mi[4:8], rdev) // rdev
	le.PutUint32(mi[8:12], 0)   // umask
	le.PutUint32(mi[12:16], 0)  // padding
	in := append(mi, append([]byte(name), 0)...)
	out, _, err := f.fuseRequest(FuseOpMknod, parent, in, fuseEntryOutSize)
	if err != nil {
		return Entry{}, err
	}
	if len(out) < fuseEntryOutSize {
		return Entry{}, ErrShortReply
	}
	nodeid := le.Uint64(out[0:8])
	if nodeid == 0 {
		return Entry{}, ErrNoEntry
	}
	attr := ParseAttr(out[40 : 40+fuseAttrSize])
	return Entry{NodeID: nodeid, Attr: attr}, nil
}

// Symlink performs FUSE_SYMLINK (Linux fuse.h, opcode 6) in directory
// `parent`, creating symlink `name` whose target is `target`. FUSE_SYMLINK
// has no fixed in-args struct: the in-args are the NUL-terminated link
// name followed by the NUL-terminated target. The reply is fuse_entry_out.
func (f *VirtioFS) Symlink(parent uint64, name, target string) (Entry, error) {
	in := append([]byte(name), 0)
	in = append(in, append([]byte(target), 0)...)
	out, _, err := f.fuseRequest(FuseOpSymlink, parent, in, fuseEntryOutSize)
	if err != nil {
		return Entry{}, err
	}
	if len(out) < fuseEntryOutSize {
		return Entry{}, ErrShortReply
	}
	nodeid := le.Uint64(out[0:8])
	if nodeid == 0 {
		return Entry{}, ErrNoEntry
	}
	attr := ParseAttr(out[40 : 40+fuseAttrSize])
	return Entry{NodeID: nodeid, Attr: attr}, nil
}

// Link performs FUSE_LINK (Linux fuse.h, opcode 13): create a hard link
// `newname` in directory `newparent` pointing at the existing node
// `oldnodeid`. The in-args are fuse_link_in{oldnodeid} followed by the
// NUL-terminated new name; the reply is fuse_entry_out.
func (f *VirtioFS) Link(oldnodeid, newparent uint64, newname string) (Entry, error) {
	li := make([]byte, fuseLinkInSize)
	le.PutUint64(li[0:8], oldnodeid) // oldnodeid
	in := append(li, append([]byte(newname), 0)...)
	out, _, err := f.fuseRequest(FuseOpLink, newparent, in, fuseEntryOutSize)
	if err != nil {
		return Entry{}, err
	}
	if len(out) < fuseEntryOutSize {
		return Entry{}, ErrShortReply
	}
	nodeid := le.Uint64(out[0:8])
	if nodeid == 0 {
		return Entry{}, ErrNoEntry
	}
	attr := ParseAttr(out[40 : 40+fuseAttrSize])
	return Entry{NodeID: nodeid, Attr: attr}, nil
}

// SetAttrIn carries the fuse_setattr_in fields the caller wants to
// change for SetAttr. Only fields flagged in Valid (FATTR_* bits) are
// applied by the device; the others are ignored.
type SetAttrIn struct {
	Valid uint32 // FATTR_* mask selecting which fields below are live
	Fh    uint64 // open handle (only if FattrFh set in Valid)
	Size  uint64 // new size (truncate; FattrSize)
	Mode  uint32 // new st_mode (chmod; FattrMode)
	UID   uint32 // new owner uid (FattrUID)
	GID   uint32 // new owner gid (FattrGID)
	Atime uint64 // access time, seconds (FattrAtime)
	Mtime uint64 // modify time, seconds (FattrMtime)
}

// SetAttr performs FUSE_SETATTR (Linux fuse.h, opcode 4) on `nodeid`,
// changing the attributes selected by in.Valid (truncate via FattrSize,
// chmod via FattrMode, chown via FattrUID/FattrGID, utimes via
// FattrAtime/FattrMtime, etc.). The in-args are fuse_setattr_in; the
// reply is fuse_attr_out, returned as the updated Attr.
func (f *VirtioFS) SetAttr(nodeid uint64, in SetAttrIn) (Attr, error) {
	si := make([]byte, fuseSetattrInSize)
	le.PutUint32(si[setattrValidOffset:setattrValidOffset+4], in.Valid)
	le.PutUint64(si[setattrFhOffset:setattrFhOffset+8], in.Fh)
	le.PutUint64(si[setattrSizeOffset:setattrSizeOffset+8], in.Size)
	le.PutUint64(si[setattrAtimeOffset:setattrAtimeOffset+8], in.Atime)
	le.PutUint64(si[setattrMtimeOffset:setattrMtimeOffset+8], in.Mtime)
	le.PutUint32(si[setattrModeOffset:setattrModeOffset+4], in.Mode)
	le.PutUint32(si[setattrUIDOffset:setattrUIDOffset+4], in.UID)
	le.PutUint32(si[setattrGIDOffset:setattrGIDOffset+4], in.GID)
	out, _, err := f.fuseRequest(FuseOpSetattr, nodeid, si, fuseAttrOutSize)
	if err != nil {
		return Attr{}, err
	}
	if len(out) < fuseAttrOutSize {
		return Attr{}, ErrShortReply
	}
	// fuse_attr_out: attr_valid(0), attr_valid_nsec(8), dummy(12), then
	// fuse_attr at offset 16 (same layout as GetAttr's reply).
	return ParseAttr(out[16 : 16+fuseAttrSize]), nil
}

// Unlink performs FUSE_UNLINK (Linux fuse.h, opcode 10): remove the
// non-directory entry `name` from directory `parent`. The in-args are
// the NUL-terminated name; the reply is error-only (no out-args).
func (f *VirtioFS) Unlink(parent uint64, name string) error {
	in := append([]byte(name), 0)
	_, _, err := f.fuseRequest(FuseOpUnlink, parent, in, 0)
	return err
}

// Rmdir performs FUSE_RMDIR (Linux fuse.h, opcode 11): remove the empty
// subdirectory `name` from directory `parent`. The in-args are the
// NUL-terminated name; the reply is error-only.
func (f *VirtioFS) Rmdir(parent uint64, name string) error {
	in := append([]byte(name), 0)
	_, _, err := f.fuseRequest(FuseOpRmdir, parent, in, 0)
	return err
}

// Rename performs FUSE_RENAME (Linux fuse.h, opcode 12): move/rename
// `oldname` in directory `oldparent` to `newname` in directory
// `newparent`. The in-args are fuse_rename_in{newdir} followed by the
// NUL-terminated oldname and the NUL-terminated newname; the reply is
// error-only.
func (f *VirtioFS) Rename(oldparent uint64, oldname string, newparent uint64, newname string) error {
	ri := make([]byte, fuseRenameInSize)
	le.PutUint64(ri[0:8], newparent) // newdir
	in := append(ri, append([]byte(oldname), 0)...)
	in = append(in, append([]byte(newname), 0)...)
	_, _, err := f.fuseRequest(FuseOpRename, oldparent, in, 0)
	return err
}

// Fsync performs FUSE_FSYNC (Linux fuse.h, opcode 20) on (nodeid, fh),
// flushing the file's data (and, unless datasync, metadata) to stable
// storage. `datasync` true sets fsync_flags bit 0 (FUSE_FSYNC_FDATASYNC).
// The in-args are fuse_fsync_in{fh, fsync_flags, padding}; error-only reply.
func (f *VirtioFS) Fsync(nodeid, fh uint64, datasync bool) error {
	fi := make([]byte, fuseFsyncInSize)
	le.PutUint64(fi[0:8], fh) // fh
	if datasync {
		le.PutUint32(fi[8:12], 1) // fsync_flags: FUSE_FSYNC_FDATASYNC (bit 0)
	}
	le.PutUint32(fi[12:16], 0) // padding
	_, _, err := f.fuseRequest(FuseOpFsync, nodeid, fi, 0)
	return err
}

// Flush performs FUSE_FLUSH (Linux fuse.h, opcode 25) on (nodeid, fh),
// issued on each close(2) of a duplicated handle (one per open). The
// in-args are fuse_flush_in{fh, unused, padding, lock_owner}; error-only
// reply.
func (f *VirtioFS) Flush(nodeid, fh uint64) error {
	fi := make([]byte, fuseFlushInSize)
	le.PutUint64(fi[0:8], fh)  // fh
	le.PutUint32(fi[8:12], 0)  // unused
	le.PutUint32(fi[12:16], 0) // padding
	le.PutUint64(fi[16:24], 0) // lock_owner
	_, _, err := f.fuseRequest(FuseOpFlush, nodeid, fi, 0)
	return err
}

// Forget performs FUSE_FORGET (Linux fuse.h, opcode 2) on `nodeid`,
// telling the device to drop `nlookup` references to the node. in-args
// are fuse_forget_in{nlookup}. FUSE_FORGET has no reply at all (the
// device does not write a fuse_out_header), so this submits the request
// and reclaims without awaiting out-args.
func (f *VirtioFS) Forget(nodeid uint64, nlookup uint64) error {
	in := make([]byte, fuseForgetInSize)
	le.PutUint64(in[0:8], nlookup)
	return f.submitNoReply(FuseOpForget, nodeid, in)
}

// Destroy performs FUSE_DESTROY (Linux fuse.h, opcode 38), tearing down
// the FUSE session. No in-args; the device writes a fuse_out_header with
// no out-args.
func (f *VirtioFS) Destroy() error {
	_, _, err := f.fuseRequest(FuseOpDestroy, 0, nil, 0)
	return err
}

// submitNoReply issues a FUSE request that the device answers without a
// fuse_out_header (FUSE_FORGET / batch-forget). The chain is still a
// single readable descriptor; we wait for the used-ring completion so
// the descriptor is reclaimed, but read no reply.
func (f *VirtioFS) submitNoReply(op uint32, nodeid uint64, inArgs []byte) error {
	inLen := fuseInHeaderSize + len(inArgs)
	inPhys, inMem, err := f.allocBytes(inLen)
	if err != nil {
		return err
	}
	unique := f.nextUnique()
	le.PutUint32(inMem[0:4], uint32(inLen))
	le.PutUint32(inMem[4:8], op)
	le.PutUint64(inMem[8:16], unique)
	le.PutUint64(inMem[16:24], nodeid)
	copy(inMem[fuseInHeaderSize:inLen], inArgs)

	chain := []common.ChainBuffer{
		{Addr: uintptr(inPhys), Phys: inPhys, Len: uint32(inLen), Writable: false},
	}
	head, err := f.rq.AddChain(chain)
	if err != nil {
		return err
	}
	if err := f.Cfg.NotifyQueue(RequestQueueIdx, f.rq.NotifyOff); err != nil {
		return err
	}
	for spin := 0; spin < TxPollIterations; spin++ {
		gotIdx, _, ok := f.rq.PollUsed()
		if !ok {
			continue
		}
		_ = f.rq.ReclaimChain(gotIdx)
		return nil
	}
	_ = f.rq.ReclaimChain(head)
	return ErrRequestTimeout
}

// ParseAttr decodes the subset of a 92-byte FUSE fuse_attr (Linux
// fuse.h) the read-only surface exposes. `b` MUST be at least
// fuseAttrSize bytes (callers slice exactly that).
func ParseAttr(b []byte) Attr {
	return Attr{
		Ino:   le.Uint64(b[attrInoOffset : attrInoOffset+8]),
		Size:  le.Uint64(b[attrSizeOffset : attrSizeOffset+8]),
		Mode:  le.Uint32(b[attrModeOffset : attrModeOffset+4]),
		Nlink: le.Uint32(b[attrNlinkOffset : attrNlinkOffset+4]),
	}
}

// Sentinel errors for the virtio-fs path.
var (
	ErrNotModernDevice   = fsError("go-virtio/fs: device doesn't offer VIRTIO_F_VERSION_1 (legacy-only)")
	ErrFeaturesNotOK     = fsError("go-virtio/fs: FEATURES_OK status bit didn't stick after DriverFeature write")
	ErrInitWrongDeviceID = fsError("go-virtio/fs: PCI device ID is not 0x105A (modern virtio-fs device)")
	ErrQueueNotAvailable = fsError("go-virtio/fs: device reports QueueSize=0 for the request queue")
	ErrRequestTimeout    = fsError("go-virtio/fs: request poll timeout (device did not complete the request)")
	ErrFuseError         = fsError("go-virtio/fs: device returned a non-zero fuse_out_header.error")
	ErrFuseVersion       = fsError("go-virtio/fs: device FUSE major version is not 7")
	ErrShortReply        = fsError("go-virtio/fs: device wrote fewer reply bytes than the op requires")
	ErrNoEntry           = fsError("go-virtio/fs: FUSE_LOOKUP returned nodeid 0 (no such entry)")
	ErrZeroSize          = fsError("go-virtio/fs: Read/Write size must be positive")
	ErrShortWrite        = fsError("go-virtio/fs: device accepted fewer bytes than requested (short write)")
)

// fsError is the package's tiny sentinel-error type.
type fsError string

func (e fsError) Error() string { return string(e) }
