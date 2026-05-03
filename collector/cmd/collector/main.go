package main

import (
	"context"
	"encoding/binary"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
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
	packetEnabled bool
	iface         string
	promiscuous   bool
	flushInterval time.Duration
	scanEnabled   bool
	scanInterval  time.Duration
	scanCIDR      string
	scanMaxHosts  int
	deviceLabels  string
	influxURL     string
	influxToken   string
	influxOrg     string
	influxBucket  string
	adguard       adguardConfig
	logLevel      slog.Level
}

type adguardConfig struct {
	enabled      bool
	baseURL      string
	username     string
	password     string
	pollInterval time.Duration
	queryLimit   int
}

type device struct {
	ip       string
	mac      string
	hostname string
	name     string
	source   string
	kind     string
}

type deviceLabel struct {
	name string
	kind string
}

type deviceLabels struct {
	byMAC map[string]deviceLabel
	byIP  map[string]deviceLabel
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
	httpClient      *http.Client
	hostCache       map[string]string
	labelsByMAC     map[string]deviceLabel
	labelsByIP      map[string]deviceLabel
	namesByMAC      map[string]string
	seenAdGuard     map[string]struct{}
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
	labels, err := loadDeviceLabels(cfg.deviceLabels)
	if err != nil {
		slog.Warn("device labels not loaded", "path", cfg.deviceLabels, "error", err)
	}

	app := &collector{
		cfg:             cfg,
		writeAPI:        writer,
		httpClient:      &http.Client{Timeout: 10 * time.Second},
		hostCache:       make(map[string]string),
		labelsByMAC:     labels.byMAC,
		labelsByIP:      labels.byIP,
		namesByMAC:      make(map[string]string),
		seenAdGuard:     make(map[string]struct{}),
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
	scanInterval, err := time.ParseDuration(env("COLLECTOR_SCAN_INTERVAL", "5m"))
	if err != nil {
		return config{}, err
	}
	adguardPollInterval, err := time.ParseDuration(env("ADGUARD_POLL_INTERVAL", "30s"))
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
		packetEnabled: boolEnv("COLLECTOR_PACKET_ENABLED", true),
		iface:         os.Getenv("COLLECTOR_INTERFACE"),
		promiscuous:   boolEnv("COLLECTOR_PROMISCUOUS", true),
		flushInterval: flushInterval,
		scanEnabled:   boolEnv("COLLECTOR_SCAN_ENABLED", true),
		scanInterval:  scanInterval,
		scanCIDR:      os.Getenv("COLLECTOR_SCAN_CIDR"),
		scanMaxHosts:  intEnv("COLLECTOR_SCAN_MAX_HOSTS", 1024),
		deviceLabels:  env("COLLECTOR_DEVICE_LABELS", "/etc/home-net-observer/devices.csv"),
		influxURL:     env("COLLECTOR_INFLUX_URL", "http://localhost:8086"),
		influxToken:   os.Getenv("INFLUXDB_TOKEN"),
		influxOrg:     env("INFLUXDB_ORG", "home"),
		influxBucket:  env("INFLUXDB_BUCKET", "network"),
		adguard: adguardConfig{
			enabled:      boolEnv("ADGUARD_ENABLED", false),
			baseURL:      strings.TrimRight(env("ADGUARD_BASE_URL", ""), "/"),
			username:     os.Getenv("ADGUARD_USERNAME"),
			password:     os.Getenv("ADGUARD_PASSWORD"),
			pollInterval: adguardPollInterval,
			queryLimit:   intEnv("ADGUARD_QUERY_LIMIT", 500),
		},
		logLevel: level,
	}

	if cfg.influxToken == "" {
		return config{}, errors.New("INFLUXDB_TOKEN is required")
	}
	return cfg, nil
}

func (c *collector) run(ctx context.Context) error {
	c.publishManualDeviceLabels(ctx)

	if c.cfg.adguard.enabled {
		go c.runAdGuard(ctx)
	}
	if !c.cfg.packetEnabled {
		slog.Info("packet collector disabled; running background collectors only")
		<-ctx.Done()
		return context.Canceled
	}

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

	if c.cfg.scanEnabled {
		go c.runScanner(ctx, iface)
	}

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

func (c *collector) publishManualDeviceLabels(ctx context.Context) {
	now := time.Now()
	for ip, label := range c.labelsByIP {
		c.writeDeviceSeen(ctx, now, device{
			ip:     ip,
			name:   label.name,
			kind:   label.kind,
			source: "manual",
		})
	}
	for mac, label := range c.labelsByMAC {
		c.writeDeviceSeen(ctx, now, device{
			mac:    mac,
			name:   label.name,
			kind:   label.kind,
			source: "manual",
		})
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
		dns := dnsLayer.(*layers.DNS)
		c.learnDNSNames(dns)
		c.writeDNSQueries(ctx, now, c.enrichDevice(src), dns)
	}

	if dhcpLayer := packet.Layer(layers.LayerTypeDHCPv4); dhcpLayer != nil {
		c.learnDHCPName(src, dhcpLayer.(*layers.DHCPv4))
	}

	src = c.enrichDevice(src)
	dst = c.enrichDevice(dst)
	c.writeDeviceSeen(ctx, now, src)
	c.writeDeviceSeen(ctx, now, dst)
}

type adguardQueryLog struct {
	Data []adguardQuery `json:"data"`
}

type adguardQuery struct {
	Client     string `json:"client"`
	ClientInfo struct {
		Name string `json:"name"`
	} `json:"client_info"`
	ClientProto string `json:"client_proto"`
	ElapsedMS   string `json:"elapsedMs"`
	Question    struct {
		Class string `json:"class"`
		Name  string `json:"name"`
		Type  string `json:"type"`
	} `json:"question"`
	Reason   string          `json:"reason"`
	Rules    []adguardRule   `json:"rules"`
	Status   string          `json:"status"`
	Time     time.Time       `json:"time"`
	Upstream string          `json:"upstream"`
	Answer   []adguardAnswer `json:"answer"`
}

type adguardRule struct {
	Text string `json:"text"`
	Rule string `json:"rule"`
}

type adguardAnswer struct {
	Type  string `json:"type"`
	Value string `json:"value"`
	TTL   int    `json:"ttl"`
}

func (c *collector) runAdGuard(ctx context.Context) {
	if c.cfg.adguard.baseURL == "" || c.cfg.adguard.username == "" || c.cfg.adguard.password == "" {
		slog.Warn("adguard collector disabled; missing base URL or credentials")
		return
	}

	slog.Info("adguard collector started", "base_url", c.cfg.adguard.baseURL, "poll_interval", c.cfg.adguard.pollInterval)
	c.pollAdGuard(ctx)

	ticker := time.NewTicker(c.cfg.adguard.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.pollAdGuard(ctx)
		}
	}
}

func (c *collector) pollAdGuard(ctx context.Context) {
	queries, err := c.fetchAdGuardQueries(ctx)
	if err != nil {
		slog.Warn("failed to fetch adguard query log", "error", err)
		return
	}

	for i := len(queries) - 1; i >= 0; i-- {
		query := queries[i]
		key := adguardQueryKey(query)
		if _, ok := c.seenAdGuard[key]; ok {
			continue
		}
		c.seenAdGuard[key] = struct{}{}
		if err := c.writeAdGuardQuery(ctx, query); err != nil {
			slog.Warn("failed to write adguard query", "error", err)
		}
	}
}

func (c *collector) fetchAdGuardQueries(ctx context.Context) ([]adguardQuery, error) {
	url := c.cfg.adguard.baseURL + path.Join("/control/querylog")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	values := req.URL.Query()
	values.Set("limit", strconv.Itoa(c.cfg.adguard.queryLimit))
	req.URL.RawQuery = values.Encode()
	req.SetBasicAuth(c.cfg.adguard.username, c.cfg.adguard.password)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("adguard querylog returned " + resp.Status)
	}

	var body adguardQueryLog
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Data, nil
}

func (c *collector) writeAdGuardQuery(ctx context.Context, query adguardQuery) error {
	domain := strings.TrimRight(query.Question.Name, ".")
	clientName := cleanDeviceName(query.ClientInfo.Name)
	clientKind := ""
	clientSource := "adguard"
	if label, ok := c.labelsByIP[query.Client]; ok {
		clientName = label.name
		clientKind = label.kind
		clientSource = "manual"
	}
	blocked := isAdGuardBlocked(query)
	rule := firstAdGuardRule(query.Rules)
	elapsed := parseFloat(query.ElapsedMS)

	point := influxdb2.NewPoint(
		"adguard_query",
		map[string]string{
			"collector":     c.cfg.collectorName,
			"client_ip":     query.Client,
			"client_name":   clientName,
			"client_kind":   clientKind,
			"client_source": clientSource,
			"client_proto":  query.ClientProto,
			"domain":        domain,
			"query_type":    query.Question.Type,
			"status":        query.Status,
			"reason":        query.Reason,
			"rule":          rule,
			"upstream":      query.Upstream,
			"blocked":       strconv.FormatBool(blocked),
		},
		map[string]any{
			"count":      1,
			"blocked":    blocked,
			"elapsed_ms": elapsed,
			"answers":    len(query.Answer),
		},
		query.Time,
	)
	return c.writeAPI.WritePoint(ctx, point)
}

func adguardQueryKey(query adguardQuery) string {
	return query.Time.Format(time.RFC3339Nano) + "|" + query.Client + "|" + query.Question.Name + "|" + query.Question.Type + "|" + query.Reason
}

func isAdGuardBlocked(query adguardQuery) bool {
	reason := strings.ToLower(query.Reason)
	if strings.HasPrefix(reason, "notfiltered") || reason == "" {
		return false
	}
	if strings.Contains(reason, "whitelist") || strings.Contains(reason, "rewrite") {
		return false
	}
	return true
}

func firstAdGuardRule(rules []adguardRule) string {
	for _, rule := range rules {
		if rule.Text != "" {
			return rule.Text
		}
		if rule.Rule != "" {
			return rule.Rule
		}
	}
	return ""
}

func parseFloat(value string) float64 {
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0
	}
	return parsed
}

func (c *collector) runScanner(ctx context.Context, iface string) {
	if iface == "any" {
		slog.Warn("active LAN scan disabled on pseudo-interface", "interface", iface)
		return
	}

	scanTarget, err := scanTargetForInterface(iface, c.cfg.scanCIDR)
	if err != nil {
		slog.Warn("active LAN scan disabled", "interface", iface, "error", err)
		return
	}

	handle, err := pcap.OpenLive(iface, 1600, false, pcap.BlockForever)
	if err != nil {
		slog.Warn("active LAN scan disabled; cannot open interface", "interface", iface, "error", err)
		return
	}
	defer handle.Close()

	slog.Info("active LAN scanner started", "interface", iface, "cidr", scanTarget.network.String(), "source_ip", scanTarget.ip)
	c.scanOnce(ctx, handle, scanTarget)

	ticker := time.NewTicker(c.cfg.scanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.scanOnce(ctx, handle, scanTarget)
		}
	}
}

type scanTarget struct {
	ip      net.IP
	mac     net.HardwareAddr
	network *net.IPNet
}

func (c *collector) scanOnce(ctx context.Context, handle *pcap.Handle, target scanTarget) {
	hosts := hostsInNetwork(target.network, c.cfg.scanMaxHosts)
	if len(hosts) == 0 {
		return
	}

	slog.Debug("active LAN scan started", "cidr", target.network.String(), "hosts", len(hosts))
	for _, ip := range hosts {
		if ctx.Err() != nil {
			return
		}
		if ip.Equal(target.ip) {
			continue
		}
		packet, err := arpRequest(target.mac, target.ip, ip)
		if err != nil {
			slog.Warn("failed to build arp probe", "target_ip", ip, "error", err)
			continue
		}
		if err := handle.WritePacketData(packet); err != nil {
			slog.Warn("failed to send arp probe", "target_ip", ip, "error", err)
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func scanTargetForInterface(pcapInterface string, overrideCIDR string) (scanTarget, error) {
	devices, err := pcap.FindAllDevs()
	if err != nil {
		return scanTarget{}, err
	}

	var sourceIP net.IP
	var network *net.IPNet
	for _, dev := range devices {
		if dev.Name != pcapInterface {
			continue
		}
		for _, addr := range dev.Addresses {
			ip := addr.IP.To4()
			if ip == nil || ip.IsLoopback() {
				continue
			}
			sourceIP = ip
			network = &net.IPNet{IP: ip.Mask(addr.Netmask), Mask: addr.Netmask}
			break
		}
	}
	if sourceIP == nil {
		return scanTarget{}, errors.New("no IPv4 address found for capture interface")
	}

	if overrideCIDR != "" {
		_, parsed, err := net.ParseCIDR(overrideCIDR)
		if err != nil {
			return scanTarget{}, err
		}
		network = parsed
	}

	sourceMAC, err := macForIP(sourceIP)
	if err != nil {
		return scanTarget{}, err
	}

	return scanTarget{ip: sourceIP, mac: sourceMAC, network: network}, nil
}

func macForIP(ip net.IP) (net.HardwareAddr, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for _, iface := range interfaces {
		if len(iface.HardwareAddr) == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			if networkIP(addr).Equal(ip) {
				return iface.HardwareAddr, nil
			}
		}
	}
	return nil, errors.New("no MAC address found for capture interface IP")
}

func networkIP(addr net.Addr) net.IP {
	switch value := addr.(type) {
	case *net.IPNet:
		return value.IP.To4()
	case *net.IPAddr:
		return value.IP.To4()
	default:
		return nil
	}
}

func arpRequest(sourceMAC net.HardwareAddr, sourceIP net.IP, targetIP net.IP) ([]byte, error) {
	eth := &layers.Ethernet{
		SrcMAC:       sourceMAC,
		DstMAC:       net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		EthernetType: layers.EthernetTypeARP,
	}
	arp := &layers.ARP{
		AddrType:          layers.LinkTypeEthernet,
		Protocol:          layers.EthernetTypeIPv4,
		HwAddressSize:     6,
		ProtAddressSize:   4,
		Operation:         layers.ARPRequest,
		SourceHwAddress:   []byte(sourceMAC),
		SourceProtAddress: []byte(sourceIP.To4()),
		DstHwAddress:      []byte{0, 0, 0, 0, 0, 0},
		DstProtAddress:    []byte(targetIP.To4()),
	}

	buffer := gopacket.NewSerializeBuffer()
	err := gopacket.SerializeLayers(buffer, gopacket.SerializeOptions{FixLengths: true}, eth, arp)
	if err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func hostsInNetwork(network *net.IPNet, maxHosts int) []net.IP {
	if network == nil || maxHosts <= 0 {
		return nil
	}
	base := network.IP.To4()
	if base == nil {
		return nil
	}

	ones, bits := network.Mask.Size()
	if bits != 32 || ones < 16 {
		return nil
	}

	total := 1 << uint(32-ones)
	if total <= 2 {
		return nil
	}
	usable := total - 2
	if usable > maxHosts {
		usable = maxHosts
	}

	start := binary.BigEndian.Uint32(base)
	hosts := make([]net.IP, 0, usable)
	for offset := 1; offset <= usable; offset++ {
		ip := make(net.IP, 4)
		binary.BigEndian.PutUint32(ip, start+uint32(offset))
		hosts = append(hosts, ip)
	}
	return hosts
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
			"name":      d.name,
			"source":    d.source,
			"kind":      d.kind,
		},
		map[string]any{"seen": 1},
		t,
	)

	if err := c.writeAPI.WritePoint(ctx, point); err != nil {
		slog.Warn("failed to write device point", "error", err)
	}
}

func (c *collector) enrichDevice(d device) device {
	d.mac = normalizeMAC(d.mac)
	if label, ok := c.labelsByIP[d.ip]; ok {
		d.name = label.name
		d.kind = label.kind
		d.source = "manual"
		return d
	}
	if d.mac != "" {
		if label, ok := c.labelsByMAC[d.mac]; ok {
			d.name = label.name
			d.kind = label.kind
			d.source = "manual"
			return d
		}
		if name, ok := c.namesByMAC[d.mac]; ok && name != "" {
			d.name = name
			d.source = "dhcp"
			return d
		}
	}

	if d.hostname == "" {
		d.hostname = c.hostname(d.ip)
	}
	if d.hostname != "" {
		d.name = d.hostname
		d.source = "dns"
	}
	return d
}

func (c *collector) learnDHCPName(src device, dhcp *layers.DHCPv4) {
	name := ""
	for _, option := range dhcp.Options {
		if option.Type == layers.DHCPOptHostname {
			name = cleanDeviceName(string(option.Data))
			break
		}
	}
	if name == "" {
		return
	}

	mac := normalizeMAC(dhcp.ClientHWAddr.String())
	if mac == "" {
		mac = normalizeMAC(src.mac)
	}
	if mac != "" {
		c.namesByMAC[mac] = name
	}

	for _, ip := range []net.IP{dhcp.YourClientIP, dhcp.ClientIP, net.ParseIP(src.ip)} {
		if ip4 := ip.To4(); ip4 != nil && !ip4.Equal(net.IPv4zero) {
			c.hostCache[ip4.String()] = name
		}
	}
}

func (c *collector) learnDNSNames(dns *layers.DNS) {
	if !dns.QR {
		return
	}
	for _, answer := range dns.Answers {
		if answer.Type != layers.DNSTypeA && answer.Type != layers.DNSTypeAAAA {
			continue
		}
		if answer.IP == nil || answer.IP.IsUnspecified() || answer.IP.IsLoopback() {
			continue
		}
		if !isLocalNameCandidate(answer.IP) {
			continue
		}
		name := cleanDeviceName(string(answer.Name))
		if name == "" {
			continue
		}
		c.hostCache[answer.IP.String()] = name
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

func loadDeviceLabels(path string) (deviceLabels, error) {
	labels := deviceLabels{byMAC: make(map[string]deviceLabel), byIP: make(map[string]deviceLabel)}
	if strings.TrimSpace(path) == "" {
		return labels, nil
	}

	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return labels, nil
		}
		return labels, err
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.TrimLeadingSpace = true
	line := 0
	for {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return labels, err
		}
		line++
		if len(record) == 0 {
			continue
		}
		if line == 1 && (strings.EqualFold(strings.TrimSpace(record[0]), "mac") || strings.EqualFold(strings.TrimSpace(record[0]), "id")) {
			continue
		}
		if len(record) < 2 {
			continue
		}

		id := strings.TrimSpace(record[0])
		mac := normalizeMAC(id)
		ip := net.ParseIP(id)
		name := cleanDeviceName(record[1])
		if mac == "" && ip == nil || name == "" {
			continue
		}

		kind := ""
		if len(record) > 2 {
			kind = cleanDeviceName(record[2])
		}
		label := deviceLabel{name: name, kind: kind}
		if mac != "" {
			labels.byMAC[mac] = label
		}
		if ip != nil {
			labels.byIP[ip.String()] = label
		}
	}
	return labels, nil
}

func normalizeMAC(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" || value == "00:00:00:00:00:00" {
		return ""
	}
	mac, err := net.ParseMAC(value)
	if err != nil {
		return ""
	}
	return mac.String()
}

func cleanDeviceName(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimRight(value, ".")
	value = strings.Trim(value, "\x00")
	if value == "" {
		return ""
	}
	return value
}

func isLocalNameCandidate(ip net.IP) bool {
	if ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	return false
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

func intEnv(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
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
