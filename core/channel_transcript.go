package core

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	channelTranscriptStoreLimit      = 100
	channelTranscriptStoreContentMax = 4000
)

// ChannelTranscriptEntry is one visible chat message stored per channel so
// different project engines can reconstruct recent shared chat context.
type ChannelTranscriptEntry struct {
	Role      string    `json:"role"` // "user" or "assistant"
	Speaker   string    `json:"speaker"`
	Content   string    `json:"content"`
	SourceID  string    `json:"source_id,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

type channelTranscriptSnapshot struct {
	Channels map[string][]ChannelTranscriptEntry `json:"channels"`
}

// ChannelTranscriptStore persists recent visible chat messages per channel.
type ChannelTranscriptStore struct {
	mu        sync.RWMutex
	channels  map[string][]ChannelTranscriptEntry
	storePath string
}

func NewChannelTranscriptStore(storePath string) *ChannelTranscriptStore {
	s := &ChannelTranscriptStore{
		channels:  make(map[string][]ChannelTranscriptEntry),
		storePath: storePath,
	}
	if storePath != "" {
		s.load()
	}
	return s
}

func (s *ChannelTranscriptStore) Add(channelKey string, entry ChannelTranscriptEntry) {
	channelKey = strings.TrimSpace(channelKey)
	if channelKey == "" {
		return
	}
	entry.Content = strings.TrimSpace(entry.Content)
	if entry.Content == "" {
		return
	}
	entry.Content = truncateRunesForTranscript(entry.Content, channelTranscriptStoreContentMax)
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if entry.SourceID != "" {
		for _, existing := range s.channels[channelKey] {
			if existing.SourceID == entry.SourceID {
				return
			}
		}
	}

	s.channels[channelKey] = append(s.channels[channelKey], entry)
	if extra := len(s.channels[channelKey]) - channelTranscriptStoreLimit; extra > 0 {
		s.channels[channelKey] = append([]ChannelTranscriptEntry(nil), s.channels[channelKey][extra:]...)
	}
	s.saveLocked()
}

func (s *ChannelTranscriptStore) Recent(channelKey string, limit int) []ChannelTranscriptEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entries := s.channels[channelKey]
	if len(entries) == 0 {
		return nil
	}
	if limit <= 0 || limit > len(entries) {
		limit = len(entries)
	}
	out := make([]ChannelTranscriptEntry, limit)
	copy(out, entries[len(entries)-limit:])
	return out
}

func (s *ChannelTranscriptStore) saveLocked() {
	if s.storePath == "" {
		return
	}
	data, err := json.MarshalIndent(channelTranscriptSnapshot{Channels: s.channels}, "", "  ")
	if err != nil {
		slog.Error("channel_transcript: failed to marshal", "error", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.storePath), 0o755); err != nil {
		slog.Error("channel_transcript: failed to create dir", "path", s.storePath, "error", err)
		return
	}
	if err := AtomicWriteFile(s.storePath, data, 0o644); err != nil {
		slog.Error("channel_transcript: failed to write", "path", s.storePath, "error", err)
	}
}

func (s *ChannelTranscriptStore) load() {
	data, err := os.ReadFile(s.storePath)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Error("channel_transcript: failed to read", "path", s.storePath, "error", err)
		}
		return
	}

	var snapshot channelTranscriptSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		slog.Error("channel_transcript: failed to unmarshal", "path", s.storePath, "error", err)
		return
	}
	if snapshot.Channels != nil {
		s.channels = snapshot.Channels
	}
}

func truncateRunesForTranscript(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "..."
}
