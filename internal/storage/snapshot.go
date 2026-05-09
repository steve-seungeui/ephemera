package storage

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// SnapshotMetadata holds everything needed to restore a VM from a snapshot.
// Stored as metadata.json inside the snapshot directory.
type SnapshotMetadata struct {
	SnapshotID     string    `json:"snapshot_id"`
	SourceVMID     string    `json:"source_vm_id"`
	Profile        string    `json:"profile"`
	SnapshotType   string    `json:"snapshot_type"`              // "full" | "diff"
	BaseSnapshotID string    `json:"base_snapshot_id,omitempty"` // set for diff snapshots
	GuestIP        string    `json:"guest_ip"`
	TapDevice      string    `json:"tap_device"`   // original TAP device name; Firecracker v1.x embeds this in state.bin
	VsockPath      string    `json:"vsock_path"`   // original vsock UDS path; Firecracker recreates it from snapshot state on restore
	MacAddr        string    `json:"mac_addr"`
	AgentToken     string    `json:"agent_token"`
	DiskPath       string    `json:"disk_path"`      // original workspace path (required by Firecracker on restore)
	MemFilePath    string    `json:"mem_file_path"`  // absolute path to memory.bin (or sparse diff)
	StatFilePath   string    `json:"stat_file_path"` // absolute path to state.bin
	DiskCopyPath   string    `json:"disk_copy_path"` // copy of disk inside snapshot dir
	CreatedAt      time.Time `json:"created_at"`
}

// SnapshotDir returns the canonical directory for a given snapshot ID.
func SnapshotDir(workDir, snapshotID string) string {
	return filepath.Join(workDir, "snapshots", snapshotID)
}

// SaveMetadata writes metadata.json into the snapshot directory.
func SaveMetadata(snapDir string, meta SnapshotMetadata) error {
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal snapshot metadata: %w", err)
	}
	return os.WriteFile(filepath.Join(snapDir, "metadata.json"), b, 0600)
}

// LoadMetadata reads and parses metadata.json from a snapshot directory.
func LoadMetadata(snapDir string) (SnapshotMetadata, error) {
	b, err := os.ReadFile(filepath.Join(snapDir, "metadata.json"))
	if err != nil {
		return SnapshotMetadata{}, fmt.Errorf("read snapshot metadata: %w", err)
	}
	var meta SnapshotMetadata
	if err := json.Unmarshal(b, &meta); err != nil {
		return SnapshotMetadata{}, fmt.Errorf("parse snapshot metadata: %w", err)
	}
	return meta, nil
}

// ListSnapshots scans {workDir}/snapshots/ and returns all valid snapshot metadata entries.
func ListSnapshots(workDir string) ([]SnapshotMetadata, error) {
	base := filepath.Join(workDir, "snapshots")
	entries, err := os.ReadDir(base)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list snapshots dir: %w", err)
	}

	var results []SnapshotMetadata
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		meta, err := LoadMetadata(filepath.Join(base, e.Name()))
		if err != nil {
			continue // skip corrupted entries silently
		}
		results = append(results, meta)
	}
	return results, nil
}

// DeleteSnapshot removes the entire snapshot directory including all files.
func DeleteSnapshot(snapDir string) error {
	if err := os.RemoveAll(snapDir); err != nil {
		return fmt.Errorf("delete snapshot dir %s: %w", snapDir, err)
	}
	return nil
}

// CopyDiskToSnapshot copies the VM disk into the snapshot directory as rootfs.ext4.
// Returns the destination path.
func CopyDiskToSnapshot(diskPath, snapDir string) (string, error) {
	dst := filepath.Join(snapDir, "rootfs.ext4")

	src, err := os.Open(diskPath)
	if err != nil {
		return "", fmt.Errorf("open source disk: %w", err)
	}
	defer src.Close()

	out, err := os.Create(dst)
	if err != nil {
		return "", fmt.Errorf("create snapshot disk copy: %w", err)
	}

	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		os.Remove(dst)
		return "", fmt.Errorf("copy disk to snapshot: %w", err)
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return "", fmt.Errorf("flush snapshot disk: %w", err)
	}
	return dst, nil
}

// RestoreDiskFromSnapshot copies the snapshot's rootfs.ext4 back to the original disk path.
// Kept for reference; prefer SetupBindMount for concurrent-restore support.
func RestoreDiskFromSnapshot(diskCopyPath, originalDiskPath string) error {
	if err := os.MkdirAll(filepath.Dir(originalDiskPath), 0755); err != nil {
		return fmt.Errorf("create workspace dir: %w", err)
	}

	src, err := os.Open(diskCopyPath)
	if err != nil {
		return fmt.Errorf("open snapshot disk copy: %w", err)
	}
	defer src.Close()

	out, err := os.Create(originalDiskPath)
	if err != nil {
		return fmt.Errorf("create restored disk: %w", err)
	}

	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		os.Remove(originalDiskPath)
		return fmt.Errorf("restore disk from snapshot: %w", err)
	}
	if err := out.Close(); err != nil {
		os.Remove(originalDiskPath)
		return fmt.Errorf("flush restored disk: %w", err)
	}
	return nil
}

// SetupBindMount prepares the disk for a snapshot restore using Linux bind mounts.
//
// It copies the snapshot disk to newDiskPath (a unique per-restore file), ensures the
// bind mount target (mountTargetPath = the original VM's disk path recorded in state.bin)
// exists as a regular file, then bind-mounts newDiskPath over mountTargetPath.
//
// Firecracker opens mountTargetPath and transparently reads/writes newDiskPath through
// the bind mount. Because each restore uses a distinct newDiskPath, multiple concurrent
// restores of the same snapshot are safe: each Firecracker acquires its own file
// descriptor to its own inode, and subsequent bind mounts simply stack on the same
// target without disturbing open descriptors already held by earlier restores.
//
// Callers must hold a per-snapshot lock across SetupBindMount + RestoreMachine.Start()
// to ensure each Firecracker opens the correct (topmost) bind mount.
func SetupBindMount(diskCopyPath, newDiskPath, mountTargetPath string) error {
	if err := os.MkdirAll(filepath.Dir(newDiskPath), 0755); err != nil {
		return fmt.Errorf("create workspace dir: %w", err)
	}

	// Copy snapshot disk to the unique per-restore path.
	if err := copyFile(diskCopyPath, newDiskPath); err != nil {
		return fmt.Errorf("copy snapshot disk to restore path: %w", err)
	}

	// Ensure mountTargetPath exists as a regular file (required bind mount target).
	// The file may already exist as an empty placeholder from a previous restore.
	if _, err := os.Stat(mountTargetPath); os.IsNotExist(err) {
		if err := os.WriteFile(mountTargetPath, nil, 0644); err != nil {
			os.Remove(newDiskPath)
			return fmt.Errorf("create bind mount target: %w", err)
		}
	}

	// Bind mount: Firecracker opens mountTargetPath → reads/writes newDiskPath.
	if out, err := exec.Command("mount", "--bind", newDiskPath, mountTargetPath).CombinedOutput(); err != nil {
		os.Remove(newDiskPath)
		return fmt.Errorf("bind mount %s → %s: %w: %s", newDiskPath, mountTargetPath, err, out)
	}

	return nil
}

// TeardownBindMount detaches a bind mount and removes the per-restore disk file.
//
// Uses lazy unmount (-l) so the target path is detached from the directory tree
// immediately, while any Firecracker process that already has an open file descriptor
// to the underlying inode continues to work uninterrupted until it exits.
//
// When called out of bind-mount stack order (i.e. not strictly LIFO), the lazy
// unmount removes the topmost stacked mount — which may belong to a different restore.
// This is safe: that restore's Firecracker already holds a file descriptor to its own
// inode, so losing the path binding does not corrupt its disk I/O. The actual disk
// bytes are freed only when both the fd is closed and os.Remove has unlinked the file.
func TeardownBindMount(mountTargetPath, newDiskPath string) {
	exec.Command("umount", "-l", mountTargetPath).Run()
	if err := os.Remove(newDiskPath); err != nil && !os.IsNotExist(err) {
		// Log but do not fail — the inode will be freed when all fds are closed.
		_ = err
	}
}

// DMSnapshotInfo holds the kernel objects created by SetupDMSnapshot.
// All fields are required for correct teardown via TeardownDMSnapshot.
type DMSnapshotInfo struct {
	LoopDevice     string // read-only loop device for the base disk (e.g. "/dev/loop3")
	COWLoopDevice  string // read-write loop device for the exception store (e.g. "/dev/loop4")
	DMDevice       string // dm-snapshot device name (e.g. "cow-vm-xxx")
	ExceptionStore string // sparse COW store file (grows on write, initially ~0 bytes)
	MountTarget    string // original disk path (from Firecracker state.bin)
}

// SetupDMSnapshot creates a block-level COW view of the snapshot's rootfs using Linux
// device mapper snapshot. The base disk is read-only; all writes from the restored VM
// accumulate in a sparse exception store. This eliminates the ~700 MB full copy that
// SetupBindMount previously required.
//
// The COW device is bind-mounted over mountTargetPath (the original disk path recorded
// in Firecracker state.bin) so that Firecracker can open the path as before.
//
// Caller must call TeardownDMSnapshot on VM destroy to release kernel resources.
func SetupDMSnapshot(baseDiskPath, exceptionStorePath, mountTargetPath string) (*DMSnapshotInfo, error) {
	// 1. Create read-only loop device for the snapshot base disk.
	out, err := exec.Command("losetup", "--read-only", "--find", "--show", baseDiskPath).Output()
	if err != nil {
		return nil, fmt.Errorf("losetup for base disk: %w", err)
	}
	loopDev := strings.TrimSpace(string(out))

	cleanup := func() { exec.Command("losetup", "-d", loopDev).Run() }

	// 2. Get the sector count of the loop device.
	sectorOut, err := exec.Command("blockdev", "--getsz", loopDev).Output()
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("blockdev --getsz %s: %w", loopDev, err)
	}
	sectors := strings.TrimSpace(string(sectorOut))

	// 3. Create a sparse exception store (initial disk usage: ~0 bytes).
	//    8 GB sparse file gives ample headroom; actual allocation grows with VM writes.
	if err := exec.Command("truncate", "-s", "8G", exceptionStorePath).Run(); err != nil {
		cleanup()
		return nil, fmt.Errorf("create exception store %s: %w", exceptionStorePath, err)
	}

	// 4. Attach the exception store as a loop device.
	//    dm-snapshot requires block devices for both origin and COW — a regular file path
	//    is not accepted by the device mapper table parser.
	cowOut, err := exec.Command("losetup", "--find", "--show", exceptionStorePath).Output()
	if err != nil {
		cleanup()
		os.Remove(exceptionStorePath)
		return nil, fmt.Errorf("losetup for exception store: %w", err)
	}
	cowLoopDev := strings.TrimSpace(string(cowOut))
	cowCleanup := func() {
		exec.Command("losetup", "-d", cowLoopDev).Run()
		cleanup()
		os.Remove(exceptionStorePath)
	}

	// 5. Create the dm-snapshot device.
	//    "P 8" = persistent, 8-sector (4 KiB) chunks matching the host page size.
	dmName := "cow-" + filepath.Base(exceptionStorePath)
	if len(dmName) > 50 { // dm names are limited; truncate if needed
		dmName = dmName[:50]
	}
	table := fmt.Sprintf("0 %s snapshot %s %s P 8", sectors, loopDev, cowLoopDev)
	if out, err := exec.Command("dmsetup", "create", dmName, "--table", table).CombinedOutput(); err != nil {
		cowCleanup()
		return nil, fmt.Errorf("dmsetup create %s: %w: %s", dmName, err, strings.TrimSpace(string(out)))
	}

	dmPath := "/dev/mapper/" + dmName

	// 6. Ensure the bind-mount target file exists (must be a regular file).
	if _, err := os.Stat(mountTargetPath); os.IsNotExist(err) {
		if err := os.WriteFile(mountTargetPath, nil, 0644); err != nil {
			exec.Command("dmsetup", "remove", dmName).Run()
			cowCleanup()
			return nil, fmt.Errorf("create mount target %s: %w", mountTargetPath, err)
		}
	}

	// 7. Bind-mount the COW device over the original disk path.
	//    Firecracker opens mountTargetPath and transparently reads/writes the COW device.
	if out, err := exec.Command("mount", "--bind", dmPath, mountTargetPath).CombinedOutput(); err != nil {
		exec.Command("dmsetup", "remove", dmName).Run()
		cowCleanup()
		return nil, fmt.Errorf("bind mount %s → %s: %w: %s", dmPath, mountTargetPath, err, strings.TrimSpace(string(out)))
	}

	return &DMSnapshotInfo{
		LoopDevice:     loopDev,
		COWLoopDevice:  cowLoopDev,
		DMDevice:       dmName,
		ExceptionStore: exceptionStorePath,
		MountTarget:    mountTargetPath,
	}, nil
}

// TeardownDMSnapshot releases all kernel resources created by SetupDMSnapshot.
// Uses lazy unmount so any Firecracker fd that was already opened continues to work
// until the process exits, at which point the underlying inode is freed.
func TeardownDMSnapshot(info *DMSnapshotInfo) {
	if info == nil {
		return
	}
	exec.Command("umount", "-l", info.MountTarget).Run()
	// Brief wait: dmsetup remove can fail if Firecracker still holds an open fd.
	// The lazy umount detaches the path immediately; the device can be removed once
	// all fds are closed. We retry a few times to handle the transition.
	for i := 0; i < 5; i++ {
		if exec.Command("dmsetup", "remove", info.DMDevice).Run() == nil {
			break
		}
		// Wait for Firecracker to fully exit before retrying.
		exec.Command("sleep", "0.2").Run()
	}
	exec.Command("losetup", "-d", info.COWLoopDevice).Run()
	exec.Command("losetup", "-d", info.LoopDevice).Run()
	os.Remove(info.ExceptionStore)
}

// MergeMemoryDiff produces a merged memory file by overlaying dirty pages from a diff
// snapshot onto a full (base) snapshot. The diff file is sparse: only dirty pages are
// written; clean pages are holes. SEEK_DATA/SEEK_HOLE is used to iterate only over the
// written regions, avoiding a full 2 GB read/write of the base.
//
// outputPath must not exist; caller is responsible for cleanup on success.
// On error, outputPath is removed if it was created.
func MergeMemoryDiff(baseMemPath, diffMemPath, outputPath string) error {
	if err := copyFile(baseMemPath, outputPath); err != nil {
		return fmt.Errorf("copy base memory: %w", err)
	}

	diff, err := os.Open(diffMemPath)
	if err != nil {
		os.Remove(outputPath)
		return fmt.Errorf("open diff memory: %w", err)
	}
	defer diff.Close()

	out, err := os.OpenFile(outputPath, os.O_WRONLY, 0)
	if err != nil {
		os.Remove(outputPath)
		return fmt.Errorf("open merged output: %w", err)
	}
	defer out.Close()

	// Walk the sparse regions of the diff file using SEEK_DATA / SEEK_HOLE.
	// Each data region contains dirty pages that must overwrite the corresponding
	// region in the merged output.
	const bufSize = 2 << 20 // 2 MiB transfer buffer
	buf := make([]byte, bufSize)
	var offset int64

	diffFd := int(diff.Fd())
	for {
		// Find the start of the next dirty data region.
		dataStart, err := unix.Seek(diffFd, offset, unix.SEEK_DATA)
		if err != nil {
			break // no more data regions (ENXIO at EOF)
		}

		// Find the end of this data region (start of next hole).
		holeStart, err := unix.Seek(diffFd, dataStart, unix.SEEK_HOLE)
		if err != nil {
			// Rest of file is data.
			fi, _ := diff.Stat()
			holeStart = fi.Size()
		}

		// Copy dirty pages from diff → output at the same offset.
		remaining := holeStart - dataStart
		if _, err := diff.Seek(dataStart, io.SeekStart); err != nil {
			os.Remove(outputPath)
			return fmt.Errorf("seek diff at %d: %w", dataStart, err)
		}
		if _, err := out.Seek(dataStart, io.SeekStart); err != nil {
			os.Remove(outputPath)
			return fmt.Errorf("seek output at %d: %w", dataStart, err)
		}

		for remaining > 0 {
			n := int64(bufSize)
			if n > remaining {
				n = remaining
			}
			nr, err := diff.Read(buf[:n])
			if nr > 0 {
				if _, werr := out.Write(buf[:nr]); werr != nil {
					os.Remove(outputPath)
					return fmt.Errorf("write merged output: %w", werr)
				}
			}
			if err != nil {
				if err == io.EOF {
					break
				}
				os.Remove(outputPath)
				return fmt.Errorf("read diff: %w", err)
			}
			remaining -= int64(nr)
		}

		offset = holeStart
	}

	return nil
}
