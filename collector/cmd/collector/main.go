package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/influxdata/influxdb-client-go/v2/api/write"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcap"
)

type config struct {
	collectorName string
	iface         string
	promiscuous   bool
	flushInterval time.Duration
	influxURL     string
	influxToken   string
	influxOrg     string
	influxBucket  string
	logLevel      slog.Level
}

type device struct {
	ip       string
	mac      string
	hostname string
}

type flowKey struct {
	srcIP    string
	dstIP    string
	srcMAC   string
	dstMAC   string
	protocol string
}

type flowStats struct {
	bytes   int64
	packets int64
}

type collector struct {
	cfg             config
	writeAPI        influxWriter
	hostCache       map[string]string
	deviceSeenCache map[string]time.Time
	flowBuffer      map[flowKey]flowStats
}

type influxWriter interface {
	WritePoint(context.Context, ...*write.Point) error
}

type blockingWriter struct {
	api interface {
		WritePoint(context.Context, ...*write.Point) error
	}
}

func (w blockingWriter) WritePoint(ctx context.Context, points ...*write.Point) error {
	return w.api.WritePoint(ctx, points...)
}

func main() {
	listInterfaces := flag.Bool("list-interfaces", false, "list packet capture interfaces and exit")
	flag.Parse()

	if *listInterfaces {
		if err := printInterfaces(); err != nil {
			slog.Error("failed to list interfaces", "error", err)
			os.Exit(1)
		}
		return
	}

	cfg, err := loadConfig()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.logLevel}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := influxdb2.NewClient(cfg.influxURL, cfg.influxToken)
	defer client.Close()

	writer := blockingWriter{api: client.WriteAPIBlocking(cfg.influxOrg, cfg.influxBucket)}
	app := &collector{
		cfg:             cfg,
		writeAPI:        writer,
		hostCache:       make(map[string]string),
		deviceSeenCache: make(map[string]time.Time),
		flowBuffer:      make(map[flowKey]flowStats),
	}

	if err := app.run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("collector stopped", "error", err)
		os.Exit(1)
	}
}

func loadConfig() (config, error) {
	flushInterval, err := time.ParseDuration(env("COLLECTOR_FLUSH_INTERVAL", "10s"))
	if err != nil {
		return config{}, err
	}

	level := slog.LevelInfo
	switch strings.ToLower(env("COLLECTOR_LOG_LEVEL", "info")) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	cfg := config{
		collectorName: env("COLLECTOR_NAME", hostnameFallback()),
		iface:         os.Getenv("COLLECTOR_INTERFACE"),
		promiscuous:   boolEnv("COLLECTOR_PROMISCUOUS", true),
		flushInterval: flushInterval,
		influxURL:     env("COLLECTOR_INFLUX_URL", "http://localhost:8086"),
		influxToken:   os.Getenv("INFLUXDB_TOKEN"),
		influxOrg:     env("INFLUXDB_ORG", "home"),
		influxBucket:  env("INFLUXDB_BUCKET", "network"),
		logLevel:      level,
	}

	if cfg.influxToken == "" {
		return config{}, errors.New("INFLUXDB_TOKEN is required")
	}
	return cfg, nil
}

func (c *collector) run(ctx context.Context) error {
	iface := c.cfg.iface
	if iface == "" {
		defaultIface, err := firstUsableInterface()
		if err != nil {
			return err
		}
		iface = defaultIface
	}

	handle, err := pcap.OpenLive(iface, 1600, c.cfg.promiscuous, pcap.BlockForever)
	if err != nil {
		return err
	}
	defer handle.Close()

	if err := handle.SetBPFFilter("ip or ip6 or arp"); err != nil {
		return err
	}

	slog.Info("collector started", "collector", c.cfg.collectorName, "interface", iface, "influx", c.cfg.influxURL)

	packetSource := gopacket.NewPacketSource(handle, handle.LinkType())
	ticker := time.NewTicker(c.cfg.flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return c.flush(context.Background())
		case packet, ok := <-packetSource.Packets():
			if !ok {
				return nil
			}
			c.observePacket(ctx, packet)
		case <-ticker.C:
			if err := c.flush(context.Background()); err != nil {
				slog.Warn("failed to flush metrics", "error", err)
			}
		}
	}
}

func (c *collector) observePacket(ctx context.Context, packet gopacket.Packet) {
	now := packet.Metadata().Timestamp
	eth := ethernet(packet)
	src := device{}
	dst := device{}

	if eth != nil {
		src.mac = eth.SrcMAC.String()
		dst.mac = eth.DstMAC.String()
	}

	protocol := "unknown"
	if ip4 := packet.Layer(layers.LayerTypeIPv4); ip4 != nil {
		layer := ip4.(*layers.IPv4)
		src.ip = layer.SrcIP.String()
		dst.ip = layer.DstIP.String()
		protocol = layer.Protocol.String()
	} else if ip6 := packet.Layer(layers.LayerTypeIPv6); ip6 != nil {
		layer := ip6.(*layers.IPv6)
		src.ip = layer.SrcIP.String()
		dst.ip = layer.DstIP.String()
		protocol = layer.NextHeader.String()
	} else if arp := packet.Layer(layers.LayerTypeARP); arp != nil {
		layer := arp.(*layers.ARP)
		src.ip = net.IP(layer.SourceProtAddress).String()
		src.mac = net.HardwareAddr(layer.SourceHwAddress).String()
		dst.ip = net.IP(layer.DstProtAddress).String()
		dst.mac = net.HardwareAddr(layer.DstHwAddress).String()
		protocol = "ARP"
	}

	src.hostname = c.hostname(src.ip)
	dst.hostname = c.hostname(dst.ip)
	c.writeDeviceSeen(ctx, now, src)
	c.writeDeviceSeen(ctx, now, dst)

	if src.ip != "" && dst.ip != "" {
		key := flowKey{
			srcIP:    src.ip,
			dstIP:    dst.ip,
			srcMAC:   src.mac,
			dstMAC:   dst.mac,
			protocol: protocol,
		}
		stats := c.flowBuffer[key]
		stats.bytes += int64(len(packet.Data()))
		stats.packets++
		c.flowBuffer[key] = stats
	}

	if dnsLayer := packet.Layer(layers.LayerTypeDNS); dnsLayer != nil {
		c.writeDNSQueries(ctx, now, src, dnsLayer.(*layers.DNS))
	}
}

func (c *collector) writeDeviceSeen(ctx context.Context, t time.Time, d device) {
	if d.ip == "" && d.mac == "" {
		return
	}

	key := d.ip + "|" + d.mac
	if lastSeen, ok := c.deviceSeenCache[key]; ok && t.Sub(lastSeen) < time.Minute {
		return
	}
	c.deviceSeenCache[key] = t

	point := influxdb2.NewPoint(
		"device_seen",
		map[string]string{
			"collector": c.cfg.collectorName,
			"ip":        d.ip,
			"mac":       d.mac,
			"hostname":  d.hostname,
		},
		map[string]any{"seen": 1},
		t,
	)

	if err := c.writeAPI.WritePoint(ctx, point); err != nil {
		slog.Warn("failed to write device point", "error", err)
	}
}

func (c *collector) writeDNSQueries(ctx context.Context, t time.Time, src device, dns *layers.DNS) {
	if !dns.QR {
		for _, question := range dns.Questions {
			point := influxdb2.NewPoint(
				"dns_query",
				map[string]string{
					"collector":    c.cfg.collectorName,
					"src_ip":       src.ip,
					"src_mac":      src.mac,
					"src_hostname": src.hostname,
					"domain":       strings.TrimRight(string(question.Name), "."),
					"qtype":        question.Type.String(),
				},
				map[string]any{"count": 1},
				t,
			)
			if err := c.writeAPI.WritePoint(ctx, point); err != nil {
				slog.Warn("failed to write dns point", "error", err)
			}
		}
	}
}

func (c *collector) flush(ctx context.Context) error {
	now := time.Now()
	for key, stats := range c.flowBuffer {
		point := influxdb2.NewPoint(
			"traffic_flow",
			map[string]string{
				"collector": c.cfg.collectorName,
				"src_ip":    key.srcIP,
				"dst_ip":    key.dstIP,
				"src_mac":   key.srcMAC,
				"dst_mac":   key.dstMAC,
				"protocol":  key.protocol,
			},
			map[string]any{
				"bytes":   stats.bytes,
				"packets": stats.packets,
			},
			now,
		)
		if err := c.writeAPI.WritePoint(ctx, point); err != nil {
			return err
		}
	}
	c.flowBuffer = make(map[flowKey]flowStats)
	return nil
}

func (c *collector) hostname(ip string) string {
	if ip == "" {
		return ""
	}
	if cached, ok := c.hostCache[ip]; ok {
		return cached
	}
	names, err := net.LookupAddr(ip)
	if err != nil || len(names) == 0 {
		c.hostCache[ip] = ""
		return ""
	}
	name := strings.TrimRight(names[0], ".")
	c.hostCache[ip] = name
	return name
}

func ethernet(packet gopacket.Packet) *layers.Ethernet {
	if layer := packet.Layer(layers.LayerTypeEthernet); layer != nil {
		return layer.(*layers.Ethernet)
	}
	return nil
}

func printInterfaces() error {
	devices, err := pcap.FindAllDevs()
	if err != nil {
		return err
	}
	for _, dev := range devices {
		slog.Info("interface", "name", dev.Name, "description", dev.Description, "addresses", dev.Addresses)
	}
	return nil
}

func firstUsableInterface() (string, error) {
	devices, err := pcap.FindAllDevs()
	if err != nil {
		return "", err
	}
	for _, dev := range devices {
		if len(dev.Addresses) == 0 {
			continue
		}
		for _, addr := range dev.Addresses {
			if addr.IP != nil && !addr.IP.IsLoopback() {
				return dev.Name, nil
			}
		}
	}
	return "", errors.New("no usable packet capture interface found; set COLLECTOR_INTERFACE")
}

func env(key string, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func boolEnv(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func hostnameFallback() string {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		return "home-net-observer"
	}
	return hostname
}
