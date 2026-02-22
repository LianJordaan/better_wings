package backup

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"emperror.dev/errors"
	"github.com/klauspost/pgzip"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/remote"
	"github.com/pterodactyl/wings/server/filesystem"
)

type ResticBackup struct {
    Backup
}

var _ BackupInterface = (*ResticBackup)(nil)

const resticManifestVersion = 1

func NewRestic(client remote.Client, uuid string, ignore string) *ResticBackup {
    return &ResticBackup{
        Backup{
            client:  client,
            Uuid:    uuid,
            Ignore:  ignore,
            adapter: ResticBackupAdapter,
        },
    }
}

func LocateRestic(client remote.Client, uuid string) (*ResticBackup, os.FileInfo, error) {
    b := NewRestic(client, uuid, "")
    st, err := os.Stat(b.Path())
    if err != nil {
        return nil, nil, err
    }
    if st.IsDir() {
        return nil, nil, errors.New("invalid restic manifest, is directory")
    }
    return b, st, nil
}

func (b *ResticBackup) WithLogContext(c map[string]interface{}) {
    b.logContext = c
}

func (b *ResticBackup) Path() string {
    return resticManifestPath(b.Identifier())
}

func (b *ResticBackup) Remove() error {
    manifest, err := readManifest(b.Path())
    if err != nil {
        if errors.Is(err, os.ErrNotExist) {
            return nil
        }
        return err
    }

    repoPath := manifest.RepoPath
    if repoPath == "" {
        return errors.New("restic: manifest missing repo path")
    }

    if _, err := os.Stat(filepath.Join(repoPath, "config")); err != nil {
        if errors.Is(err, os.ErrNotExist) {
            _ = os.Remove(b.Path())
            return nil
        }
        return err
    }

    stdout, stderr, err := runRestic(context.Background(), repoPath, "", "forget", "--prune", manifest.SnapshotID)
    if err != nil {
        b.log().WithField("stdout", string(stdout)).WithField("stderr", string(stderr)).WithError(err).Warn("restic forget failed")
        return err
    }

    if err := os.Remove(b.Path()); err != nil && !errors.Is(err, os.ErrNotExist) {
        return err
    }

    return nil
}

func (b *ResticBackup) Generate(ctx context.Context, fsys *filesystem.Filesystem, ignore string) (*ArchiveDetails, error) {
    repoPath, err := resticRepoPath(fsys)
    if err != nil {
        return nil, err
    }

	if err := ensureResticRepo(ctx, repoPath, true); err != nil {
		return nil, err
	}

    args := []string{"backup", "--json"}
    if ignore != "" {
        excludeFile, err := writeExcludeFile(ignore)
        if err != nil {
            return nil, err
        }
        defer os.Remove(excludeFile)
        args = append(args, "--exclude-file", excludeFile)
    }
    args = append(args, ".")

    stdout, stderr, err := runRestic(ctx, repoPath, fsys.Path(), args...)
    if err != nil {
        b.log().WithField("stdout", string(stdout)).WithField("stderr", string(stderr)).WithError(err).Error("restic backup failed")
        return nil, err
    }

    summary, err := parseResticSummary(stdout)
    if err != nil {
        b.log().WithField("stdout", string(stdout)).WithError(err).Error("failed parsing restic output")
        return nil, err
    }

	manifest := resticManifest{
		Version:             resticManifestVersion,
		BackupUUID:          b.Identifier(),
		SnapshotID:          summary.SnapshotID,
		RepoPath:            repoPath,
		TargetPath:          filepath.Clean(fsys.Path()),
		CreatedAt:           time.Now().UTC(),
		TotalBytesProcessed: summary.TotalBytesProcessed,
		TotalBytesAdded:     summary.TotalBytesAdded,
		TotalFilesProcessed: summary.TotalFilesProcessed,
	}

    checksum, err := writeManifestWithChecksum(b.Path(), &manifest)
    if err != nil {
        return nil, err
    }

    return &ArchiveDetails{
        Checksum:     checksum,
        ChecksumType: "sha1",
        Size:         summary.TotalBytesProcessed,
    }, nil
}

func (b *ResticBackup) Restore(ctx context.Context, _ io.Reader, _ RestoreCallback) error {
	manifest, err := readManifest(b.Path())
	if err != nil {
		return err
	}
	if manifest.SnapshotID == "" {
		return errors.New("restic: manifest missing snapshot id")
	}
	repoPath := manifest.RepoPath
	if repoPath == "" {
		return errors.New("restic: manifest missing repo path")
	}
	if manifest.TargetPath == "" {
		return errors.New("restic: manifest missing target path")
	}
	if err := ensureResticRepo(ctx, repoPath, false); err != nil {
		return err
	}

	stdout, stderr, err := runRestic(ctx, repoPath, "", "restore", manifest.SnapshotID, "--target", manifest.TargetPath)
	if err != nil {
		b.log().
			WithField("stdout", string(stdout)).
			WithField("stderr", string(stderr)).
			WithError(err).
			Error("restic restore failed")
		return err
	}
	return nil
}

// StreamArchive restores the snapshot to a temporary directory and streams a tar.gz archive to w.
func (b *ResticBackup) StreamArchive(ctx context.Context, w io.Writer) error {
	manifest, err := readManifest(b.Path())
	if err != nil {
		return err
	}
	if manifest.SnapshotID == "" {
		return errors.New("restic: manifest missing snapshot id")
	}
	if manifest.RepoPath == "" {
		return errors.New("restic: manifest missing repo path")
	}
	if err := ensureResticRepo(ctx, manifest.RepoPath, false); err != nil {
		return err
	}

	tmpDir, err := resticTempDir()
	if err != nil {
		return err
	}
	restoreDir, err := os.MkdirTemp(tmpDir, "download-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(restoreDir)

	stdout, stderr, err := runRestic(ctx, manifest.RepoPath, "", "restore", manifest.SnapshotID, "--target", restoreDir)
	if err != nil {
		b.log().
			WithField("stdout", string(stdout)).
			WithField("stderr", string(stderr)).
			WithError(err).
			Error("restic restore for download failed")
		return err
	}

	return streamTarGz(ctx, restoreDir, w)
}

func (b *ResticBackup) Checksum() ([]byte, error) {
	f, err := os.Open(b.Path())
	if err != nil {
		return nil, err
	}
    defer f.Close()

    h := sha1.New()
    if _, err := io.Copy(h, f); err != nil {
        return nil, err
    }
    return h.Sum(nil), nil
}

func (b *ResticBackup) Size() (int64, error) {
    manifest, err := readManifest(b.Path())
    if err != nil {
        return 0, err
    }
    return manifest.TotalBytesProcessed, nil
}

func (b *ResticBackup) Details(ctx context.Context, parts []remote.BackupPart) (*ArchiveDetails, error) {
    ad := ArchiveDetails{ChecksumType: "sha1", Parts: parts}

    chk, err := b.Checksum()
    if err != nil {
        return nil, err
    }
    ad.Checksum = hex.EncodeToString(chk)

    size, err := b.Size()
    if err != nil {
        return nil, err
    }
    ad.Size = size

    return &ad, nil
}

type resticManifest struct {
    Version             int       `json:"version"`
    BackupUUID          string    `json:"backup_uuid"`
    SnapshotID          string    `json:"snapshot_id"`
    RepoPath            string    `json:"repo_path"`
    TargetPath          string    `json:"target_path"`
    CreatedAt           time.Time `json:"created_at"`
    TotalBytesProcessed int64     `json:"total_bytes_processed"`
    TotalBytesAdded     int64     `json:"total_bytes_added"`
    TotalFilesProcessed int64     `json:"total_files_processed"`
}

type resticSummary struct {
    SnapshotID          string
    TotalBytesProcessed int64
    TotalBytesAdded     int64
    TotalFilesProcessed int64
}

func resticManifestPath(uuid string) string {
	return filepath.Join(config.Get().System.BackupDirectory, "restic", "manifests", uuid+".json")
}

func resticTempDir() (string, error) {
	dir := filepath.Join(config.Get().System.BackupDirectory, "restic", "tmp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func resticRepoBasePath() string {
	cfg := config.Get().System.Backups.Restic
	if cfg.RepoBasePath != "" {
		return cfg.RepoBasePath
	}
	return filepath.Join(config.Get().System.BackupDirectory, "restic", "repos")
}

func resticRepoPath(fsys *filesystem.Filesystem) (string, error) {
    base := resticRepoBasePath()
    if err := os.MkdirAll(base, 0o700); err != nil {
        return "", err
    }
    name := filepath.Base(fsys.Path())
    if name == "" || name == "." || name == string(os.PathSeparator) {
        return "", errors.New("restic: failed to determine repository name")
    }
    return filepath.Join(base, name), nil
}

// RemoveResticDataForServer removes all restic manifests and repositories that belong
// to the provided server UUID.
//
// This is intended to be called during server deletion and operates in a best-effort
// manner, attempting to clean up as much data as possible even if some operations fail.
func RemoveResticDataForServer(serverID string) error {
    serverID = strings.TrimSpace(serverID)
    if serverID == "" {
        return errors.New("restic: server id is empty")
    }

    manifestDir := filepath.Join(config.Get().System.BackupDirectory, "restic", "manifests")
    repoPaths := map[string]struct{}{
        filepath.Join(resticRepoBasePath(), serverID): {},
    }

    var failures []string

    entries, err := os.ReadDir(manifestDir)
    if err != nil {
        if !errors.Is(err, os.ErrNotExist) {
            failures = append(failures, "failed reading restic manifests directory: "+err.Error())
        }
    } else {
        for _, entry := range entries {
            if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
                continue
            }

            manifestPath := filepath.Join(manifestDir, entry.Name())
            manifest, err := readManifest(manifestPath)
            if err != nil {
                failures = append(failures, "failed reading restic manifest "+entry.Name()+": "+err.Error())
                continue
            }

            if !resticManifestBelongsToServer(manifest, serverID) {
                continue
            }

            repoPath := filepath.Clean(manifest.RepoPath)
            if repoPath != "" && repoPath != "." {
                repoPaths[repoPath] = struct{}{}
            }

            if err := os.Remove(manifestPath); err != nil && !errors.Is(err, os.ErrNotExist) {
                failures = append(failures, "failed removing restic manifest "+entry.Name()+": "+err.Error())
            }
        }
    }

    for repoPath := range repoPaths {
        cleanRepoPath := filepath.Clean(repoPath)
        if filepath.Base(cleanRepoPath) != serverID {
            continue
        }

        if err := os.RemoveAll(cleanRepoPath); err != nil {
            failures = append(failures, "failed removing restic repository "+cleanRepoPath+": "+err.Error())
        }
    }

    if len(failures) > 0 {
        return errors.New(strings.Join(failures, "; "))
    }

    return nil
}

func resticManifestBelongsToServer(manifest *resticManifest, serverID string) bool {
    if manifest == nil || serverID == "" {
        return false
    }

    repoBase := filepath.Base(filepath.Clean(manifest.RepoPath))
    if repoBase == serverID {
        return true
    }

    targetBase := filepath.Base(filepath.Clean(manifest.TargetPath))
    if targetBase == serverID {
        return true
    }

    return false
}

func ensureResticRepo(ctx context.Context, repoPath string, initIfMissing bool) error {
	if repoPath == "" {
		return errors.New("restic: repository path is empty")
	}
	if initIfMissing {
		if err := os.MkdirAll(repoPath, 0o700); err != nil {
			return err
		}
	} else {
		if _, err := os.Stat(repoPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return errors.New("restic: repository not initialized")
			}
			return err
		}
	}
	if _, err := os.Stat(filepath.Join(repoPath, "config")); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !initIfMissing {
		return errors.New("restic: repository not initialized")
	}

    stdout, stderr, err := runRestic(ctx, repoPath, "", "init")
    if err != nil {
        if looksLikeRepoExists(stderr) || looksLikeRepoExists(stdout) {
            return nil
        }
        return err
    }
    return nil
}

func runRestic(ctx context.Context, repoPath string, dir string, args ...string) ([]byte, []byte, error) {
    cfg := config.Get().System.Backups.Restic
    binary := cfg.Binary
    if binary == "" {
        binary = "restic"
    }
    if cfg.RepoPassword == "" {
        return nil, nil, errors.New("restic: repo_password is required")
    }

    cmdArgs := append([]string{"-r", repoPath}, args...)
    cmd := exec.CommandContext(ctx, binary, cmdArgs...)
    if dir != "" {
        cmd.Dir = dir
    }

    env := os.Environ()
    env = append(env, "RESTIC_PASSWORD="+cfg.RepoPassword)
    cmd.Env = env

    var stdout, stderr bytes.Buffer
    cmd.Stdout = &stdout
    cmd.Stderr = &stderr

    err := cmd.Run()
    return stdout.Bytes(), stderr.Bytes(), err
}

func parseResticSummary(output []byte) (*resticSummary, error) {
    scanner := bufio.NewScanner(bytes.NewReader(output))
    summary := resticSummary{}

    for scanner.Scan() {
        line := strings.TrimSpace(scanner.Text())
        if line == "" {
            continue
        }

        var msg struct {
            MessageType         string `json:"message_type"`
            SnapshotID          string `json:"snapshot_id"`
            TotalBytesProcessed int64  `json:"total_bytes_processed"`
            TotalBytesAdded     int64  `json:"total_bytes_added"`
            TotalFilesProcessed int64  `json:"total_files_processed"`
        }
        if err := json.Unmarshal([]byte(line), &msg); err != nil {
            return nil, err
        }

        switch msg.MessageType {
        case "summary":
            summary.SnapshotID = msg.SnapshotID
            summary.TotalBytesProcessed = msg.TotalBytesProcessed
            summary.TotalBytesAdded = msg.TotalBytesAdded
            summary.TotalFilesProcessed = msg.TotalFilesProcessed
        case "snapshot":
            if summary.SnapshotID == "" && msg.SnapshotID != "" {
                summary.SnapshotID = msg.SnapshotID
            }
        }
    }

    if err := scanner.Err(); err != nil {
        return nil, err
    }
    if summary.SnapshotID == "" {
        return nil, errors.New("restic: no snapshot id found in output")
    }

    return &summary, nil
}

func writeExcludeFile(ignore string) (string, error) {
	dir, err := resticTempDir()
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "exclude-*.txt")
    if err != nil {
        return "", err
    }
    if _, err := f.WriteString(ignore); err != nil {
        _ = f.Close()
        _ = os.Remove(f.Name())
        return "", err
    }
    if err := f.Close(); err != nil {
        _ = os.Remove(f.Name())
        return "", err
    }
    return f.Name(), nil
}

func writeManifestWithChecksum(dst string, manifest *resticManifest) (string, error) {
    if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
        return "", err
    }
    tmp, err := os.CreateTemp(filepath.Dir(dst), "manifest-*.json")
    if err != nil {
        return "", err
    }
    defer func() {
        _ = tmp.Close()
        _ = os.Remove(tmp.Name())
    }()

    h := sha1.New()
    enc := json.NewEncoder(io.MultiWriter(tmp, h))
    enc.SetIndent("", "  ")
    if err := enc.Encode(manifest); err != nil {
        return "", err
    }
    if err := tmp.Close(); err != nil {
        return "", err
    }
    if err := os.Rename(tmp.Name(), dst); err != nil {
        return "", err
    }
    return hex.EncodeToString(h.Sum(nil)), nil
}

func readManifest(path string) (*resticManifest, error) {
    f, err := os.Open(path)
    if err != nil {
        return nil, err
    }
    defer f.Close()

    var m resticManifest
    if err := json.NewDecoder(f).Decode(&m); err != nil {
        return nil, err
    }
    return &m, nil
}

func looksLikeRepoExists(output []byte) bool {
	lower := strings.ToLower(string(output))
	return strings.Contains(lower, "config file") || strings.Contains(lower, "already initialized") || strings.Contains(lower, "already exists")
}

func streamTarGz(ctx context.Context, root string, w io.Writer) error {
	level := resticCompressionLevel()
	gw, _ := pgzip.NewWriterLevel(w, level)
	_ = gw.SetConcurrency(1<<20, 1)
	defer gw.Close()

	tw := tar.NewWriter(gw)
	defer tw.Close()

	return filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if p == root {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)

		// Skip sockets (unsupported).
		if info.Mode()&os.ModeSocket != 0 {
			return nil
		}

		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(p)
			if err != nil {
				return nil
			}
			header, err := tar.FileInfoHeader(info, target)
			if err != nil {
				return err
			}
			header.Name = rel
			return tw.WriteHeader(header)
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = rel

		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		if info.IsDir() || !info.Mode().IsRegular() {
			return nil
		}

		f, err := os.Open(p)
		if err != nil {
			return err
		}
		if _, err := io.Copy(tw, f); err != nil {
			_ = f.Close()
			return err
		}
		_ = f.Close()
		return nil
	})
}

func resticCompressionLevel() int {
	switch config.Get().System.Backups.CompressionLevel {
	case "none":
		return pgzip.NoCompression
	case "best_compression":
		return pgzip.BestCompression
	default:
		return pgzip.BestSpeed
	}
}
