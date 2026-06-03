package importer

import (
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

// MediaKind is inferred for each discovered file.
type MediaKind int

const (
	FileKindOther    MediaKind = iota
	FileKindVideo              // .mkv .mp4 .m4v .ts .avi .mov .wmv .flv .webm
	FileKindNFO                // .nfo
	FileKindImage              // .jpg .jpeg .png .webp
	FileKindSubtitle           // .srt .ass .vtt .ssa .sub
)

// classifyExt returns the MediaKind for a path's extension.
func classifyExt(path string) MediaKind {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mkv", ".mp4", ".m4v", ".ts", ".avi", ".mov", ".wmv", ".flv", ".webm", ".iso", ".rmvb":
		return FileKindVideo
	case ".nfo":
		return FileKindNFO
	case ".jpg", ".jpeg", ".png", ".webp":
		return FileKindImage
	case ".srt", ".ass", ".vtt", ".ssa", ".sub":
		return FileKindSubtitle
	}
	return FileKindOther
}

// IsLibraryFile reports whether a file is relevant to media library scanning.
func IsLibraryFile(path string) bool {
	return classifyExt(path) != FileKindOther
}

// DirFiles groups the interesting files inside one directory.
type DirFiles struct {
	Path      string
	Dir       string           // basename of Path
	Media     []string         // full paths, one entry per playable video file
	NFOs      []string         // NFO files in this dir
	Images    []string         // image files
	Subtitles []string         // subtitle files
	FileSizes map[string]int64 // optional sizes collected during traversal
}

// FileSize returns a size captured during Scan when CollectFileSize was enabled.
func (d *DirFiles) FileSize(path string) (int64, bool) {
	if d == nil || d.FileSizes == nil {
		return 0, false
	}
	size, ok := d.FileSizes[path]
	return size, ok
}

// ScanOptions controls directory traversal.
type ScanOptions struct {
	// Root is the directory to scan; must exist.
	Root string
	// FollowSymlinks enables following symlinks during walk.
	FollowSymlinks bool
	// SkipHiddenDirs skips dot-prefixed directories before descending into them.
	SkipHiddenDirs bool
	// IgnoreDirNames skips directory basenames such as @eaDir or #recycle.
	IgnoreDirNames []string
	// CollectFileSize records sizes while the directory entry is hot in traversal.
	CollectFileSize bool
	// OnDir is called whenever the walker visits a directory.
	OnDir func(path string, seen int)
}

// Scan walks the tree once and returns a map of directory -> DirFiles.
// The result is keyed by absolute, cleaned path.
func Scan(opts ScanOptions) (map[string]*DirFiles, error) {
	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return nil, err
	}
	result := map[string]*DirFiles{}
	dirsSeen := 0
	ignoredDirs := ignoreDirNameSet(opts.IgnoreDirNames)

	walkFn := func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Skip unreadable entries but don't abort.
			return nil
		}
		if d.IsDir() {
			if path != root && shouldSkipScanDir(d.Name(), opts.SkipHiddenDirs, ignoredDirs) {
				return filepath.SkipDir
			}
			dirsSeen++
			if opts.OnDir != nil {
				opts.OnDir(path, dirsSeen)
			}
			return nil
		}
		// Skip hidden files (macOS ._foo, .DS_Store, etc.)
		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		kind := classifyExt(path)
		if kind == FileKindOther {
			return nil
		}
		dir := filepath.Dir(path)
		bucket, ok := result[dir]
		if !ok {
			bucket = &DirFiles{Path: dir, Dir: filepath.Base(dir)}
			result[dir] = bucket
		}
		switch kind {
		case FileKindVideo:
			bucket.Media = append(bucket.Media, path)
		case FileKindNFO:
			bucket.NFOs = append(bucket.NFOs, path)
		case FileKindImage:
			bucket.Images = append(bucket.Images, path)
		case FileKindSubtitle:
			bucket.Subtitles = append(bucket.Subtitles, path)
		}
		if opts.CollectFileSize {
			rememberFileSize(bucket, path, d)
		}
		return nil
	}

	// We rely on filepath.WalkDir which does NOT follow symlinks by default.
	// If the caller wants symlink following, we emulate it with a DFS.
	if opts.FollowSymlinks {
		if err := walkFollowSymlinks(root, walkFn, map[string]bool{}); err != nil {
			return nil, err
		}
	} else {
		if err := filepath.WalkDir(root, walkFn); err != nil {
			return nil, err
		}
	}

	// Normalize ordering so re-runs are stable.
	for _, b := range result {
		sort.Strings(b.Media)
		sort.Strings(b.NFOs)
		sort.Strings(b.Images)
		sort.Strings(b.Subtitles)
	}
	return result, nil
}

// walkFollowSymlinks is a simple recursive walk that follows symlinks, with a
// seen set to prevent cycles.
func walkFollowSymlinks(root string, fn fs.WalkDirFunc, seen map[string]bool) error {
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		real = root
	}
	if seen[real] {
		return nil
	}
	seen[real] = true

	entries, err := readDir(real)
	if err != nil {
		return fn(root, nil, err)
	}
	for _, ent := range entries {
		full := filepath.Join(root, ent.Name())
		if err := fn(full, ent, nil); err != nil {
			if err == filepath.SkipDir {
				continue
			}
			return err
		}
		if ent.IsDir() {
			if err := walkFollowSymlinks(full, fn, seen); err != nil {
				return err
			}
		}
	}
	return nil
}

// readDir is a small helper wrapping os.ReadDir for symlink-following walks.
func readDir(path string) ([]fs.DirEntry, error) {
	return readDirOS(path)
}

func ignoreDirNameSet(names []string) map[string]struct{} {
	if len(names) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.ToLower(strings.TrimSpace(name))
		if name != "" {
			out[name] = struct{}{}
		}
	}
	return out
}

func shouldSkipScanDir(name string, skipHidden bool, ignored map[string]struct{}) bool {
	if skipHidden && strings.HasPrefix(name, ".") {
		return true
	}
	if len(ignored) == 0 {
		return false
	}
	_, ok := ignored[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

func rememberFileSize(bucket *DirFiles, path string, d fs.DirEntry) {
	if bucket == nil || d == nil {
		return
	}
	info, err := d.Info()
	if err != nil || info.Size() <= 0 {
		return
	}
	if bucket.FileSizes == nil {
		bucket.FileSizes = map[string]int64{}
	}
	bucket.FileSizes[path] = info.Size()
}
