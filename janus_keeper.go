package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/promslog"
	"github.com/prometheus/common/promslog/flag"
	"github.com/prometheus/common/version"
	"github.com/alecthomas/kingpin/v2"
)

const (
	namespace = "janus" // For Prometheus metrics.
)

func newMetric(metricName string, docString string, constLabels prometheus.Labels) *prometheus.Desc {
	return prometheus.NewDesc(prometheus.BuildFQName(namespace, "", metricName), docString, nil, constLabels)
}

var (
	currentSessions = newMetric("current_sessions", "Current number of active sessions.", nil)
	currentHandles  = newMetric("current_handles", "Current number of open plugin handles", nil)
	janusUp         = newMetric("up", "Was the last scrape successful.", nil)

	streamsConfiguredDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "streams_configured"),
		"Number of streams declared in the plugin config file.",
		[]string{"plugin"},
		nil,
	)
	streamsMountedDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "streams_mounted"),
		"Number of streams currently mounted/active in Janus.",
		[]string{"plugin"},
		nil,
	)
	streamMissingDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "stream"),
		"Streaming mountpoint present in config but not currently mounted.",
		[]string{"status", "id", "type", "description"},
		nil,
	)
	pluginDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "plugin"),
		"Janus plugin currently loaded and enabled.",
		[]string{"name", "state"},
		nil,
	)
	janusInfoDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "info"),
		"Janus server info. Always 1, with version metadata in labels.",
		[]string{"name", "version", "version_string", "author"},
		nil,
	)

	// configBlockRe matches a top-level libconfig block name (identifier at column 0).
	configBlockRe = regexp.MustCompile(`^([a-zA-Z_]\w*)`)
	// configFieldRe extracts all simple key = value fields from indented lines.
	configFieldRe = regexp.MustCompile(`^\s+(\w+)\s*=\s*(.+)`)
	// mediaArrayRe detects the "media = (" array construct used in newer entries.
	mediaArrayRe = regexp.MustCompile(`^\s+media\s*=\s*\(`)
	// videoroomBlockRe matches a videoroom config block name like "room-1234".
	videoroomBlockRe = regexp.MustCompile(`^room-(\d+)`)
)

// streamConfig holds the parsed data from one streaming plugin config entry.
type streamConfig struct {
	Name        string
	ID          string
	Type        string
	Description string
	// Fields contains all key=value pairs from the config block with inferred
	// Go types (int64, bool, string). Used to reconstruct a "create" API request.
	Fields   map[string]interface{}
	// HasMedia is true when the entry uses the "media = (...)" array format,
	// which we cannot reconstruct as a flat create request.
	HasMedia bool
}

// remountState tracks exponential-backoff retry timing for a single stream.
type remountState struct {
	nextRetry time.Time
	backoff   time.Duration
}

// mountedStream is the minimal structure we decode from each item in the Janus
// streaming plugin "list" API response.
type mountedStream struct {
	ID uint64 `json:"id"`
}

// videoroomConfig holds data parsed from one room entry in the videoroom config file.
type videoroomConfig struct {
	ID          string
	Description string
}

// mountedVideoroom is one item from the videoroom plugin "list" API response.
type mountedVideoroom struct {
	ID          string `json:"room"`
	Description string `json:"description"`
}

// Exporter collects Janus stats from the given URI and exports them using
// the prometheus metrics package.
type Exporter struct {
	URI                  string
	janusURI             string
	streamingConfigPath  string
	videoroomConfigPath  string
	secret               string
	timeout              time.Duration
	mutex                sync.RWMutex

	up           prometheus.Gauge
	totalScrapes prometheus.Counter
	logger       *slog.Logger

	remountMu  sync.Mutex
	remountMap map[string]*remountState // key = stream ID string
}

// NewExporter returns an initialized Exporter.
func NewExporter(uri string, janusURI string, streamingConfigPath string, videoroomConfigPath string, secret string, timeout time.Duration, logger *slog.Logger) (*Exporter, error) {
	_, err := url.Parse(uri)
	if err != nil {
		return nil, err
	}

	return &Exporter{
		URI:                  uri,
		janusURI:             janusURI,
		streamingConfigPath:  streamingConfigPath,
		videoroomConfigPath:  videoroomConfigPath,
		secret:              secret,
		timeout:             timeout,
		up: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "up",
			Help:      "Was the last scrape of Janus successful.",
		}),
		totalScrapes: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "exporter_total_scrapes",
			Help:      "Current total Janus scrapes.",
		}),
		logger:     logger,
		remountMap: make(map[string]*remountState),
	}, nil
}

// Describe describes all the metrics ever exported by the Janus exporter. It
// implements prometheus.Collector.
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- currentSessions
	ch <- currentHandles
	ch <- janusUp
	ch <- pluginDesc
	ch <- janusInfoDesc
	ch <- streamsConfiguredDesc
	ch <- streamsMountedDesc
	ch <- streamMissingDesc
	ch <- e.totalScrapes.Desc()
}

// Collect fetches the stats from configured Janus location and delivers them
// as Prometheus metrics. It implements prometheus.Collector.
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	e.mutex.Lock() // To protect metrics from concurrent collects.
	defer e.mutex.Unlock()

	up := e.scrape(ch)

	ch <- prometheus.MustNewConstMetric(janusUp, prometheus.GaugeValue, up)
	ch <- e.totalScrapes
}

// JanusAdminRequest contains common fields of a janus admin request
type JanusAdminRequest struct {
	Janus       string `json:"janus"`
	Transaction string `json:"transaction"`
	AdminSecret string `json:"admin_secret,omitempty"`
	SessionID   uint64 `json:"session_id,omitempty"`
}

// JanusAdminResponder allows checking for successful reply
type JanusAdminResponder interface {
	check() error
}

// JanusAdminResponse contains common fields of a janus admin response
type JanusAdminResponse struct {
	Janus       string `json:"janus"`
	Transaction string `json:"transaction"`
	Error       *struct {
		Code   int32  `json:"code"`
		Reason string `json:"reason"`
	} `json:"error,omitempty"`
}

func (r JanusAdminResponse) check() error {
	if r.Janus == "success" {
		return nil
	}

	if r.Error != nil {
		return fmt.Errorf("Janus replied %s (code %d): %s", r.Janus, r.Error.Code, r.Error.Reason)
	}

	return fmt.Errorf("Janus replied %s", r.Janus)
}

// JanusSessions is a response to a "list_sessions" request
type JanusSessions struct {
	JanusAdminResponse
	Sessions []uint64 `json:"sessions"`
}

// JanusHandles is a response to a "list_handles" request
type JanusHandles struct {
	JanusAdminResponse
	SessionID uint64   `json:"session_id"`
	Handles   []uint64 `json:"handles"`
}

// janusIDResponse is the response to session create and plugin attach requests.
type janusIDResponse struct {
	Janus string `json:"janus"`
	Data  struct {
		ID uint64 `json:"id"`
	} `json:"data"`
}

// janusStreamingListResponse is the response to a streaming "list" plugin message.
type janusStreamingListResponse struct {
	Janus      string `json:"janus"`
	PluginData *struct {
		Plugin string `json:"plugin"`
		Data   struct {
			List []mountedStream `json:"list"`
		} `json:"data"`
	} `json:"plugindata"`
}

// janusVideoroomListResponse is the response to a videoroom "list" plugin message.
type janusVideoroomListResponse struct {
	Janus      string `json:"janus"`
	PluginData *struct {
		Plugin string `json:"plugin"`
		Data   struct {
			List []mountedVideoroom `json:"list"`
		} `json:"data"`
	} `json:"plugindata"`
}

// janusInfo is the response to the /info endpoint.
type janusInfo struct {
	Name          string `json:"name"`
	Version       int    `json:"version"`
	VersionString string `json:"version_string"`
	Author        string `json:"author"`
	Plugins       map[string]struct {
		Name string `json:"name"`
	} `json:"plugins"`
}

// pluginShortName extracts the last segment of a Janus plugin ID.
// e.g. "janus.plugin.streaming" → "streaming"
func pluginShortName(key string) string {
	if i := strings.LastIndex(key, "."); i >= 0 {
		return key[i+1:]
	}
	return key
}

func generateTxID() string {
	buff := make([]byte, 9)
	rand.Read(buff)
	return base64.StdEncoding.EncodeToString(buff)
}

func (e *Exporter) sendRequest(pathSuffix string, body JanusAdminRequest, result interface{}) error {
	client := http.Client{
		Timeout: e.timeout,
	}

	buf := new(bytes.Buffer)
	json.NewEncoder(buf).Encode(body)
	req, _ := http.NewRequest("POST", e.URI+pathSuffix, buf)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if !(resp.StatusCode >= 200 && resp.StatusCode < 300) {
		return fmt.Errorf("HTTP status %d", resp.StatusCode)
	}

	err = json.NewDecoder(resp.Body).Decode(result)
	if err != nil {
		return err
	}

	return nil
}

// postJSON POSTs an arbitrary JSON body to url and decodes the response into result.
func (e *Exporter) postJSON(url string, body interface{}, result interface{}) error {
	client := http.Client{Timeout: e.timeout}
	buf := new(bytes.Buffer)
	json.NewEncoder(buf).Encode(body)
	req, _ := http.NewRequest("POST", url, buf)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(result)
}

func (e *Exporter) listSessions() ([]uint64, error) {
	reqBody := JanusAdminRequest{
		Janus:       "list_sessions",
		Transaction: generateTxID(),
		AdminSecret: e.secret,
	}

	sessionList := JanusSessions{}
	err := e.sendRequest("", reqBody, &sessionList)
	if err == nil {
		err = sessionList.check()
	}

	if err != nil {
		e.logger.Error("Cannot list sessions", "err", err)
		return nil, err
	}

	return sessionList.Sessions, nil
}

func (e *Exporter) listHandles(sessionID uint64) ([]uint64, error) {
	reqBody := JanusAdminRequest{
		Janus:       "list_handles",
		Transaction: generateTxID(),
		AdminSecret: e.secret,
		SessionID:   sessionID,
	}

	handleList := JanusHandles{}
	err := e.sendRequest("", reqBody, &handleList)
	if err == nil {
		err = handleList.check()
	}

	if err != nil {
		e.logger.Error("Cannot list handles of session "+strconv.FormatUint(sessionID, 10), "err", err)
		return nil, err
	}

	return handleList.Handles, nil
}

// parseStreamingConfig parses a Janus streaming plugin libconfig file and returns
// every top-level stream entry with its id, type, and description fields.
//
// The parser tracks brace/paren depth so nested blocks (e.g. media arrays) are
// skipped and do not pollute top-level field values.
func parseStreamingConfig(path string) ([]streamConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var streams []streamConfig
	var cur *streamConfig
	depth := 0

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		// skip blank lines and comments
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			// still update depth from any braces on comment lines (there shouldn't
			// be any, but be defensive)
			depth += strings.Count(line, "{") + strings.Count(line, "(")
			depth -= strings.Count(line, "}") + strings.Count(line, ")")
			continue
		}

		if depth == 0 {
			// A line at depth 0 starting with an identifier is a new block name.
			if m := configBlockRe.FindStringSubmatch(trimmed); m != nil {
				streams = append(streams, streamConfig{
					Name:   m[1],
					Fields: make(map[string]interface{}),
				})
				cur = &streams[len(streams)-1]
			}
		} else if depth == 1 && cur != nil {
			// Detect the "media = (...)" array construct — we cannot reconstruct
			// these entries as a flat create request.
			if mediaArrayRe.MatchString(line) {
				cur.HasMedia = true
			}
			// Parse all simple key = value fields at depth 1.
			if m := configFieldRe.FindStringSubmatch(line); m != nil {
				key := m[1]
				val := strings.TrimSpace(m[2])
				val = strings.TrimRight(val, ";")
				val = strings.TrimSpace(val)
				val = strings.Trim(val, "\"")

				switch key {
				case "id":
					cur.ID = val
				case "type":
					cur.Type = val
				case "description":
					cur.Description = val
				}
				cur.Fields[key] = inferValue(val)
			}
		}

		depth += strings.Count(line, "{") + strings.Count(line, "(")
		depth -= strings.Count(line, "}") + strings.Count(line, ")")
	}
	return streams, scanner.Err()
}

// fetchServerInfo fetches the Janus /info endpoint (no auth required) and
// returns structured server information including all loaded plugins.
func (e *Exporter) fetchServerInfo() (*janusInfo, error) {
	client := http.Client{Timeout: e.timeout}
	resp, err := client.Get(e.janusURI + "/info")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP status %d", resp.StatusCode)
	}
	var info janusInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, err
	}
	return &info, nil
}

// listMountedStreams queries the Janus streaming plugin via the HTTP API and
// returns all currently mounted streaming mountpoints.
// It creates a temporary session + handle, sends a "list" request (which the
// streaming plugin answers synchronously in the POST response), then destroys
// the session.
func (e *Exporter) listMountedStreams() ([]mountedStream, error) {
	// create session
	var cr janusIDResponse
	if err := e.postJSON(e.janusURI, map[string]string{
		"janus":       "create",
		"transaction": generateTxID(),
	}, &cr); err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	if cr.Janus != "success" {
		return nil, fmt.Errorf("create session replied %q", cr.Janus)
	}
	sessionURL := fmt.Sprintf("%s/%d", e.janusURI, cr.Data.ID)
	defer e.postJSON(sessionURL, map[string]string{ //nolint:errcheck
		"janus":       "destroy",
		"transaction": generateTxID(),
	}, &struct{}{})

	// attach to streaming plugin
	var ar janusIDResponse
	if err := e.postJSON(sessionURL, map[string]interface{}{
		"janus":       "attach",
		"plugin":      "janus.plugin.streaming",
		"transaction": generateTxID(),
	}, &ar); err != nil {
		return nil, fmt.Errorf("attach: %w", err)
	}
	if ar.Janus != "success" {
		return nil, fmt.Errorf("attach replied %q", ar.Janus)
	}
	handleURL := fmt.Sprintf("%s/%d", sessionURL, ar.Data.ID)

	// send list; the streaming plugin responds synchronously inside the POST reply
	var listResp janusStreamingListResponse
	if err := e.postJSON(handleURL, map[string]interface{}{
		"janus":       "message",
		"transaction": generateTxID(),
		"body":        map[string]string{"request": "list"},
	}, &listResp); err != nil {
		return nil, fmt.Errorf("list request: %w", err)
	}
	if listResp.PluginData == nil {
		return nil, fmt.Errorf("list response contained no plugindata")
	}
	return listResp.PluginData.Data.List, nil
}

// inferValue converts a raw libconfig string value to the most specific Go type.
func inferValue(s string) interface{} {
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i
	}
	switch strings.ToLower(s) {
	case "true", "yes":
		return true
	case "false", "no":
		return false
	}
	return s
}

// parseVideoroomConfig parses a Janus videoroom plugin libconfig file.
// Rooms are declared as "room-<id>: { ... }" blocks at depth 0.
func parseVideoroomConfig(path string) ([]videoroomConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var rooms []videoroomConfig
	var cur *videoroomConfig
	depth := 0

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		if depth == 0 {
			if m := videoroomBlockRe.FindStringSubmatch(trimmed); m != nil {
				rooms = append(rooms, videoroomConfig{ID: m[1]})
				cur = &rooms[len(rooms)-1]
			}
		} else if depth == 1 && cur != nil {
			if m := configFieldRe.FindStringSubmatch(line); m != nil {
				if m[1] == "description" {
					val := strings.TrimRight(strings.TrimSpace(m[2]), ";")
					val = strings.TrimSpace(val)
					val = strings.Trim(val, "\"")
					cur.Description = val
				}
			}
		}

		depth += strings.Count(line, "{") + strings.Count(line, "(")
		depth -= strings.Count(line, "}") + strings.Count(line, ")")
	}
	return rooms, scanner.Err()
}

// listVideorooms queries the Janus videoroom plugin and returns all configured rooms.
func (e *Exporter) listVideorooms() ([]mountedVideoroom, error) {
	var cr janusIDResponse
	if err := e.postJSON(e.janusURI, map[string]string{
		"janus":       "create",
		"transaction": generateTxID(),
	}, &cr); err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	if cr.Janus != "success" {
		return nil, fmt.Errorf("create session replied %q", cr.Janus)
	}
	sessionURL := fmt.Sprintf("%s/%d", e.janusURI, cr.Data.ID)
	defer e.postJSON(sessionURL, map[string]string{ //nolint:errcheck
		"janus":       "destroy",
		"transaction": generateTxID(),
	}, &struct{}{})

	var ar janusIDResponse
	if err := e.postJSON(sessionURL, map[string]interface{}{
		"janus":       "attach",
		"plugin":      "janus.plugin.videoroom",
		"transaction": generateTxID(),
	}, &ar); err != nil {
		return nil, fmt.Errorf("attach: %w", err)
	}
	if ar.Janus != "success" {
		return nil, fmt.Errorf("attach replied %q", ar.Janus)
	}
	handleURL := fmt.Sprintf("%s/%d", sessionURL, ar.Data.ID)

	var listResp janusVideoroomListResponse
	if err := e.postJSON(handleURL, map[string]interface{}{
		"janus":       "message",
		"transaction": generateTxID(),
		"body":        map[string]string{"request": "list"},
	}, &listResp); err != nil {
		return nil, fmt.Errorf("list request: %w", err)
	}
	if listResp.PluginData == nil {
		return nil, fmt.Errorf("list response contained no plugindata")
	}
	return listResp.PluginData.Data.List, nil
}

// createMountpoint sends a streaming plugin "create" request for the given
// config entry. It builds the request body from the entry's parsed Fields map.
func (e *Exporter) createMountpoint(s streamConfig) error {
	var cr janusIDResponse
	if err := e.postJSON(e.janusURI, map[string]string{
		"janus":       "create",
		"transaction": generateTxID(),
	}, &cr); err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	if cr.Janus != "success" {
		return fmt.Errorf("create session replied %q", cr.Janus)
	}
	sessionURL := fmt.Sprintf("%s/%d", e.janusURI, cr.Data.ID)
	defer e.postJSON(sessionURL, map[string]string{ //nolint:errcheck
		"janus":       "destroy",
		"transaction": generateTxID(),
	}, &struct{}{})

	var ar janusIDResponse
	if err := e.postJSON(sessionURL, map[string]interface{}{
		"janus":       "attach",
		"plugin":      "janus.plugin.streaming",
		"transaction": generateTxID(),
	}, &ar); err != nil {
		return fmt.Errorf("attach: %w", err)
	}
	if ar.Janus != "success" {
		return fmt.Errorf("attach replied %q", ar.Janus)
	}
	handleURL := fmt.Sprintf("%s/%d", sessionURL, ar.Data.ID)

	body := make(map[string]interface{}, len(s.Fields)+1)
	for k, v := range s.Fields {
		body[k] = v
	}
	body["request"] = "create"

	var resp struct {
		Janus      string `json:"janus"`
		PluginData *struct {
			Data struct {
				Streaming string `json:"streaming"`
				Error     string `json:"error"`
			} `json:"data"`
		} `json:"plugindata"`
	}
	if err := e.postJSON(handleURL, map[string]interface{}{
		"janus":       "message",
		"transaction": generateTxID(),
		"body":        body,
	}, &resp); err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	if resp.PluginData == nil {
		return fmt.Errorf("no plugindata in create response")
	}
	if resp.PluginData.Data.Streaming != "created" {
		return fmt.Errorf("create failed (streaming=%q error=%q)",
			resp.PluginData.Data.Streaming, resp.PluginData.Data.Error)
	}
	return nil
}

// runRemountLoop runs in a background goroutine. Every 5 s it checks for
// config streams that are missing from the live Janus instance and attempts to
// recreate them. Retries use exponential backoff (5 s → 10 s → 20 s … capped
// at 5 min) per stream. The backoff resets once a stream is seen as mounted.
func (e *Exporter) runRemountLoop() {
	const initialBackoff = 5 * time.Second
	const maxBackoff = 5 * time.Minute

	ticker := time.NewTicker(initialBackoff)
	defer ticker.Stop()

	for range ticker.C {
		if e.streamingConfigPath == "" || e.janusURI == "" {
			continue
		}

		configStreams, err := parseStreamingConfig(e.streamingConfigPath)
		if err != nil {
			continue
		}

		mounted, err := e.listMountedStreams()
		if err != nil {
			continue // plugin not available or transient error
		}

		mountedIDs := make(map[uint64]bool, len(mounted))
		for _, s := range mounted {
			mountedIDs[s.ID] = true
		}

		now := time.Now()

		// clear backoff state for streams that are now mounted
		e.remountMu.Lock()
		for idStr := range e.remountMap {
			if id, err := strconv.ParseUint(idStr, 10, 64); err == nil && mountedIDs[id] {
				delete(e.remountMap, idStr)
			}
		}
		e.remountMu.Unlock()

		for _, s := range configStreams {
			if s.ID == "" || s.HasMedia {
				continue
			}
			id, err := strconv.ParseUint(s.ID, 10, 64)
			if err != nil {
				continue
			}
			if mountedIDs[id] {
				continue
			}

			// stream is missing — check backoff
			e.remountMu.Lock()
			state, exists := e.remountMap[s.ID]
			if !exists {
				state = &remountState{nextRetry: now, backoff: initialBackoff}
				e.remountMap[s.ID] = state
			}
			shouldRetry := !now.Before(state.nextRetry)
			e.remountMu.Unlock()

			if !shouldRetry {
				continue
			}

			err = e.createMountpoint(s)

			e.remountMu.Lock()
			if err != nil {
				e.logger.Error("Failed to remount stream",
					"id", s.ID, "name", s.Name, "err", err)
				next := state.backoff * 2
				if next > maxBackoff {
					next = maxBackoff
				}
				state.backoff = next
				state.nextRetry = now.Add(next)
			} else {
				e.logger.Info("Remounted stream", "id", s.ID, "name", s.Name)
				delete(e.remountMap, s.ID)
			}
			e.remountMu.Unlock()
		}
	}
}

func (e *Exporter) scrape(ch chan<- prometheus.Metric) (up float64) {
	e.totalScrapes.Inc()

	sessionIds, err := e.listSessions()
	if err != nil {
		return 0
	}

	ch <- prometheus.MustNewConstMetric(currentSessions, prometheus.GaugeValue, float64(len(sessionIds)))

	handleCount := 0
	for _, sessionID := range sessionIds {
		handles, err := e.listHandles(sessionID)
		if err != nil {
			continue
		}
		handleCount += len(handles)
	}

	ch <- prometheus.MustNewConstMetric(currentHandles, prometheus.GaugeValue, float64(handleCount))

	// --- loaded plugins ---

	streamingLoaded := false
	videoroomLoaded := false
	if e.janusURI != "" {
		info, err := e.fetchServerInfo()
		if err != nil {
			e.logger.Error("Cannot fetch server info", "err", err)
		} else {
			ch <- prometheus.MustNewConstMetric(janusInfoDesc, prometheus.GaugeValue, 1,
				info.Name, strconv.Itoa(info.Version), info.VersionString, info.Author)
			for key := range info.Plugins {
				ch <- prometheus.MustNewConstMetric(pluginDesc, prometheus.GaugeValue, 1,
					pluginShortName(key), "loaded")
				switch key {
				case "janus.plugin.streaming":
					streamingLoaded = true
				case "janus.plugin.videoroom":
					videoroomLoaded = true
				}
			}
		}
	}

	// --- streaming metrics (only when janus.plugin.streaming is loaded) ---

	if streamingLoaded {
		var configStreams []streamConfig
		if e.streamingConfigPath != "" {
			configStreams, err = parseStreamingConfig(e.streamingConfigPath)
			if err != nil {
				e.logger.Error("Cannot read streaming config", "err", err)
			} else {
				ch <- prometheus.MustNewConstMetric(streamsConfiguredDesc, prometheus.GaugeValue,
					float64(len(configStreams)), "streaming")
			}
		}

		mounted, err := e.listMountedStreams()
		if err != nil {
			e.logger.Error("Cannot list mounted streams", "err", err)
		} else {
			ch <- prometheus.MustNewConstMetric(streamsMountedDesc, prometheus.GaugeValue,
				float64(len(mounted)), "streaming")

			if configStreams != nil {
				mountedIDs := make(map[uint64]bool, len(mounted))
				for _, s := range mounted {
					mountedIDs[s.ID] = true
				}
				for _, s := range configStreams {
					if s.ID == "" {
						continue
					}
					id, err := strconv.ParseUint(s.ID, 10, 64)
					if err != nil {
						continue
					}
					if !mountedIDs[id] {
						ch <- prometheus.MustNewConstMetric(
							streamMissingDesc, prometheus.GaugeValue, 1,
							"missing", s.ID, s.Type, s.Description,
						)
					}
				}
			}
		}
	}

	// --- videoroom metrics (only when janus.plugin.videoroom is loaded) ---

	if videoroomLoaded {
		var configRooms []videoroomConfig
		if e.videoroomConfigPath != "" {
			configRooms, err = parseVideoroomConfig(e.videoroomConfigPath)
			if err != nil {
				e.logger.Error("Cannot read videoroom config", "err", err)
			} else {
				ch <- prometheus.MustNewConstMetric(streamsConfiguredDesc, prometheus.GaugeValue,
					float64(len(configRooms)), "videoroom")
			}
		}

		rooms, err := e.listVideorooms()
		if err != nil {
			e.logger.Error("Cannot list videorooms", "err", err)
		} else {
			ch <- prometheus.MustNewConstMetric(streamsMountedDesc, prometheus.GaugeValue,
				float64(len(rooms)), "videoroom")

			if configRooms != nil {
				mountedRoomIDs := make(map[string]bool, len(rooms))
				for _, r := range rooms {
					mountedRoomIDs[r.ID] = true
				}
				for _, r := range configRooms {
					if r.ID == "" {
						continue
					}
					if !mountedRoomIDs[r.ID] {
						ch <- prometheus.MustNewConstMetric(
							streamMissingDesc, prometheus.GaugeValue, 1,
							"missing", r.ID, "videoroom", r.Description,
						)
					}
				}
			}
		}
	}

	return 1
}

func main() {
	var (
		listenAddress       = kingpin.Flag("web.listen-address", "Address to listen on for web interface and telemetry.").Default(":9709").String()
		metricsPath         = kingpin.Flag("web.telemetry-path", "Path under which to expose metrics.").Default("/metrics").String()
		janusAdminURI       = kingpin.Flag("janus.admin-uri", "Janus admin api URI.").Default("http://127.0.0.1:7608/admingxy").String()
		janusURI            = kingpin.Flag("janus.uri", "Janus HTTP API URI.").Default("http://127.0.0.1:7708/janusgxy").String()
		janusAdminSecretEnv = kingpin.Flag("janus.admin-secret-env", "Environment variable containing Janus admin secret.").Default("").String()
		janusTimeout        = kingpin.Flag("janus.timeout", "Timeout for trying to get stats from Janus.").Default("5s").Duration()
		streamingConfigPath = kingpin.Flag("streaming.config", "Path to janus.plugin.streaming.jcfg config file.").Default("/usr/janusgxy/etc/janus/janus.plugin.streaming.jcfg").String()
		videoroomConfigPath = kingpin.Flag("videoroom.config", "Path to janus.plugin.videoroom.jcfg config file.").Default("/usr/janusgxy/etc/janus/janus.plugin.videoroom.jcfg").String()
	)

	promslogConfig := &promslog.Config{}
	flag.AddFlags(kingpin.CommandLine, promslogConfig)
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()
	logger := promslog.New(promslogConfig)

	logger.Info("Starting janus_keeper", "version", version.Info())
	logger.Info("Build context", "context", version.BuildContext())

	janusAdminSecret := ""
	if *janusAdminSecretEnv != "" {
		janusAdminSecret = os.Getenv(*janusAdminSecretEnv)
	}

	exporter, err := NewExporter(*janusAdminURI, *janusURI, *streamingConfigPath, *videoroomConfigPath, janusAdminSecret, *janusTimeout, logger)
	if err != nil {
		logger.Error("Error creating an exporter", "err", err)
		os.Exit(1)
	}
	prometheus.MustRegister(exporter)
	prometheus.MustRegister(collectors.NewBuildInfoCollector())

	go exporter.runRemountLoop()

	logger.Info("Listening on address", "address", *listenAddress)
	http.Handle(*metricsPath, promhttp.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
             <head><title>Janus Keeper by mluvii.com</title></head>
             <body>
             <h1>Janus Keeper by mluvii.com</h1>
             <p><a href='` + *metricsPath + `'>Metrics</a></p>
             </body>
             </html>`))
	})
	if err := http.ListenAndServe(*listenAddress, nil); err != nil {
		logger.Error("Error starting HTTP server", "err", err)
		os.Exit(1)
	}
}
