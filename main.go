package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	defaultListenHost   = "0.0.0.0"
	defaultListenPort   = "12000"
	defaultHistoryLimit = 200
	maxWebhookBodySize  = 1 << 20 // 1 MiB
)

type config struct {
	authToken           string
	homeAssistantURI    string
	listenPort          string
	listenHost          string
	notificationChannel string
	historyFile         string
	historyLimit        int
	webhookSecret       string
	webUIUsername       string
	webUIPassword       string
}

func loadConfig() (config, error) {
	cfg := config{
		authToken:           os.Getenv("AUTH_TOKEN"),
		homeAssistantURI:    os.Getenv("HM_SERVICE_URI"),
		listenPort:          envOrDefault("LISTEN_PORT", defaultListenPort),
		listenHost:          envOrDefault("LISTEN_HOST", defaultListenHost),
		notificationChannel: os.Getenv("NOTIFICATION_CHANNEL"),
		historyFile:         os.Getenv("HISTORY_FILE"),
		webhookSecret:       os.Getenv("WEBHOOK_SECRET"),
		webUIUsername:       os.Getenv("WEB_UI_USERNAME"),
		webUIPassword:       os.Getenv("WEB_UI_PASSWORD"),
	}

	limitValue := envOrDefault("HISTORY_LIMIT", strconv.Itoa(defaultHistoryLimit))
	limit, err := strconv.Atoi(limitValue)
	if err != nil || limit < 1 {
		return config{}, fmt.Errorf("HISTORY_LIMIT must be a positive integer, got %q", limitValue)
	}
	cfg.historyLimit = limit

	if (cfg.webUIUsername == "") != (cfg.webUIPassword == "") {
		return config{}, errors.New("WEB_UI_USERNAME and WEB_UI_PASSWORD must be set together")
	}

	return cfg, nil
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

type GrafanaJson struct {
	Title       string                 `json:"title"`
	RuleID      int64                  `json:"ruleId"`
	RuleName    string                 `json:"ruleName"`
	State       string                 `json:"state"`
	RuleURL     string                 `json:"ruleUrl"`
	ImageURL    string                 `json:"imageUrl"`
	Message     string                 `json:"message"`
	OrgID       int                    `json:"orgId"`
	DashboardID int                    `json:"dashboardId"`
	PanelID     int                    `json:"panelId"`
	Tags        map[string]interface{} `json:"tags"`
	EvalMatches []GrafanaEvalMatch     `json:"evalMatches"`
}

type GrafanaEvalMatch struct {
	Value  interface{}            `json:"value"`
	Metric string                 `json:"metric"`
	Tags   map[string]interface{} `json:"tags"`
}

type application struct {
	config    config
	history   *historyStore
	client    *http.Client
	now       func() time.Time
	idCounter uint64
}

func newApplication(cfg config, history *historyStore) *application {
	return &application{
		config:  cfg,
		history: history,
		client: &http.Client{
			Timeout: 5 * time.Second,
		},
		now: time.Now,
	}
}

func (a *application) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/notifications", a.handleNotifications)
	mux.HandleFunc("/healthz", a.handleHealth)
	mux.HandleFunc("/", a.handleRoot)

	return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; img-src 'self' data:; style-src 'unsafe-inline'; script-src 'unsafe-inline'; base-uri 'none'; form-action 'none'")
		rw.Header().Set("Referrer-Policy", "no-referrer")
		rw.Header().Set("X-Content-Type-Options", "nosniff")
		mux.ServeHTTP(rw, req)
	})
}

func (a *application) handleRoot(rw http.ResponseWriter, req *http.Request) {
	// The original relay accepted Grafana webhooks on any path because it used
	// a catch-all handler. Preserve that behavior for existing deployments.
	if req.Method == http.MethodPost {
		a.receiveHook(rw, req)
		return
	}

	if req.URL.Path != "/" {
		http.NotFound(rw, req)
		return
	}

	switch req.Method {
	case http.MethodGet, http.MethodHead:
		if !a.authorizeWebUI(rw, req) {
			return
		}
		rw.Header().Set("Cache-Control", "no-store")
		rw.Header().Set("Content-Type", "text/html; charset=utf-8")
		if req.Method == http.MethodGet {
			_, _ = io.WriteString(rw, historyPage)
		}
	default:
		rw.Header().Set("Allow", "GET, HEAD, POST")
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *application) handleNotifications(rw http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		rw.Header().Set("Allow", "GET, HEAD")
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.authorizeWebUI(rw, req) {
		return
	}

	limit := a.config.historyLimit
	if rawLimit := req.URL.Query().Get("limit"); rawLimit != "" {
		parsedLimit, err := strconv.Atoi(rawLimit)
		if err != nil || parsedLimit < 1 {
			http.Error(rw, "limit must be a positive integer", http.StatusBadRequest)
			return
		}
		if parsedLimit < limit {
			limit = parsedLimit
		}
	}

	response := struct {
		Notifications []NotificationRecord `json:"notifications"`
		Count         int                  `json:"count"`
	}{
		Notifications: a.history.List(limit),
	}
	response.Count = len(response.Notifications)

	rw.Header().Set("Cache-Control", "no-store")
	if req.Method == http.MethodHead {
		rw.Header().Set("Content-Type", "application/json; charset=utf-8")
		return
	}
	writeJSON(rw, http.StatusOK, response)
}

func (a *application) handleHealth(rw http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		rw.Header().Set("Allow", "GET, HEAD")
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if req.Method == http.MethodHead {
		rw.WriteHeader(http.StatusOK)
		return
	}
	writeJSON(rw, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *application) receiveHook(rw http.ResponseWriter, req *http.Request) {
	if !a.authorizeWebhook(rw, req) {
		return
	}

	defer req.Body.Close()
	body, err := ioutil.ReadAll(io.LimitReader(req.Body, maxWebhookBodySize+1))
	if err != nil {
		http.Error(rw, "could not read Grafana webhook", http.StatusBadRequest)
		return
	}
	if len(body) > maxWebhookBodySize {
		http.Error(rw, "Grafana webhook body is too large", http.StatusRequestEntityTooLarge)
		return
	}

	var hook GrafanaJson
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&hook); err != nil {
		http.Error(rw, "invalid Grafana webhook JSON", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		http.Error(rw, "request body must contain one JSON object", http.StatusBadRequest)
		return
	}

	receivedAt := a.now().UTC()
	record := NotificationRecord{
		ID:         a.newRecordID(receivedAt),
		ReceivedAt: receivedAt,
		Alert:      hook,
	}

	deliveryErr := a.notify(req.Context(), hook)
	record.Delivered = deliveryErr == nil
	if deliveryErr != nil {
		record.DeliveryError = truncate(deliveryErr.Error(), 500)
		log.Printf("Could not deliver Grafana notification %s: %v", record.ID, deliveryErr)
	}

	historyStored := true
	if err := a.history.Add(record); err != nil {
		historyStored = false
		log.Printf("Could not persist notification history: %v", err)
	}

	if deliveryErr != nil {
		writeJSON(rw, http.StatusBadGateway, map[string]interface{}{
			"status":        "delivery_failed",
			"id":            record.ID,
			"historyStored": historyStored,
		})
		return
	}

	writeJSON(rw, http.StatusOK, map[string]interface{}{
		"status":        "delivered",
		"id":            record.ID,
		"historyStored": historyStored,
	})
}

func (a *application) notify(ctx context.Context, hook GrafanaJson) error {
	data := map[string]interface{}{
		"image": hook.ImageURL,
	}
	if a.config.notificationChannel != "" {
		data["channel"] = a.config.notificationChannel
	}

	postBody, err := json.Marshal(map[string]interface{}{
		"message": hook.Message,
		"title":   hook.Title,
		"data":    data,
	})
	if err != nil {
		return fmt.Errorf("encode Home Assistant request: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.config.homeAssistantURI, bytes.NewReader(postBody))
	if err != nil {
		return fmt.Errorf("create Home Assistant request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if a.config.authToken != "" {
		request.Header.Set("Authorization", "Bearer "+a.config.authToken)
	}

	response, err := a.client.Do(request)
	if err != nil {
		return fmt.Errorf("call Home Assistant: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		responseBody, _ := ioutil.ReadAll(io.LimitReader(response.Body, 4096))
		details := strings.TrimSpace(string(responseBody))
		if details == "" {
			return fmt.Errorf("Home Assistant returned %s", response.Status)
		}
		return fmt.Errorf("Home Assistant returned %s: %s", response.Status, details)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	return nil
}

func (a *application) authorizeWebUI(rw http.ResponseWriter, req *http.Request) bool {
	if a.config.webUIUsername == "" {
		return true
	}

	username, password, ok := req.BasicAuth()
	if !ok || !secureEqual(username, a.config.webUIUsername) || !secureEqual(password, a.config.webUIPassword) {
		rw.Header().Set("WWW-Authenticate", `Basic realm="Grafana Relay history", charset="UTF-8"`)
		http.Error(rw, "authentication required", http.StatusUnauthorized)
		return false
	}
	return true
}

func (a *application) authorizeWebhook(rw http.ResponseWriter, req *http.Request) bool {
	if a.config.webhookSecret == "" {
		return true
	}

	provided := req.Header.Get("X-Webhook-Secret")
	if provided == "" {
		const bearerPrefix = "Bearer "
		authorization := req.Header.Get("Authorization")
		if strings.HasPrefix(authorization, bearerPrefix) {
			provided = strings.TrimPrefix(authorization, bearerPrefix)
		}
	}

	if !secureEqual(provided, a.config.webhookSecret) {
		http.Error(rw, "invalid webhook credentials", http.StatusUnauthorized)
		return false
	}
	return true
}

func secureEqual(left, right string) bool {
	leftHash := sha256.Sum256([]byte(left))
	rightHash := sha256.Sum256([]byte(right))
	return subtle.ConstantTimeCompare(leftHash[:], rightHash[:]) == 1
}

func (a *application) newRecordID(receivedAt time.Time) string {
	sequence := atomic.AddUint64(&a.idCounter, 1)
	return fmt.Sprintf("%d-%d", receivedAt.UnixNano(), sequence)
}

func truncate(value string, maximum int) string {
	runes := []rune(value)
	if len(runes) <= maximum {
		return value
	}
	return string(runes[:maximum]) + "…"
}

func writeJSON(rw http.ResponseWriter, status int, value interface{}) {
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.WriteHeader(status)
	if err := json.NewEncoder(rw).Encode(value); err != nil {
		log.Printf("Could not write JSON response: %v", err)
	}
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}

	history, err := newHistoryStore(cfg.historyFile, cfg.historyLimit)
	if err != nil {
		log.Fatalf("Could not load notification history: %v", err)
	}
	app := newApplication(cfg, history)

	address := net.JoinHostPort(cfg.listenHost, cfg.listenPort)
	log.Println("Listening for webhooks and history UI on: " + address)
	log.Println("Using Home Assistant at: " + cfg.homeAssistantURI)
	if cfg.notificationChannel != "" {
		log.Println("Using Android notification channel: " + cfg.notificationChannel)
	}
	if cfg.historyFile == "" {
		log.Println("Notification history is in memory; set HISTORY_FILE to keep it across restarts")
	} else {
		log.Println("Persisting up to " + strconv.Itoa(cfg.historyLimit) + " notifications in: " + cfg.historyFile)
	}
	if cfg.webUIUsername == "" && cfg.listenHost != "127.0.0.1" && cfg.listenHost != "localhost" && cfg.listenHost != "::1" {
		log.Println("Warning: history UI has no authentication; set WEB_UI_USERNAME and WEB_UI_PASSWORD or protect it with a reverse proxy")
	}

	server := &http.Server{
		Addr:              address,
		Handler:           app.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Fatal(server.ListenAndServe())
}
