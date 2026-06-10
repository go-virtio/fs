// Package fs is a pure-Go virtio-fs (FUSE-over-virtio) guest driver. It
// drives a modern (Virtio 1.0+) PCI virtio-fs device through the
// transport interfaces defined in github.com/go-virtio/common; the same
// code drives a UEFI-backed device, a bare-metal device, or a
// virtio-mmio device depending on which common.Transport implementation
// the caller supplies.
//
// Scope — this package owns device bring-up, the request virtqueue, the
// FUSE-over-virtio descriptor-chain framing (readable [fuse_in_header |
// in-args], writable [fuse_out_header | out-args]; Virtio 1.2 §5.11),
// and a read-only mount closure: Init, Lookup, GetAttr, Open, Read,
// Release, Forget, Destroy. Write-side FUSE ops are out of scope.
//
//   - Modern transport (VIRTIO_F_VERSION_1 mandatory). Legacy devices
//     are rejected by the common init sequence.
//   - Queue 0 is the hiprio queue; queues 1..num_request_queues are the
//     request queues (Virtio 1.2 §5.11.2). This driver issues all FUSE
//     requests on the first request queue (index 1) and does not use the
//     hiprio queue (no FUSE_INTERRUPT / FUSE_FORGET fast-path needed for
//     a read-only mount).
//   - No virtio feature bit beyond VIRTIO_F_VERSION_1 is negotiated.
//     (VIRTIO_FS_F_NOTIFICATION exists but is not required for the
//     request path.)
//
// The FUSE protocol itself is versioned independently of virtio: this
// driver speaks FUSE major 7 and negotiates the minor down to whatever
// the device offers (FUSE_INIT, Linux fuse.h).
//
// References:
//
//   - Virtio 1.2 §5.11   "File System Device" — device-type 26 binding,
//     virtio_fs_config (tag[36] + le32 num_request_queues), queue
//     layout (hiprio + request queues).
//   - Linux include/uapi/linux/virtio_fs.h — struct virtio_fs_config.
//   - Linux include/uapi/linux/virtio_ids.h — VIRTIO_ID_FS = 26.
//   - Linux include/uapi/linux/fuse.h — every FUSE wire struct + opcode
//     cited at its use site below (FUSE_KERNEL_VERSION = 7).
//   - Virtio 1.1 §3.1.1 "Device Initialization" — the status-bit
//     choreography (shared with blk/net/console via common).
package fs

import (
	"github.com/go-virtio/common"
)

// virtio-fs device IDs live in go-virtio/common per the org single-source-of-
// truth rule: common.DeviceTypeFS (26) and common.PCIDeviceIDModernFS (0x105A =
// 0x1040 + 26; modern-only, virtio-fs postdates the legacy transport).

// HiprioQueueIdx is the index of the hiprio queue (queue 0) — used for
// FUSE_FORGET / FUSE_INTERRUPT priority requests (Virtio 1.2 §5.11.2).
// This read-only driver does not submit on it.
const HiprioQueueIdx uint16 = 0

// RequestQueueIdx is the index of the first request queue. Queue 0 is
// hiprio; request queues start at index 1 (Virtio 1.2 §5.11.2).
const RequestQueueIdx uint16 = 1

// RequestQueueSize is the desired ring size (clamped + rounded during
// setup). A FUSE request consumes 2–4 descriptors; the driver issues
// them one at a time.
const RequestQueueSize uint16 = 16

// FuseKernelVersion is the FUSE major protocol version this driver
// speaks (Linux fuse.h: FUSE_KERNEL_VERSION = 7).
const FuseKernelVersion uint32 = 7

// FuseKernelMinorVersion is the FUSE minor version the driver proposes
// in FUSE_INIT; the negotiated minor is min(proposed, device-offered)
// (Linux fuse.h: FUSE_KERNEL_MINOR_VERSION). We propose a conservative
// 31 (the baseline virtiofsd requires) and accept whatever the device
// reports back.
const FuseKernelMinorVersion uint32 = 31

// virtio_fs_config field offsets (Linux virtio_fs.h):
//
//	struct virtio_fs_config {
//	    char  tag[36];
//	    __le32 num_request_queues;
//	} __attribute__((packed));
const (
	cfgTagOffset      uint32 = 0
	cfgTagLen                = 36
	cfgNumRequestQ    uint32 = 36
	virtioFSCfgLength uint32 = 40
)

// FUSE opcodes (Linux fuse.h: enum fuse_opcode). Only the read-only
// mount subset is defined.
const (
	FuseOpLookup  uint32 = 1
	FuseOpForget  uint32 = 2
	FuseOpGetattr uint32 = 3
	FuseOpOpen    uint32 = 14
	FuseOpRead    uint32 = 15
	FuseOpRelease uint32 = 18
	FuseOpInit    uint32 = 26
	FuseOpDestroy uint32 = 38
)

// On-the-wire sizes of the FUSE structs the driver builds or parses
// (Linux fuse.h). Each is the packed byte size; the driver reads/writes
// fields at fixed little-endian offsets rather than relying on Go struct
// layout.
const (
	// fuse_in_header{ le32 len; le32 opcode; le64 unique; le64 nodeid;
	//   le32 uid; le32 gid; le32 pid; le16 total_extlen; le16 padding; }
	// = 4+4+8+8+4+4+4+2+2 = 40.
	fuseInHeaderSize = 40

	// fuse_out_header{ le32 len; s32 error; le64 unique; } = 16.
	fuseOutHeaderSize = 16

	// fuse_init_in{ le32 major; le32 minor; le32 max_readahead;
	//   le32 flags; le32 flags2; le32 unused[11]; } = 4*5 + 44 = 64.
	fuseInitInSize = 64

	// fuse_init_out is version-dependent; the device writes only as many
	// bytes as its version defines. The driver supplies a generous
	// writable buffer and reads only major/minor (offsets 0/4). 256 is
	// comfortably larger than any released fuse_init_out.
	fuseInitOutSize = 256

	// fuse_getattr_in{ le32 getattr_flags; le32 dummy; le64 fh; } = 16.
	fuseGetattrInSize = 16

	// fuse_open_in{ le32 flags; le32 open_flags; } = 8.
	fuseOpenInSize = 8

	// fuse_open_out{ le64 fh; le32 open_flags; s32 backing_id; } = 16.
	fuseOpenOutSize = 16

	// fuse_read_in{ le64 fh; le64 offset; le32 size; le32 read_flags;
	//   le64 lock_owner; le32 flags; le32 padding; } = 8+8+4+4+8+4+4 = 40.
	fuseReadInSize = 40

	// fuse_release_in{ le64 fh; le32 flags; le32 release_flags;
	//   le64 lock_owner; } = 24.
	fuseReleaseInSize = 24

	// fuse_forget_in{ le64 nlookup; } = 8.
	fuseForgetInSize = 8

	// fuse_attr{ le64 ino,size,blocks,atime,mtime,ctime; le32 atimensec,
	//   mtimensec,ctimensec,mode,nlink,uid,gid,rdev,blksize,flags; }
	// = 6*8 + 11*4 = 92.
	fuseAttrSize = 92

	// fuse_entry_out{ le64 nodeid,generation,entry_valid,attr_valid;
	//   le32 entry_valid_nsec,attr_valid_nsec; struct fuse_attr attr; }
	// = 4*8 + 2*4 + 92 = 132.
	fuseEntryOutSize = 132

	// fuse_attr_out{ le64 attr_valid; le32 attr_valid_nsec,dummy;
	//   struct fuse_attr attr; } = 8 + 4 + 4 + 92 = 108.
	fuseAttrOutSize = 108
)

// fuse_attr field offsets inside the 92-byte struct (Linux fuse.h),
// relative to the start of the fuse_attr. The struct begins with six
// le64 fields (ino,size,blocks,atime,mtime,ctime = 48 bytes), followed
// by eleven le32 fields (atimensec,mtimensec,ctimensec,mode,nlink,uid,
// gid,rdev,blksize,flags):
//
//	ino    @ 0    size   @ 8    blocks @ 16  atime @ 24  mtime @ 32  ctime @ 40
//	atimensec @ 48  mtimensec @ 52  ctimensec @ 56
//	mode @ 60  nlink @ 64  uid @ 68  gid @ 72  rdev @ 76  blksize @ 80  flags @ 84
const (
	attrInoOffset   = 0
	attrSizeOffset  = 8
	attrModeOffset  = 60
	attrNlinkOffset = 64
)

// AcceptedFeatures is the feature mask the driver negotiates ON — only
// the non-negotiable VIRTIO_F_VERSION_1.
const AcceptedFeatures uint64 = common.FeatureVersion1

// AcceptFeatures returns the negotiated mask (requires VERSION_1).
func AcceptFeatures(deviceFeatures uint64) (uint64, error) {
	if deviceFeatures&common.FeatureVersion1 == 0 {
		return 0, ErrNotModernDevice
	}
	return deviceFeatures & AcceptedFeatures, nil
}

// TxPollIterations is the default busy-poll budget for one request.
const TxPollIterations = 200000

// Attr is the decoded subset of FUSE fuse_attr (Linux fuse.h) the
// read-only mount surface exposes. Only the fields a caller needs to
// stat + read a file are decoded.
type Attr struct {
	Ino   uint64 // inode number
	Size  uint64 // file size in bytes
	Mode  uint32 // st_mode (type + permission bits)
	Nlink uint32 // link count
}

// Entry is the decoded result of a FUSE_LOOKUP (fuse_entry_out): the
// node id assigned to the looked-up name plus its attributes.
type Entry struct {
	NodeID uint64 // nodeid to use in subsequent ops (0 = negative lookup)
	Attr   Attr
}

// VirtioFS wraps one initialised virtio-fs device.
type VirtioFS struct {
	// Cfg is the modern-transport handle.
	Cfg *common.ModernConfig

	// Tag is the virtio_fs_config mount tag (Virtio 1.2 §5.11.5), the
	// name the guest mounts ("-o tag"). Trailing NULs are trimmed.
	Tag string

	// NumRequestQueues is the device-advertised request-queue count
	// (virtio_fs_config.num_request_queues).
	NumRequestQueues uint32

	// NegotiatedFeatures records the virtio feature handshake result.
	NegotiatedFeatures uint64

	// FuseMajor / FuseMinor record the negotiated FUSE protocol version
	// (populated by Init).
	FuseMajor uint32
	FuseMinor uint32

	// unique is the running FUSE request id (fuse_in_header.unique),
	// monotonically increasing per request (Linux fuse.h).
	unique uint64

	transport common.Transport
	rq        *common.Virtqueue
}

// OpenVirtioFS drives the full bring-up of one virtio-fs device:
//
//  1. Verify the PCI device ID is 0x105A (modern virtio-fs).
//  2. InitModernConfig walks PCI caps + populates the BAR locators.
//  3. Reset → ACK → DRIVER status progression.
//  4. Read DeviceFeature, require VERSION_1, mask, write DriverFeature.
//  5. Set FEATURES_OK, verify it stuck.
//  6. Read virtio_fs_config (tag + num_request_queues).
//  7. Allocate + publish the first request queue (index 1).
//  8. DRIVER_OK status.
//
// The caller next calls Init() to perform the FUSE_INIT handshake.
func OpenVirtioFS(t common.Transport) (*VirtioFS, error) {
	did, err := t.ReadConfig16(common.PCICfgDeviceID)
	if err != nil {
		return nil, err
	}
	if did != common.PCIDeviceIDModernFS {
		return nil, ErrInitWrongDeviceID
	}

	cfg, err := common.InitModernConfig(t)
	if err != nil {
		return nil, err
	}

	if err := cfg.SetDeviceStatus(0); err != nil {
		return nil, err
	}
	if _, err := cfg.DeviceStatus(); err != nil {
		return nil, err
	}
	if err := cfg.SetDeviceStatus(common.StatusAcknowledge); err != nil {
		return nil, err
	}
	if err := cfg.SetDeviceStatus(common.StatusAcknowledge | common.StatusDriver); err != nil {
		return nil, err
	}

	deviceFeats, err := cfg.DeviceFeatures64()
	if err != nil {
		return nil, err
	}
	if deviceFeats&common.FeatureVersion1 == 0 {
		return nil, ErrNotModernDevice
	}
	negotiated := deviceFeats & AcceptedFeatures
	if err := cfg.SetDriverFeatures64(negotiated); err != nil {
		return nil, err
	}

	if err := cfg.SetDeviceStatus(common.StatusAcknowledge | common.StatusDriver | common.StatusFeaturesOK); err != nil {
		return nil, err
	}
	status, err := cfg.DeviceStatus()
	if err != nil {
		return nil, err
	}
	if status&common.StatusFeaturesOK == 0 {
		return nil, ErrFeaturesNotOK
	}

	// virtio_fs_config: tag[36] then le32 num_request_queues (Virtio 1.2
	// §5.11.5, Linux virtio_fs.h).
	tag, err := readTag(cfg)
	if err != nil {
		return nil, err
	}
	numRQ, err := cfg.DeviceCfgRead32(cfgNumRequestQ)
	if err != nil {
		return nil, err
	}

	rq, err := setupQueue(cfg, t, RequestQueueIdx, RequestQueueSize)
	if err != nil {
		return nil, err
	}

	if err := cfg.SetDeviceStatus(common.StatusAcknowledge | common.StatusDriver | common.StatusFeaturesOK | common.StatusDriverOK); err != nil {
		return nil, err
	}

	return &VirtioFS{
		Cfg:                cfg,
		Tag:                tag,
		NumRequestQueues:   numRQ,
		NegotiatedFeatures: negotiated,
		transport:          t,
		rq:                 rq,
	}, nil
}

// readTag reads the 36-byte virtio_fs_config.tag and trims trailing NULs
// (the tag is NUL-padded, not NUL-terminated per se — Linux virtio_fs.h).
func readTag(cfg *common.ModernConfig) (string, error) {
	var buf [cfgTagLen]byte
	for i := uint32(0); i < cfgTagLen; i++ {
		b, err := cfg.DeviceCfgRead8(cfgTagOffset + i)
		if err != nil {
			return "", err
		}
		buf[i] = b
	}
	n := 0
	for n < len(buf) && buf[n] != 0 {
		n++
	}
	return string(buf[:n]), nil
}

// setupQueue performs the per-queue init (select, size, allocate,
// publish addresses, enable). Identical shape to blk.setupQueue.
func setupQueue(cfg *common.ModernConfig, t common.Transport, queueIdx uint16, desiredSize uint16) (*common.Virtqueue, error) {
	if err := cfg.SelectQueue(queueIdx); err != nil {
		return nil, err
	}
	maxSize, err := cfg.QueueSize()
	if err != nil {
		return nil, err
	}
	if maxSize == 0 {
		return nil, ErrQueueNotAvailable
	}
	size := desiredSize
	if size > maxSize {
		size = maxSize
	}
	for size&(size-1) != 0 {
		size &= size - 1
	}
	if err := cfg.SetQueueSize(size); err != nil {
		return nil, err
	}
	notifyOff, err := cfg.QueueNotifyOff()
	if err != nil {
		return nil, err
	}
	q, err := common.NewVirtqueue(t, size, queueIdx, notifyOff)
	if err != nil {
		return nil, err
	}
	descAddr := q.BasePhys + uint64(q.Layout.DescTableOffset)
	availAddr := q.BasePhys + uint64(q.Layout.AvailRingOffset)
	usedAddr := q.BasePhys + uint64(q.Layout.UsedRingOffset)
	if err := cfg.SetQueueDesc(descAddr); err != nil {
		return nil, err
	}
	if err := cfg.SetQueueDriver(availAddr); err != nil {
		return nil, err
	}
	if err := cfg.SetQueueDevice(usedAddr); err != nil {
		return nil, err
	}
	if err := cfg.SetQueueEnable(1); err != nil {
		return nil, err
	}
	return q, nil
}

// RequestQueue exposes the request virtqueue handle for diagnostics.
func (f *VirtioFS) RequestQueue() *common.Virtqueue { return f.rq }

// nextUnique returns a fresh fuse_in_header.unique value. Per Linux
// fuse.h the unique id identifies in-flight requests; it must be
// non-zero and increasing.
func (f *VirtioFS) nextUnique() uint64 {
	f.unique++
	return f.unique
}
