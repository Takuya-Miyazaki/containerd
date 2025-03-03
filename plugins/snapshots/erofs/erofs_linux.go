/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package erofs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/snapshots/storage"
	"github.com/containerd/containerd/v2/internal/fsverity"
	"github.com/containerd/containerd/v2/plugins/snapshots/erofs/erofsutils"
	"github.com/containerd/continuity/fs"
	"github.com/containerd/log"
	"github.com/containerd/plugin"
	"golang.org/x/sys/unix"
)

// SnapshotterConfig is used to configure the erofs snapshotter instance
type SnapshotterConfig struct {
	// ovlOptions are the base options added to the overlayfs mount (defaults to [""])
	ovlOptions []string
	// enableFsverity enables fsverity for EROFS layers
	enableFsverity bool
}

// Opt is an option to configure the erofs snapshotter
type Opt func(config *SnapshotterConfig)

// WithOvlOptions defines the extra mount options for overlayfs
func WithOvlOptions(options []string) Opt {
	return func(config *SnapshotterConfig) {
		config.ovlOptions = options
	}
}

// WithFsverity enables fsverity for EROFS layers
func WithFsverity() Opt {
	return func(config *SnapshotterConfig) {
		config.enableFsverity = true
	}
}

type MetaStore interface {
	TransactionContext(ctx context.Context, writable bool) (context.Context, storage.Transactor, error)
	WithTransaction(ctx context.Context, writable bool, fn storage.TransactionCallback) error
	Close() error
}

type snapshotter struct {
	root           string
	ms             *storage.MetaStore
	ovlOptions     []string
	enableFsverity bool
}

// check if EROFS kernel filesystem is registered or not
func findErofs() bool {
	fs, err := os.ReadFile("/proc/filesystems")
	if err != nil {
		return false
	}
	return bytes.Contains(fs, []byte("\terofs\n"))
}

// we have to claim it as uint32, otherwise s390x CI will complain.. :(
const erofsSuperMagic = uint32(0xE0F5E1E2)

// Check if a directory is actually an EROFS mount, which is used to setup or
// recover EROFS mounts for lowerdirs.
func isErofs(dir string) bool {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return false
	}
	return uint32(st.Type) == erofsSuperMagic
}

// NewSnapshotter returns a Snapshotter which uses EROFS+OverlayFS. The layers
// are stored under the provided root. A metadata file is stored under the root.
func NewSnapshotter(root string, opts ...Opt) (snapshots.Snapshotter, error) {
	var config SnapshotterConfig
	for _, opt := range opts {
		opt(&config)
	}

	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	supportsDType, err := fs.SupportsDType(root)
	if err != nil {
		return nil, err
	}
	if !supportsDType {
		return nil, fmt.Errorf("%s does not support d_type. If the backing filesystem is xfs, please reformat with ftype=1 to enable d_type support", root)
	}

	if !findErofs() {
		return nil, fmt.Errorf("EROFS unsupported, please `modprobe erofs`: %w", plugin.ErrSkipPlugin)
	}

	// Check fsverity support if enabled
	if config.enableFsverity {
		supported, err := fsverity.IsSupported(root)
		if err != nil {
			return nil, fmt.Errorf("failed to check fsverity support on %q: %w", root, err)
		}
		if !supported {
			return nil, fmt.Errorf("fsverity is not supported on the filesystem of %q", root)
		}
	}

	ms, err := storage.NewMetaStore(filepath.Join(root, "metadata.db"))
	if err != nil {
		return nil, err
	}

	if err := os.Mkdir(filepath.Join(root, "snapshots"), 0700); err != nil && !os.IsExist(err) {
		return nil, err
	}

	return &snapshotter{
		root:           root,
		ms:             ms,
		ovlOptions:     config.ovlOptions,
		enableFsverity: config.enableFsverity,
	}, nil
}

// Close closes the snapshotter
func (s *snapshotter) Close() error {
	return s.ms.Close()
}

func (s *snapshotter) upperPath(id string) string {
	return filepath.Join(s.root, "snapshots", id, "fs")
}

func (s *snapshotter) workPath(id string) string {
	return filepath.Join(s.root, "snapshots", id, "work")
}

// A committed layer blob generated by the EROFS differ
func (s *snapshotter) layerBlobPath(id string) string {
	return filepath.Join(s.root, "snapshots", id, "layer.erofs")
}

func (s *snapshotter) lowerPath(id string) (mount.Mount, string, error) {
	layerBlob := s.layerBlobPath(id)
	if _, err := os.Stat(layerBlob); err != nil {
		return mount.Mount{}, "", fmt.Errorf("failed to find valid erofs layer blob: %w", err)
	}

	return mount.Mount{
		Source:  layerBlob,
		Type:    "erofs",
		Options: []string{"ro"},
	}, s.upperPath(id), nil
}

func (s *snapshotter) prepareDirectory(ctx context.Context, snapshotDir string, kind snapshots.Kind) (string, error) {
	td, err := os.MkdirTemp(snapshotDir, "new-")
	if err != nil {
		return "", fmt.Errorf("failed to create temp dir: %w", err)
	}

	if err := os.Mkdir(filepath.Join(td, "fs"), 0755); err != nil {
		return td, err
	}

	if kind == snapshots.KindActive {
		if err := os.Mkdir(filepath.Join(td, "work"), 0711); err != nil {
			return td, err
		}
	}
	// Create a special file for the EROFS differ to indicate it will be
	// prepared as an EROFS layer by the EROFS snapshotter.
	if err := os.WriteFile(filepath.Join(td, ".erofslayer"), []byte{}, 0644); err != nil {
		return td, err
	}
	return td, nil
}

func (s *snapshotter) mounts(snap storage.Snapshot, info snapshots.Info) ([]mount.Mount, error) {
	var options []string

	if len(snap.ParentIDs) == 0 {
		m, _, err := s.lowerPath(snap.ID)
		if err == nil {
			if snap.Kind != snapshots.KindView {
				return nil, fmt.Errorf("only works for snapshots.KindView on a committed snapshot: %w", err)
			}
			if s.enableFsverity {
				if err := s.verifyFsverity(m.Source); err != nil {
					return nil, err
				}
			}
			// We have to force a loop device here since mount[] is static.
			m.Options = append(m.Options, "loop")
			return []mount.Mount{m}, nil
		}
		// if we only have one layer/no parents then just return a bind mount as overlay
		// will not work
		roFlag := "rw"
		if snap.Kind == snapshots.KindView {
			roFlag = "ro"
		}
		return []mount.Mount{
			{
				Source: s.upperPath(snap.ID),
				Type:   "bind",
				Options: append(options,
					roFlag,
					"rbind",
				),
			},
		}, nil
	}

	if snap.Kind == snapshots.KindActive {
		options = append(options,
			fmt.Sprintf("workdir=%s", s.workPath(snap.ID)),
			fmt.Sprintf("upperdir=%s", s.upperPath(snap.ID)),
		)
	} else if len(snap.ParentIDs) == 1 {
		m, _, err := s.lowerPath(snap.ParentIDs[0])
		if err != nil {
			return nil, err
		}
		return []mount.Mount{m}, nil
	}

	var lowerdirs []string
	for i := range snap.ParentIDs {
		m, mntpoint, err := s.lowerPath(snap.ParentIDs[i])
		if err != nil {
			return nil, err
		}

		// If the lowerdir is actually an EROFS committed layer but
		// doesn't have an EROFS mount.  Let's recover now.
		if mntpoint != m.Source && !isErofs(mntpoint) {
			err := m.Mount(mntpoint)
			// Use loop if the current kernel (6.12+) doesn't support file-backed mount
			if err == unix.ENOTBLK {
				m.Options = append(m.Options, "loop")
				err = m.Mount(mntpoint)
			}
			if err != nil {
				return nil, err
			}
		}
		lowerdirs = append(lowerdirs, mntpoint)
	}
	options = append(options, fmt.Sprintf("lowerdir=%s", strings.Join(lowerdirs, ":")))
	options = append(options, s.ovlOptions...)

	return []mount.Mount{{
		Type:    "overlay",
		Source:  "overlay",
		Options: options,
	}}, nil
}

func (s *snapshotter) createSnapshot(ctx context.Context, kind snapshots.Kind, key, parent string, opts []snapshots.Opt) (_ []mount.Mount, err error) {
	var (
		snap     storage.Snapshot
		td, path string
		info     snapshots.Info
	)

	defer func() {
		if err != nil {
			if td != "" {
				if err1 := os.RemoveAll(td); err1 != nil {
					log.G(ctx).WithError(err1).Warn("failed to cleanup temp snapshot directory")
				}
			}
			if path != "" {
				if err1 := os.RemoveAll(path); err1 != nil {
					log.G(ctx).WithError(err1).WithField("path", path).Error("failed to reclaim snapshot directory, directory may need removal")
					err = fmt.Errorf("failed to remove path: %v: %w", err1, err)
				}
			}
		}
	}()

	if err := s.ms.WithTransaction(ctx, true, func(ctx context.Context) (err error) {
		snapshotDir := filepath.Join(s.root, "snapshots")
		td, err = s.prepareDirectory(ctx, snapshotDir, kind)
		if err != nil {
			return fmt.Errorf("failed to create prepare snapshot dir: %w", err)
		}

		snap, err = storage.CreateSnapshot(ctx, kind, key, parent, opts...)
		if err != nil {
			return fmt.Errorf("failed to create snapshot: %w", err)
		}

		_, info, _, err = storage.GetInfo(ctx, key)
		if err != nil {
			return fmt.Errorf("failed to get snapshot info: %w", err)
		}

		if len(snap.ParentIDs) > 0 {
			st, err := os.Stat(s.upperPath(snap.ParentIDs[0]))
			if err != nil {
				return fmt.Errorf("failed to stat parent: %w", err)
			}

			stat := st.Sys().(*syscall.Stat_t)
			if err := os.Lchown(filepath.Join(td, "fs"), int(stat.Uid), int(stat.Gid)); err != nil {
				return fmt.Errorf("failed to chown: %w", err)
			}
		}

		path = filepath.Join(snapshotDir, snap.ID)
		if err = os.Rename(td, path); err != nil {
			return fmt.Errorf("failed to rename: %w", err)
		}
		td = ""

		return nil
	}); err != nil {
		return nil, err
	}
	return s.mounts(snap, info)
}

func (s *snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	return s.createSnapshot(ctx, snapshots.KindActive, key, parent, opts)
}

func (s *snapshotter) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	return s.createSnapshot(ctx, snapshots.KindView, key, parent, opts)
}

func setImmutable(path string, enable bool) error {
	//nolint:revive	// silence "don't use ALL_CAPS in Go names; use CamelCase"
	const (
		FS_IMMUTABLE_FL = 0x10
	)
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open: %w", err)
	}
	defer f.Close()

	oldattr, err := unix.IoctlGetInt(int(f.Fd()), unix.FS_IOC_GETFLAGS)
	if err != nil {
		return fmt.Errorf("error getting inode flags: %w", err)
	}
	newattr := oldattr | FS_IMMUTABLE_FL
	if !enable {
		newattr ^= FS_IMMUTABLE_FL
	}
	if newattr == oldattr {
		return nil
	}
	return unix.IoctlSetPointerInt(int(f.Fd()), unix.FS_IOC_SETFLAGS, newattr)
}

func (s *snapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
	var layerBlob, upperDir string

	// Apply the overlayfs upperdir (generated by non-EROFS differs) into a EROFS blob
	// in a read transaction first since conversion could be slow.
	err := s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		id, _, _, err := storage.GetInfo(ctx, key)
		if err != nil {
			return err
		}

		// If the layer blob doesn't exist, which means this layer wasn't applied by
		// the EROFS differ (possibly the walking differ), convert the upperdir instead.
		layerBlob = s.layerBlobPath(id)
		if _, err := os.Stat(layerBlob); err != nil {
			upperDir = s.upperPath(id)
			err = erofsutils.ConvertErofs(ctx, layerBlob, upperDir, nil)
			if err != nil {
				return err
			}

			// Remove all sub-directories in the overlayfs upperdir.  Leave the
			// overlayfs upperdir itself since it's used for Lchown.
			fd, err := os.Open(upperDir)
			if err != nil {
				return err
			}
			defer fd.Close()

			dirs, err := fd.Readdirnames(0)
			if err != nil {
				return err
			}

			for _, d := range dirs {
				dir := filepath.Join(upperDir, d)
				if err := os.RemoveAll(dir); err != nil {
					log.G(ctx).WithError(err).WithField("path", dir).Warn("failed to remove directory")
				}
			}
		}

		// Enable fsverity on the EROFS layer if configured
		if s.enableFsverity {
			if err := fsverity.Enable(layerBlob); err != nil {
				return fmt.Errorf("failed to enable fsverity: %w", err)
			}
		}
		// Set IMMUTABLE_FL on the EROFS layer to avoid artificial data loss
		if err := setImmutable(layerBlob, true); err != nil {
			log.G(ctx).WithError(err).Warnf("failed to set IMMUTABLE_FL for %s", layerBlob)
		}
		return nil
	})

	if err != nil {
		return err
	}
	return s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		if _, err := os.Stat(layerBlob); err != nil {
			return fmt.Errorf("failed to get the converted erofs blob: %w", err)
		}

		usage, err := fs.DiskUsage(ctx, layerBlob)
		if err != nil {
			return err
		}
		if _, err = storage.CommitActive(ctx, key, name, snapshots.Usage(usage), opts...); err != nil {
			return fmt.Errorf("failed to commit snapshot %s: %w", key, err)
		}
		return nil
	})
}

func (s *snapshotter) Mounts(ctx context.Context, key string) (_ []mount.Mount, err error) {
	var snap storage.Snapshot
	var info snapshots.Info
	if err := s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		snap, err = storage.GetSnapshot(ctx, key)
		if err != nil {
			return fmt.Errorf("failed to get active mount: %w", err)
		}

		_, info, _, err = storage.GetInfo(ctx, key)
		if err != nil {
			return fmt.Errorf("failed to get snapshot info: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return s.mounts(snap, info)
}

func (s *snapshotter) getCleanupDirectories(ctx context.Context) ([]string, error) {
	ids, err := storage.IDMap(ctx)
	if err != nil {
		return nil, err
	}

	snapshotDir := filepath.Join(s.root, "snapshots")
	fd, err := os.Open(snapshotDir)
	if err != nil {
		return nil, err
	}
	defer fd.Close()

	dirs, err := fd.Readdirnames(0)
	if err != nil {
		return nil, err
	}

	cleanup := []string{}
	for _, d := range dirs {
		if _, ok := ids[d]; ok {
			continue
		}
		cleanup = append(cleanup, filepath.Join(snapshotDir, d))
	}

	return cleanup, nil
}

// Remove abandons the snapshot identified by key. The snapshot will
// immediately become unavailable and unrecoverable. Disk space will
// be freed up on the next call to `Cleanup`.
func (s *snapshotter) Remove(ctx context.Context, key string) (err error) {
	var removals []string
	var id string
	// Remove directories after the transaction is closed, failures must not
	// return error since the transaction is committed with the removal
	// key no longer available.
	defer func() {
		if err == nil {
			if err := mount.UnmountAll(s.upperPath(id), 0); err != nil {
				log.G(ctx).Warnf("failed to unmount EROFS mount for %v", id)
			}

			for _, dir := range removals {
				if err := os.RemoveAll(dir); err != nil {
					log.G(ctx).WithError(err).WithField("path", dir).Warn("failed to remove directory")
				}
			}
		}
	}()
	return s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		id, _, err = storage.Remove(ctx, key)
		if err != nil {
			return fmt.Errorf("failed to remove snapshot %s: %w", key, err)
		}

		removals, err = s.getCleanupDirectories(ctx)
		if err != nil {
			return fmt.Errorf("unable to get directories for removal: %w", err)
		}
		// Clear IMMUTABLE_FL before removal, since this flag avoids it.
		err = setImmutable(s.layerBlobPath(id), false)
		if err != nil {
			return fmt.Errorf("failed to clear IMMUTABLE_FL: %w", err)
		}
		return nil
	})
}

func (s *snapshotter) Stat(ctx context.Context, key string) (info snapshots.Info, err error) {
	err = s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		_, info, _, err = storage.GetInfo(ctx, key)
		return err
	})
	if err != nil {
		return snapshots.Info{}, err
	}

	return info, nil
}

func (s *snapshotter) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) (_ snapshots.Info, err error) {
	err = s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		info, err = storage.UpdateInfo(ctx, info, fieldpaths...)
		return err
	})
	if err != nil {
		return snapshots.Info{}, err
	}

	return info, nil
}

func (s *snapshotter) Walk(ctx context.Context, fn snapshots.WalkFunc, fs ...string) error {
	return s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		return storage.WalkInfo(ctx, fn, fs...)
	})
}

// Usage returns the resources taken by the snapshot identified by key.
//
// For active snapshots, this will scan the usage of the overlay "diff" (aka
// "upper") directory and may take some time.
//
// For committed snapshots, the value is returned from the metadata database.
func (s *snapshotter) Usage(ctx context.Context, key string) (_ snapshots.Usage, err error) {
	var (
		usage snapshots.Usage
		info  snapshots.Info
		id    string
	)
	if err := s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		id, info, usage, err = storage.GetInfo(ctx, key)
		return err
	}); err != nil {
		return usage, err
	}

	if info.Kind == snapshots.KindActive {
		upperPath := s.upperPath(id)
		du, err := fs.DiskUsage(ctx, upperPath)
		if err != nil {
			// TODO(stevvooe): Consider not reporting an error in this case.
			return snapshots.Usage{}, err
		}
		usage = snapshots.Usage(du)
	}
	return usage, nil
}

// Add a method to verify fsverity
func (s *snapshotter) verifyFsverity(path string) error {
	if !s.enableFsverity {
		return nil
	}
	enabled, err := fsverity.IsEnabled(path)
	if err != nil {
		return fmt.Errorf("failed to check fsverity status: %w", err)
	}
	if !enabled {
		return fmt.Errorf("fsverity is not enabled on %s", path)
	}
	return nil
}
