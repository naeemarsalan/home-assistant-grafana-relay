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

func TestHistoryStoreLoadsLegacyRecordsAndPersistsSeenState(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "notifications.json")
	legacy := `[{
		"id":"legacy",
		"receivedAt":"2026-08-07T12:00:00Z",
		"delivered":true,
		"alert":{"title":"Legacy alert","ruleId":42,"state":"alerting"}
	}]`
	if err := os.WriteFile(filePath, []byte(legacy), 0600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	store, err := newHistoryStore(filePath, 10)
	if err != nil {
		t.Fatalf("newHistoryStore() error = %v", err)
	}
	records := store.List(10)
	if len(records) != 1 || records[0].Seen || records[0].GroupKey == "" {
		t.Fatalf("legacy record after load = %#v", records)
	}

	updated, err := store.MarkSeen([]string{"legacy"}, false)
	if err != nil {
		t.Fatalf("MarkSeen() error = %v", err)
	}
	if updated != 1 {
		t.Fatalf("MarkSeen() updated = %d, want 1", updated)
	}

	reloaded, err := newHistoryStore(filePath, 10)
	if err != nil {
		t.Fatalf("reload error = %v", err)
	}
	reloadedRecords := reloaded.List(10)
	if len(reloadedRecords) != 1 || !reloadedRecords[0].Seen || reloadedRecords[0].GroupKey == "" {
		t.Fatalf("record after reload = %#v", reloadedRecords)
	}
}

func TestHistoryStoreMarksSelectedAndAllRecordsSeen(t *testing.T) {
	store, err := newHistoryStore("", 10)
	if err != nil {
		t.Fatalf("newHistoryStore() error = %v", err)
	}
	for _, id := range []string{"one", "two", "three"} {
		if err := store.Add(NotificationRecord{ID: id, ReceivedAt: time.Now(), Alert: GrafanaJson{RuleID: 1}}); err != nil {
			t.Fatalf("Add(%s) error = %v", id, err)
		}
	}

	updated, err := store.MarkSeen([]string{"one", "two", "missing", "one"}, false)
	if err != nil || updated != 2 {
		t.Fatalf("MarkSeen(selected) = %d, %v; want 2, nil", updated, err)
	}
	updated, err = store.MarkSeen([]string{"one"}, false)
	if err != nil || updated != 0 {
		t.Fatalf("repeated MarkSeen() = %d, %v; want 0, nil", updated, err)
	}
	updated, err = store.MarkSeen(nil, true)
	if err != nil || updated != 1 {
		t.Fatalf("MarkSeen(all) = %d, %v; want 1, nil", updated, err)
	}
	for _, record := range store.List(10) {
		if !record.Seen {
			t.Fatalf("record %q remains unseen", record.ID)
		}
	}
}

func TestHistoryStoreRollsBackSeenStateWhenPersistenceFails(t *testing.T) {
	store, err := newHistoryStore("", 10)
	if err != nil {
		t.Fatalf("newHistoryStore() error = %v", err)
	}
	if err := store.Add(NotificationRecord{ID: "one", ReceivedAt: time.Now()}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	blockingPath := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockingPath, []byte("block"), 0600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	store.filePath = filepath.Join(blockingPath, "notifications.json")
	updated, err := store.MarkSeen([]string{"one"}, false)
	if err == nil || updated != 0 {
		t.Fatalf("MarkSeen() = %d, %v; want 0 and persistence error", updated, err)
	}
	if store.List(1)[0].Seen {
		t.Fatal("record stayed seen after persistence failure")
	}
}

func TestNotificationGroupKeyCombinesRepeatedRuleOccurrences(t *testing.T) {
	first := GrafanaJson{
		Title:    "Basement humidity",
		Message:  "Humidity is 76%",
		RuleID:   42,
		RuleName: "High humidity",
		State:    "ALERTING",
		OrgID:    1,
		ImageURL: "https://grafana.example/first.png",
		Tags:     map[string]interface{}{"room": "basement"},
		EvalMatches: []GrafanaEvalMatch{{
			Value:  76.0,
			Metric: "humidity",
			Tags:   map[string]interface{}{"sensor": "one"},
		}},
	}
	second := first
	second.Title = "A changed display title"
	second.Message = "Humidity is now 81%"
	second.ImageURL = "https://grafana.example/second.png"
	second.EvalMatches = []GrafanaEvalMatch{{
		Value:  81.0,
		Metric: " humidity ",
		Tags:   map[string]interface{}{"sensor": "one"},
	}}

	firstKey, err := notificationGroupKey(first)
	if err != nil {
		t.Fatalf("notificationGroupKey(first) error = %v", err)
	}
	secondKey, err := notificationGroupKey(second)
	if err != nil {
		t.Fatalf("notificationGroupKey(second) error = %v", err)
	}
	if firstKey != secondKey {
		t.Fatalf("repeat keys differ: %q != %q", firstKey, secondKey)
	}

	resolved := second
	resolved.State = "ok"
	resolvedKey, _ := notificationGroupKey(resolved)
	if resolvedKey == firstKey {
		t.Fatal("resolved and alerting occurrences were grouped together")
	}

	differentLabels := second
	differentLabels.Tags = map[string]interface{}{"room": "attic"}
	differentKey, _ := notificationGroupKey(differentLabels)
	if differentKey == firstKey {
		t.Fatal("different alert label sets were grouped together")
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
