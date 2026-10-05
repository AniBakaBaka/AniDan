// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
)

// Set by Make/Docker at link time to bind the advertised archive to the build.
var SourceArchiveSHA256 string

// Public corresponding-source download. Its fixed directory is part of the
// deployment artifact, never a request/configuration supplied filesystem path.
// /source remains the existing SPA's source-management route.
func (s *Server) registerSourceOffer(mux *http.ServeMux) {
	mux.HandleFunc("GET /source-code", sourceOfferHandler("source", SourceArchiveSHA256))
}

func sourceOfferHandler(directory, expectedDigest string) http.HandlerFunc {
	gate := make(chan struct{}, 4)
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case gate <- struct{}{}:
			defer func() { <-gate }()
		default:
			httpError(w, 503, "Source downloads are busy; retry shortly")
			return
		}
		root, e := os.OpenRoot(directory)
		if e != nil {
			httpError(w, 404, "Corresponding source archive is not installed; build with make source or use the packaged image")
			return
		}
		defer root.Close()
		f, e := root.Open("anidan-source.tar.gz")
		if e != nil {
			httpError(w, 404, "Corresponding source archive is not installed")
			return
		}
		defer f.Close()
		meta, e := f.Stat()
		if e != nil || !meta.Mode().IsRegular() || meta.Size() <= 0 || meta.Size() > 256<<20 {
			httpError(w, 503, "Corresponding source archive is invalid")
			return
		}
		h := sha256.New()
		if _, e = io.Copy(h, io.LimitReader(f, (256<<20)+1)); e != nil {
			httpError(w, 503, "Cannot read corresponding source archive")
			return
		}
		digest := hex.EncodeToString(h.Sum(nil))
		if expectedDigest != "" && digest != expectedDigest {
			httpError(w, 503, "Source archive does not match this server build")
			return
		}
		if _, e = f.Seek(0, io.SeekStart); e != nil {
			httpError(w, 503, "Cannot read corresponding source archive")
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", `attachment; filename="anidan-source.tar.gz"`)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "public, max-age=300")
		w.Header().Set("ETag", `"`+digest+`"`)
		w.Header().Set("X-Source-Archive-SHA256", digest)
		http.ServeContent(w, r, "anidan-source.tar.gz", meta.ModTime(), f)
	}
}
