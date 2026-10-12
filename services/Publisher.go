package services

import (
	"ch/kirari04/videocms/config"
	"ch/kirari04/videocms/models"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"gorm.io/gorm"
)

const (
	publisherPending  = "pending"
	publisherRunning  = "running"
	publisherFailed   = "failed"
	publisherReady    = "ready"
	publisherCanceled = "canceled"
)

// Publisher polls a durable database queue. Encoding workers only set the
// Publishing flag; this worker owns all staging-to-final filesystem transfer.
func (w *WorkerGroup) Publisher(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := w.deps.DB.Model(&models.PublisherTask{}).
		Where("status = ?", publisherRunning).
		Updates(map[string]interface{}{"status": publisherPending, "last_error": "", "started_at": nil}).Error; err != nil {
		log.Printf("[Services->Publisher] [ERROR]: Failed to recover interrupted tasks: %v", err)
	}

	// External subtitles waiting for file-level publication are not encoding.
	// Normalize progress for records created by earlier builds where 100 was
	// stored as a percentage instead of the 0..1 fraction expected by the UI.
	if err := w.deps.DB.Model(&models.Subtitle{}).
		Where("source = ? AND publishing = ? AND ready = ?", "external", true, false).
		Update("progress", 0).Error; err != nil {
		log.Printf("[Services->Publisher] [ERROR]: Failed to normalize pending external subtitle progress: %v", err)
	}

	for {
		w.reconcilePublisherQueue()
		w.runPublisherQueue(ctx)
		if !sleepContext(ctx, 5*time.Second) {
			return
		}
	}
}

func (w *WorkerGroup) reconcilePublisherQueue() {
	fileIDs := map[uint]struct{}{}
	var qualities []models.Quality
	if err := w.deps.DB.Where("publishing = ?", true).Find(&qualities).Error; err != nil {
		log.Printf("[Services->Publisher->reconcilePublisherQueue] [ERROR]: Failed to scan publishing qualities: %v", err)
		return
	}
	for _, item := range qualities {
		fileIDs[item.FileID] = struct{}{}
	}
	var audios []models.Audio
	if err := w.deps.DB.Where("publishing = ?", true).Find(&audios).Error; err != nil {
		log.Printf("[Services->Publisher->reconcilePublisherQueue] [ERROR]: Failed to scan publishing audios: %v", err)
		return
	}
	for _, item := range audios {
		fileIDs[item.FileID] = struct{}{}
	}
	var subtitles []models.Subtitle
	if err := w.deps.DB.Where("publishing = ?", true).Find(&subtitles).Error; err != nil {
		log.Printf("[Services->Publisher->reconcilePublisherQueue] [ERROR]: Failed to scan publishing subtitles: %v", err)
		return
	}
	for _, item := range subtitles {
		fileIDs[item.FileID] = struct{}{}
	}

	for fileID := range fileIDs {
		var file models.File
		if err := w.deps.DB.Where("id = ?", fileID).First(&file).Error; err != nil {
			log.Printf("[Services->Publisher->reconcilePublisherQueue] [ERROR]: Failed to load file with ID %v: %v", fileID, err)
			continue
		}
		if file.DeletedAt != nil && file.DeletedAt.Valid {
			continue
		}
		ready, anyPublishing, err := w.fileReadyToPublish(fileID)
		if err != nil || !ready || !anyPublishing {
			continue
		}

		var task models.PublisherTask
		err = w.deps.DB.Where("file_id = ?", fileID).First(&task).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			task = models.PublisherTask{FileID: file.ID, FileUUID: file.UUID, Status: publisherPending}
			if createErr := w.deps.DB.Create(&task).Error; createErr != nil {
				log.Printf("[Services->Publisher->reconcilePublisherQueue] [ERROR]: Failed to enqueue file %s: %v", file.UUID, createErr)
			}
			continue
		}
		if err != nil {
			log.Printf("[Services->Publisher->reconcilePublisherQueue] [ERROR]: Failed to load task for file %s: %v", file.UUID, err)
			continue
		}
		if task.Status == publisherReady || task.Status == publisherCanceled {
			if err := w.deps.DB.Model(&task).Updates(map[string]interface{}{"status": publisherPending, "last_error": "", "completed_at": nil}).Error; err != nil {
				log.Printf("[Services->Publisher->reconcilePublisherQueue] [ERROR]: Failed to requeue file %s: %v", file.UUID, err)
			}
		}
	}
}

// fileReadyToPublish only returns true when every expected asset has finished
// encoding (or is an already-ready external subtitle) and no asset has failed.
func (w *WorkerGroup) fileReadyToPublish(fileID uint) (bool, bool, error) {
	var qualities []models.Quality
	var audios []models.Audio
	var subtitles []models.Subtitle
	if err := w.deps.DB.Where("file_id = ?", fileID).Find(&qualities).Error; err != nil {
		return false, false, err
	}
	if err := w.deps.DB.Where("file_id = ?", fileID).Find(&audios).Error; err != nil {
		return false, false, err
	}
	if err := w.deps.DB.Where("file_id = ?", fileID).Find(&subtitles).Error; err != nil {
		return false, false, err
	}

	allDone, anyPublishing := true, false
	for _, item := range qualities {
		if item.Failed || item.Encoding || (!item.Publishing && !item.Ready) {
			allDone = false
		}
		anyPublishing = anyPublishing || item.Publishing
	}
	for _, item := range audios {
		if item.Failed || item.Encoding || (!item.Publishing && !item.Ready) {
			allDone = false
		}
		anyPublishing = anyPublishing || item.Publishing
	}
	for _, item := range subtitles {
		if item.Failed || item.Encoding || (!item.Publishing && !item.Ready) {
			allDone = false
		}
		anyPublishing = anyPublishing || item.Publishing
	}
	if len(qualities) == 0 {
		allDone = false
	}
	return allDone, anyPublishing, nil
}

func (w *WorkerGroup) runPublisherQueue(ctx context.Context) {
	var tasks []models.PublisherTask
	if err := w.deps.DB.Where("status = ? OR (status = ? AND updated_at < ?)", publisherPending, publisherFailed, time.Now().Add(-30*time.Second)).
		Order("id ASC").Limit(3).Find(&tasks).Error; err != nil {
		log.Printf("[Services->Publisher->runPublisherQueue] [ERROR]: Failed to load queue: %v", err)
		return
	}
	for _, task := range tasks {
		select {
		case <-ctx.Done():
			return
		default:
		}
		var file models.File
		if err := w.deps.DB.Where("id = ?", task.FileID).First(&file).Error; err != nil || (file.DeletedAt != nil && file.DeletedAt.Valid) {
			_ = w.deps.DB.Model(&task).Updates(map[string]interface{}{"status": publisherCanceled, "last_error": "file was deleted"}).Error
			continue
		}
		publishable, anyPublishing, stateErr := w.fileReadyToPublish(file.ID)
		if stateErr != nil || !publishable || !anyPublishing {
			continue
		}
		started := time.Now()
		claim := w.deps.DB.Model(&models.PublisherTask{}).
			Where("id = ? AND status IN ?", task.ID, []string{publisherPending, publisherFailed}).
			Updates(map[string]interface{}{"status": publisherRunning, "started_at": started, "attempts": gorm.Expr("attempts + 1"), "last_error": ""})
		if claim.Error != nil || claim.RowsAffected == 0 {
			continue
		}

		// Measure the complete local staging tree before publication
		// cleanup removes it. This works for both local and rsync-ssh modes.
		stagingPath := filepath.Join(w.Config().FolderVideoQualitysPriv, file.UUID)
		stagingSize, sizeErr := dirSize(stagingPath)
		if sizeErr != nil {
			log.Printf("[Services->Publisher->runPublisherQueue] [ERROR]: Failed to calculate staged output size for %s: %v", file.UUID, sizeErr)
			_ = w.deps.DB.Model(&models.PublisherTask{}).
				Where("id = ?", task.ID).
				Updates(map[string]interface{}{
					"status":     publisherFailed,
					"last_error": sizeErr.Error(),
				}).Error
			continue
		}

		if err := w.publishDirectory(ctx, file.UUID); err != nil {
			log.Printf("[Services->Publisher->runPublisherQueue] [ERROR]: File %s publish attempt failed: %v", file.UUID, err)
			_ = w.deps.DB.Model(&models.PublisherTask{}).Where("id = ?", task.ID).
				Updates(map[string]interface{}{"status": publisherFailed, "last_error": err.Error()}).Error
			continue
		}
		if err := w.commitPublishedState(task, file, stagingSize); err != nil {
			log.Printf("[Services->Publisher->runPublisherQueue] [ERROR]: Failed to commit published state for %s: %v", file.UUID, err)
			_ = w.deps.DB.Model(&models.PublisherTask{}).Where("id = ?", task.ID).
				Updates(map[string]interface{}{"status": publisherFailed, "last_error": err.Error()}).Error
			continue
		}
		if filepath.Clean(stagingPath) != filepath.Clean(w.Config().PublishedQualitysRoot()) {
			if err := os.RemoveAll(stagingPath); err != nil {
				log.Printf("[Services->Publisher->runPublisherQueue] [ERROR]: published %s but could not clean staging directory: %v", file.UUID, err)
			}
		}
		log.Printf("[Services->Publisher->runPublisherQueue] [INFO]: File %s published successfully in %s", file.UUID, time.Since(started).Round(time.Millisecond))
	}
}

func (w *WorkerGroup) publishDirectory(ctx context.Context, fileUUID string) error {
	cfg := w.Config()
	source := filepath.Join(cfg.FolderVideoQualitysPriv, fileUUID)
	destination := filepath.Join(cfg.PublishedQualitysRoot(), fileUUID)
	if info, err := os.Stat(source); err != nil || !info.IsDir() {
		return fmt.Errorf("[Services->Publisher->publishDirectory] [ERROR]: Staging directory %q is missing", source)
	}
	switch strings.ToLower(strings.TrimSpace(cfg.PublisherMode)) {
	case "", "local", "nfs":
		if filepath.Clean(source) == filepath.Clean(destination) {
			return nil
		}
		return publishLocalTree(ctx, source, destination)
	case "rsync-ssh":
		return w.publishRsyncSSH(ctx, source, filepath.Join(cfg.PublisherSSHRemoteRoot, fileUUID))
	default:
		return fmt.Errorf("[Services->Publisher->publishDirectory] [ERROR]: Unsupported PublisherMode %q (use local or rsync-ssh)", cfg.PublisherMode)
	}
}

func (w *WorkerGroup) publishRsyncSSH(
    ctx context.Context,
    source, destination string,
) error {
    cfg := w.Config()

    if strings.TrimSpace(cfg.PublisherSSHRemoteRoot) == "" {
        return errors.New(
            "PublisherSSHRemoteRoot is required for rsync-ssh mode",
        )
    }

    remote, sshArgs, passwordAuth, err := publisherSSHSettings(cfg)
    if err != nil {
        return err
    }

    sshCommand := "ssh " + strings.Join(shellQuoteArgs(sshArgs), " ")
    if passwordAuth {
        sshCommand = "sshpass -e " + sshCommand
    }

    tmpDestination := destination +
        ".publishing-" +
        fmt.Sprintf("%d", time.Now().UnixNano())

    // Create a temporary directory on VPS3.
    out, err := runPublisherSSH(
        ctx,
        cfg,
        remote,
        sshArgs,
        "mkdir -p -- "+shellQuote(tmpDestination),
    )
    if err != nil {
        return fmt.Errorf(
            "[Services->Publisher->publishRsyncSSH] [ERROR]: Failed to prepare remote staging directory: %w: %s",
            err,
            strings.TrimSpace(string(out)),
        )
    }

    // Transfer from local NVMe staging to VPS3.
    rsyncArgs := []string{
        "-a",
        "--delete",
        "--partial",
        "--human-readable",
        "-e", sshCommand,
        source + string(os.PathSeparator),
        remote + ":" + tmpDestination + "/",
    }

    rsyncCmd := exec.CommandContext(ctx, "rsync", rsyncArgs...)
    if passwordAuth {
        rsyncCmd.Env = publisherSSHEnvironment(cfg.PublisherSSHPassword)
    }

    out, err = rsyncCmd.CombinedOutput()
    if err != nil {
        return fmt.Errorf(
            "[Services->Publisher->publishRsyncSSH] [ERROR]: Rsync to target machine failed: %w: %s",
            err,
            strings.TrimSpace(string(out)),
        )
    }

    // Promote the transfer while preserving unrelated published assets.
    promote := "mkdir -p -- " +
        shellQuote(filepath.Dir(destination)) +
        " && if [ -d " + shellQuote(destination) + " ]; then " +
        "for entry in " + shellQuote(tmpDestination) +
        "/*; do [ -e \"$entry\" ] || continue; " +
        "name=${entry##*/}; " +
        "if [ -d \"$entry\" ]; then " +
        "mkdir -p -- " + shellQuote(destination) + "/\"$name\" && " +
        "rsync -a --delete --delay-updates \"$entry/\" " +
        shellQuote(destination) + "/\"$name/\"; " +
        "else rsync -a --delay-updates \"$entry\" " +
        shellQuote(destination) + "/\"$name\"; fi || exit $?; " +
        "done && rm -rf -- " + shellQuote(tmpDestination) +
        "; else mv -- " + shellQuote(tmpDestination) +
        " " + shellQuote(destination) + "; fi"

    out, err = runPublisherSSH(
        ctx,
        cfg,
        remote,
        sshArgs,
        promote,
    )
    if err != nil {
        return fmt.Errorf(
            "[Services->Publisher->publishRsyncSSH] [ERROR]: Failed to promote remote publication: %w: %s",
            err,
            strings.TrimSpace(string(out)),
        )
    }

    return nil
}

func publisherSSHSettings(cfg config.Config) (remote string, sshArgs []string, passwordAuth bool, err error,) {
    host := strings.TrimSpace(cfg.PublisherSSHHost)
    user := strings.TrimSpace(cfg.PublisherSSHUser)
    passwordAuth = cfg.PublisherSSHPassword != ""

    if host == "" {
        return "", nil, false, errors.New(
            "[Services->Publisher->publisherSSHSettings] [ERROR]: PublisherSSHHost is required for rsync-ssh mode",
        )
    }

    // Preserve backward compatibility with Host=user@host.
    if user != "" {
        if strings.Contains(host, "@") {
            return "", nil, false, errors.New(
                "[Services->Publisher->publisherSSHSettings] [ERROR]: use either PublisherSSHUser with a hostname, or user@host in PublisherSSHHost, not both",
            )
        }
        remote = user + "@" + host
    } else {
        remote = host
    }

    if passwordAuth && !strings.Contains(remote, "@") {
        return "", nil, false, errors.New(
            "[Services->Publisher->publisherSSHSettings] [ERROR]: PublisherSSHUser is required when using PublisherSSHPassword",
        )
    }

    port := strings.TrimSpace(cfg.PublisherSSHPort)
    if port == "" {
        port = "22"
    }

	// StrictHostKeyChecking can be set to one of three values:
	// accept-new	: relaxed mode, automatically trusts on first connection and rejects a changed key when previous key is available.
	// yes			: strict mode, requires a trusted host key to be provisioned in advance. 
	// no			: disabled mode, disables meaningful host-key verification. 
    sshArgs = []string{
        "-p", port,
        "-o", "StrictHostKeyChecking=accept-new",
    }

    if passwordAuth {
        // Password authentication takes precedence over the SSH key.
        sshArgs = append(
            sshArgs,
            "-o", "PreferredAuthentications=password,keyboard-interactive",
            "-o", "PubkeyAuthentication=no",
            "-o", "BatchMode=no",
        )
    } else {
        // Preserve the existing key/default-key behavior.
        sshArgs = append(sshArgs, "-o", "BatchMode=yes")

        if cfg.PublisherSSHKey != "" {
            sshArgs = append(sshArgs, "-i", cfg.PublisherSSHKey)
        }
    }

    return remote, sshArgs, passwordAuth, nil
}

func publisherSSHEnvironment(password string) []string {
    env := os.Environ()
    if password == "" {
        return env
    }

    // Avoid duplicate SSHPASS entries.
    filtered := make([]string, 0, len(env)+1)
    for _, entry := range env {
        if !strings.HasPrefix(entry, "SSHPASS=") {
            filtered = append(filtered, entry)
        }
    }

    return append(filtered, "SSHPASS="+password)
}

func runPublisherSSH(ctx context.Context, cfg config.Config, remote string, sshArgs []string, remoteCommand string,) ([]byte, error) {
    args := append([]string{}, sshArgs...)
    args = append(args, remote, remoteCommand)

    var cmd *exec.Cmd

    if cfg.PublisherSSHPassword != "" {
        passwordArgs := append([]string{"-e", "ssh"}, args...)
        cmd = exec.CommandContext(ctx, "sshpass", passwordArgs...)
        cmd.Env = publisherSSHEnvironment(cfg.PublisherSSHPassword)
    } else {
        cmd = exec.CommandContext(ctx, "ssh", args...)
    }

    return cmd.CombinedOutput()
}

func (w *WorkerGroup) commitPublishedState(task models.PublisherTask, file models.File, publishedSize int64) error {
	finalRoot := w.Config().PublishedQualitysRoot()
	finalFolder := filepath.Join(finalRoot, file.UUID)
	now := time.Now()
	return w.deps.DB.Transaction(func(tx *gorm.DB) error {
		var qualities []models.Quality
		if err := tx.Where("file_id = ?", file.ID).Find(&qualities).Error; err != nil {
			return err
		}
		for _, item := range qualities {
			item.Path = filepath.Join(finalFolder, filepath.Base(item.Path))
			item.Publishing, item.Ready, item.Encoding = false, true, false
			item.Failed = false
			if err := tx.Save(&item).Error; err != nil {
				return err
			}
		}
		var audios []models.Audio
		if err := tx.Where("file_id = ?", file.ID).Find(&audios).Error; err != nil {
			return err
		}
		for _, item := range audios {
			item.Path = filepath.Join(finalFolder, filepath.Base(item.Path))
			item.Publishing, item.Ready, item.Encoding = false, true, false
			item.Failed = false
			if err := tx.Save(&item).Error; err != nil {
				return err
			}
		}
		var subtitles []models.Subtitle
		if err := tx.Where("file_id = ?", file.ID).Find(&subtitles).Error; err != nil {
			return err
		}
		for _, item := range subtitles {
			item.Path = filepath.Join(finalFolder, filepath.Base(item.Path))
			item.Publishing, item.Ready, item.Encoding = false, true, false
			item.Failed = false
			if err := tx.Save(&item).Error; err != nil {
				return err
			}
		}
		// Save the published output folder and its measured byte size
		// in the same transaction as the Ready state.
		if err := tx.Model(&models.File{}).
			Where("id = ?", file.ID).
			Updates(map[string]interface{}{
				"folder": finalFolder,
				"size":   publishedSize,
			}).Error; err != nil {
			return err
		}
		return tx.Model(&models.PublisherTask{}).Where("id = ?", task.ID).Updates(map[string]interface{}{
			"status": publisherReady, "last_error": "", "completed_at": now,
		}).Error
	})
}

// DeleteMedia removes both transient and permanent copies. Deleter uses this
// centralized operation so storage backend details are not spread through code.
func (w *WorkerGroup) DeleteMedia(ctx context.Context, fileUUID string) error {
	return DeleteMediaAt(ctx, w.Config(), fileUUID)
}

// DeleteMediaAt removes the same relative media path from staging and final
// storage. It is shared by file deletion and per-asset delete/re-encode flows.
func DeleteMediaAt(ctx context.Context, cfg config.Config, relativePath string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	clean := filepath.Clean(relativePath)
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("[Services->Publisher->DeleteMediaAt] [ERROR]: invalid relative media path %q", relativePath)
	}
	var errs []string
	mode := strings.ToLower(strings.TrimSpace(cfg.PublisherMode))

	if err := os.RemoveAll(filepath.Join(cfg.FolderVideoQualitysPriv, clean)); err != nil {
		errs = append(errs, "[Services->Publisher->DeleteMediaAt] [ERROR]: Failed to delete local target: "+err.Error())
	}
	if mode == "rsync-ssh" {
		if strings.TrimSpace(cfg.PublisherSSHHost) == "" {
			errs = append(errs, "[Services->Publisher->DeleteMediaAt] [ERROR]: PublisherSSHHost is required for rsync-ssh mode")
		} else if strings.TrimSpace(cfg.PublisherSSHRemoteRoot) == "" {
			errs = append(errs, "[Services->Publisher->DeleteMediaAt] [ERROR]: PublisherSSHRemoteRoot is required for rsync-ssh mode")
		} else {
			remote, sshArgs, _, sshErr := publisherSSHSettings(cfg)
			if sshErr != nil {
				errs = append(errs,
					"[Services->Publisher->DeleteMediaAt] [ERROR]: "+sshErr.Error(),
				)
			} else {
				// Remote paths use POSIX separators, regardless of the
				// operating system running VideoCMS.
				remotePath := path.Join(
					strings.TrimSpace(cfg.PublisherSSHRemoteRoot),
					filepath.ToSlash(clean),
				)
				target := shellQuote(remotePath)

				// Confirm the target exists before deleting it, since
				// rm -rf normally succeeds if the target is missing.
				command := "if [ -e " + target + " ] || [ -L " + target + " ]; then " +
					"rm -rf -- " + target + " && echo REMOTE_DELETE_REMOVED; " +
					"else echo REMOTE_DELETE_NOT_FOUND; fi"

				log.Printf(
					"[Services->Publisher->DeleteMediaAt] [INFO]: Deleting remote media at %s:%s",
					remote,
					remotePath,
				)

				out, err := runPublisherSSH(ctx, cfg, remote, sshArgs, command)
				output := strings.TrimSpace(string(out))

				if err != nil {
					errs = append(errs, fmt.Sprintf(
						"[Services->Publisher->DeleteMediaAt] [ERROR]: remote delete: %v: %s",
						err,
						output,
					))
				} else if strings.Contains(output, "REMOTE_DELETE_NOT_FOUND") {
					errs = append(errs, fmt.Sprintf(
                        "[Services->Publisher->DeleteMediaAt] [ERROR]: Remote target was not found: %s:%s",
                        remote,
                        remotePath,
                    ))
				} else if strings.Contains(output, "REMOTE_DELETE_REMOVED") {
					log.Printf(
						"[Services->Publisher->DeleteMediaAt] [INFO]: Remote media deleted: %s:%s",
						remote,
						remotePath,
					)
				} else {
					errs = append(errs, fmt.Sprintf(
						"[Services->Publisher->DeleteMediaAt] [ERROR]: remote delete returned no confirmation for %s:%s; output: %s",
						remote,
						remotePath,
						output,
					))
				}
			}
		}
	} else if mode == "" || mode == "local" || mode == "nfs" {
        // Local/NFS publishing: remove the published media directory.
        if err := os.RemoveAll(
            filepath.Join(cfg.PublishedQualitysRoot(), clean),
        ); err != nil {
            errs = append(errs,
                "[Services->Publisher->DeleteMediaAt] [ERROR]: Failed to delete published target: "+err.Error(),
            )
        }
    } else {
        errs = append(errs, fmt.Sprintf(
            "[Services->Publisher->DeleteMediaAt] [ERROR]: Unsupported PublisherMode %q; refusing local-only deletion",
            cfg.PublisherMode,
        ))
    }

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}

	return nil
}

// publishLocalTree atomically promotes a first publication. For a re-publication,
// it replaces each top-level asset directory independently so re-encoding one
// quality cannot delete unrelated published audio/subtitle/quality directories.
func publishLocalTree(ctx context.Context, source, destination string) error {
	if _, err := os.Stat(destination); os.IsNotExist(err) {
		return copyDirectoryAtomic(ctx, source, destination)
	} else if err != nil {
		return err
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		src := filepath.Join(source, entry.Name())
		dst := filepath.Join(destination, entry.Name())
		if entry.IsDir() {
			if err := copyDirectoryAtomic(ctx, src, dst); err != nil {
				return fmt.Errorf("[Services->Publisher->publishLocalTree] [ERROR]: publish asset directory %s: %w", entry.Name(), err)
			}
			continue
		}
		if err := copyFileAtomic(ctx, src, dst); err != nil {
			return fmt.Errorf("[Services->Publisher->publishLocalTree] [ERROR]: publish asset %s: %w", entry.Name(), err)
		}
	}
	return nil
}

func copyFileAtomic(ctx context.Context, source, destination string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
		return err
	}
	src, err := os.Open(source)
	if err != nil {
		return err
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(destination), ".publish-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_, copyErr := io.Copy(tmp, src)
	if copyErr == nil {
		copyErr = tmp.Chmod(info.Mode().Perm())
	}
	closeErr := tmp.Close()
	if copyErr != nil {
		_ = os.Remove(tmpName)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmpName)
		return closeErr
	}
	if err := os.Rename(tmpName, destination); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

func copyDirectoryAtomic(ctx context.Context, source, destination string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
		return err
	}
	tmp := destination + ".publishing-" + fmt.Sprintf("%d", time.Now().UnixNano())
	_ = os.RemoveAll(tmp)
	if err := copyDirectory(ctx, source, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	backup := destination + ".previous"
	_ = os.RemoveAll(backup)
	if _, err := os.Stat(destination); err == nil {
		if err := os.Rename(destination, backup); err != nil {
			_ = os.RemoveAll(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, destination); err != nil {
		if _, backupErr := os.Stat(backup); backupErr == nil {
			_ = os.Rename(backup, destination)
		}
		_ = os.RemoveAll(tmp)
		return err
	}
	_ = os.RemoveAll(backup)
	return nil
}

func copyDirectory(ctx context.Context, source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("[Services->Publisher->copyDirectory] [ERROR]: symlinks are not supported in publish tree: %s", path)
		}
		if entry.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("[Services->Publisher->copyDirectory] [ERROR]: unsupported file type in publish tree: %s", path)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()
		dst, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(dst, src)
		closeErr := dst.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
func shellQuoteArgs(args []string) []string {
	out := make([]string, len(args))
	for i, arg := range args {
		out[i] = shellQuote(arg)
	}
	return out
}
