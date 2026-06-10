// Transport-error + alloc-failure injection harness for the virtio-fs
// driver, mirroring go-virtio/blk's injectTransport. It wraps a
// fakeFSDevice and fails the Nth call to a named transport method (or
// returns phys=0 from AllocatePages) so every error-return branch in
// OpenVirtioFS / setupQueue / fuseRequest / submitNoReply / allocBytes
// is exercised for 100% statement coverage.

package fs

import (
	"errors"
	"testing"

	"github.com/go-virtio/common"
)

var errInjected = errors.New("injected transport failure")

type failPoint struct {
	method string
	nth    int
}

type injectTransport struct {
	*fakeFSDevice
	fp            failPoint
	counts        map[string]int
	enable        bool
	zeroPhys      bool
	zeroPhysAfter int
	allocCalls    int
}

func newInject(d *fakeFSDevice, enable bool) *injectTransport {
	return &injectTransport{fakeFSDevice: d, counts: map[string]int{}, enable: enable}
}

func (t *injectTransport) fail(m string) bool {
	if !t.enable || t.fp.method != m {
		return false
	}
	t.counts[m]++
	return t.counts[m] == t.fp.nth
}

func (t *injectTransport) ReadConfig8(o uint8) (uint8, error) {
	if t.fail("ReadConfig8") {
		return 0, errInjected
	}
	return t.fakeFSDevice.ReadConfig8(o)
}
func (t *injectTransport) ReadConfig16(o uint8) (uint16, error) {
	if t.fail("ReadConfig16") {
		return 0, errInjected
	}
	return t.fakeFSDevice.ReadConfig16(o)
}
func (t *injectTransport) Read8(b uint8, o uint64) (uint8, error) {
	if t.fail("Read8") {
		return 0, errInjected
	}
	return t.fakeFSDevice.Read8(b, o)
}
func (t *injectTransport) Read16(b uint8, o uint64) (uint16, error) {
	if t.fail("Read16") {
		return 0, errInjected
	}
	return t.fakeFSDevice.Read16(b, o)
}
func (t *injectTransport) Read32(b uint8, o uint64) (uint32, error) {
	if t.fail("Read32") {
		return 0, errInjected
	}
	return t.fakeFSDevice.Read32(b, o)
}
func (t *injectTransport) Read64(b uint8, o uint64) (uint64, error) {
	if t.fail("Read64") {
		return 0, errInjected
	}
	return t.fakeFSDevice.Read64(b, o)
}
func (t *injectTransport) Write8(b uint8, o uint64, v uint8) error {
	if t.fail("Write8") {
		return errInjected
	}
	return t.fakeFSDevice.Write8(b, o, v)
}
func (t *injectTransport) Write16(b uint8, o uint64, v uint16) error {
	if t.fail("Write16") {
		return errInjected
	}
	return t.fakeFSDevice.Write16(b, o, v)
}
func (t *injectTransport) Write32(b uint8, o uint64, v uint32) error {
	if t.fail("Write32") {
		return errInjected
	}
	return t.fakeFSDevice.Write32(b, o, v)
}
func (t *injectTransport) Write64(b uint8, o uint64, v uint64) error {
	if t.fail("Write64") {
		return errInjected
	}
	return t.fakeFSDevice.Write64(b, o, v)
}
func (t *injectTransport) AllocatePages(c int) (uint64, []byte, error) {
	if t.fail("AllocatePages") {
		return 0, nil, errInjected
	}
	phys, mem, err := t.fakeFSDevice.AllocatePages(c)
	if t.enable {
		t.allocCalls++
		if t.zeroPhys && t.allocCalls > t.zeroPhysAfter {
			return 0, mem, nil
		}
	}
	return phys, mem, err
}

// TestOpenVirtioFS_TransportErrors drives a failure at each transport
// touch-point of the bring-up sequence, asserting OpenVirtioFS surfaces
// an error. The fail-point ordinals follow the call order in
// OpenVirtioFS + setupQueue.
func TestOpenVirtioFS_TransportErrors(t *testing.T) {
	cases := []struct {
		name string
		fp   failPoint
	}{
		{"DIDRead", failPoint{"ReadConfig16", 1}},
		{"InitModernConfig", failPoint{"ReadConfig16", 2}},
		{"ResetStatus", failPoint{"Write8", 1}},
		{"PostResetStatusRead", failPoint{"Read8", 1}},
		{"AckStatus", failPoint{"Write8", 2}},
		{"DriverStatus", failPoint{"Write8", 3}},
		{"DeviceFeatures", failPoint{"Write32", 1}},
		{"DriverFeatures", failPoint{"Write32", 3}},
		{"FeaturesOKStatus", failPoint{"Write8", 4}},
		{"PostFeaturesStatusRead", failPoint{"Read8", 2}},
		{"ReadTag", failPoint{"Read8", 3}}, // first tag byte
		// DeviceFeatures64 does two Read32 (low+high words) before the
		// num_request_queues read, so the config Read32 is the 3rd.
		{"NumRequestQueues", failPoint{"Read32", 3}},
		{"SelectQueue", failPoint{"Write16", 1}},
		{"QueueSize", failPoint{"Read16", 1}},
		{"SetQueueSize", failPoint{"Write16", 2}},
		{"QueueNotifyOff", failPoint{"Read16", 2}},
		{"AllocVirtqueue", failPoint{"AllocatePages", 1}},
		{"SetQueueDesc", failPoint{"Write64", 1}},
		{"SetQueueDriver", failPoint{"Write64", 2}},
		{"SetQueueDevice", failPoint{"Write64", 3}},
		{"SetQueueEnable", failPoint{"Write16", 3}},
		{"DriverOKStatus", failPoint{"Write8", 5}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeFSDevice(common.FeatureVersion1)
			it := newInject(d, true)
			it.fp = tc.fp
			if _, err := OpenVirtioFS(it); err == nil {
				t.Fatalf("%s: expected error at %+v", tc.name, tc.fp)
			}
		})
	}
}

// openInject brings the device up with injection DISABLED, then arms the
// harness — so request-path fail-points are relative to the op under
// test, not to the Open sequence.
func openInject(t *testing.T, d *fakeFSDevice) *injectTransport {
	t.Helper()
	it := newInject(d, false)
	if _, err := OpenVirtioFS(it); err != nil {
		t.Fatalf("OpenVirtioFS: %v", err)
	}
	it.enable = true
	return it
}

func TestInit_AllocInFail(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	it := newInject(d, false)
	v, err := OpenVirtioFS(it)
	if err != nil {
		t.Fatalf("OpenVirtioFS: %v", err)
	}
	it.enable = true
	it.fp = failPoint{"AllocatePages", 1} // in-buffer alloc
	if err := v.Init(); !errors.Is(err, errInjected) {
		t.Errorf("got %v", err)
	}
}

func TestInit_AllocInZeroPhys(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	it := newInject(d, false)
	v, _ := OpenVirtioFS(it)
	it.enable = true
	it.zeroPhys = true
	it.zeroPhysAfter = 0 // first request alloc (in-buffer) returns phys 0
	if err := v.Init(); !errors.Is(err, common.ErrAllocReturnedZero) {
		t.Errorf("got %v", err)
	}
}

func TestInit_AllocOutFail(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	it := newInject(d, false)
	v, _ := OpenVirtioFS(it)
	it.enable = true
	it.fp = failPoint{"AllocatePages", 2} // out-buffer alloc (2nd this req)
	if err := v.Init(); !errors.Is(err, errInjected) {
		t.Errorf("got %v", err)
	}
}

func TestInit_AddChainFull(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	v := openFS(t, d)
	q := v.RequestQueue()
	phys, _, _ := d.AllocatePages(1)
	for i := uint16(0); i < q.Layout.Size; i++ {
		if _, err := q.AddBuffer(uintptr(phys), phys, 16, false); err != nil {
			t.Fatalf("saturate[%d]: %v", i, err)
		}
	}
	if err := v.Init(); err == nil {
		t.Error("expected AddChain queue-full error")
	}
}

func TestInit_NotifyFail(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	it := newInject(d, false)
	v, _ := OpenVirtioFS(it)
	it.enable = true
	it.fp = failPoint{"Write32", 1} // request doorbell write
	if err := v.Init(); !errors.Is(err, errInjected) {
		t.Errorf("got %v", err)
	}
}

// --- submitNoReply (Forget) injection branches ------------------------

func TestForget_AllocFail(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	it := newInject(d, false)
	v, _ := OpenVirtioFS(it)
	it.enable = true
	it.fp = failPoint{"AllocatePages", 1}
	if err := v.Forget(2, 1); !errors.Is(err, errInjected) {
		t.Errorf("got %v", err)
	}
}

func TestForget_NotifyFail(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	it := newInject(d, false)
	v, _ := OpenVirtioFS(it)
	it.enable = true
	it.fp = failPoint{"Write32", 1}
	if err := v.Forget(2, 1); !errors.Is(err, errInjected) {
		t.Errorf("got %v", err)
	}
}

func TestForget_AddChainFull(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	v := openFS(t, d)
	q := v.RequestQueue()
	phys, _, _ := d.AllocatePages(1)
	for i := uint16(0); i < q.Layout.Size; i++ {
		if _, err := q.AddBuffer(uintptr(phys), phys, 16, false); err != nil {
			t.Fatalf("saturate[%d]: %v", i, err)
		}
	}
	if err := v.Forget(2, 1); err == nil {
		t.Error("expected AddChain queue-full error")
	}
}

// --- writeRequest (Write) 3-region-chain injection branches -----------

func TestWrite_AllocInFail(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	it := newInject(d, false)
	v, _ := OpenVirtioFS(it)
	it.enable = true
	it.fp = failPoint{"AllocatePages", 1} // in-header+in-args alloc
	if _, err := v.Write(2, 42, 0, []byte("x")); !errors.Is(err, errInjected) {
		t.Errorf("got %v", err)
	}
}

func TestWrite_AllocInZeroPhys(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	it := newInject(d, false)
	v, _ := OpenVirtioFS(it)
	it.enable = true
	it.zeroPhys = true
	it.zeroPhysAfter = 0 // first write alloc (in-buffer) returns phys 0
	if _, err := v.Write(2, 42, 0, []byte("x")); !errors.Is(err, common.ErrAllocReturnedZero) {
		t.Errorf("got %v", err)
	}
}

func TestWrite_AllocDataFail(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	it := newInject(d, false)
	v, _ := OpenVirtioFS(it)
	it.enable = true
	it.fp = failPoint{"AllocatePages", 2} // data-region alloc (2nd this req)
	if _, err := v.Write(2, 42, 0, []byte("x")); !errors.Is(err, errInjected) {
		t.Errorf("got %v", err)
	}
}

func TestWrite_AllocOutFail(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	it := newInject(d, false)
	v, _ := OpenVirtioFS(it)
	it.enable = true
	it.fp = failPoint{"AllocatePages", 3} // out-buffer alloc (3rd this req)
	if _, err := v.Write(2, 42, 0, []byte("x")); !errors.Is(err, errInjected) {
		t.Errorf("got %v", err)
	}
}

func TestWrite_NotifyFail(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	it := newInject(d, false)
	v, _ := OpenVirtioFS(it)
	it.enable = true
	it.fp = failPoint{"Write32", 1} // request doorbell write
	if _, err := v.Write(2, 42, 0, []byte("x")); !errors.Is(err, errInjected) {
		t.Errorf("got %v", err)
	}
}

func TestWrite_AddChainFull(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	v := openFS(t, d)
	q := v.RequestQueue()
	phys, _, _ := d.AllocatePages(1)
	for i := uint16(0); i < q.Layout.Size; i++ {
		if _, err := q.AddBuffer(uintptr(phys), phys, 16, false); err != nil {
			t.Fatalf("saturate[%d]: %v", i, err)
		}
	}
	if _, err := v.Write(2, 42, 0, []byte("x")); err == nil {
		t.Error("expected AddChain queue-full error")
	}
}

func TestWrite_Timeout(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	v := openFS(t, d)
	d.completes = false
	if _, err := v.Write(2, 42, 0, []byte("x")); !errors.Is(err, ErrRequestTimeout) {
		t.Errorf("got %v", err)
	}
}

// TestWrite_NoReplyLenClamp drives writeRequest's got<0 clamp (replyLen=0
// < fuseOutHeaderSize) so the out slice is empty and Write sees
// ErrShortReply.
func TestWrite_NoReplyLenClamp(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.noReplyLenField = true
	v := openFS(t, d)
	if _, err := v.Write(2, 42, 0, []byte("x")); !errors.Is(err, ErrShortReply) {
		t.Errorf("got %v", err)
	}
}

// TestWrite_OverlongReplyClamp drives writeRequest's got>outArgsLen clamp.
func TestWrite_OverlongReplyClamp(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.overlongRead = true
	v := openFS(t, d)
	// overlong makes replyLen huge; got clamps to fuseWriteOutSize so the
	// reply still parses and reports the accepted size.
	if _, err := v.Write(2, 42, 0, []byte("abcd")); err != nil {
		t.Errorf("Write: %v", err)
	}
}

// TestRead_OverlongReplyClamp covers fuseRequest's got>outArgsLen clamp:
// the device claims a replyLen larger than the buffer the driver sized.
func TestRead_OverlongReplyClamp(t *testing.T) {
	d := newFakeFSDevice(common.FeatureVersion1)
	d.backing[2] = []byte("abcd")
	d.overlongRead = true
	v := openFS(t, d)
	got, err := v.Read(2, 42, 0, 4)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 4 { // clamped to the 4-byte buffer
		t.Errorf("Read clamp: got %d bytes, want 4", len(got))
	}
}
