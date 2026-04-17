package core

import (
	"path/filepath"
	"testing"
)

func TestChannelTranscriptStore_PersistsAndDedupesBySourceID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "channel_transcripts.json")
	store := NewChannelTranscriptStore(path)

	store.Add("discord:room-1", ChannelTranscriptEntry{
		Role:     "user",
		Speaker:  "MeteorSky",
		Content:  "hello",
		SourceID: "user:discord:msg-1",
	})
	store.Add("discord:room-1", ChannelTranscriptEntry{
		Role:     "user",
		Speaker:  "MeteorSky",
		Content:  "hello duplicate",
		SourceID: "user:discord:msg-1",
	})
	store.Add("discord:room-1", ChannelTranscriptEntry{
		Role:     "assistant",
		Speaker:  "claude",
		Content:  "hi back",
		SourceID: "assistant:claude:msg-1",
	})

	got := store.Recent("discord:room-1", 10)
	if len(got) != 2 {
		t.Fatalf("Recent() len = %d, want 2", len(got))
	}
	if got[0].Content != "hello" {
		t.Fatalf("first content = %q, want %q", got[0].Content, "hello")
	}
	if got[1].Content != "hi back" {
		t.Fatalf("second content = %q, want %q", got[1].Content, "hi back")
	}

	reloaded := NewChannelTranscriptStore(path)
	got = reloaded.Recent("discord:room-1", 10)
	if len(got) != 2 {
		t.Fatalf("reloaded Recent() len = %d, want 2", len(got))
	}
	if got[0].Content != "hello" || got[1].Content != "hi back" {
		t.Fatalf("reloaded contents = %#v, want preserved transcript", got)
	}
}
