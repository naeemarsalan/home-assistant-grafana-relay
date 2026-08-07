package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWebhookDeliversAndAppearsInHistory(t *testing.T) {
	type deliveredPayload struct {
		Message string                 `json:"message"`
		Title   string                 `json:"title"`
		Data    map[string]interface{} `json:"data"`
	}
	delivered := make(chan deliveredPayload, 1)
	homeAssistant := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			t.Errorf("Home Assistant method = %s, want POST", req.Method)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want bearer token", got)
		}
		var payload deliveredPayload
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Errorf("decode Home Assistant payload: %v", err)
		}
		delivered <- payload
		rw.WriteHeader(http.StatusOK)
	}))
	defer homeAssistant.Close()

	app := newTestApplication(t, homeAssistant.URL)
	webhook := `{
		"title":"Basement humidity",
		"message":"Humidity has remained above 75% for 10 minutes.\nCheck the dehumidifier.",
		"state":"alerting",
		"ruleName":"High humidity",
		"ruleUrl":"https://grafana.example/alert/1",
		"imageUrl":"https://grafana.example/chart.png",
		"dashboardId":12,
		"panelId":4,
		"tags":{"room":"basement"},
		"evalMatches":[{"value":76.4,"metric":"humidity","tags":{"sensor":"one"}}]
	}`

	response := performRequest(app.routes(), http.MethodPost, "/", webhook, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("POST / status = %d, want 200; body = %s", response.Code, response.Body.String())
	}

	select {
	case payload := <-delivered:
		if payload.Title != "Basement humidity" {
			t.Errorf("delivered title = %q", payload.Title)
		}
		if !strings.Contains(payload.Message, "Check the dehumidifier") {
			t.Errorf("delivered message = %q", payload.Message)
		}
		if payload.Data["channel"] != "grafana_alerts" {
			t.Errorf("delivered channel = %#v", payload.Data["channel"])
		}
	case <-time.After(time.Second):
		t.Fatal("Home Assistant did not receive notification")
	}

	historyResponse := performRequest(app.routes(), http.MethodGet, "/api/notifications", "", nil)
	if historyResponse.Code != http.StatusOK {
		t.Fatalf("GET history status = %d, want 200", historyResponse.Code)
	}
	var history struct {
		Notifications []NotificationRecord `json:"notifications"`
		Count         int                  `json:"count"`
	}
	if err := json.NewDecoder(historyResponse.Body).Decode(&history); err != nil {
		t.Fatalf("decode history response: %v", err)
	}
	if history.Count != 1 || len(history.Notifications) != 1 {
		t.Fatalf("history = %#v, want one notification", history)
	}
	record := history.Notifications[0]
	if !record.Delivered || record.Alert.Message != "Humidity has remained above 75% for 10 minutes.\nCheck the dehumidifier." {
		t.Fatalf("history record = %#v", record)
	}
	if record.Alert.EvalMatches[0].Value != float64(76.4) {
		t.Errorf("evaluation value = %#v", record.Alert.EvalMatches[0].Value)
	}
}

func TestDeliveryFailureIsRecordedWithoutStoppingServer(t *testing.T) {
	homeAssistant := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		http.Error(rw, "notify service unavailable", http.StatusServiceUnavailable)
	}))
	defer homeAssistant.Close()

	app := newTestApplication(t, homeAssistant.URL)
	for attempt := 0; attempt < 2; attempt++ {
		response := performRequest(app.routes(), http.MethodPost, "/", `{"title":"Failure","message":"Still visible"}`, nil)
		if response.Code != http.StatusBadGateway {
			t.Fatalf("attempt %d status = %d, want 502", attempt, response.Code)
		}
	}

	records := app.history.List(10)
	if len(records) != 2 {
		t.Fatalf("history length = %d, want 2", len(records))
	}
	if records[0].Delivered || !strings.Contains(records[0].DeliveryError, "503 Service Unavailable") {
		t.Fatalf("failure record = %#v", records[0])
	}
}

func TestHistoryPageAndAuthentication(t *testing.T) {
	app := newTestApplication(t, "http://127.0.0.1")
	app.config.webUIUsername = "viewer"
	app.config.webUIPassword = "a-strong-password"

	unauthorized := performRequest(app.routes(), http.MethodGet, "/", "", nil)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized GET / status = %d, want 401", unauthorized.Code)
	}
	if unauthorized.Header().Get("WWW-Authenticate") == "" {
		t.Error("unauthorized response has no WWW-Authenticate header")
	}

	authorized := performRequest(app.routes(), http.MethodGet, "/", "", func(req *http.Request) {
		req.SetBasicAuth("viewer", "a-strong-password")
	})
	if authorized.Code != http.StatusOK {
		t.Fatalf("authorized GET / status = %d, want 200", authorized.Code)
	}
	if !strings.Contains(authorized.Body.String(), "Grafana alert history") || !strings.Contains(authorized.Body.String(), "white-space: pre-wrap") {
		t.Error("history page does not contain expected full-message UI")
	}

	apiUnauthorized := performRequest(app.routes(), http.MethodGet, "/api/notifications", "", nil)
	if apiUnauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized API status = %d, want 401", apiUnauthorized.Code)
	}
}

func TestWebhookSecretAndInvalidRequests(t *testing.T) {
	var deliveries int32
	homeAssistant := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		atomic.AddInt32(&deliveries, 1)
		rw.WriteHeader(http.StatusOK)
	}))
	defer homeAssistant.Close()

	app := newTestApplication(t, homeAssistant.URL)
	app.config.webhookSecret = "webhook-password"

	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		headers    func(*http.Request)
		wantStatus int
	}{
		{name: "missing secret", method: http.MethodPost, path: "/", body: `{}`, wantStatus: http.StatusUnauthorized},
		{name: "invalid JSON", method: http.MethodPost, path: "/", body: `{`, headers: webhookSecretHeader, wantStatus: http.StatusBadRequest},
		{name: "multiple values", method: http.MethodPost, path: "/", body: `{} {}`, headers: webhookSecretHeader, wantStatus: http.StatusBadRequest},
		{name: "wrong method", method: http.MethodDelete, path: "/", wantStatus: http.StatusMethodNotAllowed},
		{name: "unknown path", method: http.MethodGet, path: "/missing", wantStatus: http.StatusNotFound},
		{name: "invalid limit", method: http.MethodGet, path: "/api/notifications?limit=zero", wantStatus: http.StatusBadRequest},
		{name: "valid secret", method: http.MethodPost, path: "/", body: `{"title":"Allowed"}`, headers: webhookSecretHeader, wantStatus: http.StatusOK},
		{name: "legacy custom webhook path", method: http.MethodPost, path: "/grafana-webhook", body: `{"title":"Also allowed"}`, headers: webhookSecretHeader, wantStatus: http.StatusOK},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := performRequest(app.routes(), test.method, test.path, test.body, test.headers)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, test.wantStatus, response.Body.String())
			}
		})
	}
	if got := atomic.LoadInt32(&deliveries); got != 2 {
		t.Fatalf("Home Assistant deliveries = %d, want 2", got)
	}
}

func TestWebhookBodyLimit(t *testing.T) {
	app := newTestApplication(t, "http://127.0.0.1")
	body := `{"message":"` + strings.Repeat("x", maxWebhookBodySize) + `"}`
	response := performRequest(app.routes(), http.MethodPost, "/", body, nil)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized webhook status = %d, want 413; body = %s", response.Code, response.Body.String())
	}
}

func TestHealthEndpoint(t *testing.T) {
	app := newTestApplication(t, "http://127.0.0.1")
	response := performRequest(app.routes(), http.MethodGet, "/healthz", "", nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"ok"`) {
		t.Fatalf("health response = %d %s", response.Code, response.Body.String())
	}
}

func TestLoadConfigDefaultsAndValidation(t *testing.T) {
	for _, name := range []string{
		"AUTH_TOKEN",
		"HM_SERVICE_URI",
		"LISTEN_PORT",
		"LISTEN_HOST",
		"NOTIFICATION_CHANNEL",
		"HISTORY_FILE",
		"HISTORY_LIMIT",
		"WEBHOOK_SECRET",
		"WEB_UI_USERNAME",
		"WEB_UI_PASSWORD",
	} {
		replaceEnvironment(t, name, "", false)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if cfg.listenHost != defaultListenHost || cfg.listenPort != defaultListenPort || cfg.historyLimit != defaultHistoryLimit {
		t.Fatalf("loadConfig() defaults = host %q, port %q, limit %d", cfg.listenHost, cfg.listenPort, cfg.historyLimit)
	}

	replaceEnvironment(t, "HISTORY_LIMIT", "0", true)
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "HISTORY_LIMIT") {
		t.Fatalf("loadConfig() invalid limit error = %v", err)
	}
	replaceEnvironment(t, "HISTORY_LIMIT", "25", true)
	replaceEnvironment(t, "WEB_UI_USERNAME", "viewer", true)
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "must be set together") {
		t.Fatalf("loadConfig() incomplete UI credentials error = %v", err)
	}
}

func newTestApplication(t *testing.T, homeAssistantURI string) *application {
	t.Helper()
	history, err := newHistoryStore("", 10)
	if err != nil {
		t.Fatalf("newHistoryStore() error = %v", err)
	}
	app := newApplication(config{
		authToken:           "test-token",
		homeAssistantURI:    homeAssistantURI,
		notificationChannel: "grafana_alerts",
		historyLimit:        10,
	}, history)
	app.now = func() time.Time {
		return time.Date(2026, time.August, 7, 12, 30, 0, 0, time.UTC)
	}
	return app
}

func performRequest(handler http.Handler, method, target, body string, configure func(*http.Request)) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	if configure != nil {
		configure(request)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func webhookSecretHeader(req *http.Request) {
	req.Header.Set("X-Webhook-Secret", "webhook-password")
}

func replaceEnvironment(t *testing.T, name, value string, set bool) {
	t.Helper()
	oldValue, wasSet := os.LookupEnv(name)
	if set {
		if err := os.Setenv(name, value); err != nil {
			t.Fatalf("Setenv(%s) error = %v", name, err)
		}
	} else if err := os.Unsetenv(name); err != nil {
		t.Fatalf("Unsetenv(%s) error = %v", name, err)
	}
	t.Cleanup(func() {
		if wasSet {
			_ = os.Setenv(name, oldValue)
		} else {
			_ = os.Unsetenv(name)
		}
	})
}

func Example_application_routes() {
	store, _ := newHistoryStore("", 10)
	app := newApplication(config{historyLimit: 10}, store)
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	response := httptest.NewRecorder()
	app.routes().ServeHTTP(response, request)
	fmt.Println(response.Code)
	// Output: 200
}
