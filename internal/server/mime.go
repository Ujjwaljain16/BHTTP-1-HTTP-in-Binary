package server

import (
	"path/filepath"
	"strings"
)

var contentTypes = map[string]string{
	".html": "text/html",
	".htm":  "text/html",
	".css":  "text/css",
	".js":   "text/javascript",
	".json": "application/json",
	".txt":  "text/plain",
	".xml":  "application/xml",
	".svg":  "image/svg+xml",
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".ico":  "image/x-icon",
	".pdf":  "application/pdf",
	".zip":  "application/zip",
	".wasm": "application/wasm",
}

// The type is only a hint for the client; unknown extensions fall back to
// plain bytes rather than guessing.
func contentType(name string) string {
	if t, ok := contentTypes[strings.ToLower(filepath.Ext(name))]; ok {
		return t
	}
	return "application/octet-stream"
}
