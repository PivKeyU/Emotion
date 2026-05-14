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
	FileKindSTRM               // .strm
	FileKindNFO                // .nfo
	FileKindImage              // .jpg .jpeg .png .webp
	FileKindSubtitle           // .srt .ass .vtt .ssa .sub
)

// classifyExt returns the MediaKind for a path's extension.
func classifyExt(path string) MediaKind {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mkv", ".mp4", ".m4v", ".ts", ".avi", ".mov", ".wmv", ".flv", ".webm", ".iso", ".rmvb":
		return FileKindVideo
	case ".strm":
		return FileKindSTRM
	case ".nfo":
		return FileKindNFO
	case ".jpg", ".jpeg", ".png", ".webp":
		return FileKindImage
	case ".srt", ".ass", ".vtt", ".ssa", ".sub":
		return FileKindSubtitle
	}
	return FileKindOther
}

// DirFiles groups the interesting files inside one directory.
type DirFiles struct {
	Path      string
	Dir       string   // basename of Path
	Media     []string // full paths, video + strm (one entry per playable)
	NFOs      []string // NFO files in this dir
	Images    []string // image files
	Subtitles []string // subtitle files
}

// ScanOptions controls directory traversal.
type ScanOptions struct {
	// Root is the directory to scan; must exist.
	Root string
	// FollowSymlinks enables following symlinks during walk.
	FollowSymlinks bool
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

	walkFn := func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Skip unreadable entries but don't abort.
			return nil
		}
		if d.IsDir() {
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
		case FileKindVideo, FileKindSTRM:
			bucket.Media = append(bucket.Media, path)
		case FileKindNFO:
			bucket.NFOs = append(bucket.NFOs, path)
		case FileKindImage:
			bucket.Images = append(bucket.Images, path)
		case FileKindSubtitle:
			bucket.Subtitles = append(bucket.Subtitles, path)
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
