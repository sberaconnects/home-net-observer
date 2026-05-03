# Architecture

Home Net Observer has three responsibilities:

1. Capture local network metadata.
2. Store time-series measurements in InfluxDB.
3. Visualize household activity in Grafana.

## Components

### Collector

The collector is a Go application that reads packets from a configured capture
interface using libpcap/Npcap through `gopacket`.

It extracts metadata only:

- IP addresses
- MAC addresses when visible at the capture point
- reverse DNS hostnames when available
- DNS query names and query types
- protocol, byte count, and packet count
- active ARP scan responses for local subnet device discovery

The collector writes directly to InfluxDB 2.x.

### InfluxDB

InfluxDB stores measurements in the configured bucket. The default bucket is
`network`.

Measurements:

- `traffic_flow`
- `dns_query`
- `device_seen`

### Grafana

Grafana is provisioned with:

- an InfluxDB datasource
- a starter overview dashboard
- a device detail dashboard
- panels for traffic rate, top DNS queries, source IPs, and devices seen
- drill-down links from device rows to device history and traffic

## Deployment Modes

### Local All-In-One

Run InfluxDB, Grafana, and the collector on one host. This is simplest on Linux.
On Windows Docker Desktop, the stack runs well, but full physical NIC capture is
usually better with a native collector and Npcap.

### Central Home Server

Run InfluxDB and Grafana once on a home server. Run collectors on PCs or network
devices and point them at the central InfluxDB URL.

### Future Gateway Mode

The best household-wide visibility comes from a router, firewall, mirrored switch
port, or Raspberry Pi bridge. The current collector is designed so that mode can
be added without changing the storage or dashboard layer.

## Roadmap

- Better hostname enrichment from DHCP leases, mDNS, and NetBIOS.
- Optional device inventory labels.
- Packaged Windows collector release.
- Alerts for unusual traffic spikes.
- Optional denylist/allowlist panels.
- Multi-collector dashboard variables.
