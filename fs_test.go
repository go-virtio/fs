// Tests for the virtio-fs driver: OpenVirtioFS bring-up + the FUSE
// request path (Init / Lookup / GetAttr / Open / Read / Release /
// Forget / Destroy). fakeFSDevice is a minimal in-memory virtio-fs
// device that, on a request-queue doorbell, walks the descriptor chain,
// reads the fuse_in_header to dispatch on opcode, writes a canned reply
// (fuse_out_header + op-specific out-args) into the writable descriptor,
// and publishes a used-ring entry.
//
// White-box (package fs) so the wire offsets + sentinel errors are
// directly assertable. The device side uses unsafe to read/write guest
// memory by physical address (in this fake, phys is a real Go pointer
// produced by AllocatePages), mirroring blk's fakeBlkDevice.

package fs

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"unsafe"

	"github.com/go-virtio/common"
)

func uintptrFromSlice(b []byte) uintptr {
	if len(b) == 0 {
		return 0
	}
	return uintptr(unsafe.Pointer(&b[0]))
}

func sliceAt(phys uint64, n int) []byte {
	if phys == 0 || n <= 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(uintptr(phys))), n)
}

func TestDeviceConstants(t *testing.T) {
	if common.DeviceTypeFS != 26 {
		t.Errorf("common.DeviceTypeFS: got %d, want 26", common.DeviceTypeFS)
	}
	if common.PCIDeviceIDModernFS != 0x105A {
		t.Errorf("common.PCIDeviceIDModernFS: got 0x%x, want 0x105A", common.PCIDeviceIDModernFS)
	}
	if HiprioQueueIdx != 0 || RequestQueueIdx != 1 {
		t.Errorf("queue indices: hiprio=%d req=%d", HiprioQueueIdx, RequestQueueIdx)
	}
}

// --- fake virtio-fs device -------------------------------------------

type fakeFSDevice struct {
	mu sync.Mutex

	cfg []byte

	deviceFeatureSelect uint32
	deviceFeatures      uint64
	driverFeatures      uint64
	deviceStatus        uint8
	currentQueue        uint16

	qsize      map[uint16]uint16
	qenable    map[uint16]uint16
	qdesc      map[uint16]uint64
	qdriver    map[uint16]uint64
	qdevice    map[uint16]uint64
	qnotifyOff map[uint16]uint16

	bar map[uint64]uint64

	tag     string
	numRQ   uint32
	backing map[uint64][]byte // nodeid -> file contents (for Read)

	// reply-shaping knobs
	completes       bool
	clearFeaturesOK bool
	fuseErrno       int32  // non-zero => write this into fuse_out_header.error
	forceMajor      int32  // -1 => echo FuseKernelVersion; >=0 => force this major
	lookupNodeID    uint64 // nodeid Lookup returns (0 => negative)
	openFH          uint64
	createNodeID    uint64 // nodeid Create/Mkdir/Mknod/Symlink/Link return
	writeAccept     int32  // bytes the device "accepts" for a write; -1 => echo size
	lastWriteData   []byte // captured FUSE_WRITE data region (for verification)
	shortReply      bool   // truncate out-args (replyLen too small)
	noReplyLenField bool   // leave replyLen=0 to exercise got<0 clamp
	overlongRead    bool   // claim replyLen > buffer (exercise got>outArgsLen clamp)

	reqConsumed map[uint16]uint16

	heldPages [][]byte
	allocFail bool
}

func newFakeFSDevice(deviceFeats uint64) *fakeFSDevice {
	d := &fakeFSDevice{
		deviceFeatures: deviceFeats,
		qsize:          map[uint16]uint16{0: 32, 1: 32},
		qenable:        map[uint16]uint16{},
		qdesc:          map[uint16]uint64{},
		qdriver:        map[uint16]uint64{},
		qdevice:        map[uint16]uint64{},
		qnotifyOff:     map[uint16]uint16{0: 0, 1: 0},
		bar:            map[uint64]uint64{},
		tag:            "myfs",
		numRQ:          1,
		backing:        map[uint64][]byte{},
		completes:      true,
		forceMajor:     -1,
		lookupNodeID:   2,
		openFH:         42,
		createNodeID:   3,
		writeAccept:    -1,
		reqConsumed:    map[uint16]uint16{},
	}
	d.cfg = buildVirtioFSCfgSpace()
	return d
}

func barKey(bar uint8, off uint64) uint64 { return uint64(bar)<<48 | off }

func (d *fakeFSDevice) ReadConfig8(off uint8) (uint8, error) {
	if int(off) >= len(d.cfg) {
		return 0, errors.New("read past cfg")
	}
	return d.cfg[off], nil
}
func (d *fakeFSDevice) ReadConfig16(off uint8) (uint16, error) {
	if int(off)+2 > len(d.cfg) {
		return 0, errors.New("read past cfg")
	}
	return le.Uint16(d.cfg[off : off+2]), nil
}
func (d *fakeFSDevice) ReadConfig32(off uint8) (uint32, error) {
	if int(off)+4 > len(d.cfg) {
		return 0, errors.New("read past cfg")
	}
	return le.Uint32(d.cfg[off : off+4]), nil
}

func (d *fakeFSDevice) AllocatePages(count int) (uint64, []byte, error) {
	if d.allocFail {
		return 0, nil, errors.New("alloc fail")
	}
	mem := make([]byte, count*int(common.PageSize))
	addr := uintptr(0)
	if len(mem) > 0 {
		d.heldPages = append(d.heldPages, mem)
		addr = uintptrFromSlice(mem)
	}
	return uint64(addr), mem, nil
}

func (d *fakeFSDevice) commonCfgBAR() uint8     { return 0 }
func (d *fakeFSDevice) commonCfgOffset() uint64 { return 0 }

const deviceCfgOff = 0x8000

func (d *fakeFSDevice) Read8(bar uint8, off uint64) (uint8, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgDeviceStatus:
			return d.deviceStatus, nil
		case common.CfgConfigGeneration:
			return 0, nil
		}
	}
	// device-config window: tag[36] at deviceCfgOff..+36.
	if bar == 0 && off >= deviceCfgOff && off < deviceCfgOff+uint64(cfgTagLen) {
		i := off - deviceCfgOff
		if int(i) < len(d.tag) {
			return d.tag[i], nil
		}
		return 0, nil // NUL pad
	}
	return uint8(d.bar[barKey(bar, off)] & 0xFF), nil
}

func (d *fakeFSDevice) Read16(bar uint8, off uint64) (uint16, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgNumQueues:
			return 2, nil
		case common.CfgQueueSelect:
			return d.currentQueue, nil
		case common.CfgQueueSize:
			return d.qsize[d.currentQueue], nil
		case common.CfgQueueEnable:
			return d.qenable[d.currentQueue], nil
		case common.CfgQueueNotifyOff:
			return d.qnotifyOff[d.currentQueue], nil
		}
	}
	return uint16(d.bar[barKey(bar, off)] & 0xFFFF), nil
}

func (d *fakeFSDevice) Read32(bar uint8, off uint64) (uint32, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgDeviceFeatureSelect:
			return d.deviceFeatureSelect, nil
		case common.CfgDeviceFeature:
			if d.deviceFeatureSelect == 0 {
				return uint32(d.deviceFeatures & 0xFFFFFFFF), nil
			}
			return uint32(d.deviceFeatures >> 32), nil
		}
	}
	// device-config window: num_request_queues le32 at deviceCfgOff+36.
	if bar == 0 && off >= deviceCfgOff+uint64(cfgNumRequestQ) && off < deviceCfgOff+uint64(virtioFSCfgLength) {
		return d.numRQ, nil
	}
	return uint32(d.bar[barKey(bar, off)] & 0xFFFFFFFF), nil
}

func (d *fakeFSDevice) Read64(bar uint8, off uint64) (uint64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgQueueDesc:
			return d.qdesc[d.currentQueue], nil
		case common.CfgQueueDriver:
			return d.qdriver[d.currentQueue], nil
		case common.CfgQueueDevice:
			return d.qdevice[d.currentQueue], nil
		}
	}
	return d.bar[barKey(bar, off)], nil
}

func (d *fakeFSDevice) Write8(bar uint8, off uint64, v uint8) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() && off-d.commonCfgOffset() == common.CfgDeviceStatus {
		if v&common.StatusFeaturesOK != 0 {
			if d.clearFeaturesOK || d.driverFeatures&common.FeatureVersion1 == 0 {
				v &^= common.StatusFeaturesOK
			}
		}
		d.deviceStatus = v
		return nil
	}
	d.bar[barKey(bar, off)] = uint64(v)
	return nil
}

func (d *fakeFSDevice) Write16(bar uint8, off uint64, v uint16) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgQueueSelect:
			d.currentQueue = v
			return nil
		case common.CfgQueueSize:
			d.qsize[d.currentQueue] = v
			return nil
		case common.CfgQueueEnable:
			d.qenable[d.currentQueue] = v
			return nil
		}
	}
	if off >= 0x1000 && off < 0x2000 {
		d.handleRequest()
	}
	d.bar[barKey(bar, off)] = uint64(v)
	return nil
}

func (d *fakeFSDevice) Write32(bar uint8, off uint64, v uint32) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgDeviceFeatureSelect:
			d.deviceFeatureSelect = v
			return nil
		case common.CfgDriverFeatureSelect:
			d.bar[barKey(bar, off)] = uint64(v)
			return nil
		case common.CfgDriverFeature:
			sel := d.bar[barKey(bar, common.CfgDriverFeatureSelect)]
			if sel == 0 {
				d.driverFeatures = (d.driverFeatures &^ 0xFFFFFFFF) | uint64(v)
			} else {
				d.driverFeatures = (d.driverFeatures & 0xFFFFFFFF) | (uint64(v) << 32)
			}
			return nil
		}
	}
	if off >= 0x1000 && off < 0x2000 {
		d.handleRequest()
	}
	d.bar[barKey(bar, off)] = uint64(v)
	return nil
}

func (d *fakeFSDevice) Write64(bar uint8, off uint64, v uint64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgQueueDesc:
			d.qdesc[d.currentQueue] = v
			return nil
		case common.CfgQueueDriver:
			d.qdriver[d.currentQueue] = v
			return nil
		case common.CfgQueueDevice:
			d.qdevice[d.currentQueue] = v
			return nil
		}
	}
	d.bar[barKey(bar, off)] = v
	return nil
}

type fakeDesc struct {
	addr   uint64
	length uint32
	flags  uint16
	next   uint16
}

// handleRequest is the device side of one FUSE request on the request
// queue (index 1): walk the chain, read the in-header, dispatch on
// opcode, write the canned reply into the writable descriptor, publish
// used. Called from Write16/Write32 with d.mu held.
func (d *fakeFSDevice) handleRequest() {
	if !d.completes {
		return
	}
	const q = RequestQueueIdx
	availAddr := d.qdriver[q]
	usedAddr := d.qdevice[q]
	descAddr := d.qdesc[q]
	if availAddr == 0 || usedAddr == 0 || descAddr == 0 {
		return
	}
	size := d.qsize[q]
	availSlice := sliceAt(availAddr, 4+2*int(size))
	availIdx := le.Uint16(availSlice[2:4])
	if d.reqConsumed[q] >= availIdx {
		return
	}
	slot := d.reqConsumed[q] % size
	head := le.Uint16(availSlice[4+slot*2 : 4+slot*2+2])

	descSlice := sliceAt(descAddr, 16*int(size))
	var descs []fakeDesc
	idx := head
	for i := 0; i < int(size); i++ {
		o := int(idx) * 16
		dd := fakeDesc{
			addr:   le.Uint64(descSlice[o : o+8]),
			length: le.Uint32(descSlice[o+8 : o+12]),
			flags:  le.Uint16(descSlice[o+12 : o+14]),
			next:   le.Uint16(descSlice[o+14 : o+16]),
		}
		descs = append(descs, dd)
		if dd.flags&common.VirtqDescFNext == 0 {
			break
		}
		idx = dd.next
	}

	// First descriptor is the readable in-header (+ in-args).
	inDesc := descs[0]
	inBuf := sliceAt(inDesc.addr, int(inDesc.length))
	op := le.Uint32(inBuf[4:8])
	nodeid := le.Uint64(inBuf[16:24])

	// Locate the writable out descriptor (if any).
	var outDesc *fakeDesc
	for i := range descs {
		if descs[i].flags&common.VirtqDescFWrite != 0 {
			outDesc = &descs[i]
			break
		}
	}

	usedLen := uint32(0)
	if outDesc != nil {
		usedLen = d.writeReply(op, nodeid, inBuf, outDesc, descs)
	}

	usedSlice := sliceAt(usedAddr, 4+8*int(size))
	usedIdx := le.Uint16(usedSlice[2:4])
	uslot := usedIdx % size
	uo := 4 + int(uslot)*8
	le.PutUint32(usedSlice[uo:uo+4], uint32(head))
	le.PutUint32(usedSlice[uo+4:uo+8], usedLen)
	le.PutUint16(usedSlice[2:4], usedIdx+1)
	d.reqConsumed[q]++
}

// writeReply fills the writable descriptor with fuse_out_header +
// op-specific out-args and returns the total bytes written (used-ring
// len). Honours d.fuseErrno / d.shortReply / d.noReplyLenField.
func (d *fakeFSDevice) writeReply(op uint32, nodeid uint64, inBuf []byte, outDesc *fakeDesc, descs []fakeDesc) uint32 {
	out := sliceAt(outDesc.addr, int(outDesc.length))
	unique := le.Uint64(inBuf[8:16])

	if d.fuseErrno != 0 {
		// Error reply: header only, error set.
		le.PutUint32(out[0:4], uint32(fuseOutHeaderSize))
		le.PutUint32(out[4:8], uint32(d.fuseErrno))
		le.PutUint64(out[8:16], unique)
		return uint32(fuseOutHeaderSize)
	}

	var args []byte
	switch op {
	case FuseOpInit:
		// Echo a full fuse_init_out prefix: major, minor, max_readahead,
		// flags. flags at offset 12 lets the driver record FuseFlags.
		args = make([]byte, 16)
		major := FuseKernelVersion
		if d.forceMajor >= 0 {
			major = uint32(d.forceMajor)
		}
		le.PutUint32(args[0:4], major)
		le.PutUint32(args[4:8], FuseKernelMinorVersion)
		le.PutUint32(args[8:12], 0)              // max_readahead
		le.PutUint32(args[12:16], FuseInitFlags) // offer everything proposed
	case FuseOpLookup:
		args = make([]byte, fuseEntryOutSize)
		le.PutUint64(args[0:8], d.lookupNodeID)
		// attr at offset 40; set size + mode for verification.
		d.fillAttr(args[40:40+fuseAttrSize], d.lookupNodeID, 1234, 0o100644)
	case FuseOpGetattr:
		args = make([]byte, fuseAttrOutSize)
		// attr at offset 16.
		d.fillAttr(args[16:16+fuseAttrSize], nodeid, 4096, 0o100644)
	case FuseOpOpen:
		args = make([]byte, fuseOpenOutSize)
		le.PutUint64(args[0:8], d.openFH)
	case FuseOpRead:
		// raw file bytes from backing[nodeid], honouring fh-offset-size.
		off := le.Uint64(inBuf[fuseInHeaderSize+8 : fuseInHeaderSize+16])
		size := le.Uint32(inBuf[fuseInHeaderSize+16 : fuseInHeaderSize+20])
		content := d.backing[nodeid]
		var chunk []byte
		if off < uint64(len(content)) {
			end := off + uint64(size)
			if end > uint64(len(content)) {
				end = uint64(len(content))
			}
			chunk = content[off:end]
		}
		args = chunk
	case FuseOpWrite:
		// The data region is the second readable descriptor (descs[1]).
		// fuse_write_in.size is at in-args offset 16.
		reqSize := le.Uint32(inBuf[fuseInHeaderSize+16 : fuseInHeaderSize+20])
		if len(descs) >= 2 {
			dataDesc := descs[1]
			d.lastWriteData = append([]byte(nil), sliceAt(dataDesc.addr, int(dataDesc.length))...)
		}
		accepted := reqSize
		if d.writeAccept >= 0 {
			accepted = uint32(d.writeAccept)
		}
		args = make([]byte, fuseWriteOutSize)
		le.PutUint32(args[0:4], accepted) // fuse_write_out.size
	case FuseOpCreate:
		// fuse_entry_out (132) immediately followed by fuse_open_out (16).
		args = make([]byte, fuseEntryOutSize+fuseOpenOutSize)
		le.PutUint64(args[0:8], d.createNodeID)
		d.fillAttr(args[40:40+fuseAttrSize], d.createNodeID, 0, 0o100644)
		le.PutUint64(args[fuseEntryOutSize:fuseEntryOutSize+8], d.openFH)
	case FuseOpMkdir, FuseOpMknod, FuseOpSymlink, FuseOpLink:
		args = make([]byte, fuseEntryOutSize)
		le.PutUint64(args[0:8], d.createNodeID)
		d.fillAttr(args[40:40+fuseAttrSize], d.createNodeID, 0, 0o040755)
	case FuseOpSetattr:
		args = make([]byte, fuseAttrOutSize)
		// Echo the requested size/mode back in the attr (offset 16).
		newSize := le.Uint64(inBuf[fuseInHeaderSize+setattrSizeOffset : fuseInHeaderSize+setattrSizeOffset+8])
		newMode := le.Uint32(inBuf[fuseInHeaderSize+setattrModeOffset : fuseInHeaderSize+setattrModeOffset+4])
		d.fillAttr(args[16:16+fuseAttrSize], nodeid, newSize, newMode)
	case FuseOpRelease, FuseOpDestroy, FuseOpUnlink, FuseOpRmdir, FuseOpRename, FuseOpFsync, FuseOpFlush:
		args = nil // no out-args (error-only reply)
	default:
		args = nil
	}

	replyLen := uint32(fuseOutHeaderSize + len(args))
	if d.overlongRead {
		// Claim more bytes than the writable buffer holds; the driver
		// must clamp got to outArgsLen (the size it requested).
		replyLen = uint32(fuseOutHeaderSize + len(out))
	}
	if d.shortReply {
		// Claim a longer reply than we wrote args for is the inverse;
		// here we under-report so the driver's ErrShortReply path fires
		// by writing fewer out-arg bytes than the op requires.
		replyLen = uint32(fuseOutHeaderSize + 4) // tiny
		args = args[:0]
		le.PutUint32(out[fuseOutHeaderSize:fuseOutHeaderSize+4], 0)
	}
	if d.noReplyLenField {
		replyLen = 0 // exercises got<0 clamp in fuseRequest
	}

	le.PutUint32(out[0:4], replyLen)
	le.PutUint32(out[4:8], 0) // error = 0
	le.PutUint64(out[8:16], unique)
	copy(out[fuseOutHeaderSize:], args)
	if d.noReplyLenField {
		return 0
	}
	return replyLen
}

func (d *fakeFSDevice) fillAttr(b []byte, ino, size uint64, mode uint32) {
	le.PutUint64(b[attrInoOffset:attrInoOffset+8], ino)
	le.PutUint64(b[attrSizeOffset:attrSizeOffset+8], size)
	le.PutUint32(b[attrModeOffset:attrModeOffset+4], mode)
	le.PutUint32(b[attrNlinkOffset:attrNlinkOffset+4], 1)
}

func buildVirtioFSCfgSpace() []byte {
	cfg := make([]byte, 256)
	le.PutUint16(cfg[0:], common.PCIVendorID)
	le.PutUint16(cfg[2:], common.PCIDeviceIDModernFS)
	le.PutUint16(cfg[6:], common.PCIStatusCapabilityList)
	cfg[0x34] = 0x40

	cfg[0x40] = common.PCICapIDVendorSpecific
	cfg[0x41] = 0x50
	cfg[0x42] = 16
	cfg[0x43] = common.PCICapCommonCfg
	le.PutUint32(cfg[0x48:], 0)
	le.PutUint32(cfg[0x4C:], 0x38)

	cfg[0x50] = common.PCICapIDVendorSpecific
	cfg[0x51] = 0x68
	cfg[0x52] = 20
	cfg[0x53] = common.PCICapNotifyCfg
	le.PutUint32(cfg[0x58:], 0x1000)
	le.PutUint32(cfg[0x5C:], 0x100)
	le.PutUint32(cfg[0x60:], 4)

	cfg[0x68] = common.PCICapIDVendorSpecific
	cfg[0x69] = 0x00
	cfg[0x6A] = 16
	cfg[0x6B] = common.PCICapDeviceCfg
	le.PutUint32(cfg[0x70:], deviceCfgOff)
	le.PutUint32(cfg[0x74:], virtioFSCfgLength)

	return cfg
}

// --- bring-up happy path + semantics ---------------------------------

func TestOpenVirtioFS_Success(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	v, err := OpenVirtioFS(d)
	if err != nil {
		t.Fatalf("OpenVirtioFS: %v", err)
	}
	if v.Tag != "myfs" {
		t.Errorf("Tag: got %q, want myfs", v.Tag)
	}
	if v.NumRequestQueues != 1 {
		t.Errorf("NumRequestQueues: got %d, want 1", v.NumRequestQueues)
	}
	if v.RequestQueue() == nil {
		t.Error("RequestQueue nil")
	}
	if v.NegotiatedFeatures != common.FeatureVersion1 {
		t.Errorf("NegotiatedFeatures: got 0x%x", v.NegotiatedFeatures)
	}
}

func TestAcceptFeatures(t *testing.T) {
	if got, err := AcceptFeatures(common.FeatureVersion1); err != nil || got != common.FeatureVersion1 {
		t.Errorf("modern: got 0x%x, %v", got, err)
	}
	if _, err := AcceptFeatures(0); !errors.Is(err, ErrNotModernDevice) {
		t.Errorf("legacy: got %v", err)
	}
}

func TestOpenVirtioFS_WrongDeviceID(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	le.PutUint16(d.cfg[2:], common.PCIDeviceIDModernBlock)
	if _, err := OpenVirtioFS(d); !errors.Is(err, ErrInitWrongDeviceID) {
		t.Errorf("got %v", err)
	}
}

func TestOpenVirtioFS_LegacyDevice(t *testing.T) {
	d := newFakeFSDevice(0) // no VERSION_1
	if _, err := OpenVirtioFS(d); !errors.Is(err, ErrNotModernDevice) {
		t.Errorf("got %v", err)
	}
}

func TestOpenVirtioFS_FeaturesNotOK(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.clearFeaturesOK = true
	if _, err := OpenVirtioFS(d); !errors.Is(err, ErrFeaturesNotOK) {
		t.Errorf("got %v", err)
	}
}

func TestOpenVirtioFS_QueueZeroSize(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.qsize[1] = 0
	if _, err := OpenVirtioFS(d); !errors.Is(err, ErrQueueNotAvailable) {
		t.Errorf("got %v", err)
	}
}

func TestOpenVirtioFS_QueueSizeClampAndRound(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.qsize[1] = 6 // clamp 16->6, round 6->4
	v, err := OpenVirtioFS(d)
	if err != nil {
		t.Fatalf("OpenVirtioFS: %v", err)
	}
	if got := v.RequestQueue().Layout.Size; got != 4 {
		t.Errorf("queue size: got %d, want 4", got)
	}
}

func TestReadTag_FullLength(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	// 36-char tag (no NUL padding) exercises the n==len(buf) loop bound.
	d.tag = "0123456789012345678901234567890123456789"[:cfgTagLen]
	v, err := OpenVirtioFS(d)
	if err != nil {
		t.Fatalf("OpenVirtioFS: %v", err)
	}
	if len(v.Tag) != cfgTagLen {
		t.Errorf("Tag len: got %d, want %d", len(v.Tag), cfgTagLen)
	}
}

// --- FUSE op round-trips ----------------------------------------------

func openFS(t *testing.T, d *fakeFSDevice) *VirtioFS {
	t.Helper()
	v, err := OpenVirtioFS(d)
	if err != nil {
		t.Fatalf("OpenVirtioFS: %v", err)
	}
	return v
}

func TestInit_RoundTrip(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	v := openFS(t, d)
	if err := v.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if v.FuseMajor != FuseKernelVersion {
		t.Errorf("FuseMajor: got %d, want %d", v.FuseMajor, FuseKernelVersion)
	}
	if v.FuseMinor != FuseKernelMinorVersion {
		t.Errorf("FuseMinor: got %d", v.FuseMinor)
	}
}

func TestInit_WrongMajor(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.forceMajor = 8
	v := openFS(t, d)
	if err := v.Init(); !errors.Is(err, ErrFuseVersion) {
		t.Errorf("got %v", err)
	}
}

func TestInit_FuseError(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.fuseErrno = -5 // -EIO
	v := openFS(t, d)
	if err := v.Init(); !errors.Is(err, ErrFuseError) {
		t.Errorf("got %v", err)
	}
}

func TestInit_ShortReply(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.shortReply = true
	v := openFS(t, d)
	if err := v.Init(); !errors.Is(err, ErrShortReply) {
		t.Errorf("got %v", err)
	}
}

func TestLookup_RoundTrip(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.lookupNodeID = 7
	v := openFS(t, d)
	e, err := v.Lookup(1, "file.txt")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if e.NodeID != 7 {
		t.Errorf("NodeID: got %d, want 7", e.NodeID)
	}
	if e.Attr.Size != 1234 {
		t.Errorf("Attr.Size: got %d, want 1234", e.Attr.Size)
	}
	if e.Attr.Mode != 0o100644 {
		t.Errorf("Attr.Mode: got 0o%o", e.Attr.Mode)
	}
}

func TestLookup_NegativeEntry(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.lookupNodeID = 0
	v := openFS(t, d)
	if _, err := v.Lookup(1, "missing"); !errors.Is(err, ErrNoEntry) {
		t.Errorf("got %v", err)
	}
}

func TestLookup_ShortReply(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.shortReply = true
	v := openFS(t, d)
	if _, err := v.Lookup(1, "x"); !errors.Is(err, ErrShortReply) {
		t.Errorf("got %v", err)
	}
}

func TestLookup_FuseError(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.fuseErrno = -2 // -ENOENT
	v := openFS(t, d)
	if _, err := v.Lookup(1, "x"); !errors.Is(err, ErrFuseError) {
		t.Errorf("got %v", err)
	}
}

func TestGetAttr_RoundTrip(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	v := openFS(t, d)
	a, err := v.GetAttr(2)
	if err != nil {
		t.Fatalf("GetAttr: %v", err)
	}
	if a.Ino != 2 || a.Size != 4096 || a.Mode != 0o100644 {
		t.Errorf("attr: %+v", a)
	}
}

func TestGetAttr_ShortReply(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.shortReply = true
	v := openFS(t, d)
	if _, err := v.GetAttr(2); !errors.Is(err, ErrShortReply) {
		t.Errorf("got %v", err)
	}
}

func TestGetAttr_FuseError(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.fuseErrno = -2 // -ENOENT
	v := openFS(t, d)
	if _, err := v.GetAttr(2); !errors.Is(err, ErrFuseError) {
		t.Errorf("got %v", err)
	}
}

func TestOpen_FuseError(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.fuseErrno = -13 // -EACCES
	v := openFS(t, d)
	if _, err := v.Open(2); !errors.Is(err, ErrFuseError) {
		t.Errorf("got %v", err)
	}
}

func TestOpen_RoundTrip(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.openFH = 99
	v := openFS(t, d)
	fh, err := v.Open(2)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if fh != 99 {
		t.Errorf("fh: got %d, want 99", fh)
	}
}

func TestOpen_ShortReply(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.shortReply = true
	v := openFS(t, d)
	if _, err := v.Open(2); !errors.Is(err, ErrShortReply) {
		t.Errorf("got %v", err)
	}
}

func TestRead_RoundTrip(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	content := []byte("hello virtio-fs world")
	d.backing[2] = content
	v := openFS(t, d)
	got, err := v.Read(2, 42, 0, uint32(len(content)))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("Read: got %q, want %q", got, content)
	}
}

func TestRead_ShortAtEOF(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	content := []byte("abc")
	d.backing[2] = content
	v := openFS(t, d)
	got, err := v.Read(2, 42, 1, 100) // offset 1, ask 100 -> "bc"
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(got, []byte("bc")) {
		t.Errorf("Read: got %q, want bc", got)
	}
}

func TestRead_PastEOF(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.backing[2] = []byte("abc")
	v := openFS(t, d)
	got, err := v.Read(2, 42, 99, 10) // offset past end -> empty
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Read past EOF: got %q, want empty", got)
	}
}

func TestRead_ZeroSize(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	v := openFS(t, d)
	if _, err := v.Read(2, 42, 0, 0); !errors.Is(err, ErrZeroSize) {
		t.Errorf("got %v", err)
	}
}

func TestRead_FuseError(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.fuseErrno = -9 // -EBADF
	v := openFS(t, d)
	if _, err := v.Read(2, 42, 0, 4); !errors.Is(err, ErrFuseError) {
		t.Errorf("got %v", err)
	}
}

func TestRelease_RoundTrip(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	v := openFS(t, d)
	if err := v.Release(2, 42); err != nil {
		t.Errorf("Release: %v", err)
	}
}

func TestDestroy_RoundTrip(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	v := openFS(t, d)
	if err := v.Destroy(); err != nil {
		t.Errorf("Destroy: %v", err)
	}
}

func TestForget_RoundTrip(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	v := openFS(t, d)
	if err := v.Forget(2, 1); err != nil {
		t.Errorf("Forget: %v", err)
	}
}

// --- read-write closure round-trips -----------------------------------

func TestInit_NegotiatesFuseFlags(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	v := openFS(t, d)
	if err := v.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if v.FuseFlags != FuseInitFlags {
		t.Errorf("FuseFlags: got 0x%x, want 0x%x", v.FuseFlags, FuseInitFlags)
	}
	if FuseInitFlags&FuseWritebackCach != 0 {
		t.Error("FuseWritebackCach must not be proposed")
	}
}

func TestOpenRW_RoundTrip(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.openFH = 77
	v := openFS(t, d)
	fh, err := v.OpenRW(2, OpenReadWrite)
	if err != nil {
		t.Fatalf("OpenRW: %v", err)
	}
	if fh != 77 {
		t.Errorf("fh: got %d, want 77", fh)
	}
}

func TestOpenRW_ShortReply(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.shortReply = true
	v := openFS(t, d)
	if _, err := v.OpenRW(2, OpenWriteOnly); !errors.Is(err, ErrShortReply) {
		t.Errorf("got %v", err)
	}
}

func TestWrite_RoundTrip(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	v := openFS(t, d)
	data := []byte("hello write path")
	n, err := v.Write(2, 42, 8, data)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(data) {
		t.Errorf("n: got %d, want %d", n, len(data))
	}
	if !bytes.Equal(d.lastWriteData, data) {
		t.Errorf("device data region: got %q, want %q", d.lastWriteData, data)
	}
}

func TestWrite_ZeroSize(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	v := openFS(t, d)
	if _, err := v.Write(2, 42, 0, nil); !errors.Is(err, ErrZeroSize) {
		t.Errorf("got %v", err)
	}
}

func TestWrite_ShortWrite(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.writeAccept = 2 // accept fewer than requested
	v := openFS(t, d)
	n, err := v.Write(2, 42, 0, []byte("abcdef"))
	if !errors.Is(err, ErrShortWrite) {
		t.Errorf("got %v", err)
	}
	if n != 2 {
		t.Errorf("n: got %d, want 2", n)
	}
}

func TestWrite_ShortReply(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.shortReply = true
	v := openFS(t, d)
	if _, err := v.Write(2, 42, 0, []byte("x")); !errors.Is(err, ErrShortReply) {
		t.Errorf("got %v", err)
	}
}

func TestWrite_FuseError(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.fuseErrno = -28 // -ENOSPC
	v := openFS(t, d)
	if _, err := v.Write(2, 42, 0, []byte("x")); !errors.Is(err, ErrFuseError) {
		t.Errorf("got %v", err)
	}
}

func TestCreate_RoundTrip(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.createNodeID = 9
	d.openFH = 55
	v := openFS(t, d)
	e, fh, err := v.Create(1, "new.txt", 0o100644, OpenReadWrite)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if e.NodeID != 9 || fh != 55 {
		t.Errorf("Create: nodeid=%d fh=%d, want 9/55", e.NodeID, fh)
	}
	if e.Attr.Mode != 0o100644 {
		t.Errorf("Attr.Mode: got 0o%o", e.Attr.Mode)
	}
}

func TestCreate_ShortReply(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.shortReply = true
	v := openFS(t, d)
	if _, _, err := v.Create(1, "x", 0o100644, OpenWriteOnly); !errors.Is(err, ErrShortReply) {
		t.Errorf("got %v", err)
	}
}

func TestCreate_FuseError(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.fuseErrno = -17 // -EEXIST
	v := openFS(t, d)
	if _, _, err := v.Create(1, "x", 0o100644, 0); !errors.Is(err, ErrFuseError) {
		t.Errorf("got %v", err)
	}
}

func TestMkdir_RoundTrip(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.createNodeID = 11
	v := openFS(t, d)
	e, err := v.Mkdir(1, "sub", 0o755)
	if err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if e.NodeID != 11 {
		t.Errorf("NodeID: got %d, want 11", e.NodeID)
	}
}

func TestMkdir_NegativeEntry(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.createNodeID = 0
	v := openFS(t, d)
	if _, err := v.Mkdir(1, "sub", 0o755); !errors.Is(err, ErrNoEntry) {
		t.Errorf("got %v", err)
	}
}

func TestMkdir_ShortReply(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.shortReply = true
	v := openFS(t, d)
	if _, err := v.Mkdir(1, "sub", 0o755); !errors.Is(err, ErrShortReply) {
		t.Errorf("got %v", err)
	}
}

func TestMkdir_FuseError(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.fuseErrno = -13
	v := openFS(t, d)
	if _, err := v.Mkdir(1, "sub", 0o755); !errors.Is(err, ErrFuseError) {
		t.Errorf("got %v", err)
	}
}

func TestMknod_RoundTrip(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.createNodeID = 12
	v := openFS(t, d)
	e, err := v.Mknod(1, "fifo", 0o010644, 0)
	if err != nil {
		t.Fatalf("Mknod: %v", err)
	}
	if e.NodeID != 12 {
		t.Errorf("NodeID: got %d, want 12", e.NodeID)
	}
}

func TestMknod_NegativeEntry(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.createNodeID = 0
	v := openFS(t, d)
	if _, err := v.Mknod(1, "fifo", 0o010644, 0); !errors.Is(err, ErrNoEntry) {
		t.Errorf("got %v", err)
	}
}

func TestMknod_ShortReply(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.shortReply = true
	v := openFS(t, d)
	if _, err := v.Mknod(1, "fifo", 0o010644, 0); !errors.Is(err, ErrShortReply) {
		t.Errorf("got %v", err)
	}
}

func TestMknod_FuseError(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.fuseErrno = -1
	v := openFS(t, d)
	if _, err := v.Mknod(1, "fifo", 0o010644, 0); !errors.Is(err, ErrFuseError) {
		t.Errorf("got %v", err)
	}
}

func TestSymlink_RoundTrip(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.createNodeID = 13
	v := openFS(t, d)
	e, err := v.Symlink(1, "lnk", "/target/path")
	if err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if e.NodeID != 13 {
		t.Errorf("NodeID: got %d, want 13", e.NodeID)
	}
}

func TestSymlink_NegativeEntry(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.createNodeID = 0
	v := openFS(t, d)
	if _, err := v.Symlink(1, "lnk", "t"); !errors.Is(err, ErrNoEntry) {
		t.Errorf("got %v", err)
	}
}

func TestSymlink_ShortReply(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.shortReply = true
	v := openFS(t, d)
	if _, err := v.Symlink(1, "lnk", "t"); !errors.Is(err, ErrShortReply) {
		t.Errorf("got %v", err)
	}
}

func TestSymlink_FuseError(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.fuseErrno = -13
	v := openFS(t, d)
	if _, err := v.Symlink(1, "lnk", "t"); !errors.Is(err, ErrFuseError) {
		t.Errorf("got %v", err)
	}
}

func TestLink_RoundTrip(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.createNodeID = 2 // hard link to existing node
	v := openFS(t, d)
	e, err := v.Link(2, 1, "alias")
	if err != nil {
		t.Fatalf("Link: %v", err)
	}
	if e.NodeID != 2 {
		t.Errorf("NodeID: got %d, want 2", e.NodeID)
	}
}

func TestLink_NegativeEntry(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.createNodeID = 0
	v := openFS(t, d)
	if _, err := v.Link(2, 1, "alias"); !errors.Is(err, ErrNoEntry) {
		t.Errorf("got %v", err)
	}
}

func TestLink_ShortReply(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.shortReply = true
	v := openFS(t, d)
	if _, err := v.Link(2, 1, "alias"); !errors.Is(err, ErrShortReply) {
		t.Errorf("got %v", err)
	}
}

func TestLink_FuseError(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.fuseErrno = -31 // -EMLINK
	v := openFS(t, d)
	if _, err := v.Link(2, 1, "alias"); !errors.Is(err, ErrFuseError) {
		t.Errorf("got %v", err)
	}
}

func TestSetAttr_Truncate(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	v := openFS(t, d)
	a, err := v.SetAttr(2, SetAttrIn{Valid: FattrSize, Size: 4096})
	if err != nil {
		t.Fatalf("SetAttr: %v", err)
	}
	if a.Size != 4096 {
		t.Errorf("Size: got %d, want 4096", a.Size)
	}
}

func TestSetAttr_ChmodChownUtimes(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	v := openFS(t, d)
	in := SetAttrIn{
		Valid: FattrMode | FattrUID | FattrGID | FattrAtime | FattrMtime | FattrFh,
		Fh:    42,
		Mode:  0o100600,
		UID:   1000,
		GID:   1000,
		Atime: 111,
		Mtime: 222,
	}
	a, err := v.SetAttr(2, in)
	if err != nil {
		t.Fatalf("SetAttr: %v", err)
	}
	if a.Mode != 0o100600 {
		t.Errorf("Mode: got 0o%o, want 0o100600", a.Mode)
	}
}

func TestSetAttr_ShortReply(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.shortReply = true
	v := openFS(t, d)
	if _, err := v.SetAttr(2, SetAttrIn{Valid: FattrSize}); !errors.Is(err, ErrShortReply) {
		t.Errorf("got %v", err)
	}
}

func TestSetAttr_FuseError(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.fuseErrno = -1
	v := openFS(t, d)
	if _, err := v.SetAttr(2, SetAttrIn{Valid: FattrMode, Mode: 0o600}); !errors.Is(err, ErrFuseError) {
		t.Errorf("got %v", err)
	}
}

func TestUnlink_RoundTrip(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	v := openFS(t, d)
	if err := v.Unlink(1, "gone.txt"); err != nil {
		t.Errorf("Unlink: %v", err)
	}
}

func TestUnlink_FuseError(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.fuseErrno = -2 // -ENOENT
	v := openFS(t, d)
	if err := v.Unlink(1, "gone.txt"); !errors.Is(err, ErrFuseError) {
		t.Errorf("got %v", err)
	}
}

func TestRmdir_RoundTrip(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	v := openFS(t, d)
	if err := v.Rmdir(1, "sub"); err != nil {
		t.Errorf("Rmdir: %v", err)
	}
}

func TestRmdir_FuseError(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.fuseErrno = -39 // -ENOTEMPTY
	v := openFS(t, d)
	if err := v.Rmdir(1, "sub"); !errors.Is(err, ErrFuseError) {
		t.Errorf("got %v", err)
	}
}

func TestRename_RoundTrip(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	v := openFS(t, d)
	if err := v.Rename(1, "old", 5, "new"); err != nil {
		t.Errorf("Rename: %v", err)
	}
}

func TestRename_FuseError(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.fuseErrno = -2
	v := openFS(t, d)
	if err := v.Rename(1, "old", 5, "new"); !errors.Is(err, ErrFuseError) {
		t.Errorf("got %v", err)
	}
}

func TestFsync_RoundTrip(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	v := openFS(t, d)
	if err := v.Fsync(2, 42, false); err != nil {
		t.Errorf("Fsync: %v", err)
	}
}

func TestFsync_Datasync(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	v := openFS(t, d)
	if err := v.Fsync(2, 42, true); err != nil {
		t.Errorf("Fsync datasync: %v", err)
	}
}

func TestFsync_FuseError(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.fuseErrno = -5
	v := openFS(t, d)
	if err := v.Fsync(2, 42, false); !errors.Is(err, ErrFuseError) {
		t.Errorf("got %v", err)
	}
}

func TestFlush_RoundTrip(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	v := openFS(t, d)
	if err := v.Flush(2, 42); err != nil {
		t.Errorf("Flush: %v", err)
	}
}

func TestFlush_FuseError(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.fuseErrno = -5
	v := openFS(t, d)
	if err := v.Flush(2, 42); !errors.Is(err, ErrFuseError) {
		t.Errorf("got %v", err)
	}
}

// noReplyLenField exercises the got<0 clamp in fuseRequest (replyLen=0 <
// fuseOutHeaderSize). Init then sees an empty out slice -> ErrShortReply.
func TestRequest_NoReplyLenClamp(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.noReplyLenField = true
	v := openFS(t, d)
	if err := v.Init(); !errors.Is(err, ErrShortReply) {
		t.Errorf("got %v", err)
	}
}

// --- timeout + alloc failure branches ---------------------------------

func TestRequest_Timeout(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	v := openFS(t, d)
	d.completes = false
	if err := v.Init(); !errors.Is(err, ErrRequestTimeout) {
		t.Errorf("got %v", err)
	}
}

func TestNoReply_Timeout(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	v := openFS(t, d)
	d.completes = false
	if err := v.Forget(2, 1); !errors.Is(err, ErrRequestTimeout) {
		t.Errorf("got %v", err)
	}
}

func TestSentinelError(t *testing.T) {
	if got := ErrFuseError.Error(); got != string(ErrFuseError) {
		t.Errorf("Error(): %q", got)
	}
}
