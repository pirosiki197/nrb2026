package main

import (
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// spaHandler は dir 配下のファイルを配信し、解決できない path には index.html の中身を
// 404 で返す (BrowserRouter のリロード経路を SPA 側のルータに任せるため)。
func spaHandler(dir, index string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET,HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		path, ok := resolveStaticPath(dir, r.URL.Path)
		if !ok {
			serveNotFoundFile(w, r, index)
			return
		}
		if st, err := os.Stat(path); err == nil && st.IsDir() {
			if !strings.HasSuffix(r.URL.Path, "/") {
				location := r.URL.Path + "/"
				if r.URL.RawQuery != "" {
					location += "?" + r.URL.RawQuery
				}
				w.Header().Set("Location", location)
				w.WriteHeader(http.StatusTemporaryRedirect)
				return
			}
			path = filepath.Join(path, "index.html")
		}
		f, err := os.Open(path)
		if err != nil {
			serveNotFoundFile(w, r, index)
			return
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil || st.IsDir() {
			serveNotFoundFile(w, r, index)
			return
		}
		w.Header().Set("Content-Type", contentTypeByExt(path))
		http.ServeContent(w, r, path, st.ModTime(), f)
	})
}

// resolveStaticPath は URL path を dir 配下のファイルパスに変換する。".." などで dir の外を指すなら false。
func resolveStaticPath(dir, urlPath string) (string, bool) {
	parts := []string{dir}
	for _, seg := range strings.Split(strings.TrimLeft(urlPath, "/"), "/") {
		switch seg {
		case "", ".":
			continue
		case "..":
			return "", false
		}
		if strings.ContainsRune(seg, '\\') {
			return "", false
		}
		parts = append(parts, seg)
	}
	return filepath.Join(parts...), true
}

// contentTypeByExt は拡張子から charset パラメータ無しの MIME type を返す。不明なら application/octet-stream。
func contentTypeByExt(path string) string {
	t := mime.TypeByExtension(filepath.Ext(path))
	if t == "" {
		return "application/octet-stream"
	}
	if i := strings.Index(t, ";"); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	return t
}

func serveNotFoundFile(w http.ResponseWriter, r *http.Request, path string) {
	f, err := os.Open(path)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	h := w.Header()
	h.Set("Content-Type", contentTypeByExt(path))
	h.Set("Accept-Ranges", "bytes")
	h.Set("Last-Modified", st.ModTime().UTC().Format(http.TimeFormat))
	h.Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	w.WriteHeader(http.StatusNotFound)
	if r.Method != http.MethodHead {
		io.Copy(w, f)
	}
}
