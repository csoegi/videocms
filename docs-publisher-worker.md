# Publisher worker configuration

The publisher worker separates local encoding output from permanent media storage.

## Paths

- `FolderVideoUploadsPriv=./videos/uploads`: uploaded originals on VPS1.
- `FolderVideoQualitysPriv=./videos/staging`: local staging root used by FFmpeg.
- `FolderVideoQualitysFinal=./videos/qualitys`: permanent media root as seen by VideoCMS. In `local` mode this can be the VPS3 NFS mount. Media HTTP handlers read from this root.
- `FolderVideoQualitysPub=/videos/qualitys`: public URL prefix; it is not a filesystem path.

Keep the staging and final roots separate. The local staging directory for a file is `<FolderVideoQualitysPriv>/<file UUID>`.

## Publisher modes

### `PublisherMode=local` (default)

Copies completed staging assets to `FolderVideoQualitysFinal`. If the final root is an NFS mount, writes pass through that mount. The worker publishes the first file directory atomically and replaces individual asset directories on re-publication so re-encoding one quality does not remove other qualities/audio/subtitles.

### `PublisherMode=rsync-ssh`

Transfers staging assets directly to VPS3 over SSH, bypassing NFS for writes. Configure all of:

- `PublisherSSHHost=user@vps3-host`
- `PublisherSSHPort=22`
- `PublisherSSHKey=/path/to/private/key` (optional if the container's SSH agent/default key is configured)
- `PublisherSSHRemoteRoot=/data/media/videos/qualitys` (absolute path on VPS3)

The `FolderVideoQualitysFinal` path must still resolve to the permanent media through the VPS1 mount for existing playback handlers. `PublisherSSHRemoteRoot` is the physical destination on VPS3 and may differ from the mount path on VPS1. The SSH key must be mounted into the container read-only, and the remote SSH host key must already be trusted. `rsync` and `openssh-client` are included in the application image; the VPS3 host also needs `rsync` installed.

## Lifecycle

- Encoder output rows are selected only when `Encoding`, `Publishing`, `Ready`, and `Failed` are all false.
- Successful encoding sets `Publishing=true` and leaves `Ready=false`.
- A durable `publisher_tasks` row is created once every required quality/audio/subtitle output has completed.
- The publisher verifies the staging directory can be transferred, syncs it, updates database paths to the final root, sets assets `Ready=true`, and then removes the local staging directory.
- Publisher tasks in `running` state are returned to `pending` at startup. Failed tasks retry automatically; inspect `last_error` and `attempts` in `publisher_tasks` when diagnosing problems.

Before production deployment, test one new upload, a failed/retried publish, file deletion before publishing, and re-encoding one quality of an already-published file.
