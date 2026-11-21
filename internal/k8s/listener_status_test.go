package k8s

import (
	"context"
	"reflect"
	"testing"

	nl "github.com/nginx/kubernetes-ingress/internal/logger"
	conf "github.com/nginx/kubernetes-ingress/pkg/apis/configuration/v1"
	fake "github.com/nginx/kubernetes-ingress/pkg/client/clientset/versioned/fake"
	core "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/cache"
)

func TestListenerServiceEndpoints(t *testing.T) {
	service := func(name, listener, ip string, port, target int32) *core.Service {
		return &core.Service{
			ObjectMeta: meta.ObjectMeta{Name: name, Annotations: map[string]string{listenerServiceAnnotation: listener}},
			Spec:       core.ServiceSpec{ExternalIPs: []string{ip}, Ports: []core.ServicePort{{Port: port, TargetPort: intstr.FromInt32(target)}}},
		}
	}
	first := service("first", "public", "192.0.2.1", 443, 8443)
	second := service("second", "public", "192.0.2.2", 9443, 8443)
	unrelated := service("private", "private", "192.0.2.3", 443, 8443)
	wrongPort := service("wrong", "public", "192.0.2.4", 443, 8444)
	got := listenerServiceEndpoints("public", 8443, []*core.Service{second, unrelated, wrongPort, first}, "private", nil)
	want := []conf.ExternalEndpoint{{IP: "192.0.2.1", Ports: "[443]"}, {IP: "192.0.2.2", Ports: "[9443]"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("endpoints = %#v, want %#v", got, want)
	}
	got = listenerServiceEndpoints("public", 8443, []*core.Service{second, unrelated, wrongPort}, "private", nil)
	if !reflect.DeepEqual(got, want[1:]) {
		t.Fatalf("after Service removal, endpoints = %#v, want %#v", got, want[1:])
	}
}

func TestListenerServiceEndpointsNamedPort(t *testing.T) {
	service := &core.Service{
		ObjectMeta: meta.ObjectMeta{Name: "controller"},
		Spec: core.ServiceSpec{
			ExternalIPs: []string{"2001:db8::1", "2001:db8::1"},
			Ports: []core.ServicePort{
				{Name: "https-public", Port: 9443, TargetPort: intstr.FromString("listener")},
				{Name: "https-alias", Port: 443, TargetPort: intstr.FromString("listener")},
			},
		},
	}
	port := int32(8443)
	getSlices := func(*core.Service) []discovery.EndpointSlice {
		return []discovery.EndpointSlice{{Ports: []discovery.EndpointPort{
			{Name: &service.Spec.Ports[0].Name, Port: &port},
			{Name: &service.Spec.Ports[1].Name, Port: &port},
		}}}
	}
	got := listenerServiceEndpoints("public", 8443, []*core.Service{service}, "controller", getSlices)
	want := []conf.ExternalEndpoint{{IP: "2001:db8::1", Ports: "[443,9443]"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("endpoints = %#v, want %#v", got, want)
	}
	if got := listenerServiceEndpoints("public", 8443, []*core.Service{service}, "controller", nil); len(got) != 0 {
		t.Fatalf("unresolved named port produced endpoints: %#v", got)
	}
}

func TestVirtualServerStatusUsesListenerServices(t *testing.T) {
	ctx := context.Background()
	nsi := &namespacedInformer{
		svcLister:           cache.NewStore(cache.MetaNamespaceKeyFunc),
		virtualServerLister: cache.NewStore(cache.MetaNamespaceKeyFunc),
	}
	listener := conf.Listener{Name: "public", Port: 8080, Protocol: conf.HTTPProtocol}
	vs := &conf.VirtualServer{
		ObjectMeta: meta.ObjectMeta{Namespace: "apps", Name: "app"},
		Spec:       conf.VirtualServerSpec{Listener: &conf.VirtualServerListener{HTTP: listener.Name}},
	}
	client := fake.NewSimpleClientset(vs)
	su := statusUpdater{
		namespace: "controller",
		configuration: &Configuration{
			globalConfiguration: &conf.GlobalConfiguration{},
			listenerMap:         map[string]conf.Listener{listener.Name: listener},
		},
		confClient:          client,
		namespacedInformers: registryFrom(map[string]*namespacedInformer{"": nsi}),
		logger:              nl.LoggerFromContext(ctx),
	}
	if err := nsi.virtualServerLister.Add(vs); err != nil {
		t.Fatal(err)
	}
	var services []*core.Service
	for _, item := range []struct{ ns, name, ip string }{
		{"controller", "first", "192.0.2.1"},
		{"controller", "second", "192.0.2.2"},
		{"other", "unrelated", "192.0.2.3"},
	} {
		svc := &core.Service{
			ObjectMeta: meta.ObjectMeta{Namespace: item.ns, Name: item.name, Annotations: map[string]string{listenerServiceAnnotation: listener.Name}},
			Spec:       core.ServiceSpec{ExternalIPs: []string{item.ip}, Ports: []core.ServicePort{{Port: 80, TargetPort: intstr.FromInt32(8080)}}},
		}
		if err := nsi.svcLister.Add(svc); err != nil {
			t.Fatal(err)
		}
		services = append(services, svc)
	}
	want := []conf.ExternalEndpoint{{IP: "192.0.2.1", Ports: "[80]"}, {IP: "192.0.2.2", Ports: "[80]"}}
	if err := su.UpdateVirtualServerStatus(vs, "Valid", "AddedOrUpdated", "configured"); err != nil {
		t.Fatal(err)
	}
	reported, err := client.K8sV1().VirtualServers(vs.Namespace).Get(ctx, vs.Name, meta.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reported.Status.ExternalEndpoints, want) {
		t.Fatalf("endpoints = %#v, want %#v", reported.Status.ExternalEndpoints, want)
	}
	if err := nsi.virtualServerLister.Update(reported); err != nil {
		t.Fatal(err)
	}
	if err := nsi.svcLister.Delete(services[0]); err != nil {
		t.Fatal(err)
	}
	if err := su.UpdateExternalEndpointsForResource(&VirtualServerConfiguration{VirtualServer: vs}); err != nil {
		t.Fatal(err)
	}
	reported, err = client.K8sV1().VirtualServers(vs.Namespace).Get(ctx, vs.Name, meta.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reported.Status.ExternalEndpoints, want[1:]) {
		t.Fatalf("after Service removal, endpoints = %#v, want %#v", reported.Status.ExternalEndpoints, want[1:])
	}
}
