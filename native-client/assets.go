package main

import (
	"io/fs"

	webdist "github.com/projanvil/woca/web"
)

func frontendAssets() fs.FS {
	return webdist.DistFS()
}
