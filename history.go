package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type NotificationRecord struct {
	ID            string      `json:"id"`
	ReceivedAt    time.Time   `json:"receivedAt"`
	Delivered     bool        `json:"delivered"`
	DeliveryError string      `json:"deliveryError,omitempty"`
	Seen          bool        `json:"seen"`
	GroupKey      string      `json:"groupKey"`
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
	for index := range store.records {
		if err := prepareNotificationRecord(&store.records[index]); err != nil {
			return nil, fmt.Errorf("prepare record %q: %w", store.records[index].ID, err)
		}
	}
	store.sortAndTrimLocked()
	return store, nil
}

func (s *historyStore) Add(record NotificationRecord) error {
	if err := prepareNotificationRecord(&record); err != nil {
		return err
	}

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

func (s *historyStore) MarkSeen(ids []string, all bool) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	idSet := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		idSet[id] = struct{}{}
	}

	changed := make([]int, 0)
	for index := range s.records {
		if s.records[index].Seen {
			continue
		}
		if !all {
			if _, selected := idSet[s.records[index].ID]; !selected {
				continue
			}
		}
		s.records[index].Seen = true
		changed = append(changed, index)
	}

	if len(changed) == 0 || s.filePath == "" {
		return len(changed), nil
	}
	if err := s.persistLocked(); err != nil {
		for _, index := range changed {
			s.records[index].Seen = false
		}
		return 0, err
	}
	return len(changed), nil
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

type alertGroupIdentity struct {
	Version     int                    `json:"version"`
	OrgID       int                    `json:"orgId"`
	RuleID      int64                  `json:"ruleId,omitempty"`
	RuleURL     string                 `json:"ruleUrl,omitempty"`
	Name        string                 `json:"name,omitempty"`
	DashboardID int                    `json:"dashboardId,omitempty"`
	PanelID     int                    `json:"panelId,omitempty"`
	State       string                 `json:"state"`
	Tags        map[string]interface{} `json:"tags,omitempty"`
	Evaluations []string               `json:"evaluations,omitempty"`
}

type alertEvaluationIdentity struct {
	Metric string                 `json:"metric,omitempty"`
	Tags   map[string]interface{} `json:"tags,omitempty"`
}

func prepareNotificationRecord(record *NotificationRecord) error {
	if record.GroupKey != "" {
		return nil
	}
	groupKey, err := notificationGroupKey(record.Alert)
	if err != nil {
		return fmt.Errorf("create notification group key: %w", err)
	}
	record.GroupKey = groupKey
	return nil
}

func notificationGroupKey(alert GrafanaJson) (string, error) {
	identity := alertGroupIdentity{
		Version: 1,
		OrgID:   alert.OrgID,
		RuleID:  alert.RuleID,
		State:   normalizeIdentityText(alert.State),
	}
	if len(alert.Tags) > 0 {
		identity.Tags = alert.Tags
	}

	for _, evaluation := range alert.EvalMatches {
		if evaluation.Metric == "" && len(evaluation.Tags) == 0 {
			continue
		}
		evaluationIdentity := alertEvaluationIdentity{
			Metric: normalizeIdentityText(evaluation.Metric),
		}
		if len(evaluation.Tags) > 0 {
			evaluationIdentity.Tags = evaluation.Tags
		}
		encoded, err := json.Marshal(evaluationIdentity)
		if err != nil {
			return "", err
		}
		identity.Evaluations = append(identity.Evaluations, string(encoded))
	}
	sort.Strings(identity.Evaluations)

	if alert.RuleID == 0 {
		identity.RuleURL = strings.TrimSpace(alert.RuleURL)
		identity.DashboardID = alert.DashboardID
		identity.PanelID = alert.PanelID
		identity.Name = normalizeIdentityText(alert.RuleName)
		if identity.Name == "" {
			identity.Name = normalizeIdentityText(alert.Title)
		}
		if identity.Name == "" {
			identity.Name = normalizeIdentityText(alert.Message)
		}
	}

	encoded, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("v1-%x", digest[:]), nil
}

func normalizeIdentityText(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}
