package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// maxUpload is Telegram's per-file limit for non-Premium accounts.
const maxUpload = 2000 << 20

// fileGuard confines file access to configured directories, resolving
// symlinks so a link inside a root can't point outside it.
type fileGuard struct {
	readRoots []string // send_file may read under these
	download  string   // download_media writes here
}

func newFileGuard(cfg Config) (*fileGuard, error) {
	if err := os.MkdirAll(cfg.DownloadDir, 0o700); err != nil {
		return nil, err
	}
	dl, err := filepath.EvalSymlinks(cfg.DownloadDir)
	if err != nil {
		return nil, err
	}
	g := &fileGuard{download: dl, readRoots: []string{dl}}
	for _, r := range cfg.AllowedRoots {
		abs, err := filepath.Abs(expandHome(r))
		if err != nil {
			return nil, err
		}
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return nil, fmt.Errorf("TELEX_ALLOWED_ROOTS: %w", err)
		}
		g.readRoots = append(g.readRoots, real)
	}
	return g, nil
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[1:])
		}
	}
	return p
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// readable validates a path for upload and returns its real path and size.
func (g *fileGuard) readable(p string) (string, int64, error) {
	abs, err := filepath.Abs(expandHome(p))
	if err != nil {
		return "", 0, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", 0, err
	}
	ok := false
	for _, r := range g.readRoots {
		if within(r, real) {
			ok = true
			break
		}
	}
	if !ok {
		return "", 0, fmt.Errorf("%s is outside the allowed roots (%s); set TELEX_ALLOWED_ROOTS", p, strings.Join(g.readRoots, ", "))
	}
	st, err := os.Stat(real)
	if err != nil {
		return "", 0, err
	}
	if !st.Mode().IsRegular() {
		return "", 0, fmt.Errorf("%s is not a regular file", p)
	}
	if st.Size() == 0 {
		return "", 0, fmt.Errorf("%s is empty", p)
	}
	if st.Size() > maxUpload {
		return "", 0, fmt.Errorf("%s is %d MB; Telegram's limit is 2000 MB", p, st.Size()>>20)
	}
	return real, st.Size(), nil
}

var unsafeName = regexp.MustCompile(`[^\w.\- ()\[\]]+`)

// downloadPath picks a fresh path under the download dir for name.
func (g *fileGuard) downloadPath(name string) (string, error) {
	name = strings.Trim(unsafeName.ReplaceAllString(filepath.Base(name), "_"), ". ")
	if name == "" {
		name = "file"
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 0; i < 1000; i++ {
		cand := name
		if i > 0 {
			cand = fmt.Sprintf("%s (%d)%s", stem, i, ext)
		}
		p := filepath.Join(g.download, cand)
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		_ = f.Close() // reserve the name; the downloader overwrites it
		return p, nil
	}
	return "", errors.New("could not pick a download file name")
}

func extForMIME(m string) string {
	if exts, _ := mime.ExtensionsByType(m); len(exts) > 0 {
		return exts[0]
	}
	return ""
}

// auditLog appends one JSON line per write action, so the owner can review
// what agents sent on their behalf.
type auditLog struct {
	mu   sync.Mutex
	path string
}

func (a *auditLog) Write(action string, fields map[string]any) {
	fields["ts"] = time.Now().UTC().Format(time.RFC3339)
	fields["action"] = action
	b, err := json.Marshal(fields)
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	f, err := os.OpenFile(a.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}
