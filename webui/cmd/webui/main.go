package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/influxdata/influxdb-client-go/v2/api"
)

//go:embed index.html
var indexHTML string

type config struct {
	listenAddr   string
	influxURL    string
	influxToken  string
	influxOrg    string
	influxBucket string
	lanPrefix    string
}

type app struct {
	cfg      config
	queryAPI api.QueryAPI
	index    *template.Template
}

type dashboard struct {
	GeneratedAt    time.Time          `json:"generatedAt"`
	Window         string             `json:"window"`
	Summary        summary            `json:"summary"`
	Insights       insights           `json:"insights"`
	Devices        []deviceRow        `json:"devices"`
	Clients        []clientRow        `json:"clients"`
	TopDomains     []domainRow        `json:"topDomains"`
	BlockedDomains []blockedRow       `json:"blockedDomains"`
	Talkers        []talkerRow        `json:"talkers"`
	RecentBlocked  []recentBlockedRow `json:"recentBlocked"`
}

type deviceDashboard struct {
	GeneratedAt     time.Time            `json:"generatedAt"`
	Window          string               `json:"window"`
	IP              string               `json:"ip"`
	Summary         deviceSummary        `json:"summary"`
	TopDomains      []domainRow          `json:"topDomains"`
	BlockedDomains  []blockedRow         `json:"blockedDomains"`
	TrafficPeers    []trafficPeerRow     `json:"trafficPeers"`
	RecentBlocked   []recentBlockedRow   `json:"recentBlocked"`
	WebsiteActivity []websiteActivityRow `json:"websiteActivity"`
}

type summary struct {
	Devices        int     `json:"devices"`
	DNSQueries     int64   `json:"dnsQueries"`
	BlockedQueries int64   `json:"blockedQueries"`
	BlockRate      float64 `json:"blockRate"`
	NetworkBytes   int64   `json:"networkBytes"`
}

type insights struct {
	UnknownDevices     []deviceRow    `json:"unknownDevices"`
	MostBlockedClients []clientRow    `json:"mostBlockedClients"`
	NoisyDomains       []domainRow    `json:"noisyDomains"`
	Freshness          []freshnessRow `json:"freshness"`
}

type freshnessRow struct {
	Measurement string `json:"measurement"`
	LastSeen    string `json:"lastSeen"`
	AgeSeconds  int64  `json:"ageSeconds"`
	Status      string `json:"status"`
}

type deviceSummary struct {
	IP             string  `json:"ip"`
	Name           string  `json:"name"`
	Kind           string  `json:"kind"`
	Hostname       string  `json:"hostname"`
	MAC            string  `json:"mac"`
	Source         string  `json:"source"`
	DNSQueries     int64   `json:"dnsQueries"`
	BlockedQueries int64   `json:"blockedQueries"`
	BlockRate      float64 `json:"blockRate"`
	NetworkBytes   int64   `json:"networkBytes"`
}

type deviceRow struct {
	IP       string    `json:"ip"`
	Name     string    `json:"name"`
	Kind     string    `json:"kind"`
	Source   string    `json:"source"`
	Hostname string    `json:"hostname"`
	MAC      string    `json:"mac"`
	Seen     time.Time `json:"-"`
}

type clientRow struct {
	IP      string `json:"ip"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Queries int64  `json:"queries"`
	Blocked int64  `json:"blocked"`
}

type domainRow struct {
	Domain  string `json:"domain"`
	Queries int64  `json:"queries"`
}

type blockedRow struct {
	Domain  string `json:"domain"`
	Reason  string `json:"reason"`
	Rule    string `json:"rule"`
	Blocked int64  `json:"blocked"`
}

type talkerRow struct {
	IP    string  `json:"ip"`
	Name  string  `json:"name"`
	Kind  string  `json:"kind"`
	Bytes int64   `json:"bytes"`
	Share float64 `json:"share"`
}

type trafficPeerRow struct {
	Peer     string `json:"peer"`
	Protocol string `json:"protocol"`
	Bytes    int64  `json:"bytes"`
}

type recentBlockedRow struct {
	Time   string `json:"time"`
	Client string `json:"client"`
	Name   string `json:"name"`
	Domain string `json:"domain"`
	Reason string `json:"reason"`
}

type websiteActivityRow struct {
	Time      string `json:"time"`
	Domain    string `json:"domain"`
	QueryType string `json:"queryType"`
	Blocked   bool   `json:"blocked"`
	Status    string `json:"status"`
	Reason    string `json:"reason"`
	Rule      string `json:"rule"`
	Upstream  string `json:"upstream"`
}

func main() {
	cfg := loadConfig()
	if cfg.influxToken == "" {
		slog.Error("INFLUXDB_TOKEN is required")
		os.Exit(1)
	}

	client := influxdb2.NewClient(cfg.influxURL, cfg.influxToken)
	defer client.Close()

	tmpl := template.Must(template.New("index").Parse(indexHTML))
	app := &app{
		cfg:      cfg,
		queryAPI: client.QueryAPI(cfg.influxOrg),
		index:    tmpl,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", app.handleIndex)
	mux.HandleFunc("/api/dashboard", app.handleDashboard)
	mux.HandleFunc("/api/device", app.handleDevice)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	slog.Info("webui started", "listen", cfg.listenAddr, "influx", cfg.influxURL)
	if err := http.ListenAndServe(cfg.listenAddr, mux); err != nil {
		slog.Error("webui stopped", "error", err)
		os.Exit(1)
	}
}

func (a *app) handleDevice(w http.ResponseWriter, r *http.Request) {
	ip := strings.TrimSpace(r.URL.Query().Get("ip"))
	if ip == "" {
		http.Error(w, "ip is required", http.StatusBadRequest)
		return
	}
	window := r.URL.Query().Get("window")
	if window == "" {
		window = "-6h"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	data := deviceDashboard{
		GeneratedAt:     time.Now(),
		Window:          window,
		IP:              ip,
		Summary:         a.deviceSummary(ctx, window, ip),
		TopDomains:      a.deviceTopDomains(ctx, window, ip),
		BlockedDomains:  a.deviceBlockedDomains(ctx, window, ip),
		TrafficPeers:    a.deviceTrafficPeers(ctx, window, ip),
		RecentBlocked:   a.deviceRecentBlocked(ctx, window, ip),
		WebsiteActivity: a.deviceWebsiteActivity(ctx, window, ip),
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func loadConfig() config {
	return config{
		listenAddr:   env("WEBUI_LISTEN_ADDR", ":8080"),
		influxURL:    env("WEBUI_INFLUX_URL", env("COLLECTOR_INFLUX_URL", "http://localhost:8086")),
		influxToken:  env("INFLUXDB_TOKEN", ""),
		influxOrg:    env("INFLUXDB_ORG", "home"),
		influxBucket: env("INFLUXDB_BUCKET", "network"),
		lanPrefix:    env("WEBUI_LAN_PREFIX", ""),
	}
}

func (a *app) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/device" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.index.Execute(w, nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (a *app) handleDashboard(w http.ResponseWriter, r *http.Request) {
	window := r.URL.Query().Get("window")
	if window == "" {
		window = "-6h"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	data := dashboard{
		GeneratedAt:    time.Now(),
		Window:         window,
		Devices:        a.devices(ctx, window),
		TopDomains:     a.topDomains(ctx, window),
		BlockedDomains: a.blockedDomains(ctx, window),
	}
	deviceLookup := devicesByIP(data.Devices)
	data.Talkers = a.talkers(ctx, window, deviceLookup)
	data.Clients = a.clients(ctx, window, deviceLookup)
	data.RecentBlocked = a.recentBlocked(ctx, window, deviceLookup)
	data.Summary = a.summary(ctx, window, data)
	data.Insights = a.insights(ctx, data)

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (a *app) devices(ctx context.Context, window string) []deviceRow {
	query := `from(bucket: "` + a.cfg.influxBucket + `")
  |> range(start: ` + window + `)
  |> filter(fn: (r) => r._measurement == "device_seen" and r._field == "seen")
  |> group(columns: ["ip", "mac", "name", "hostname", "kind", "source"])
  |> last()`

	byIP := map[string]deviceRow{}
	a.each(ctx, query, func(values map[string]any) {
		row := deviceRow{
			IP:       str(values["ip"]),
			MAC:      str(values["mac"]),
			Name:     str(values["name"]),
			Hostname: str(values["hostname"]),
			Kind:     str(values["kind"]),
			Source:   str(values["source"]),
			Seen:     timeFrom(values["_time"]),
		}
		if !a.keepIP(row.IP) {
			return
		}
		current, ok := byIP[row.IP]
		if !ok {
			byIP[row.IP] = row
			return
		}
		byIP[row.IP] = betterDevice(current, row)
	})
	rows := make([]deviceRow, 0, len(byIP))
	for _, row := range byIP {
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].IP < rows[j].IP })
	return rows
}

func (a *app) insights(ctx context.Context, data dashboard) insights {
	out := insights{
		UnknownDevices:     unknownDevices(data.Devices),
		MostBlockedClients: mostBlockedClients(data.Clients),
		NoisyDomains:       noisyDomains(data.TopDomains),
		Freshness:          a.freshness(ctx),
	}
	return out
}

func unknownDevices(devices []deviceRow) []deviceRow {
	rows := []deviceRow{}
	for _, device := range devices {
		name := strings.TrimSpace(displayName(device))
		kind := strings.TrimSpace(device.Kind)
		if name == "" || strings.EqualFold(kind, "unknown") {
			rows = append(rows, device)
		}
	}
	if len(rows) > 8 {
		return rows[:8]
	}
	return rows
}

func mostBlockedClients(clients []clientRow) []clientRow {
	rows := []clientRow{}
	for _, client := range clients {
		if client.Blocked > 0 {
			rows = append(rows, client)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Blocked == rows[j].Blocked {
			return rows[i].Queries > rows[j].Queries
		}
		return rows[i].Blocked > rows[j].Blocked
	})
	if len(rows) > 8 {
		return rows[:8]
	}
	return rows
}

func noisyDomains(domains []domainRow) []domainRow {
	if len(domains) > 8 {
		return domains[:8]
	}
	return domains
}

func (a *app) freshness(ctx context.Context) []freshnessRow {
	checks := []struct {
		measurement string
		field       string
	}{
		{measurement: "adguard_query", field: "count"},
		{measurement: "device_seen", field: "seen"},
		{measurement: "traffic_flow", field: "bytes"},
	}

	rows := make([]freshnessRow, 0, len(checks))
	now := time.Now()
	for _, check := range checks {
		seen := a.latestMeasurement(ctx, check.measurement, check.field)
		status := "missing"
		var age int64
		var lastSeen string
		if !seen.IsZero() {
			age = int64(now.Sub(seen).Seconds())
			lastSeen = seen.Format("2006-01-02 15:04:05")
			status = "fresh"
			if age > 15*60 {
				status = "stale"
			}
		}
		rows = append(rows, freshnessRow{
			Measurement: check.measurement,
			LastSeen:    lastSeen,
			AgeSeconds:  age,
			Status:      status,
		})
	}
	return rows
}

func (a *app) latestMeasurement(ctx context.Context, measurement string, field string) time.Time {
	query := `from(bucket: "` + a.cfg.influxBucket + `")
  |> range(start: -24h)
  |> filter(fn: (r) => r._measurement == ` + fluxString(measurement) + ` and r._field == ` + fluxString(field) + `)
  |> group()
  |> last()`

	var latest time.Time
	a.each(ctx, query, func(values map[string]any) {
		seen := timeFrom(values["_time"])
		if seen.After(latest) {
			latest = seen
		}
	})
	return latest
}

func (a *app) deviceSummary(ctx context.Context, window string, ip string) deviceSummary {
	info := a.deviceInfo(ctx, ip)
	s := deviceSummary{
		IP:       ip,
		Name:     displayName(info),
		Kind:     info.Kind,
		Hostname: info.Hostname,
		MAC:      info.MAC,
		Source:   info.Source,
	}

	query := `from(bucket: "` + a.cfg.influxBucket + `")
  |> range(start: ` + window + `)
  |> filter(fn: (r) => r._measurement == "adguard_query" and r._field == "count")
  |> filter(fn: (r) => r.client_ip == ` + fluxString(ip) + `)
  |> group(columns: ["blocked"])
  |> sum()`

	a.each(ctx, query, func(values map[string]any) {
		count := int64From(values["_value"])
		s.DNSQueries += count
		if str(values["blocked"]) == "true" {
			s.BlockedQueries += count
		}
	})
	if s.DNSQueries > 0 {
		s.BlockRate = float64(s.BlockedQueries) / float64(s.DNSQueries) * 100
	}

	trafficQuery := `from(bucket: "` + a.cfg.influxBucket + `")
  |> range(start: ` + window + `)
  |> filter(fn: (r) => r._measurement == "traffic_flow" and r._field == "bytes")
  |> filter(fn: (r) => r.src_ip == ` + fluxString(ip) + ` or r.dst_ip == ` + fluxString(ip) + `)
  |> group()
  |> sum()`

	a.each(ctx, trafficQuery, func(values map[string]any) {
		s.NetworkBytes += int64From(values["_value"])
	})
	return s
}

func (a *app) deviceInfo(ctx context.Context, ip string) deviceRow {
	query := `from(bucket: "` + a.cfg.influxBucket + `")
  |> range(start: -30d)
  |> filter(fn: (r) => r._measurement == "device_seen" and r._field == "seen")
  |> filter(fn: (r) => r.ip == ` + fluxString(ip) + `)
  |> group(columns: ["ip", "mac", "name", "hostname", "kind", "source"])
  |> last()`

	found := false
	var best deviceRow
	a.each(ctx, query, func(values map[string]any) {
		row := deviceRow{
			IP:       str(values["ip"]),
			MAC:      str(values["mac"]),
			Name:     str(values["name"]),
			Hostname: str(values["hostname"]),
			Kind:     str(values["kind"]),
			Source:   str(values["source"]),
			Seen:     timeFrom(values["_time"]),
		}
		if !found {
			best = row
			found = true
			return
		}
		best = betterDevice(best, row)
	})
	if !found {
		return deviceRow{IP: ip}
	}
	return best
}

func (a *app) clients(ctx context.Context, window string, devices map[string]deviceRow) []clientRow {
	query := `from(bucket: "` + a.cfg.influxBucket + `")
  |> range(start: ` + window + `)
  |> filter(fn: (r) => r._measurement == "adguard_query" and r._field == "count")
  |> group(columns: ["client_ip", "client_name", "client_kind", "blocked"])
  |> sum()`

	byKey := map[string]*clientRow{}
	a.each(ctx, query, func(values map[string]any) {
		ip := str(values["client_ip"])
		if !a.keepIP(ip) {
			return
		}
		name := str(values["client_name"])
		kind := str(values["client_kind"])
		if device, ok := devices[ip]; ok {
			if name == "" {
				name = displayName(device)
			}
			if kind == "" {
				kind = device.Kind
			}
		}
		row := byKey[ip]
		if row == nil {
			row = &clientRow{IP: ip, Name: name, Kind: kind}
			byKey[ip] = row
		}
		if row.Name == "" {
			row.Name = name
		}
		if row.Kind == "" {
			row.Kind = kind
		}
		if str(values["blocked"]) == "true" {
			row.Blocked += int64From(values["_value"])
		}
		row.Queries += int64From(values["_value"])
	})

	rows := make([]clientRow, 0, len(byKey))
	for _, row := range byKey {
		rows = append(rows, *row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Queries > rows[j].Queries })
	return rows
}

func (a *app) topDomains(ctx context.Context, window string) []domainRow {
	query := `from(bucket: "` + a.cfg.influxBucket + `")
  |> range(start: ` + window + `)
  |> filter(fn: (r) => r._measurement == "adguard_query" and r._field == "count")
  |> group(columns: ["domain"])
  |> sum()
  |> group()
  |> sort(columns: ["_value"], desc: true)
  |> limit(n: 25)`

	rows := []domainRow{}
	a.each(ctx, query, func(values map[string]any) {
		rows = append(rows, domainRow{Domain: str(values["domain"]), Queries: int64From(values["_value"])})
	})
	return rows
}

func (a *app) deviceTopDomains(ctx context.Context, window string, ip string) []domainRow {
	query := `from(bucket: "` + a.cfg.influxBucket + `")
  |> range(start: ` + window + `)
  |> filter(fn: (r) => r._measurement == "adguard_query" and r._field == "count")
  |> filter(fn: (r) => r.client_ip == ` + fluxString(ip) + `)
  |> group(columns: ["domain"])
  |> sum()
  |> group()
  |> sort(columns: ["_value"], desc: true)
  |> limit(n: 50)`

	rows := []domainRow{}
	a.each(ctx, query, func(values map[string]any) {
		rows = append(rows, domainRow{Domain: str(values["domain"]), Queries: int64From(values["_value"])})
	})
	return rows
}

func (a *app) blockedDomains(ctx context.Context, window string) []blockedRow {
	query := `from(bucket: "` + a.cfg.influxBucket + `")
  |> range(start: ` + window + `)
  |> filter(fn: (r) => r._measurement == "adguard_query" and r._field == "count")
  |> filter(fn: (r) => r.blocked == "true")
  |> group(columns: ["domain", "reason", "rule"])
  |> sum()
  |> group()
  |> sort(columns: ["_value"], desc: true)
  |> limit(n: 25)`

	rows := []blockedRow{}
	a.each(ctx, query, func(values map[string]any) {
		rows = append(rows, blockedRow{
			Domain:  str(values["domain"]),
			Reason:  str(values["reason"]),
			Rule:    str(values["rule"]),
			Blocked: int64From(values["_value"]),
		})
	})
	return rows
}

func (a *app) deviceBlockedDomains(ctx context.Context, window string, ip string) []blockedRow {
	query := `from(bucket: "` + a.cfg.influxBucket + `")
  |> range(start: ` + window + `)
  |> filter(fn: (r) => r._measurement == "adguard_query" and r._field == "count")
  |> filter(fn: (r) => r.blocked == "true" and r.client_ip == ` + fluxString(ip) + `)
  |> group(columns: ["domain", "reason", "rule"])
  |> sum()
  |> group()
  |> sort(columns: ["_value"], desc: true)
  |> limit(n: 50)`

	rows := []blockedRow{}
	a.each(ctx, query, func(values map[string]any) {
		rows = append(rows, blockedRow{
			Domain:  str(values["domain"]),
			Reason:  str(values["reason"]),
			Rule:    str(values["rule"]),
			Blocked: int64From(values["_value"]),
		})
	})
	return rows
}

func (a *app) talkers(ctx context.Context, window string, devices map[string]deviceRow) []talkerRow {
	query := `from(bucket: "` + a.cfg.influxBucket + `")
  |> range(start: ` + window + `)
  |> filter(fn: (r) => r._measurement == "traffic_flow" and r._field == "bytes")
  |> group(columns: ["src_ip", "dst_ip"])
  |> sum()`

	bytesByIP := map[string]int64{}
	for ip := range devices {
		bytesByIP[ip] = 0
	}
	a.each(ctx, query, func(values map[string]any) {
		bytes := int64From(values["_value"])
		for _, ip := range []string{str(values["src_ip"]), str(values["dst_ip"])} {
			if a.keepIP(ip) {
				bytesByIP[ip] += bytes
			}
		}
	})

	var maxBytes int64
	rows := make([]talkerRow, 0, len(bytesByIP))
	for ip, bytes := range bytesByIP {
		device := devices[ip]
		if bytes > maxBytes {
			maxBytes = bytes
		}
		rows = append(rows, talkerRow{
			IP:    ip,
			Name:  displayName(device),
			Kind:  device.Kind,
			Bytes: bytes,
		})
	}
	if maxBytes > 0 {
		for i := range rows {
			rows[i].Share = float64(rows[i].Bytes) / float64(maxBytes) * 100
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Bytes == rows[j].Bytes {
			return rows[i].IP < rows[j].IP
		}
		return rows[i].Bytes > rows[j].Bytes
	})
	return rows
}

func (a *app) recentBlocked(ctx context.Context, window string, devices map[string]deviceRow) []recentBlockedRow {
	query := `from(bucket: "` + a.cfg.influxBucket + `")
  |> range(start: ` + window + `)
  |> filter(fn: (r) => r._measurement == "adguard_query" and r._field == "count")
  |> filter(fn: (r) => r.blocked == "true")
  |> group()
  |> sort(columns: ["_time"], desc: true)
  |> limit(n: 50)`

	rows := []recentBlockedRow{}
	a.each(ctx, query, func(values map[string]any) {
		client := str(values["client_ip"])
		if !a.keepIP(client) {
			return
		}
		name := str(values["client_name"])
		if name == "" {
			name = displayName(devices[client])
		}
		rows = append(rows, recentBlockedRow{
			Time:   timeString(values["_time"]),
			Client: client,
			Name:   name,
			Domain: str(values["domain"]),
			Reason: str(values["reason"]),
		})
	})
	return rows
}

func (a *app) deviceRecentBlocked(ctx context.Context, window string, ip string) []recentBlockedRow {
	query := `from(bucket: "` + a.cfg.influxBucket + `")
  |> range(start: ` + window + `)
  |> filter(fn: (r) => r._measurement == "adguard_query" and r._field == "count")
  |> filter(fn: (r) => r.blocked == "true" and r.client_ip == ` + fluxString(ip) + `)
  |> group()
  |> sort(columns: ["_time"], desc: true)
  |> limit(n: 100)`

	rows := []recentBlockedRow{}
	a.each(ctx, query, func(values map[string]any) {
		rows = append(rows, recentBlockedRow{
			Time:   timeString(values["_time"]),
			Client: str(values["client_ip"]),
			Name:   str(values["client_name"]),
			Domain: str(values["domain"]),
			Reason: str(values["reason"]),
		})
	})
	return rows
}

func (a *app) deviceWebsiteActivity(ctx context.Context, window string, ip string) []websiteActivityRow {
	query := `from(bucket: "` + a.cfg.influxBucket + `")
  |> range(start: ` + window + `)
  |> filter(fn: (r) => r._measurement == "adguard_query" and r._field == "count")
  |> filter(fn: (r) => r.client_ip == ` + fluxString(ip) + `)
  |> group()
  |> sort(columns: ["_time"], desc: true)
  |> limit(n: 250)`

	rows := []websiteActivityRow{}
	a.each(ctx, query, func(values map[string]any) {
		rows = append(rows, websiteActivityRow{
			Time:      timeString(values["_time"]),
			Domain:    str(values["domain"]),
			QueryType: str(values["query_type"]),
			Blocked:   str(values["blocked"]) == "true",
			Status:    str(values["status"]),
			Reason:    str(values["reason"]),
			Rule:      str(values["rule"]),
			Upstream:  str(values["upstream"]),
		})
	})
	return rows
}

func (a *app) deviceTrafficPeers(ctx context.Context, window string, ip string) []trafficPeerRow {
	query := `from(bucket: "` + a.cfg.influxBucket + `")
  |> range(start: ` + window + `)
  |> filter(fn: (r) => r._measurement == "traffic_flow" and r._field == "bytes")
  |> filter(fn: (r) => r.src_ip == ` + fluxString(ip) + ` or r.dst_ip == ` + fluxString(ip) + `)
  |> group(columns: ["src_ip", "dst_ip", "protocol"])
  |> sum()
  |> group()
  |> sort(columns: ["_value"], desc: true)
  |> limit(n: 50)`

	rows := []trafficPeerRow{}
	a.each(ctx, query, func(values map[string]any) {
		src := str(values["src_ip"])
		dst := str(values["dst_ip"])
		peer := dst
		if dst == ip {
			peer = src
		}
		rows = append(rows, trafficPeerRow{
			Peer:     peer,
			Protocol: str(values["protocol"]),
			Bytes:    int64From(values["_value"]),
		})
	})
	return rows
}

func (a *app) keepIP(ip string) bool {
	return a.cfg.lanPrefix == "" || strings.HasPrefix(ip, a.cfg.lanPrefix)
}

func devicesByIP(rows []deviceRow) map[string]deviceRow {
	byIP := make(map[string]deviceRow, len(rows))
	for _, row := range rows {
		byIP[row.IP] = row
	}
	return byIP
}

func betterDevice(current, candidate deviceRow) deviceRow {
	if candidate.Source == "manual" && current.Source != "manual" {
		return fillDevice(candidate, current)
	}
	if candidate.Source == current.Source && candidate.Seen.After(current.Seen) {
		return fillDevice(candidate, current)
	}
	return fillDevice(current, candidate)
}

func fillDevice(primary, fallback deviceRow) deviceRow {
	if primary.Name == "" {
		primary.Name = fallback.Name
	}
	if primary.Hostname == "" {
		primary.Hostname = fallback.Hostname
	}
	if primary.Kind == "" {
		primary.Kind = fallback.Kind
	}
	if primary.MAC == "" {
		primary.MAC = fallback.MAC
	}
	if primary.Source == "" {
		primary.Source = fallback.Source
	}
	return primary
}

func displayName(row deviceRow) string {
	if row.Name != "" {
		return row.Name
	}
	return row.Hostname
}

func (a *app) summary(ctx context.Context, window string, data dashboard) summary {
	s := summary{Devices: len(data.Devices)}
	for _, client := range data.Clients {
		s.DNSQueries += client.Queries
		s.BlockedQueries += client.Blocked
	}
	for _, talker := range data.Talkers {
		s.NetworkBytes += talker.Bytes
	}
	if s.DNSQueries > 0 {
		s.BlockRate = float64(s.BlockedQueries) / float64(s.DNSQueries) * 100
	}
	return s
}

func (a *app) each(ctx context.Context, query string, fn func(map[string]any)) {
	result, err := a.queryAPI.Query(ctx, query)
	if err != nil {
		slog.Warn("query failed", "error", err)
		return
	}
	defer result.Close()
	for result.Next() {
		fn(result.Record().Values())
	}
	if err := result.Err(); err != nil {
		slog.Warn("query result failed", "error", err)
	}
}

func env(key string, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func fluxString(value string) string {
	return strconv.Quote(value)
}

func str(value any) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(strings.TrimRight(strings.TrimSpace(toString(value)), "."))
}

func toString(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	default:
		return fmt.Sprint(v)
	}
}

func int64From(value any) int64 {
	switch v := value.(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case uint64:
		return int64(v)
	case float64:
		return int64(v)
	default:
		return 0
	}
}

func timeString(value any) string {
	t, ok := value.(time.Time)
	if !ok {
		return ""
	}
	return t.Format("2006-01-02 15:04:05")
}

func timeFrom(value any) time.Time {
	t, ok := value.(time.Time)
	if !ok {
		return time.Time{}
	}
	return t
}
