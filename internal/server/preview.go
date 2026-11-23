package server

import (
	"container/list"
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type frameCacheKey struct {
	recID  int64
	bucket int64
}

type frameCacheEntry struct {
	key  frameCacheKey
	data []byte
}

// frameCache is a bounded in-memory LRU cache for extracted preview JPEG frames.
type frameCache struct {
	mu       sync.Mutex
	maxBytes int64
	curBytes int64
	ll       *list.List
	items    map[frameCacheKey]*list.Element
}

func newFrameCache(maxBytes int64) *frameCache {
	if maxBytes <= 0 {
		maxBytes = 16 * 1024 * 1024
	}
	return &frameCache{
		maxBytes: maxBytes,
		ll:       list.New(),
		items:    make(map[frameCacheKey]*list.Element),
	}
}

func (c *frameCache) Get(key frameCacheKey) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(*frameCacheEntry).data, true
	}
	return nil, false
}

func (c *frameCache) Put(key frameCacheKey, data []byte) {
	if c == nil || len(data) == 0 || int64(len(data)) > c.maxBytes {
		return
	}
	copied := make([]byte, len(data))
	copy(copied, data)

	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.items[key]; ok {
		ent := el.Value.(*frameCacheEntry)
		c.curBytes -= int64(len(ent.data))
		ent.data = copied
		c.curBytes += int64(len(copied))
		c.ll.MoveToFront(el)
	} else {
		ent := &frameCacheEntry{key: key, data: copied}
		el := c.ll.PushFront(ent)
		c.items[key] = el
		c.curBytes += int64(len(copied))
	}

	for c.curBytes > c.maxBytes {
		back := c.ll.Back()
		if back == nil {
			break
		}
		c.removeElementLocked(back)
	}
}

func (c *frameCache) DeleteRecording(recID int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for el := c.ll.Front(); el != nil; {
		next := el.Next()
		if el.Value.(*frameCacheEntry).key.recID == recID {
			c.removeElementLocked(el)
		}
		el = next
	}
}

func (c *frameCache) removeElementLocked(el *list.Element) {
	c.ll.Remove(el)
	ent := el.Value.(*frameCacheEntry)
	delete(c.items, ent.key)
	c.curBytes -= int64(len(ent.data))
	if c.curBytes < 0 {
		c.curBytes = 0
	}
}

func (s *Server) validateSafeStoragePath(rawRel string) (string, int) {
	if rawRel == "" || strings.ContainsRune(rawRel, '\\') || strings.ContainsRune(rawRel, 0) {
		return "", http.StatusNotFound
	}
	for _, seg := range strings.Split(rawRel, "/") {
		if seg == ".." {
			return "", http.StatusForbidden
		}
	}

	cleaned := strings.TrimPrefix(path.Clean("/"+rawRel), "/")
	if cleaned == "" || cleaned == "." {
		return "", http.StatusNotFound
	}

	abs := filepath.Join(s.storageAbs, filepath.FromSlash(cleaned))
	relCheck, err := filepath.Rel(s.storageAbs, abs)
	if err != nil || relCheck == "." || strings.HasPrefix(relCheck, "..") || filepath.IsAbs(relCheck) {
		return "", http.StatusForbidden
	}
	return abs, http.StatusOK
}

func (s *Server) resolveSafeStoragePath(rawRel string) (string, os.FileInfo, int) {
	abs, status := s.validateSafeStoragePath(rawRel)
	if status != http.StatusOK {
		return "", nil, status
	}
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		return "", nil, http.StatusNotFound
	}
	return abs, info, http.StatusOK
}

func writeJPEGFrame(w http.ResponseWriter, data []byte, cacheControl string) {
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", cacheControl)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) serveFallbackThumb(w http.ResponseWriter, thumbPath string) bool {
	thumbPath = strings.TrimSpace(thumbPath)
	if thumbPath == "" {
		return false
	}
	abs, info, status := s.resolveSafeStoragePath(thumbPath)
	if status != http.StatusOK || info.Size() <= 0 || info.Size() > 2*1024*1024 {
		return false
	}
	data, err := os.ReadFile(abs)
	if err != nil || len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return false
	}
	writeJPEGFrame(w, data, "no-store")
	return true
}

func (s *Server) handleRecordingFrame(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		writeJSONError(w, http.StatusBadRequest, "invalid recording id")
		return
	}

	var seekSec float64
	if tStr := strings.TrimSpace(r.URL.Query().Get("t")); tStr != "" {
		v, parseErr := strconv.ParseFloat(tStr, 64)
		if parseErr != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			writeJSONError(w, http.StatusBadRequest, "invalid t query parameter")
			return
		}
		seekSec = v
	}

	rec, err := s.db.GetRecording(r.Context(), id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeJSONError(w, http.StatusNotFound, "recording not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	abs, status := s.validateSafeStoragePath(rec.FilePath)
	if status != http.StatusOK {
		if status == http.StatusForbidden {
			http.Error(w, "Forbidden", http.StatusForbidden)
		} else {
			http.NotFound(w, r)
		}
		return
	}

	if rec.DurationSec > 0.5 && seekSec > rec.DurationSec-0.2 {
		seekSec = math.Max(0, rec.DurationSec-0.2)
	}
	bucket := int64(math.Round(seekSec * 2))
	seekSec = float64(bucket) / 2.0

	key := frameCacheKey{recID: id, bucket: bucket}
	if cached, ok := s.frameCache.Get(key); ok {
		writeJPEGFrame(w, cached, "public, max-age=3600")
		return
	}

	if info, statErr := os.Stat(abs); statErr != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}

	ffmpegBin := ""
	if s.cfg != nil {
		ffmpegBin = strings.TrimSpace(s.cfg.FFmpegBin)
	}
	if ffmpegBin == "" {
		if s.serveFallbackThumb(w, rec.ThumbPath) {
			return
		}
		writeJSONError(w, http.StatusServiceUnavailable, "ffmpeg unavailable for frame preview")
		return
	}

	select {
	case s.frameSem <- struct{}{}:
		defer func() { <-s.frameSem }()
	case <-r.Context().Done():
		return
	}

	if cached, ok := s.frameCache.Get(key); ok {
		writeJPEGFrame(w, cached, "public, max-age=3600")
		return
	}

	cmdCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-threads", "1",
		"-noaccurate_seek",
		"-ss", strconv.FormatFloat(seekSec, 'f', 2, 64),
		"-i", abs,
		"-frames:v", "1",
		"-vf", "scale=240:-2",
		"-q:v", "6",
		"-f", "image2pipe",
		"-vcodec", "mjpeg",
		"-",
	}

	cmd := exec.CommandContext(cmdCtx, ffmpegBin, args...)
	stdout, pipeErr := cmd.StdoutPipe()
	if pipeErr != nil {
		if !s.serveFallbackThumb(w, rec.ThumbPath) {
			writeJSONError(w, http.StatusInternalServerError, pipeErr.Error())
		}
		return
	}

	if startErr := cmd.Start(); startErr != nil {
		if !s.serveFallbackThumb(w, rec.ThumbPath) {
			writeJSONError(w, http.StatusInternalServerError, startErr.Error())
		}
		return
	}
	if s.procJob != nil && cmd.Process != nil {
		_ = s.procJob.Assign(cmd.Process.Pid)
	}

	jpg, readErr := io.ReadAll(io.LimitReader(stdout, 512*1024))
	waitErr := cmd.Wait()

	if readErr == nil && waitErr == nil && len(jpg) >= 4 && jpg[0] == 0xFF && jpg[1] == 0xD8 {
		s.frameCache.Put(key, jpg)
		writeJPEGFrame(w, jpg, "public, max-age=3600")
		return
	}

	if r.Context().Err() != nil {
		return
	}
	if s.serveFallbackThumb(w, rec.ThumbPath) {
		return
	}
	http.NotFound(w, r)
}
