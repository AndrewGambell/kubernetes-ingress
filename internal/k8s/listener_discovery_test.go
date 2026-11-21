package k8s

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"testing"

	nic_glog "github.com/nginx/kubernetes-ingress/internal/logger/glog"
	"github.com/nginx/kubernetes-ingress/internal/logger/levels"
	conf "github.com/nginx/kubernetes-ingress/pkg/apis/configuration/v1"
	fake_v1 "github.com/nginx/kubernetes-ingress/pkg/client/clientset/versioned/fake"
	core "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/cache"
)

const discoveryNamespace = "ingress-system"

func discoveryLogger() *slog.Logger {
	return slog.New(nic_glog.New(io.Discard, &nic_glog.Options{Level: levels.LevelInfo}))
}

func discoveryPod() *core.Pod {
	return &core.Pod{ObjectMeta: meta.ObjectMeta{
		Namespace: discoveryNamespace,
		Labels:    map[string]string{"component": "controller"},
	}}
}

func listenerTestService(ns, name, ip, component string, http, https int32) *core.Service {
	return &core.Service{
		ObjectMeta: meta.ObjectMeta{Namespace: ns, Name: name},
		Spec: core.ServiceSpec{
			Type:     core.ServiceTypeLoadBalancer,
			Selector: map[string]string{"component": component},
			Ports: []core.ServicePort{
				{Port: 80, TargetPort: intstr.FromInt32(http)},
				{Port: 443, TargetPort: intstr.FromInt32(https)},
			},
		},
		Status: core.ServiceStatus{LoadBalancer: core.LoadBalancerStatus{
			Ingress: []core.LoadBalancerIngress{{IP: ip}},
		}},
	}
}

func listenerTestConfiguration() *Configuration {
	return &Configuration{
		globalConfiguration: &conf.GlobalConfiguration{},
		listenerMap: map[string]conf.Listener{
			"global-public-http":  {Name: "global-public-http", Port: 10080, Protocol: conf.HTTPProtocol},
			"global-public-https": {Name: "global-public-https", Port: 10443, Protocol: conf.HTTPProtocol, Ssl: true},
		},
		vsrToVSConfigs: make(map[string][]*conf.VirtualServer),
	}
}

func listenerTestVirtualServer() *conf.VirtualServer {
	return &conf.VirtualServer{
		ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "cafe"},
		Spec: conf.VirtualServerSpec{
			Listener: &conf.VirtualServerListener{HTTP: "global-public-http", HTTPS: "global-public-https"},
			TLS:      &conf.TLS{Secret: "tls"},
		},
	}
}

func newStore(t *testing.T, objs ...interface{}) cache.Store {
	t.Helper()
	store := cache.NewStore(cache.MetaNamespaceKeyFunc)
	for _, obj := range objs {
		if err := store.Add(obj); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

func TestDiscoverListenerServices(t *testing.T) {
	t.Parallel()
	public := listenerTestService(discoveryNamespace, "global-public", "192.0.2.10", "controller", 10080, 10443)
	alias := listenerTestService(discoveryNamespace, "also-public", "192.0.2.20", "controller", 20080, 10443)
	unrelated := listenerTestService(discoveryNamespace, "other-listener", "192.0.2.2", "controller", 20080, 20443)
	wrongPod := listenerTestService(discoveryNamespace, "unrelated", "192.0.2.3", "app", 10080, 10443)
	wrongNamespace := listenerTestService("unwatched", "public", "192.0.2.4", "controller", 10080, 10443)
	store := newStore(t, public, alias, unrelated, wrongPod, wrongNamespace)
	su := statusUpdater{
		listenerDiscovery: newListenerServiceDiscovery(discoveryLogger(), discoveryPod(), listenerTestConfiguration(), false, nil),
		namespacedInformers: registryFrom(map[string]*namespacedInformer{
			"": {svcLister: store},
		}),
	}
	vs := listenerTestVirtualServer()
	want := []conf.ExternalEndpoint{{IP: "192.0.2.10", Ports: "[80,443]"}, {IP: "192.0.2.20", Ports: "[443]"}}
	if got := su.virtualServerEndpoints(vs); !reflect.DeepEqual(got, want) {
		t.Fatalf("endpoints = %#v, want %#v", got, want)
	}
	if err := store.Delete(alias); err != nil {
		t.Fatal(err)
	}
	if got := su.virtualServerEndpoints(vs); !reflect.DeepEqual(got, want[:1]) {
		t.Fatalf("endpoints after Service removal = %#v, want %#v", got, want[:1])
	}
	su.namespacedInformers.Remove("")
	if got := su.virtualServerEndpoints(vs); len(got) != 0 {
		t.Fatalf("unwatched controller namespace contributed endpoints: %#v", got)
	}
}

func TestListenerDiscoveryRejectsUntrustedAddresses(t *testing.T) {
	t.Parallel()
	public := listenerTestService(discoveryNamespace, "global-public", "192.0.2.10", "controller", 10080, 10443)
	// externalIPs are set by whoever writes the Service spec, so they must never be reported.
	public.Spec.ExternalIPs = []string{"198.51.100.66"}
	clusterIP := listenerTestService(discoveryNamespace, "cluster-ip", "", "controller", 10080, 10443)
	clusterIP.Spec.Type = core.ServiceTypeClusterIP
	clusterIP.Spec.ExternalIPs = []string{"198.51.100.67"}
	clusterIP.Status = core.ServiceStatus{}
	nodePort := listenerTestService(discoveryNamespace, "node-port", "198.51.100.68", "controller", 10080, 10443)
	nodePort.Spec.Type = core.ServiceTypeNodePort
	externalName := listenerTestService(discoveryNamespace, "external-name", "", "controller", 10080, 10443)
	externalName.Spec.Type = core.ServiceTypeExternalName
	externalName.Spec.ExternalName = "attacker.example.com"
	su := statusUpdater{
		listenerDiscovery: newListenerServiceDiscovery(discoveryLogger(), discoveryPod(), listenerTestConfiguration(), false, nil),
		namespacedInformers: registryFrom(map[string]*namespacedInformer{
			"": {svcLister: newStore(t, public, clusterIP, nodePort, externalName)},
		}),
	}
	want := []conf.ExternalEndpoint{{IP: "192.0.2.10", Ports: "[80,443]"}}
	if got := su.virtualServerEndpoints(listenerTestVirtualServer()); !reflect.DeepEqual(got, want) {
		t.Fatalf("endpoints = %#v, want %#v", got, want)
	}
}

func TestListenerDiscoveryServiceNameAllowlist(t *testing.T) {
	t.Parallel()
	public := listenerTestService(discoveryNamespace, "global-public", "192.0.2.10", "controller", 10080, 10443)
	rogue := listenerTestService(discoveryNamespace, "rogue", "198.51.100.66", "controller", 10080, 10443)
	d := newListenerServiceDiscovery(discoveryLogger(), discoveryPod(), listenerTestConfiguration(), false, []string{"global-public"})
	su := statusUpdater{
		listenerDiscovery: d,
		namespacedInformers: registryFrom(map[string]*namespacedInformer{
			"": {svcLister: newStore(t, public, rogue)},
		}),
	}
	want := []conf.ExternalEndpoint{{IP: "192.0.2.10", Ports: "[80,443]"}}
	if got := su.virtualServerEndpoints(listenerTestVirtualServer()); !reflect.DeepEqual(got, want) {
		t.Fatalf("endpoints = %#v, want %#v", got, want)
	}
	if d.affectedBy("rogue", rogue) {
		t.Error("a Service outside the allowlist should not trigger a refresh")
	}
}

func TestListenerDiscoveryHTTPSListener(t *testing.T) {
	t.Parallel()
	public := listenerTestService(discoveryNamespace, "global-public", "192.0.2.10", "controller", 10080, 10443)
	tests := []struct {
		name        string
		tls         *conf.TLS
		wildcard    bool
		passthrough bool
		want        string
	}{
		{name: "TLS secret", tls: &conf.TLS{Secret: "tls"}, want: "[80,443]"},
		{name: "no TLS", want: "[80]"},
		{name: "TLS without secret", tls: &conf.TLS{}, want: "[80]"},
		{name: "TLS without secret and wildcard secret", tls: &conf.TLS{}, wildcard: true, want: "[80,443]"},
		{name: "TLS passthrough", tls: &conf.TLS{Secret: "tls"}, passthrough: true, want: "[80]"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			configuration := listenerTestConfiguration()
			configuration.isTLSPassthroughEnabled = test.passthrough
			su := statusUpdater{
				listenerDiscovery: newListenerServiceDiscovery(discoveryLogger(), discoveryPod(), configuration, test.wildcard, nil),
				namespacedInformers: registryFrom(map[string]*namespacedInformer{
					"": {svcLister: newStore(t, public)},
				}),
			}
			vs := listenerTestVirtualServer()
			vs.Spec.TLS = test.tls
			want := []conf.ExternalEndpoint{{IP: "192.0.2.10", Ports: test.want}}
			if got := su.virtualServerEndpoints(vs); !reflect.DeepEqual(got, want) {
				t.Fatalf("endpoints = %#v, want %#v", got, want)
			}
		})
	}
}

func TestListenerDiscoveryMatchesBackendBind(t *testing.T) {
	t.Parallel()
	name, port := "https", int32(8443)
	svc := &core.Service{}
	svcPort := core.ServicePort{Name: name, Port: 443, TargetPort: intstr.FromString("nginx-https")}
	slices := []discovery.EndpointSlice{{
		AddressType: discovery.AddressTypeIPv4,
		Ports:       []discovery.EndpointPort{{Name: &name, Port: &port}},
		Endpoints:   []discovery.Endpoint{{Addresses: []string{"192.0.2.10"}}},
	}}
	matching := listenerBinding{port: 8443, ipv4: "192.0.2.10"}
	other := listenerBinding{port: 8443, ipv4: "192.0.2.11"}
	if !matching.matches(svc, svcPort, slices, false) {
		t.Fatal("named port and backend IP should match the listener")
	}
	if other.matches(svc, svcPort, slices, false) {
		t.Fatal("the same port on a different bind address should not match")
	}
	svcPort.TargetPort = intstr.FromInt32(9443)
	if matching.matches(svc, svcPort, slices, false) {
		t.Fatal("a stale EndpointSlice should not override a numeric targetPort")
	}
}

func TestListenerDiscoveryAffectedBy(t *testing.T) {
	t.Parallel()
	public := listenerTestService(discoveryNamespace, "global-public", "192.0.2.10", "controller", 10080, 10443)
	other := listenerTestService(discoveryNamespace, "app", "192.0.2.11", "app", 8080, 8443)
	store := newStore(t, public, other)
	d := newListenerServiceDiscovery(discoveryLogger(), discoveryPod(), listenerTestConfiguration(), false, nil)
	informers := registryFrom(map[string]*namespacedInformer{"": {svcLister: store}})

	if !d.affectedBy(public.Name, public) {
		t.Error("a Service that selects the controller should trigger a refresh")
	}
	if d.affectedBy(other.Name, other) {
		t.Error("a Service that does not select the controller should not trigger a refresh")
	}
	if d.affectedBy(public.Name, nil) {
		t.Error("an unknown deleted Service should not trigger a refresh")
	}
	d.listServices(informers)
	if !d.affectedBy(public.Name, nil) {
		t.Error("deleting a previously discovered Service should trigger a refresh")
	}
	changed := public.DeepCopy()
	changed.Spec.Selector = map[string]string{"component": "other"}
	if !d.affectedBy(changed.Name, changed) {
		t.Error("a discovered Service that stops selecting the controller should trigger a refresh")
	}
}

func newDiscoveryStatusUpdater(t *testing.T, vs *conf.VirtualServer, vsr *conf.VirtualServerRoute, services ...interface{}) (*statusUpdater, *fake_v1.Clientset, cache.Store, cache.Store) {
	t.Helper()
	vsStore := newStore(t)
	vsrStore := newStore(t)
	vsList := &conf.VirtualServerList{}
	vsrList := &conf.VirtualServerRouteList{}
	if vs != nil {
		if err := vsStore.Add(vs); err != nil {
			t.Fatal(err)
		}
		vsList.Items = append(vsList.Items, *vs)
	}
	if vsr != nil {
		if err := vsrStore.Add(vsr); err != nil {
			t.Fatal(err)
		}
		vsrList.Items = append(vsrList.Items, *vsr)
	}
	client := fake_v1.NewSimpleClientset(vsList, vsrList)
	client.ClearActions()
	configuration := listenerTestConfiguration()
	if vs != nil && vsr != nil {
		configuration.vsrToVSConfigs[vsr.Namespace+"/"+vsr.Name] = []*conf.VirtualServer{vs}
	}
	svcStore := newStore(t, services...)
	su := &statusUpdater{
		listenerDiscovery: newListenerServiceDiscovery(discoveryLogger(), discoveryPod(), configuration, false, nil),
		namespace:         discoveryNamespace,
		confClient:        client,
		keyFunc:           cache.MetaNamespaceKeyFunc,
		logger:            discoveryLogger(),
		namespacedInformers: registryFrom(map[string]*namespacedInformer{
			"": {svcLister: svcStore, virtualServerLister: vsStore, virtualServerRouteLister: vsrStore},
		}),
	}
	return su, client, vsStore, vsrStore
}

func countUpdateStatus(client *fake_v1.Clientset) int {
	n := 0
	for _, action := range client.Actions() {
		if action.GetVerb() == "update" && action.GetSubresource() == "status" {
			n++
		}
	}
	return n
}

func TestUpdateVirtualServerStatusWithListenerDiscovery(t *testing.T) {
	t.Parallel()
	vs := listenerTestVirtualServer()
	public := listenerTestService(discoveryNamespace, "global-public", "192.0.2.10", "controller", 10080, 10443)
	su, client, vsStore, _ := newDiscoveryStatusUpdater(t, vs, nil, public)

	if err := su.UpdateVirtualServerStatus(vs, "Valid", "AddedOrUpdated", "ok"); err != nil {
		t.Fatal(err)
	}
	got, err := client.K8sV1().VirtualServers(vs.Namespace).Get(context.TODO(), vs.Name, meta.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := []conf.ExternalEndpoint{{IP: "192.0.2.10", Ports: "[80,443]"}}
	if !reflect.DeepEqual(got.Status.ExternalEndpoints, want) {
		t.Fatalf("status endpoints = %#v, want %#v", got.Status.ExternalEndpoints, want)
	}

	// An unchanged status must not be written again.
	if err := vsStore.Update(got); err != nil {
		t.Fatal(err)
	}
	client.ClearActions()
	if err := su.UpdateVirtualServerStatus(vs, "Valid", "AddedOrUpdated", "ok"); err != nil {
		t.Fatal(err)
	}
	if err := su.updateVirtualServerExternalEndpoints(vs); err != nil {
		t.Fatal(err)
	}
	if n := countUpdateStatus(client); n != 0 {
		t.Fatalf("unchanged status caused %d UpdateStatus calls", n)
	}
}

func TestListenerDiscoveryExternalStatusAddressPrecedence(t *testing.T) {
	t.Parallel()
	vs := listenerTestVirtualServer()
	public := listenerTestService(discoveryNamespace, "global-public", "192.0.2.10", "controller", 10080, 10443)
	su, _, _, _ := newDiscoveryStatusUpdater(t, vs, nil, public)

	su.SaveStatusFromExternalStatus("203.0.113.5")
	want := []conf.ExternalEndpoint{{IP: "203.0.113.5"}}
	if got := su.virtualServerEndpoints(vs); !reflect.DeepEqual(got, want) {
		t.Fatalf("with external-status-address, endpoints = %#v, want %#v", got, want)
	}

	su.SaveStatusFromExternalStatus("")
	want = []conf.ExternalEndpoint{{IP: "192.0.2.10", Ports: "[80,443]"}}
	if got := su.virtualServerEndpoints(vs); !reflect.DeepEqual(got, want) {
		t.Fatalf("after removing external-status-address, endpoints = %#v, want %#v", got, want)
	}
}

func TestSaveStatusFromExternalStatusWithoutFallbackClearsStatus(t *testing.T) {
	t.Parallel()
	su := statusUpdater{}
	su.SaveStatusFromExternalStatus("203.0.113.5")
	su.SaveStatusFromExternalStatus("")
	if !su.statusInitialized || len(su.status) != 0 {
		t.Fatalf("status = %#v (initialized %v), want an empty, initialized status", su.status, su.statusInitialized)
	}
	if su.externalEndpoints != nil {
		t.Fatalf("externalEndpoints = %#v, want nil", su.externalEndpoints)
	}
}

func TestUpdateVirtualServerRouteWithListenerDiscovery(t *testing.T) {
	t.Parallel()
	vs := listenerTestVirtualServer()
	vsr := &conf.VirtualServerRoute{ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "tea"}}
	public := listenerTestService(discoveryNamespace, "global-public", "192.0.2.10", "controller", 10080, 10443)
	su, client, _, vsrStore := newDiscoveryStatusUpdater(t, vs, vsr, public)

	if err := su.UpdateVirtualServerRouteReferencedBy(vsr, []*conf.VirtualServer{vs}); err != nil {
		t.Fatal(err)
	}
	got, err := client.K8sV1().VirtualServerRoutes(vsr.Namespace).Get(context.TODO(), vsr.Name, meta.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := []conf.ExternalEndpoint{{IP: "192.0.2.10", Ports: "[80,443]"}}
	if !reflect.DeepEqual(got.Status.ExternalEndpoints, want) || got.Status.ReferencedBy != "default/cafe" {
		t.Fatalf("status = %#v, want endpoints %#v referenced by default/cafe", got.Status, want)
	}

	if err := vsrStore.Update(got); err != nil {
		t.Fatal(err)
	}
	client.ClearActions()
	if err := su.UpdateVirtualServerRouteReferencedBy(vsr, []*conf.VirtualServer{vs}); err != nil {
		t.Fatal(err)
	}
	if err := su.retryUpdateVirtualServerRouteReferencedBy(vsr.Namespace, vsr.Name, "default/cafe"); err != nil {
		t.Fatal(err)
	}
	if n := countUpdateStatus(client); n != 0 {
		t.Fatalf("unchanged VirtualServerRoute status caused %d UpdateStatus calls", n)
	}
}

func TestUpdateDiscoveredEndpoints(t *testing.T) {
	t.Parallel()
	vs := listenerTestVirtualServer()
	vsr := &conf.VirtualServerRoute{ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: "tea"}}
	public := listenerTestService(discoveryNamespace, "global-public", "192.0.2.10", "controller", 10080, 10443)
	su, client, _, _ := newDiscoveryStatusUpdater(t, vs, vsr, public)

	resources := []Resource{&VirtualServerConfiguration{VirtualServer: vs, VirtualServerRoutes: []*conf.VirtualServerRoute{vsr}}}
	if err := su.updateDiscoveredEndpoints(resources); err != nil {
		t.Fatal(err)
	}
	want := []conf.ExternalEndpoint{{IP: "192.0.2.10", Ports: "[80,443]"}}
	gotVS, _ := client.K8sV1().VirtualServers(vs.Namespace).Get(context.TODO(), vs.Name, meta.GetOptions{})
	if !reflect.DeepEqual(gotVS.Status.ExternalEndpoints, want) {
		t.Errorf("VirtualServer endpoints = %#v, want %#v", gotVS.Status.ExternalEndpoints, want)
	}
	gotVSR, _ := client.K8sV1().VirtualServerRoutes(vsr.Namespace).Get(context.TODO(), vsr.Name, meta.GetOptions{})
	if !reflect.DeepEqual(gotVSR.Status.ExternalEndpoints, want) {
		t.Errorf("VirtualServerRoute endpoints = %#v, want %#v", gotVSR.Status.ExternalEndpoints, want)
	}
}

func TestRefreshDiscoveredEndpointsForService(t *testing.T) {
	t.Parallel()
	vs := listenerTestVirtualServer()
	public := listenerTestService(discoveryNamespace, "global-public", "192.0.2.10", "controller", 10080, 10443)
	other := listenerTestService(discoveryNamespace, "app", "192.0.2.11", "app", 10080, 10443)
	su, client, _, _ := newDiscoveryStatusUpdater(t, vs, nil, public, other)
	su.listenerDiscovery.configuration.hosts = map[string]Resource{"cafe.example.com": &VirtualServerConfiguration{VirtualServer: vs}}
	lbc := &LoadBalancerController{statusUpdater: su, configuration: su.listenerDiscovery.configuration, isNginxReady: true}

	if err := lbc.refreshDiscoveredEndpointsForService(discoveryNamespace, other.Name, other); err != nil {
		t.Fatal(err)
	}
	if err := lbc.refreshDiscoveredEndpointsForService("default", public.Name, public); err != nil {
		t.Fatal(err)
	}
	if n := countUpdateStatus(client); n != 0 {
		t.Fatalf("unrelated Service changes caused %d UpdateStatus calls", n)
	}

	if err := lbc.refreshDiscoveredEndpointsForService(discoveryNamespace, public.Name, public); err != nil {
		t.Fatal(err)
	}
	if n := countUpdateStatus(client); n != 1 {
		t.Fatalf("discovered Service change caused %d UpdateStatus calls, want 1", n)
	}
}
