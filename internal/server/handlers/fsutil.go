package handlers

import (
	"io/fs"
	"os"
)

// osStat is extracted so handler tests can stub filesystem access.
var osStat = func(path string) (fs.FileInfo, error) {
	return os.Stat(path)
}
