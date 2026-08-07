package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type NotificationRecord struct {
	ID            string      `json:"id"`
	ReceivedAt    time.Time   `json:"receivedAt"`
	Delivered     bool        `json:"delivered"`
	DeliveryError string      `json:"deliveryError,omitempty"`
	Alert         GrafanaJson `json:"alert"`
}

type historyStore struct {
	mu       sync.RWMutex
	records  []NotificationRecord
	filePath string
	limit    int
}

func newHistoryStore(filePath string, limit int) (*historyStore, error) {
	if limit < 1 {
		return nil, fmt.Errorf("history limit must be positive")
	}

	store := &historyStore{
		records:  make([]NotificationRecord, 0),
		filePath: filePath,
		limit:    limit,
	}
	if filePath == "" {
		return store, nil
	}

	contents, err := ioutil.ReadFile(filePath)
	if os.IsNotExist(err) {
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	if len(contents) == 0 {
		return store, nil
	}
	if err := json.Unmarshal(contents, &store.records); err != nil {
		return nil, fmt.Errorf("decode %s: %w", filePath, err)
	}
	store.sortAndTrimLocked()
	return store, nil
}

func (s *historyStore) Add(record NotificationRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.records = append(s.records, record)
	s.sortAndTrimLocked()
	if s.filePath == "" {
		return nil
	}
	return s.persistLocked()
}

func (s *historyStore) List(limit int) []NotificationRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit > len(s.records) {
		limit = len(s.records)
	}
	if limit < 0 {
		limit = 0
	}

	records := make([]NotificationRecord, limit)
	copy(records, s.records[:limit])
	return records
}

func (s *historyStore) sortAndTrimLocked() {
	sort.SliceStable(s.records, func(left, right int) bool {
		return s.records[left].ReceivedAt.After(s.records[right].ReceivedAt)
	})
	if len(s.records) > s.limit {
		s.records = s.records[:s.limit]
	}
}

func (s *historyStore) persistLocked() error {
	directory := filepath.Dir(s.filePath)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return fmt.Errorf("create history directory: %w", err)
	}

	contents, err := json.MarshalIndent(s.records, "", "  ")
	if err != nil {
		return fmt.Errorf("encode history: %w", err)
	}

	temporaryFile, err := ioutil.TempFile(directory, ".notifications-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary history file: %w", err)
	}
	temporaryPath := temporaryFile.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()

	if err := temporaryFile.Chmod(0600); err != nil {
		_ = temporaryFile.Close()
		return fmt.Errorf("secure temporary history file: %w", err)
	}
	if _, err := temporaryFile.Write(contents); err != nil {
		_ = temporaryFile.Close()
		return fmt.Errorf("write temporary history file: %w", err)
	}
	if err := temporaryFile.Sync(); err != nil {
		_ = temporaryFile.Close()
		return fmt.Errorf("sync temporary history file: %w", err)
	}
	if err := temporaryFile.Close(); err != nil {
		return fmt.Errorf("close temporary history file: %w", err)
	}
	if err := os.Rename(temporaryPath, s.filePath); err != nil {
		return fmt.Errorf("replace history file: %w", err)
	}
	removeTemporary = false
	return nil
}
