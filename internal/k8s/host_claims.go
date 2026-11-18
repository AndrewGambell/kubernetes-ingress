package k8s

import (
	"fmt"
	"net/netip"
	"sort"

	"github.com/nginx/kubernetes-ingress/internal/configs"
	conf_v1 "github.com/nginx/kubernetes-ingress/pkg/apis/configuration/v1"
)

// A claim owns a whole server block for one hostname. A partial listener
// overlap rejects the claim rather than silently dropping some of its listeners.
type hostClaim struct {
	host      string
	resource  Resource
	listeners []hostSocket
}

type hostSocket struct {
	port int
	ipv4 string
	ipv6 string
}

func (c *Configuration) selectHostClaims(claims []hostClaim) map[string]Resource {
	// Resolve all contenders in winner order, including contenders of different
	// kinds. Otherwise an older claim can displace a newer claim after that newer
	// claim has already rejected a third, disjoint resource.
	sort.SliceStable(claims, func(i, j int) bool {
		return claims[i].resource.Wins(claims[j].resource)
	})
	owners := make(map[string][]hostClaim)
	hosts := make(map[string]Resource)
	for _, claim := range claims {
		conflict := false
		for _, owner := range owners[claim.host] {
			if c.hostClaimsOverlap(claim, owner) {
				conflict = true
				break
			}
		}
		if conflict {
			claim.resource.AddWarning(fmt.Sprintf("host %s is taken by another resource", claim.host))
			continue
		}
		key := claim.host
		if len(owners[claim.host]) != 0 {
			key = additionalHostKey(claim.host, claim.resource)
		}
		hosts[key] = claim.resource
		owners[claim.host] = append(owners[claim.host], claim)
	}
	return hosts
}

func additionalHostKey(host string, resource Resource) string {
	return host + "|" + resource.GetKeyWithKind()
}

// activeHostResource also handles the original hostname-only key, retained for
// the first owner so existing default-host and TLS-passthrough paths stay stable.
func activeHostResource(hosts map[string]Resource, host string, resource Resource) Resource {
	if r := hosts[host]; r != nil && r.GetKeyWithKind() == resource.GetKeyWithKind() {
		return r
	}
	return hosts[additionalHostKey(host, resource)]
}

func (c *Configuration) hostClaimsOverlap(a, b hostClaim) bool {
	// An unresolved listener has no usable socket to compare. Keep ownership
	// conservative until GlobalConfiguration supplies a valid listener.
	if len(a.listeners) == 0 || len(b.listeners) == 0 {
		return true
	}
	for _, x := range a.listeners {
		for _, y := range b.listeners {
			if x.port == y.port && (addressesOverlap(x.ipv4, y.ipv4, "0.0.0.0") ||
				(!c.isIPV6Disabled && addressesOverlap(x.ipv6, y.ipv6, "::"))) {
				return true
			}
		}
	}
	return false
}

func addressesOverlap(a, b, wildcard string) bool {
	if a == "" || b == "" {
		return true
	}
	x, errX := netip.ParseAddr(a)
	y, errY := netip.ParseAddr(b)
	if errX == nil && errY == nil {
		return x == y || x.IsUnspecified() || y.IsUnspecified()
	}
	return a == b || a == wildcard || b == wildcard
}

func (c *Configuration) newHostClaim(host string, resource Resource) hostClaim {
	claim := hostClaim{host: host, resource: resource}
	switch r := resource.(type) {
	case *VirtualServerConfiguration:
		claim.listeners = c.virtualServerHostSockets(r)
	case *IngressConfiguration:
		claim.listeners = c.ingressHostSockets(host, r)
	// TLS-passthrough host ownership remains global: its SNI map has no custom
	// HTTP listener dimension.
	case *TransportServerConfiguration:
	}
	return claim
}

func (c *Configuration) virtualServerHostSockets(r *VirtualServerConfiguration) []hostSocket {
	if c.isTLSPassthroughEnabled && r.VirtualServer.Spec.TLS != nil {
		// All terminating HTTPS servers share the passthrough Unix socket.
		return nil
	}
	if r.VirtualServer.Spec.Listener == nil {
		listeners := []hostSocket{{port: 80}}
		if r.VirtualServer.Spec.TLS != nil {
			listeners = append(listeners, hostSocket{port: 443})
		}
		return listeners
	}
	var sockets []hostSocket
	listeners := r.VirtualServer.Spec.Listener
	if listeners.HTTP != "" {
		if r.HTTPPort == 0 {
			return nil
		}
		sockets = append(sockets, hostSocket{port: r.HTTPPort, ipv4: r.HTTPIPv4, ipv6: r.HTTPIPv6})
	}
	if listeners.HTTPS != "" && r.VirtualServer.Spec.TLS != nil {
		if r.HTTPSPort == 0 {
			return nil
		}
		sockets = append(sockets, hostSocket{port: r.HTTPSPort, ipv4: r.HTTPSIPv4, ipv6: r.HTTPSIPv6})
	}
	return sockets
}

func (c *Configuration) ingressHostSockets(host string, r *IngressConfiguration) []hostSocket {
	if host == "" || c.isTLSPassthroughEnabled && ingressHostHasTLS(r, host) {
		return nil
	}
	var sockets []hostSocket
	for annotation, defaultPort := range map[string]int{"nginx.org/listen-ports": 80, "nginx.org/listen-ports-ssl": 443} {
		if annotation == "nginx.org/listen-ports-ssl" && !ingressHostHasTLS(r, host) {
			continue
		}
		ports, err := configs.ParsePortList(r.Ingress.Annotations[annotation])
		if err != nil || len(ports) == 0 {
			ports = []int{defaultPort}
		}
		for _, port := range ports {
			sockets = append(sockets, hostSocket{port: port})
		}
	}
	return sockets
}

func ingressHostHasTLS(r *IngressConfiguration, host string) bool {
	for _, tls := range r.Ingress.Spec.TLS {
		for _, h := range tls.Hosts {
			if h == host {
				return true
			}
		}
	}
	return false
}

// A host-based route is used when any active owner of that host references it.
// Otherwise return the first owner for the existing ignored-route diagnostic.
func (c *Configuration) activeVirtualServerForRoute(vsr *conf_v1.VirtualServerRoute) *VirtualServerConfiguration {
	var first *VirtualServerConfiguration
	for _, key := range getSortedResourceKeys(c.hosts) {
		vsc, ok := c.hosts[key].(*VirtualServerConfiguration)
		if !ok || vsc.VirtualServer.Spec.Host != vsr.Spec.Host {
			continue
		}
		if first == nil {
			first = vsc
		}
		for _, route := range vsc.VirtualServerRoutes {
			if route.Namespace == vsr.Namespace && route.Name == vsr.Name {
				return vsc
			}
		}
	}
	return first
}
