package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config is everything the server reads from the environment.
type Config struct {
	AppID   int
	AppHash string

	// Home holds the session, peer cache, lock and audit log. Created 0700.
	Home string

	// AllowedRoots are directories send_file may read from. Empty disables
	// sending local files (download_media still writes to DownloadDir).
	AllowedRoots []string
	// DownloadDir is where download_media writes. It is always readable by
	// send_file too, so an agent can pass along a file it fetched.
	DownloadDir string

	// AllowedChats, when non-empty, limits every tool to these chats (marked
	// IDs or @usernames). Chats outside it are invisible.
	AllowedChats []string

	// ReadOnly hides every tool that changes anything on Telegram.
	ReadOnly bool
}

func (c Config) SessionPath() string { return filepath.Join(c.Home, "session.json") }
func (c Config) PeersDBPath() string { return filepath.Join(c.Home, "peers.db") }
func (c Config) LockPath() string    { return filepath.Join(c.Home, "session.lock") }
func (c Config) AuditPath() string   { return filepath.Join(c.Home, "audit.jsonl") }
func (c Config) CredsPath() string   { return filepath.Join(c.Home, "credentials.json") }

// LoadConfig reads TELEX_* settings. Credentials come from TG_APP_ID /
// TG_API_HASH, falling back to the credentials file `telex login` writes.
func LoadConfig(requireCreds bool) (Config, error) {
	home := os.Getenv("TELEX_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return Config{}, err
		}
		home = filepath.Join(h, ".local", "share", "telex")
	}
	cfg := Config{
		Home:         home,
		AllowedRoots: splitList(os.Getenv("TELEX_ALLOWED_ROOTS")),
		DownloadDir:  os.Getenv("TELEX_DOWNLOAD_DIR"),
		AllowedChats: splitList(os.Getenv("TELEX_ALLOWED_CHATS")),
	}
	if cfg.DownloadDir == "" {
		cfg.DownloadDir = filepath.Join(home, "downloads")
	}
	if v := os.Getenv("TELEX_READ_ONLY"); v != "" {
		ro, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("TELEX_READ_ONLY: %w", err)
		}
		cfg.ReadOnly = ro
	}
	if err := os.MkdirAll(cfg.Home, 0o700); err != nil {
		return Config{}, err
	}
	if err := os.Chmod(cfg.Home, 0o700); err != nil {
		return Config{}, err
	}

	saved, err := loadCreds(cfg.CredsPath())
	if err != nil {
		return Config{}, err
	}
	cfg.AppID, cfg.AppHash = saved.AppID, saved.AppHash
	if v := os.Getenv("TG_APP_ID"); v != "" {
		id, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return Config{}, fmt.Errorf("TG_APP_ID %q: %w", v, err)
		}
		cfg.AppID = id
	}
	if v := os.Getenv("TG_API_HASH"); v != "" {
		cfg.AppHash = v
	}
	if requireCreds && (cfg.AppID == 0 || cfg.AppHash == "") {
		return Config{}, errors.New("no Telegram API credentials: run `telex login` (or set TG_APP_ID and TG_API_HASH)")
	}
	return cfg, nil
}

// splitList splits on commas, and on the OS list separator for paths.
func splitList(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == os.PathListSeparator || r == ';' }) {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// creds is the credentials file: like the session next to it, it is
// readable only by the owner.
type creds struct {
	AppID   int    `json:"app_id"`
	AppHash string `json:"api_hash"`
}

func loadCreds(path string) (creds, error) {
	var c creds
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// saveCreds writes atomically with 0600 permissions.
func saveCreds(path string, c creds) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".credentials-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
