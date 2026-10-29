package main

import (
	"context"
	"net"
	"net/netip"
	"os"

	"github.com/swenske/Janus/internal/cluster"
	"github.com/swenske/Janus/internal/consul"
	"github.com/swenske/Janus/internal/exporter"
	"github.com/swenske/Janus/internal/extensions"
	"github.com/swenske/Janus/internal/haproxy"
	"github.com/swenske/Janus/internal/nodeexporter"
	"github.com/swenske/Janus/internal/shutdown"
)

// The node's own services in Consul (internal/consul's Registrar), when
// a Controller manages the agent: read from HAProxy's configuration and
// maps as they are, the exporters' settings, janusd's version; in
// maintenance while the member is drained, its HAProxy stopped, the
// node going down.

// mainIPv4 is the first IPv4 address of main, "" if none.
func mainIPv4(main []netip.Addr) string {
	for _, a := range main {
		if a.Is4() {
			return a.String()
		}
	}
	return ""
}

// endpointOf is where an exporter listening on address:port is reached:
// address "auto" or empty is the node's.
func endpointOf(address string, port uint32) *consul.Endpoint {
	host := ""
	if net.ParseIP(address) != nil {
		host = address
	}
	return &consul.Endpoint{Host: host, Port: int(port)}
}

// startConsulRegistrar starts registering the node's services; it
// returns what puts them in maintenance at once, before janusd stops.
func startConsulRegistrar(mgr *consul.Manager, haproxyMgr *haproxy.Manager, haproxyCfg string, ext *extensions.Manager, clusterStore *cluster.Store) *consul.Registrar {
	r := &consul.Registrar{
		Manager: mgr,
		Sources: func() (consul.Sources, error) {
			cfg, err := os.ReadFile(haproxyCfg)
			if err != nil {
				return consul.Sources{}, err
			}
			src := consul.Sources{Address: mgr.Address(), Version: version, APIPort: 9505,
				HAProxyConfig: string(cfg), Map: haproxyMgr.MapGet}
			if c, _, err := exporter.Load(); err == nil && c.Enabled {
				src.Exporter = endpointOf(c.Address, c.Port)
			}
			if ext.Has(nodeexporter.ServiceID) {
				if st, err := ext.State(nodeexporter.ServiceID); err == nil && st.State == "running" {
					if c, _, err := nodeexporter.Load(); err == nil && c.Enabled {
						src.NodeExporter = endpointOf(c.Address, c.Port)
					}
				}
			}
			return src, nil
		},
		Maintenance: func() string {
			switch {
			case clusterStore.Drained():
				return "drained"
			case clusterStore.OutOfService():
				return "janusd is stopping"
			case !haproxyMgr.Serving():
				return "HAProxy is stopped"
			}
			return ""
		},
		Wake: func() []<-chan struct{} {
			return []<-chan struct{}{haproxyMgr.Updated(), haproxyMgr.Changed(), clusterStore.DrainChanged()}
		},
	}
	shutdown.Before(func() { r.GoingDown("the node is going down") })
	go r.Run(context.Background())
	return r
}
