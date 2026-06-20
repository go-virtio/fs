<p align="center"><img src="https://raw.githubusercontent.com/go-virtio/brand/main/social/go-virtio-fs.png" alt="go-virtio/fs" width="720"></p>

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

The driver exposes a **read-write mount closure**, each op cited from
Linux `include/uapi/linux/fuse.h`.

Read-side ops:

| Method                            | FUSE opcode        | in-args / out-args                               |
| --------------------------------- | ------------------ | ------------------------------------------------ |
| `Init()`                          | `FUSE_INIT` (26)   | `fuse_init_in` → `fuse_init_out` (major 7 nego.) |
| `Lookup(parent, name)` → `Entry`  | `FUSE_LOOKUP` (1)  | NUL-terminated name → `fuse_entry_out`           |
| `GetAttr(nodeid)` → `Attr`        | `FUSE_GETATTR` (3) | `fuse_getattr_in` → `fuse_attr_out`              |
| `Open(nodeid)` → `fh`             | `FUSE_OPEN` (14)   | `fuse_open_in` (O_RDONLY) → `fuse_open_out`      |
| `Read(nodeid, fh, off, size)` → `[]byte` | `FUSE_READ` (15) | `fuse_read_in` → raw bytes                   |
| `Release(nodeid, fh)`             | `FUSE_RELEASE` (18)| `fuse_release_in` → (no out-args)                |
| `Forget(nodeid, nlookup)`         | `FUSE_FORGET` (2)  | `fuse_forget_in` → (no reply at all)             |
| `Destroy()`                       | `FUSE_DESTROY` (38)| (no in-args) → (no out-args)                     |

Write-side ops (added in v0.2.0):

| Method                                          | FUSE opcode        | in-args / out-args                                                   |
| ----------------------------------------------- | ------------------ | ------------------------------------------------------------------- |
| `OpenRW(nodeid, flags)` → `fh`                  | `FUSE_OPEN` (14)   | `fuse_open_in` (O_WRONLY/O_RDWR) → `fuse_open_out`                   |
| `Write(nodeid, fh, off, data)` → `n`            | `FUSE_WRITE` (16)  | `fuse_write_in` + **data region** (3-region chain) → `fuse_write_out` |
| `Create(parent, name, mode, flags)` → `(Entry, fh)` | `FUSE_CREATE` (35) | `fuse_create_in` + name → `fuse_entry_out` ‖ `fuse_open_out`     |
| `Mkdir(parent, name, mode)` → `Entry`           | `FUSE_MKDIR` (9)   | `fuse_mkdir_in` + name → `fuse_entry_out`                           |
| `Mknod(parent, name, mode, rdev)` → `Entry`     | `FUSE_MKNOD` (8)   | `fuse_mknod_in` + name → `fuse_entry_out`                           |
| `Symlink(parent, name, target)` → `Entry`       | `FUSE_SYMLINK` (6) | name + target → `fuse_entry_out`                                    |
| `Link(oldnodeid, newparent, newname)` → `Entry` | `FUSE_LINK` (13)   | `fuse_link_in` + name → `fuse_entry_out`                            |
| `SetAttr(nodeid, SetAttrIn)` → `Attr`           | `FUSE_SETATTR` (4) | `fuse_setattr_in` (FATTR_* mask) → `fuse_attr_out`                  |
| `Unlink(parent, name)`                          | `FUSE_UNLINK` (10) | name → (error-only)                                                 |
| `Rmdir(parent, name)`                           | `FUSE_RMDIR` (11)  | name → (error-only)                                                 |
| `Rename(oldparent, old, newparent, new)`        | `FUSE_RENAME` (12) | `fuse_rename_in` + old + new → (error-only)                        |
| `Fsync(nodeid, fh, datasync)`                   | `FUSE_FSYNC` (20)  | `fuse_fsync_in` → (error-only)                                     |
| `Flush(nodeid, fh)`                             | `FUSE_FLUSH` (25)  | `fuse_flush_in` → (error-only)                                     |

`SetAttr` covers truncate (`FattrSize`), chmod (`FattrMode`), chown
(`FattrUID`/`FattrGID`) and utimes (`FattrAtime`/`FattrMtime`) via the
`fuse_setattr_in.valid` mask. `FUSE_WRITE` is the only op whose request is
a **three-region** descriptor chain — readable `[fuse_in_header |
fuse_write_in]`, readable `[data]`, writable `[fuse_out_header |
fuse_write_out]`; all other ops keep the two-region read/write split.

The FUSE protocol major is 7; the minor is negotiated down to whatever the
device offers in `FUSE_INIT`. The `FUSE_INIT` request also proposes the
two write-relevant protocol flags `FUSE_BIG_WRITES` (1<<5, permit
multi-page write data regions) and `FUSE_ATOMIC_O_TRUNC` (1<<3, atomic
O_TRUNC on open/create); the negotiated intersection is recorded in
`VirtioFS.FuseFlags`. `FUSE_WRITEBACK_CACHE` is deliberately **not**
negotiated (it shifts page-cache writeback to the kernel, which this raw
request driver does not implement).

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

// Create + write a new file, then truncate + fsync + close it.
ne, wfh, err := fs.Create(1, "out.txt", 0o100644, virtiofs.OpenReadWrite)
if err != nil { return err }
n, err := fs.Write(ne.NodeID, wfh, 0, []byte("written via virtio-fs"))
if err != nil { return err }
_, err = fs.SetAttr(ne.NodeID, virtiofs.SetAttrIn{Valid: virtiofs.FattrSize, Size: uint64(n)})
if err != nil { return err }
if err := fs.Fsync(ne.NodeID, wfh, false); err != nil { return err }
_ = fs.Flush(ne.NodeID, wfh)
_ = fs.Release(ne.NodeID, wfh)
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
