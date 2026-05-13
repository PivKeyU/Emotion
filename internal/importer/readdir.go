package importer

import (
	"io/fs"
	"os"
)

// readDirOS is extracted so tests can stub it out.
func readDirOS(path string) ([]fs.DirEntry, error) {
	return os.ReadDir(path)
}
