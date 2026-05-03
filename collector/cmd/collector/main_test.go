package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/influxdata/influxdb-client-go/v2/api/write"
)

type fakeWriter struct {
	points []*write.Point
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
