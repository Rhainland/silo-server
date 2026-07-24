package scanner

import "errors"

// errFolderHasNoMedia signals that a candidate folder contained zero media
// files of the kind its parser looks for. Folder-scoped parsers
// (parseAudiobookFolder, parsePodcastShow) return it so their reconcile
// callers can skip the folder quietly.
//
// It deliberately does NOT wrap os.ErrNotExist. The folder parsers shell out
// to ffprobe, and a missing or misconfigured ffprobe binary surfaces as an
// exec error that wraps fs.ErrNotExist ("fork/exec /path/ffprobe: no such
// file or directory"). Skipping on os.ErrNotExist therefore swallowed a
// server misconfiguration as "this folder has no audio", leaving scans that
// reported processed=N failed=0 while indexing nothing at all.
var errFolderHasNoMedia = errors.New("folder contains no media files")
