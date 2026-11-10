package transcript

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
)

type PersistentCache struct {
	Directory string
}

type cacheEntry struct {
	Key        string            `json:"key"`
	Transcript domain.Transcript `json:"transcript"`
}

func DefaultCacheDirectory() (string, error) {
	root, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "MyClipGo", "cache", "transcripts"), nil
}

func (c *PersistentCache) Load(videoPath, identity string) (domain.Transcript, bool) {
	key, err := cacheKey(videoPath, identity)
	if err != nil {
		return domain.Transcript{}, false
	}
	path := filepath.Join(c.Directory, key+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return domain.Transcript{}, false
	}
	var entry cacheEntry
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&entry); err != nil {
		return domain.Transcript{}, false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return domain.Transcript{}, false
	}
	if entry.Key != key || entry.Transcript.Source != identity || !usable(entry.Transcript) {
		return domain.Transcript{}, false
	}
	return entry.Transcript, true
}

func (c *PersistentCache) Store(videoPath, identity string, result domain.Transcript) error {
	if result.Source != identity || !usable(result) {
		return &InvalidResultError{}
	}
	key, err := cacheKey(videoPath, identity)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(c.Directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(c.Directory, key+".*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	encoder := json.NewEncoder(temporary)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(cacheEntry{Key: key, Transcript: result}); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Chmod(temporaryPath, 0o600); err != nil {
		return err
	}
	return os.Rename(temporaryPath, filepath.Join(c.Directory, key+".json"))
}

// CacheKey returns the SHA-256 identity for the normalized source file and
// complete transcriber/model identity.
func CacheKey(videoPath, identity string) (string, error) { return cacheKey(videoPath, identity) }

func cacheKey(videoPath, identity string) (string, error) {
	absolute, err := filepath.Abs(videoPath)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	if runtime.GOOS == "windows" {
		absolute = strings.ToLower(absolute)
	}
	info, err := os.Stat(videoPath)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("视频不是普通文件")
	}
	payload := fmt.Sprintf("%s\x00%d\x00%d\x00%s", absolute, info.Size(), info.ModTime().UnixNano(), identity)
	digest := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(digest[:]), nil
}
