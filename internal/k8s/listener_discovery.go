package k8s

import (
	"fmt"
	"log/slog"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"

	nl "github.com/nginx/kubernetes-ingress/internal/logger"
	"github.com/nginx/kubernetes-ingress/internal/nsregistry"
	conf "github.com/nginx/kubernetes-ingress/pkg/apis/configuration/v1"
	core "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"
)

// listenerServiceDiscovery reports VirtualServer endpoints from the LoadBalancer Services that expose
// the controller pods on the VirtualServer's listener ports.
type listenerServiceDiscovery struct {
	pod           *core.Pod
	configuration *Configuration
	wildcardTLS   bool
	// serviceNames, when non-empty, restricts discovery to these Service names in the controller namespace.
	serviceNames sets.Set[string]

	logger *slog.Logger

	// mu guards candidates, the names of the Services that selected the controller pods at the last listing,
	// and warnedUnwatched, which limits the unwatched-namespace warning to once per transition.
	mu              sync.Mutex
	candidates      sets.Set[string]
	warnedUnwatched bool
}

type listenerBinding struct {
	port int
	ipv4 string
	ipv6 string
}

// listenerService is a Service that selects the controller pods, with its EndpointSlices and
// load balancer addresses.
type listenerService struct {
	svc            *core.Service
	endpointSlices []discovery.EndpointSlice
	addresses      []string
}

func newListenerServiceDiscovery(logger *slog.Logger, pod *core.Pod, configuration *Configuration, wildcardTLS bool, serviceNames []string) *listenerServiceDiscovery {
	return &listenerServiceDiscovery{
		logger:        logger,
		pod:           pod,
		configuration: configuration,
		wildcardTLS:   wildcardTLS,
		serviceNames:  sets.New(serviceNames...),
		candidates:    sets.New[string](),
	}
}

// selectsController reports whether svc is a LoadBalancer Service, allowed by serviceNames, that routes to the controller pod.
func (d *listenerServiceDiscovery) selectsController(svc *core.Service) bool {
	return d != nil && svc != nil && d.pod != nil && d.pod.Namespace == svc.Namespace &&
		svc.Spec.Type == core.ServiceTypeLoadBalancer && !isHeadless(svc) &&
		(d.serviceNames.Len() == 0 || d.serviceNames.Has(svc.Name)) &&
		len(svc.Spec.Selector) != 0 && labels.SelectorFromSet(svc.Spec.Selector).Matches(labels.Set(d.pod.Labels))
}

// affectedBy reports whether a change to the Service named name, now svc (nil when deleted),
// can change the discovered endpoints.
func (d *listenerServiceDiscovery) affectedBy(name string, svc *core.Service) bool {
	if d == nil {
		return false
	}
	if d.selectsController(svc) {
		return true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.candidates.Has(name)
}

// listServices returns the Services in the controller namespace that select the controller pods.
func (d *listenerServiceDiscovery) listServices(informers *nsregistry.Registry[namespacedInformer]) []listenerService {
	if d.pod == nil {
		return nil
	}
	var result []listenerService
	names := sets.New[string]()
	watched := informers.WithInformer(d.pod.Namespace, func(nsi *namespacedInformer) {
		if nsi.svcLister == nil {
			return
		}
		for _, obj := range nsi.svcLister.List() {
			svc := obj.(*core.Service)
			if !d.selectsController(svc) {
				continue
			}
			names.Insert(svc.Name)
			ls := listenerService{svc: svc}
			for _, ingress := range svc.Status.LoadBalancer.Ingress {
				if ingress.IP != "" {
					ls.addresses = append(ls.addresses, ingress.IP)
				} else if ingress.Hostname != "" {
					ls.addresses = append(ls.addresses, ingress.Hostname)
				}
			}
			if len(ls.addresses) == 0 {
				continue
			}
			if nsi.endpointSliceLister.Store != nil {
				// GetServiceEndpointSlices only errors when the Service has no EndpointSlices.
				ls.endpointSlices, _ = nsi.endpointSliceLister.GetServiceEndpointSlices(svc)
			}
			result = append(result, ls)
		}
	})
	d.mu.Lock()
	d.candidates = names
	warn := !watched && !d.warnedUnwatched
	d.warnedUnwatched = !watched
	d.mu.Unlock()
	if warn {
		nl.Warnf(d.logger, "-discover-listener-services is set, but the controller namespace %q is not watched; no listener Services will be discovered", d.pod.Namespace)
	}
	return result
}

func (d *listenerServiceDiscovery) bindings(vs *conf.VirtualServer) []listenerBinding {
	vsc := &VirtualServerConfiguration{VirtualServer: vs}
	if vs.Spec.Listener == nil {
		// VirtualServer templates use these defaults independently of the Ingress listener flags.
		vsc.HTTPPort, vsc.HTTPSPort = 80, 443
	} else {
		d.configuration.lock.RLock()
		d.configuration.buildListenersForVSConfiguration(vsc)
		d.configuration.lock.RUnlock()
	}
	var result []listenerBinding
	if vsc.HTTPPort > 0 {
		result = append(result, listenerBinding{vsc.HTTPPort, vsc.HTTPIPv4, vsc.HTTPIPv6})
	}
	if vsc.HTTPSPort > 0 && !d.configuration.isTLSPassthroughEnabled && vs.Spec.TLS != nil &&
		(vs.Spec.TLS.Secret != "" || d.wildcardTLS) {
		result = append(result, listenerBinding{vsc.HTTPSPort, vsc.HTTPSIPv4, vsc.HTTPSIPv6})
	}
	return result
}

// servicePorts returns the public ports of ls that reach one of the bindings.
func (d *listenerServiceDiscovery) servicePorts(ls listenerService, bindings []listenerBinding) []int32 {
	var ports []int32
	for _, port := range ls.svc.Spec.Ports {
		if port.Protocol != "" && port.Protocol != core.ProtocolTCP {
			continue
		}
		for _, binding := range bindings {
			if binding.matches(ls.svc, port, ls.endpointSlices, d.configuration.isIPV6Disabled) {
				ports = append(ports, port.Port)
				break
			}
		}
	}
	return ports
}

func (b listenerBinding) needsEndpoint(port core.ServicePort) bool {
	return port.TargetPort.Type == intstr.String || b.ipv4 != "" || b.ipv6 != ""
}

func (b listenerBinding) matches(svc *core.Service, port core.ServicePort, endpointSlices []discovery.EndpointSlice, disableIPv6 bool) bool {
	if port.TargetPort.Type == intstr.Int {
		targetPort := port.TargetPort.IntVal
		if targetPort == 0 {
			targetPort = port.Port
		}
		if int(targetPort) != b.port {
			return false
		}
		if !b.needsEndpoint(port) {
			return !disableIPv6 || !slices.Equal(svc.Spec.IPFamilies, []core.IPFamily{core.IPv6Protocol})
		}
	}

	for _, slice := range endpointSlices {
		if disableIPv6 && slice.AddressType == discovery.AddressTypeIPv6 {
			continue
		}
		if b.matchesSlice(port.Name, slice) {
			return true
		}
	}
	return false
}

func (b listenerBinding) matchesSlice(name string, slice discovery.EndpointSlice) bool {
	bindIP := b.ipv4
	if slice.AddressType == discovery.AddressTypeIPv6 {
		bindIP = b.ipv6
	}
	for _, port := range slice.Ports {
		if ptr.Deref(port.Name, "") != name || int(ptr.Deref(port.Port, 0)) != b.port ||
			ptr.Deref(port.Protocol, core.ProtocolTCP) != core.ProtocolTCP {
			continue
		}
		for _, endpoint := range slice.Endpoints {
			for _, address := range endpoint.Addresses {
				if ip := net.ParseIP(bindIP); bindIP == "" || ip.IsUnspecified() || ip.Equal(net.ParseIP(address)) {
					return true
				}
			}
		}
	}
	return false
}

func (su *statusUpdater) usesListenerDiscovery() bool {
	return su.listenerDiscovery != nil && su.externalStatusAddress == ""
}

func (su *statusUpdater) virtualServerEndpoints(vs *conf.VirtualServer) []conf.ExternalEndpoint {
	if !su.usesListenerDiscovery() {
		return su.externalEndpoints
	}
	return su.discoveredEndpoints([]*conf.VirtualServer{vs}, su.listenerDiscovery.listServices(su.namespacedInformers))
}

func (su *statusUpdater) virtualServerRouteEndpoints(vsr *conf.VirtualServerRoute) []conf.ExternalEndpoint {
	if !su.usesListenerDiscovery() {
		return su.externalEndpoints
	}
	return su.discoveredEndpoints(su.listenerDiscovery.configuration.GetVirtualServersForVirtualServerRoute(vsr),
		su.listenerDiscovery.listServices(su.namespacedInformers))
}

func (su *statusUpdater) discoveredEndpoints(virtualServers []*conf.VirtualServer, services []listenerService) []conf.ExternalEndpoint {
	d := su.listenerDiscovery
	var bindings []listenerBinding
	for _, vs := range virtualServers {
		bindings = append(bindings, d.bindings(vs)...)
	}
	if len(bindings) == 0 {
		return nil
	}

	addressPorts := make(map[string]sets.Set[int32])
	for _, ls := range services {
		ports := d.servicePorts(ls, bindings)
		if len(ports) == 0 {
			continue
		}
		for _, address := range ls.addresses {
			if addressPorts[address] == nil {
				addressPorts[address] = sets.New[int32]()
			}
			addressPorts[address].Insert(ports...)
		}
	}

	var result []conf.ExternalEndpoint
	for _, address := range slices.Sorted(maps.Keys(addressPorts)) {
		portNumbers := sets.List(addressPorts[address])
		ports := make([]string, 0, len(portNumbers))
		for _, port := range portNumbers {
			ports = append(ports, strconv.Itoa(int(port)))
		}
		endpoint := conf.ExternalEndpoint{Ports: "[" + strings.Join(ports, ",") + "]"}
		if net.ParseIP(address) != nil {
			endpoint.IP = address
		} else {
			endpoint.Hostname = address
		}
		result = append(result, endpoint)
	}
	return result
}

// updateDiscoveredEndpoints writes discovered endpoints to every VirtualServer and VirtualServerRoute,
// listing the controller Services once for the whole pass.
func (su *statusUpdater) updateDiscoveredEndpoints(resources []Resource) error {
	if !su.usesListenerDiscovery() {
		return su.UpdateExternalEndpointsForResources(resources)
	}
	services := su.listenerDiscovery.listServices(su.namespacedInformers)
	vsEndpoints := func(vs *conf.VirtualServer) []conf.ExternalEndpoint {
		return su.discoveredEndpoints([]*conf.VirtualServer{vs}, services)
	}
	vsrEndpoints := func(vsr *conf.VirtualServerRoute) []conf.ExternalEndpoint {
		return su.discoveredEndpoints(su.listenerDiscovery.configuration.GetVirtualServersForVirtualServerRoute(vsr), services)
	}
	failed := false
	for _, r := range resources {
		vsc, ok := r.(*VirtualServerConfiguration)
		if !ok {
			continue
		}
		if err := su.setVirtualServerExternalEndpoints(vsc.VirtualServer, vsEndpoints); err != nil {
			failed = true
		}
		for _, vsr := range vsc.VirtualServerRoutes {
			if err := su.setVirtualServerRouteExternalEndpoints(vsr, vsrEndpoints); err != nil {
				failed = true
			}
		}
	}
	if failed {
		return fmt.Errorf("not all Resources updated")
	}
	return nil
}

// refreshDiscoveredEndpoints updates VirtualServer endpoints after a change in the controller namespace.
func (lbc *LoadBalancerController) refreshDiscoveredEndpoints(namespace string) error {
	if lbc.statusUpdater == nil || lbc.statusUpdater.listenerDiscovery == nil ||
		namespace != lbc.statusUpdater.namespace || !lbc.isNginxReady || !lbc.reportCustomResourceStatusEnabled() {
		return nil
	}
	servers := lbc.configuration.GetResourcesWithFilter(resourceFilter{VirtualServers: true})
	return lbc.statusUpdater.updateDiscoveredEndpoints(servers)
}

// refreshDiscoveredEndpointsForService refreshes endpoints when the Service named name, now svc (nil when
// deleted), can change them.
func (lbc *LoadBalancerController) refreshDiscoveredEndpointsForService(namespace, name string, svc *core.Service) error {
	if lbc.statusUpdater == nil || namespace != lbc.statusUpdater.namespace ||
		!lbc.statusUpdater.listenerDiscovery.affectedBy(name, svc) {
		return nil
	}
	return lbc.refreshDiscoveredEndpoints(namespace)
}
