package k8s

import (
	"net"
	"slices"
	"strconv"
	"strings"

	nl "github.com/nginx/kubernetes-ingress/internal/logger"
	conf "github.com/nginx/kubernetes-ingress/pkg/apis/configuration/v1"
	core "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const listenerServiceAnnotation = "nginx.org/listeners"

func serviceExposesListener(svc *core.Service, listener string) bool {
	for _, name := range strings.Split(svc.Annotations[listenerServiceAnnotation], ",") {
		if strings.TrimSpace(name) == listener {
			return true
		}
	}
	return false
}

func servicesForListener(listener string, services []*core.Service, defaultService string) []*core.Service {
	var matching []*core.Service
	var fallback *core.Service
	for _, svc := range services {
		if serviceExposesListener(svc, listener) {
			matching = append(matching, svc)
		}
		if svc.Name == defaultService && svc.Annotations[listenerServiceAnnotation] == "" {
			fallback = svc
		}
	}
	if len(matching) == 0 && fallback != nil {
		matching = append(matching, fallback)
	}
	return matching
}

func listenerServiceEndpoints(listener string, target int, services []*core.Service, defaultService string, endpointSlices func(*core.Service) []discovery.EndpointSlice) []conf.ExternalEndpoint {
	var endpoints []conf.ExternalEndpoint
	for _, svc := range servicesForListener(listener, services, defaultService) {
		var ports []string
		for _, port := range svc.Spec.Ports {
			if port.Protocol != "" && port.Protocol != core.ProtocolTCP {
				continue
			}
			if servicePortTargetsListener(svc, port, target, endpointSlices) {
				ports = append(ports, strconv.Itoa(int(port.Port)))
			}
		}
		if len(ports) == 0 {
			continue
		}
		for _, address := range getExternalServiceAddress(svc) {
			if address == "" {
				continue
			}
			ep := conf.ExternalEndpoint{Ports: "[" + strings.Join(ports, ",") + "]"}
			if net.ParseIP(address) != nil {
				ep.IP = address
			} else {
				ep.Hostname = address
			}
			endpoints = append(endpoints, ep)
		}
	}
	return mergeListenerEndpoints(endpoints)
}

func servicePortTargetsListener(svc *core.Service, port core.ServicePort, target int, endpointSlices func(*core.Service) []discovery.EndpointSlice) bool {
	if port.TargetPort.Type == intstr.Int {
		if port.TargetPort.IntVal == 0 {
			return int(port.Port) == target
		}
		return int(port.TargetPort.IntVal) == target
	}
	if endpointSlices == nil {
		return false
	}
	for _, slice := range endpointSlices(svc) {
		for _, resolved := range slice.Ports {
			if resolved.Name != nil && *resolved.Name == port.Name && resolved.Port != nil && int(*resolved.Port) == target &&
				(resolved.Protocol == nil || *resolved.Protocol == core.ProtocolTCP) {
				return true
			}
		}
	}
	return false
}

func mergeListenerEndpoints(endpoints []conf.ExternalEndpoint) []conf.ExternalEndpoint {
	type address struct{ ip, hostname string }
	portsByAddress := make(map[address][]int)
	for _, endpoint := range endpoints {
		key := address{endpoint.IP, endpoint.Hostname}
		portsByAddress[key] = append(portsByAddress[key], parseEndpointPorts(endpoint.Ports)...)
	}
	var result []conf.ExternalEndpoint
	for key, ports := range portsByAddress {
		slices.Sort(ports)
		var portStrings []string
		for _, port := range slices.Compact(ports) {
			portStrings = append(portStrings, strconv.Itoa(port))
		}
		ep := conf.ExternalEndpoint{IP: key.ip, Hostname: key.hostname}
		if len(portStrings) != 0 {
			ep.Ports = "[" + strings.Join(portStrings, ",") + "]"
		}
		result = append(result, ep)
	}
	slices.SortFunc(result, func(a, b conf.ExternalEndpoint) int {
		return strings.Compare(a.IP+a.Hostname, b.IP+b.Hostname)
	})
	return result
}

func parseEndpointPorts(value string) []int {
	var ports []int
	for _, item := range strings.Split(strings.Trim(value, "[]"), ",") {
		if port, err := strconv.Atoi(item); err == nil {
			ports = append(ports, port)
		}
	}
	return ports
}

func (su *statusUpdater) virtualServerEndpoints(vs *conf.VirtualServer) []conf.ExternalEndpoint {
	if su.configuration == nil || su.externalStatusAddress != "" || su.bigIPAddress != "" || vs.Spec.Listener == nil {
		return su.externalEndpoints
	}

	resolved := &VirtualServerConfiguration{VirtualServer: vs}
	su.configuration.lock.RLock()
	su.configuration.buildListenersForVSConfiguration(resolved)
	su.configuration.lock.RUnlock()

	var endpoints []conf.ExternalEndpoint
	su.namespacedInformers.WithInformer(su.namespace, func(nsi *namespacedInformer) {
		if nsi.svcLister == nil {
			return
		}
		var services []*core.Service
		for _, obj := range nsi.svcLister.List() {
			svc := obj.(*core.Service)
			if svc.Namespace == su.namespace {
				services = append(services, svc)
			}
		}
		endpointSlices := func(svc *core.Service) []discovery.EndpointSlice {
			if nsi.endpointSliceLister.Store == nil {
				return nil
			}
			items, _ := nsi.endpointSliceLister.GetServiceEndpointSlices(svc)
			return items
		}
		if resolved.HTTPPort > 0 {
			endpoints = append(endpoints, listenerServiceEndpoints(vs.Spec.Listener.HTTP, resolved.HTTPPort, services, su.externalServiceName, endpointSlices)...)
		}
		if resolved.HTTPSPort > 0 && vs.Spec.TLS != nil && !su.configuration.isTLSPassthroughEnabled {
			endpoints = append(endpoints, listenerServiceEndpoints(vs.Spec.Listener.HTTPS, resolved.HTTPSPort, services, su.externalServiceName, endpointSlices)...)
		}
	})
	return mergeListenerEndpoints(endpoints)
}

func (su *statusUpdater) virtualServerRouteEndpoints(vsr *conf.VirtualServerRoute) []conf.ExternalEndpoint {
	if su.configuration == nil || su.externalStatusAddress != "" || su.bigIPAddress != "" {
		return su.externalEndpoints
	}
	parents := su.configuration.GetVirtualServersForVirtualServerRoute(vsr)
	if len(parents) == 0 {
		return su.externalEndpoints
	}
	if len(parents) == 1 {
		return su.virtualServerEndpoints(parents[0])
	}
	var endpoints []conf.ExternalEndpoint
	for _, vs := range parents {
		endpoints = append(endpoints, su.virtualServerEndpoints(vs)...)
	}
	return mergeListenerEndpoints(endpoints)
}

func (lbc *LoadBalancerController) refreshCustomListenerEndpoints() {
	if !lbc.areCustomResourcesEnabled || !lbc.reportCustomResourceStatusEnabled() || lbc.statusUpdater.configuration == nil {
		return
	}
	for _, resource := range lbc.configuration.GetResourcesWithFilter(resourceFilter{VirtualServers: true}) {
		vs := resource.(*VirtualServerConfiguration).VirtualServer
		if vs.Spec.Listener != nil {
			if err := lbc.statusUpdater.UpdateExternalEndpointsForResource(resource); err != nil {
				nl.Errorf(lbc.Logger, "updating custom listener endpoints for %s/%s: %v", vs.Namespace, vs.Name, err)
			}
		}
	}
}
