package k8s

import (
	"slices"
	"testing"
	"time"

	conf_v1 "github.com/nginx/kubernetes-ingress/pkg/apis/configuration/v1"
	networking "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func listenerClaimIngress(name, http, https string) *networking.Ingress {
	ing := createTestIngress(name, "shared.example.com")
	if http != "" {
		ing.Annotations["nginx.org/listen-ports"] = http
	}
	if https != "" {
		ing.Annotations["nginx.org/listen-ports-ssl"] = https
		ing.Spec.TLS = []networking.IngressTLS{{Hosts: []string{"shared.example.com"}, SecretName: "tls"}}
	}
	return ing
}

func listenerClaimVS(name, http, https string) *conf_v1.VirtualServer {
	vs := createTestVirtualServerWithListeners(name, "shared.example.com", http, https)
	if https != "" {
		vs.Spec.TLS = &conf_v1.TLS{Secret: "tls"}
	}
	return vs
}

func assertListenerClaimResources(t *testing.T, c *Configuration, want ...string) {
	t.Helper()
	var got []string
	for _, r := range c.GetResources() {
		got = append(got, r.GetKeyWithKind())
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("active resources = %v, want %v", got, want)
	}
}

func TestListenerScopedHostOwnership(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		first, second metav1.Object
		want          []string
	}{
		{"virtualservers", listenerClaimVS("a", "http-a", "https-a"), listenerClaimVS("b", "http-b", "https-b"), []string{"VirtualServer/default/a", "VirtualServer/default/b"}},
		{"ingresses", listenerClaimIngress("a", "8080", "8443"), listenerClaimIngress("b", "9080", "9443"), []string{"Ingress/default/a", "Ingress/default/b"}},
		{"mixed kinds", listenerClaimIngress("a", "8080", "8443"), listenerClaimVS("b", "http-b", "https-b"), []string{"Ingress/default/a", "VirtualServer/default/b"}},
		{"mixed overlap", listenerClaimIngress("a", "8080", "8443"), listenerClaimVS("b", "http-a", "https-b"), []string{"Ingress/default/a"}},
		{"partial HTTP overlap", listenerClaimVS("a", "http-a", "https-a"), listenerClaimVS("b", "http-a", "https-b"), []string{"VirtualServer/default/a"}},
		{"partial HTTPS overlap", listenerClaimIngress("a", "8080", "8443"), listenerClaimIngress("b", "9080", "8443"), []string{"Ingress/default/a"}},
		{"Ingress port lists overlap", listenerClaimIngress("a", "8080,9080", ""), listenerClaimIngress("b", "9080,10080", ""), []string{"Ingress/default/a"}},
		{"default and custom", createTestVirtualServer("a", "shared.example.com"), listenerClaimVS("b", "http-a", ""), []string{"VirtualServer/default/a", "VirtualServer/default/b"}},
		{"HTTP-only default does not claim HTTPS", createTestVirtualServer("a", "shared.example.com"), listenerClaimIngress("b", "9080", "443"), []string{"VirtualServer/default/a", "Ingress/default/b"}},
		{"default Ingress overlap", listenerClaimIngress("a", "", ""), createTestVirtualServer("b", "shared.example.com"), []string{"Ingress/default/a"}},
		{"invalid Ingress ports fall back", listenerClaimIngress("a", "invalid", ""), createTestVirtualServer("b", "shared.example.com"), []string{"Ingress/default/a"}},
		{"distinct IPs same port", listenerClaimVS("a", "ip-a", ""), listenerClaimVS("b", "ip-b", ""), []string{"VirtualServer/default/a", "VirtualServer/default/b"}},
		{"wildcard IP overlaps specific", listenerClaimVS("a", "http-a", ""), listenerClaimVS("b", "ip-a", ""), []string{"VirtualServer/default/a"}},
		{"listener aliases overlap", listenerClaimVS("a", "http-a", ""), listenerClaimVS("b", "alias-a", ""), []string{"VirtualServer/default/a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := createTestConfiguration()
			c.isTLSPassthroughEnabled = false
			c.isIPV6Disabled = true
			c.globalConfiguration = createTestGlobalConfiguration([]conf_v1.Listener{
				{Name: "http-a", Port: 8080, Protocol: "HTTP"},
				{Name: "http-b", Port: 9080, Protocol: "HTTP"},
				{Name: "https-a", Port: 8443, Protocol: "HTTP", Ssl: true},
				{Name: "https-b", Port: 9443, Protocol: "HTTP", Ssl: true},
				{Name: "alias-a", Port: 8080, Protocol: "HTTP"},
				{Name: "ip-a", Port: 8080, Protocol: "HTTP", IPv4: "192.0.2.1"},
				{Name: "ip-b", Port: 8080, Protocol: "HTTP", IPv4: "192.0.2.2"},
			})
			c.setGlobalConfigListenerMap()
			// Name order deliberately disagrees with creation order in the second pass.
			for _, reverse := range []bool{false, true} {
				c.ingresses = make(map[string]*networking.Ingress)
				c.virtualServers = make(map[string]*conf_v1.VirtualServer)
				older := metav1.NewTime(time.Unix(1, 0))
				newer := metav1.NewTime(time.Unix(2, 0))
				tt.first.SetCreationTimestamp(older)
				tt.second.SetCreationTimestamp(newer)
				if reverse {
					tt.first.SetName("z")
				}
				for _, obj := range []metav1.Object{tt.second, tt.first} {
					switch r := obj.(type) {
					case *networking.Ingress:
						c.ingresses["default/"+r.Name] = r
					case *conf_v1.VirtualServer:
						c.virtualServers["default/"+r.Name] = r
					}
				}
				c.rebuildHosts()
				want := slices.Clone(tt.want)
				if reverse {
					for i, key := range want {
						if key[len(key)-1] == 'a' {
							want[i] = key[:len(key)-1] + "z"
						}
					}
				}
				assertListenerClaimResources(t, c, want...)
			}
		})
	}
}

func TestListenerScopedVirtualServerWithoutRenderedListeners(t *testing.T) {
	t.Parallel()
	c := createTestConfiguration()
	c.globalConfiguration = createTestGlobalConfiguration([]conf_v1.Listener{
		{Name: "https-a", Port: 8443, Protocol: "HTTP", Ssl: true},
	})
	c.setGlobalConfigListenerMap()
	a := listenerClaimVS("a", "", "https-a")
	a.Spec.TLS = nil
	b := createTestVirtualServer("b", "shared.example.com")
	a.CreationTimestamp = metav1.NewTime(time.Unix(1, 0))
	b.CreationTimestamp = metav1.NewTime(time.Unix(2, 0))
	c.virtualServers["default/a"], c.virtualServers["default/b"] = a, b
	c.rebuildHosts()
	// With no explicit listen directive, NGINX supplies an implicit listener.
	// Keep ownership conservative instead of assuming the unused HTTPS port.
	assertListenerClaimResources(t, c, "VirtualServer/default/a")
}

func TestListenerScopedIngressPromotion(t *testing.T) {
	t.Parallel()
	c := createTestConfiguration()
	a := listenerClaimIngress("a", "8080,9080", "")
	b := listenerClaimIngress("b", "9080", "")
	d := listenerClaimIngress("c", "10080", "")
	a.CreationTimestamp = metav1.NewTime(time.Unix(1, 0))
	b.CreationTimestamp = metav1.NewTime(time.Unix(2, 0))
	c.AddOrUpdateIngress(a)
	c.AddOrUpdateIngress(b)
	c.AddOrUpdateIngress(d)
	assertListenerClaimResources(t, c, "Ingress/default/a", "Ingress/default/c")
	changes, _ := c.DeleteIngress("default/a")
	assertListenerClaimResources(t, c, "Ingress/default/b", "Ingress/default/c")
	if len(changes) == 0 || changes[0].Op != Delete {
		t.Fatalf("expected delete before promotion, got %v", changes)
	}
}

func TestListenerScopedVirtualServerRoute(t *testing.T) {
	t.Parallel()
	c := createTestConfiguration()
	vsr := createTestVirtualServerRoute("routes", "default", "shared.example.com", "/")
	c.AddOrUpdateVirtualServerRoute(vsr)
	for _, name := range []string{"a", "b"} {
		vs := listenerClaimVS(name, "http-"+name, "")
		vs.Spec.Routes = []conf_v1.Route{{Path: "/", Route: "routes"}}
		c.virtualServers["default/"+name] = vs
	}
	c.globalConfiguration = createTestGlobalConfiguration([]conf_v1.Listener{
		{Name: "http-a", Port: 8080, Protocol: "HTTP"},
		{Name: "http-b", Port: 9080, Protocol: "HTTP"},
	})
	c.setGlobalConfigListenerMap()
	_, problems := c.rebuildHosts()
	for _, p := range problems {
		if p.Object == vsr {
			t.Fatalf("referenced route reported as orphan: %+v", p)
		}
	}
	if got := len(c.GetVirtualServersForVirtualServerRoute(vsr)); got != 2 {
		t.Fatalf("route has %d parents, want 2", got)
	}
	c.DeleteVirtualServer("default/a")
	if got := len(c.GetVirtualServersForVirtualServerRoute(vsr)); got != 1 {
		t.Fatalf("route has %d parents after delete, want 1", got)
	}
}

func TestListenerScopedUIDTieBreak(t *testing.T) {
	t.Parallel()
	c := createTestConfiguration()
	a := listenerClaimIngress("a", "8080", "")
	b := listenerClaimIngress("b", "8080", "")
	a.CreationTimestamp = metav1.NewTime(time.Unix(1, 0))
	b.CreationTimestamp = a.CreationTimestamp
	a.UID, b.UID = types.UID("a"), types.UID("b")
	c.AddOrUpdateIngress(a)
	c.AddOrUpdateIngress(b)
	// Preserve the existing metadata winner rule.
	assertListenerClaimResources(t, c, "Ingress/default/b")
}

func TestListenerScopedIngressUpdateAndPartialHosts(t *testing.T) {
	t.Parallel()
	c := createTestConfiguration()
	a := listenerClaimIngress("a", "8080", "")
	b := listenerClaimIngress("b", "9080", "")
	a.CreationTimestamp = metav1.NewTime(time.Unix(1, 0))
	b.CreationTimestamp = metav1.NewTime(time.Unix(2, 0))
	b.Spec.Rules = append(b.Spec.Rules, networking.IngressRule{Host: "other.example.com"})
	c.AddOrUpdateIngress(a)
	c.AddOrUpdateIngress(b)
	assertListenerClaimResources(t, c, "Ingress/default/a", "Ingress/default/b")
	b = b.DeepCopy()
	b.Annotations["nginx.org/listen-ports"] = "8080"
	b.Generation++
	c.AddOrUpdateIngress(b)
	for _, r := range c.GetResources() {
		if ing, ok := r.(*IngressConfiguration); ok && ing.Ingress.Name == "b" {
			if ing.ValidHosts["shared.example.com"] || !ing.ValidHosts["other.example.com"] {
				t.Fatalf("partial host ownership = %v", ing.ValidHosts)
			}
		}
	}
	c.DeleteIngress("default/a")
	for _, r := range c.GetResources() {
		if ing, ok := r.(*IngressConfiguration); ok && !ing.ValidHosts["shared.example.com"] {
			t.Fatal("shared host was not promoted after deleting the winner")
		}
	}
}

func TestListenerScopedNonTransitiveOverlap(t *testing.T) {
	t.Parallel()
	c := createTestConfiguration()
	// Name order encounters the middle-aged, overlapping claim first. Choosing
	// winners incrementally would discard c before discovering older z.
	for i, spec := range []struct{ name, ports string }{{"z", "8080"}, {"a", "8080,9080"}, {"c", "9080"}} {
		ing := listenerClaimIngress(spec.name, spec.ports, "")
		ing.CreationTimestamp = metav1.NewTime(time.Unix(int64(i+1), 0))
		c.AddOrUpdateIngress(ing)
	}
	assertListenerClaimResources(t, c, "Ingress/default/c", "Ingress/default/z")
}

func TestListenerScopedGlobalConfigurationUpdate(t *testing.T) {
	t.Parallel()
	c := createTestConfiguration()
	gc := createTestGlobalConfiguration([]conf_v1.Listener{
		{Name: "http-a", Port: 8080, Protocol: "HTTP"},
		{Name: "http-b", Port: 9080, Protocol: "HTTP"},
	})
	if _, _, err := c.AddOrUpdateGlobalConfiguration(gc); err != nil {
		t.Fatal(err)
	}
	a := listenerClaimVS("a", "http-a", "")
	b := listenerClaimVS("b", "http-b", "")
	c.AddOrUpdateVirtualServer(a)
	c.AddOrUpdateVirtualServer(b)
	assertListenerClaimResources(t, c, "VirtualServer/default/a", "VirtualServer/default/b")
	gc = gc.DeepCopy()
	gc.Spec.Listeners[1].Port = 10080
	if _, _, err := c.AddOrUpdateGlobalConfiguration(gc); err != nil {
		t.Fatal(err)
	}
	for _, r := range c.GetResources() {
		vsc := r.(*VirtualServerConfiguration)
		if vsc.VirtualServer.Name == "b" && vsc.HTTPPort != 10080 {
			t.Fatalf("HTTP port = %d, want 10080", vsc.HTTPPort)
		}
	}
	b = b.DeepCopy()
	b.Spec.Listener.HTTP = "http-a"
	b.Generation++
	c.AddOrUpdateVirtualServer(b)
	assertListenerClaimResources(t, c, "VirtualServer/default/a")
	c.DeleteVirtualServer("default/a")
	assertListenerClaimResources(t, c, "VirtualServer/default/b")
}

func TestListenerScopedMergeableIngresses(t *testing.T) {
	t.Parallel()
	c := createTestConfiguration()
	for _, spec := range []struct{ name, ports string }{{"a", "8080"}, {"b", "9080"}} {
		ing := listenerClaimIngress(spec.name, spec.ports, "")
		ing.Annotations["nginx.org/mergeable-ingress-type"] = "master"
		c.AddOrUpdateIngress(ing)
	}
	minion := createTestIngressMinion("minion", "shared.example.com", "/")
	_, problems := c.AddOrUpdateIngress(minion)
	if len(problems) != 0 {
		t.Fatalf("minion unexpectedly orphaned: %v", problems)
	}
	assertListenerClaimResources(t, c, "Ingress/default/a", "Ingress/default/b")
	for _, r := range c.GetResources() {
		ing := r.(*IngressConfiguration)
		if len(ing.Minions) != 1 {
			t.Fatalf("master %s has %d minions", ing.Ingress.Name, len(ing.Minions))
		}
	}
}

func TestListenerScopedIPv6EquivalentAddresses(t *testing.T) {
	t.Parallel()
	c := createTestConfiguration()
	c.globalConfiguration = createTestGlobalConfiguration([]conf_v1.Listener{
		{Name: "http-a", Port: 8080, Protocol: "HTTP", IPv4: "192.0.2.1", IPv6: "2001:db8::1"},
		{Name: "http-b", Port: 8080, Protocol: "HTTP", IPv4: "192.0.2.2", IPv6: "2001:db8:0:0:0:0:0:1"},
	})
	c.setGlobalConfigListenerMap()
	a := listenerClaimVS("a", "http-a", "")
	b := listenerClaimVS("b", "http-b", "")
	a.CreationTimestamp = metav1.NewTime(time.Unix(1, 0))
	b.CreationTimestamp = metav1.NewTime(time.Unix(2, 0))
	c.virtualServers["default/a"], c.virtualServers["default/b"] = a, b
	c.rebuildHosts()
	assertListenerClaimResources(t, c, "VirtualServer/default/a")
}
