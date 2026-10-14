# Direct MP4 Import API

These endpoints are intended for the separate `videocms-importer` service. They use the existing authenticated API middleware, so send the VideoCMS API key as `Authorization: Bearer ak_…`. The key's owner becomes the owner of the imported VideoCMS file.

The API writes database records only. It does not download media, run FFmpeg, or invoke the existing upload/encoding/publishing flow. The importer and VideoCMS must see the same published media volume. VideoCMS uses `FolderVideoQualitysFinal` (with its existing fallback) as the storage root; the paths returned below are relative to that root so the importer can resolve them against its own `MEDIA_ROOT` setting.

## 1. Create or resume an import

`POST /api/importer/imports`

Example request:

```json
{
  "request_id": "b9dbdd50-6d4e-4d25-a96d-7dcb6c93e17b",
  "name": "Example title",
  "parent_folder_id": 0,
  "size": 1050963052,
  "duration": 3600,
  "width": 1280,
  "height": 720,
  "quality_name": "720p",
  "thumbnail": "thumbnail.webp",
  "subtitles": [
    {
      "name": "English",
      "lang": "en",
      "filename": "subtitle.vtt",
      "type": "vtt"
    }
  ]
}
```

`size` and `duration` are required. `width`, `height`, and `avg_frame_rate` can be omitted when unknown; do not invent metadata. `quality_name` defaults to `720p`. Thumbnail and subtitle entries are optional. Subtitle files must be VTT, with a simple filename containing no path separators.

Before the first request, the importer must generate and persist a UUID `request_id` in its durable job record. Reuse that exact ID on every retry of the same logical import. Go generates the canonical `import_key`, `link_uuid`, `file_uuid`, and subtitle UUIDs. The new `VideoImports` table maps `(user_id, request_id)` to those generated IDs and records the import status. This is an additive table created by the existing GORM startup migration; it does not add columns or indexes to the existing `Files` or `Links` tables.

The endpoint creates `Files`, `Links`, one pending `Qualities` record (`Type="mp4"`, `OutputFile="video.mp4"`), optional pending `Subtitles` (`Source="external"`, `Type="vtt"`, `Codec="webvtt"`), and a `VideoImports` row in one transaction. It intentionally creates no `Audios` row: audio is already inside the MP4. Repeating a request with the same `request_id` returns the original generated IDs and paths rather than creating a duplicate record set.

The response includes the echoed `request_id`, Go-generated `import_key`, `link_uuid`, `file_uuid`, and paths relative to the shared media root. Keep the entire response in the importer job record before beginning the file download. Typical paths are:

- `<file_uuid>/720p/video.mp4`
- `<file_uuid>/thumbnail.webp`
- `<file_uuid>/<subtitle_uuid>/subtitle.vtt`

The actual paths and filenames returned by the API are authoritative. Create the required directories and write files to those paths before calling completion.

## 2. Mark the import ready

`POST /api/importer/imports/{import_key}/complete`

The `import_key` in the path is the UUID returned by Go during creation. The body is optional; if provided, it may contain `{"import_key":"<returned-import-key>"}`.

VideoCMS checks that the MP4 and every declared thumbnail/subtitle exist in its published storage root, then transactionally marks the MP4 quality and subtitles ready and updates the file size from the actual MP4. It returns `ready: true` only after that succeeds. A missing or empty asset leaves the records pending so the importer can retry the download and completion step.

## Playback and downloads

Ready `mp4` qualities are accepted by player readiness checks. The player uses the existing authenticated media route `/{link_uuid}/{quality}/1/stream/video.mp4`, which serves the stored MP4 directly and supports HTTP byte ranges. The download page offers a direct-download action for these qualities; it does not create an FFmpeg download job. Existing HLS qualities and their download pipeline remain unchanged.
