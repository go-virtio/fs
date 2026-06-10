# go-virtio/fs

Pure-Go virtio-fs (FUSE-over-virtio) guest driver targeting the
`go-virtio/common` transport interfaces. Implements the modern-transport
(Virtio 1.0+) init sequence, the request virtqueue, and the
FUSE-over-virtio wire framing for the standard PCI-bound virtio-fs device
(VID 0x1AF4, DID 0x105A — device type 26).

CGO=0, no architecture-specific assembly, 100% statement test coverage.

## Scope

Like [`go-virtio/blk`](https://github.com/go-virtio/blk) this package owns
device bring-up, a request virtqueue, and the on-the-wire request format.
For virtio-fs the request format is a FUSE message carried as a descriptor
chain (Virtio 1.2 §5.11.6):

```
readable descriptors : [ struct fuse_in_header  | op-specific in-args  ]
writable descriptors : [ struct fuse_out_header | op-specific out-args ]
```

The driver exposes a **read-only mount closure**, each op cited from
Linux `include/uapi/linux/fuse.h`:

| Method                            | FUSE opcode        | in-args / out-args                               |
| --------------------------------- | ------------------ | ------------------------------------------------ |
| `Init()`                          | `FUSE_INIT` (26)   | `fuse_init_in` → `fuse_init_out` (major 7 nego.) |
| `Lookup(parent, name)` → `Entry`  | `FUSE_LOOKUP` (1)  | NUL-terminated name → `fuse_entry_out`           |
| `GetAttr(nodeid)` → `Attr`        | `FUSE_GETATTR` (3) | `fuse_getattr_in` → `fuse_attr_out`              |
| `Open(nodeid)` → `fh`             | `FUSE_OPEN` (14)   | `fuse_open_in` → `fuse_open_out`                 |
| `Read(nodeid, fh, off, size)` → `[]byte` | `FUSE_READ` (15) | `fuse_read_in` → raw bytes                   |
| `Release(nodeid, fh)`             | `FUSE_RELEASE` (18)| `fuse_release_in` → (no out-args)                |
| `Forget(nodeid, nlookup)`         | `FUSE_FORGET` (2)  | `fuse_forget_in` → (no reply at all)             |
| `Destroy()`                       | `FUSE_DESTROY` (38)| (no in-args) → (no out-args)                     |

Write-side FUSE ops (CREATE/WRITE/MKDIR/…) are out of scope. The FUSE
protocol major is 7; the minor is negotiated down to whatever the device
offers in `FUSE_INIT`.

### Queues

Queue 0 is the **hiprio** queue (FUSE_FORGET / FUSE_INTERRUPT fast-path);
queues 1..`num_request_queues` are the **request** queues (Virtio 1.2
§5.11.2). This driver submits every request on the first request queue
(index 1) and does not use the hiprio queue.

### Device config

`virtio_fs_config` (Linux `virtio_fs.h`) is `char tag[36]` followed by
`__le32 num_request_queues`; both are read at `OpenVirtioFS` into
`VirtioFS.Tag` (trailing NULs trimmed) and `VirtioFS.NumRequestQueues`.

## Quick start

```go
import virtiofs "github.com/go-virtio/fs"

// transport is any value that implements go-virtio/common.Transport.
fs, err := virtiofs.OpenVirtioFS(transport)
if err != nil { return err }
if err := fs.Init(); err != nil { return err } // FUSE_INIT handshake

// Read /hello.txt from the shared dir (root nodeid is always 1).
e, err := fs.Lookup(1, "hello.txt")
if err != nil { return err }
fh, err := fs.Open(e.NodeID)
if err != nil { return err }
data, err := fs.Read(e.NodeID, fh, 0, uint32(e.Attr.Size))
if err != nil { return err }
_ = fs.Release(e.NodeID, fh)
fmt.Printf("%s\n", data)
```

## Device-ID centralization

Per the org convention, virtio device IDs live in `go-virtio/common`. This
driver consumes them from `common` v0.1.5, which centralizes
`DeviceTypeFS` (26), `PCIDeviceIDModernFS` (0x105A) and `PCIDeviceIDIsFS`
alongside the other `DeviceType*` / `PCIDeviceIDModern*` constants.

## Tests

```sh
GOWORK=off go test -cover ./...   # 100.0% of statements
```

White-box tests drive a fake `common.Transport` (`fakeFSDevice`) that
serves canned FUSE replies, plus an injection harness (`injectTransport`)
that fails each transport touch-point to exercise every error branch.

## References

- Virtio 1.2 §5.11 "File System Device".
- Linux `include/uapi/linux/virtio_fs.h` — `struct virtio_fs_config`.
- Linux `include/uapi/linux/virtio_ids.h` — `VIRTIO_ID_FS = 26`.
- Linux `include/uapi/linux/fuse.h` — every FUSE struct + opcode.
