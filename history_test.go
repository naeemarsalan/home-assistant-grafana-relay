package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHistoryStorePersistsNewestRecordsWithinLimit(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "nested", "notifications.json")
	store, err := newHistoryStore(filePath, 2)
	if err != nil {
		t.Fatalf("newHistoryStore() error = %v", err)
	}

	baseTime := time.Date(2026, time.August, 7, 12, 0, 0, 0, time.UTC)
	records := []NotificationRecord{
		{ID: "oldest", ReceivedAt: baseTime, Alert: GrafanaJson{Title: "Oldest"}},
		{ID: "newest", ReceivedAt: baseTime.Add(2 * time.Minute), Alert: GrafanaJson{Title: "Newest"}},
		{ID: "middle", ReceivedAt: baseTime.Add(time.Minute), Alert: GrafanaJson{Title: "Middle"}},
	}
	for _, record := range records {
		if err := store.Add(record); err != nil {
			t.Fatalf("Add() error = %v", err)
		}
	}

	got := store.List(10)
	if len(got) != 2 {
		t.Fatalf("List() length = %d, want 2", len(got))
	}
	if got[0].ID != "newest" || got[1].ID != "middle" {
		t.Fatalf("List() order = [%s, %s], want [newest, middle]", got[0].ID, got[1].ID)
	}

	info, err := os.Stat(filePath)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("history permissions = %o, want 600", info.Mode().Perm())
	}

	reloaded, err := newHistoryStore(filePath, 2)
	if err != nil {
		t.Fatalf("reloading history error = %v", err)
	}
	reloadedRecords := reloaded.List(10)
	if len(reloadedRecords) != 2 || reloadedRecords[0].ID != "newest" || reloadedRecords[1].ID != "middle" {
		t.Fatalf("reloaded history = %#v", reloadedRecords)
	}
}

func TestHistoryStoreRejectsInvalidConfigurationAndCorruptData(t *testing.T) {
	if _, err := newHistoryStore("", 0); err == nil {
		t.Fatal("newHistoryStore() with zero limit returned no error")
	}

	filePath := filepath.Join(t.TempDir(), "notifications.json")
	if err := os.WriteFile(filePath, []byte("not json"), 0600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := newHistoryStore(filePath, 10); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("newHistoryStore() error = %v, want decode error", err)
	}
}

func TestHistoryStoreSupportsConcurrentAdds(t *testing.T) {
	store, err := newHistoryStore("", 25)
	if err != nil {
		t.Fatalf("newHistoryStore() error = %v", err)
	}

	var waitGroup sync.WaitGroup
	for index := 0; index < 100; index++ {
		waitGroup.Add(1)
		go func(index int) {
			defer waitGroup.Done()
			_ = store.Add(NotificationRecord{
				ID:         string(rune(index + 1)),
				ReceivedAt: time.Unix(int64(index), 0),
			})
		}(index)
	}
	waitGroup.Wait()

	got := store.List(100)
	if len(got) != 25 {
		t.Fatalf("List() length = %d, want 25", len(got))
	}
	for index := 1; index < len(got); index++ {
		if got[index].ReceivedAt.After(got[index-1].ReceivedAt) {
			t.Fatalf("history is not newest-first at index %d", index)
		}
	}
}
