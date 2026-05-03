package main

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/influxdata/influxdb-client-go/v2/api/write"
)

type fakeWriter struct {
	points []*write.Point
}

func TestHostsInNetworkLimitsScanRange(t *testing.T) {
	_, network, err := net.ParseCIDR("192.168.1.0/24")
	if err != nil {
		t.Fatalf("failed to parse cidr: %v", err)
	}

	hosts := hostsInNetwork(network, 3)
	if len(hosts) != 3 {
		t.Fatalf("expected 3 hosts, got %d", len(hosts))
	}
	if hosts[0].String() != "192.168.1.1" {
		t.Fatalf("expected first host 192.168.1.1, got %s", hosts[0])
	}
	if hosts[2].String() != "192.168.1.3" {
		t.Fatalf("expected third host 192.168.1.3, got %s", hosts[2])
	}
}

func TestHostsInNetworkSkipsVeryLargeRanges(t *testing.T) {
	_, network, err := net.ParseCIDR("10.0.0.0/8")
	if err != nil {
		t.Fatalf("failed to parse cidr: %v", err)
	}

	hosts := hostsInNetwork(network, 1024)
	if len(hosts) != 0 {
		t.Fatalf("expected large range to be skipped, got %d hosts", len(hosts))
	}
}

func (w *fakeWriter) WritePoint(_ context.Context, points ...*write.Point) error {
	w.points = append(w.points, points...)
	return nil
}

func TestObservePacketWritesDNSQueryMetadata(t *testing.T) {
	writer := &fakeWriter{}
	app := &collector{
		cfg: config{
			collectorName: "test-collector",
		},
		writeAPI:        writer,
		hostCache:       map[string]string{},
		labelsByMAC:     map[string]deviceLabel{},
		labelsByIP:      map[string]deviceLabel{},
		namesByMAC:      map[string]string{},
		deviceSeenCache: map[string]time.Time{},
		flowBuffer:      map[flowKey]flowStats{},
	}

	packet := dnsQueryPacket(t, "example.com")
	app.observePacket(context.Background(), packet)

	foundDNS := false
	foundFlow := false
	foundDevice := false
	for _, point := range writer.points {
		switch {
		case point.Name() == "dns_query" && tagValue(point, "domain") == "example.com":
			foundDNS = true
		case point.Name() == "traffic_flow":
			foundFlow = true
		case point.Name() == "device_seen":
			foundDevice = true
		}
	}

	if !foundDNS {
		t.Fatalf("expected dns_query point, got %#v", writer.points)
	}
	if !foundDevice {
		t.Fatalf("expected device_seen point, got %#v", writer.points)
	}

	if err := app.flush(context.Background()); err != nil {
		t.Fatalf("flush failed: %v", err)
	}
	for _, point := range writer.points {
		if point.Name() == "traffic_flow" {
			foundFlow = true
			break
		}
	}
	if !foundFlow {
		t.Fatalf("expected traffic_flow point after flush, got %#v", writer.points)
	}
}

func TestEnrichDeviceUsesManualLabelBeforeLearnedNames(t *testing.T) {
	app := &collector{
		hostCache: map[string]string{},
		labelsByMAC: map[string]deviceLabel{
			"aa:bb:cc:dd:ee:ff": {name: "Living room TV", kind: "tv"},
		},
		namesByMAC: map[string]string{
			"aa:bb:cc:dd:ee:ff": "dhcp-tv",
		},
	}

	device := app.enrichDevice(device{mac: "AA-BB-CC-DD-EE-FF", ip: "192.168.1.20"})

	if device.name != "Living room TV" {
		t.Fatalf("expected manual label, got %q", device.name)
	}
	if device.source != "manual" {
		t.Fatalf("expected manual source, got %q", device.source)
	}
	if device.kind != "tv" {
		t.Fatalf("expected tv kind, got %q", device.kind)
	}
}

func TestEnrichDeviceUsesIPLabel(t *testing.T) {
	app := &collector{
		hostCache:   map[string]string{},
		labelsByMAC: map[string]deviceLabel{},
		labelsByIP: map[string]deviceLabel{
			"192.168.178.50": {name: "Teen phone", kind: "phone"},
		},
		namesByMAC: map[string]string{},
	}

	device := app.enrichDevice(device{ip: "192.168.178.50"})

	if device.name != "Teen phone" {
		t.Fatalf("expected IP label, got %q", device.name)
	}
	if device.source != "manual" {
		t.Fatalf("expected manual source, got %q", device.source)
	}
}

func TestLearnDHCPNameMapsMACAndIP(t *testing.T) {
	app := &collector{
		hostCache:  map[string]string{},
		namesByMAC: map[string]string{},
	}
	dhcp := &layers.DHCPv4{
		ClientIP:     net.IPv4(192, 168, 1, 25),
		ClientHWAddr: net.HardwareAddr{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff},
		Options: layers.DHCPOptions{
			layers.NewDHCPOption(layers.DHCPOptHostname, []byte("work-laptop")),
		},
	}

	app.learnDHCPName(device{}, dhcp)

	if app.namesByMAC["aa:bb:cc:dd:ee:ff"] != "work-laptop" {
		t.Fatalf("expected DHCP name by MAC, got %#v", app.namesByMAC)
	}
	if app.hostCache["192.168.1.25"] != "work-laptop" {
		t.Fatalf("expected DHCP name by IP, got %#v", app.hostCache)
	}
}

func TestLoadDeviceLabelsSupportsIPAndMAC(t *testing.T) {
	path := tempDeviceLabels(t, `id,name,kind
aa:bb:cc:dd:ee:ff,Living room TV,tv
192.168.178.50,Teen phone,phone
`)
	labels, err := loadDeviceLabels(path)
	if err != nil {
		t.Fatalf("failed to load labels: %v", err)
	}

	if labels.byMAC["aa:bb:cc:dd:ee:ff"].name != "Living room TV" {
		t.Fatalf("expected MAC label, got %#v", labels.byMAC)
	}
	if labels.byIP["192.168.178.50"].name != "Teen phone" {
		t.Fatalf("expected IP label, got %#v", labels.byIP)
	}
}

func TestPublishManualDeviceLabelsWritesDeviceSeen(t *testing.T) {
	writer := &fakeWriter{}
	app := &collector{
		writeAPI: writer,
		labelsByIP: map[string]deviceLabel{
			"192.168.178.51": {name: "Teen tablet", kind: "tablet"},
		},
		labelsByMAC:     map[string]deviceLabel{},
		deviceSeenCache: map[string]time.Time{},
	}

	app.publishManualDeviceLabels(context.Background())

	found := false
	for _, point := range writer.points {
		if point.Name() == "device_seen" && tagValue(point, "ip") == "192.168.178.51" && tagValue(point, "name") == "Teen tablet" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected manual device_seen point, got %#v", writer.points)
	}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working directory: %v", err)
	}
	return wd
}

func tempDeviceLabels(t *testing.T, content string) string {
	t.Helper()
	path := t.TempDir() + "/devices.csv"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write temp device labels: %v", err)
	}
	return path
}

func TestAdGuardBlockedReasonDetection(t *testing.T) {
	allowed := adguardQuery{Reason: "NotFilteredNotFound"}
	if isAdGuardBlocked(allowed) {
		t.Fatal("expected NotFilteredNotFound to be allowed")
	}

	blocked := adguardQuery{Reason: "FilteredBlackList"}
	if !isAdGuardBlocked(blocked) {
		t.Fatal("expected FilteredBlackList to be blocked")
	}
}

func TestAdGuardQueryKeyIncludesStableFields(t *testing.T) {
	query := adguardQuery{
		Client: "192.168.178.20",
		Reason: "FilteredBlackList",
		Time:   time.Date(2026, 5, 3, 7, 34, 0, 0, time.UTC),
	}
	query.Question.Name = "example.com"
	query.Question.Type = "A"

	key := adguardQueryKey(query)
	expected := "2026-05-03T07:34:00Z|192.168.178.20|example.com|A|FilteredBlackList"
	if key != expected {
		t.Fatalf("expected %q, got %q", expected, key)
	}
}

func dnsQueryPacket(t *testing.T, domain string) gopacket.Packet {
	t.Helper()

	eth := &layers.Ethernet{
		SrcMAC:       []byte{0x02, 0x42, 0xac, 0x11, 0x00, 0x02},
		DstMAC:       []byte{0x02, 0x42, 0xac, 0x11, 0x00, 0x03},
		EthernetType: layers.EthernetTypeIPv4,
	}
	ip := &layers.IPv4{
		Version:  4,
		TTL:      64,
		Protocol: layers.IPProtocolUDP,
		SrcIP:    []byte{192, 168, 1, 10},
		DstIP:    []byte{192, 168, 1, 1},
	}
	udp := &layers.UDP{
		SrcPort: 51000,
		DstPort: 53,
	}
	if err := udp.SetNetworkLayerForChecksum(ip); err != nil {
		t.Fatalf("failed to set checksum layer: %v", err)
	}
	dns := &layers.DNS{
		ID:      100,
		QR:      false,
		OpCode:  layers.DNSOpCodeQuery,
		RD:      true,
		QDCount: 1,
		Questions: []layers.DNSQuestion{
			{
				Name:  []byte(domain),
				Type:  layers.DNSTypeA,
				Class: layers.DNSClassIN,
			},
		},
	}

	buffer := gopacket.NewSerializeBuffer()
	err := gopacket.SerializeLayers(
		buffer,
		gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true},
		eth,
		ip,
		udp,
		dns,
	)
	if err != nil {
		t.Fatalf("failed to serialize packet: %v", err)
	}

	packet := gopacket.NewPacket(buffer.Bytes(), layers.LayerTypeEthernet, gopacket.Default)
	packet.Metadata().Timestamp = time.Unix(1700000000, 0)
	return packet
}

func tagValue(point *write.Point, key string) string {
	for _, tag := range point.TagList() {
		if tag.Key == key {
			return tag.Value
		}
	}
	return ""
}
