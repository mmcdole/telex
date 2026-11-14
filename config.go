package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// keychainService is the macOS Keychain service that holds the API credentials.
const keychainService = "telex"

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

// LoadConfig reads TELEX_* settings. Credentials come from TG_APP_ID /
// TG_API_HASH, falling back to the Keychain entries `telex login` stores.
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

	idStr := firstNonEmpty(os.Getenv("TG_APP_ID"), keychainGet("app_id"))
	cfg.AppHash = firstNonEmpty(os.Getenv("TG_API_HASH"), keychainGet("api_hash"))
	if idStr != "" {
		id, err := strconv.Atoi(strings.TrimSpace(idStr))
		if err != nil {
			return Config{}, fmt.Errorf("app id %q: %w", idStr, err)
		}
		cfg.AppID = id
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

func keychainGet(account string) string {
	out, err := exec.Command("security", "find-generic-password", "-s", keychainService, "-a", account, "-w").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func keychainSet(account, value string) error {
	// -U updates an existing item instead of failing.
	cmd := exec.Command("security", "add-generic-password", "-U", "-s", keychainService, "-a", account, "-w", value)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("keychain: %v: %s", err, out)
	}
	return nil
}
