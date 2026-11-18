# Share a hostname across listeners

Use separate VirtualServer or Ingress resources to route the same hostname and path to different backend Services on different listeners. For example, `app.example.com:8080/` can use `app-a`, while `app.example.com:9080/` uses `app-b`.

## VirtualServer resources

Define the listeners in the GlobalConfiguration resource watched by your controller:

```yaml
apiVersion: k8s.nginx.org/v1
kind: GlobalConfiguration
metadata:
  name: nginx-configuration
  namespace: nginx-ingress
spec:
  listeners:
  - name: http-a
    port: 8080
    protocol: HTTP
  - name: http-b
    port: 9080
    protocol: HTTP
```

Create both VirtualServers in the namespace containing the backend Services:

```yaml
apiVersion: k8s.nginx.org/v1
kind: VirtualServer
metadata:
  name: app-a
spec:
  host: app.example.com
  listener:
    http: http-a
  upstreams:
  - name: app
    service: app-a
    port: 80
  routes:
  - path: /
    action:
      pass: app
---
apiVersion: k8s.nginx.org/v1
kind: VirtualServer
metadata:
  name: app-b
spec:
  host: app.example.com
  listener:
    http: http-b
  upstreams:
  - name: app
    service: app-b
    port: 80
  routes:
  - path: /
    action:
      pass: app
```

Configure the controller's `-global-configuration` argument to watch `nginx-ingress/nginx-configuration`, and expose ports 8080 and 9080 through its Service. These examples assume backend Services `app-a` and `app-b` already exist.

## Ingress resources

For Ingress, select HTTP ports with `nginx.org/listen-ports` and HTTPS ports with `nginx.org/listen-ports-ssl`. The Ingress annotations do not require GlobalConfiguration listeners. For example:

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: app-a
  annotations:
    nginx.org/listen-ports: "8080"
spec:
  ingressClassName: nginx
  rules:
  - host: app.example.com
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: app-a
            port:
              number: 80
```

Create a second Ingress with name and backend Service `app-b` and `nginx.org/listen-ports: "9080"`. Both can serve `/` on `app.example.com`. VirtualServer and Ingress resources can also share a hostname when their listeners are disjoint.

## Collision rules

- Listener names alone do not establish separation. Matching ports with overlapping IP addresses still conflict, including a wildcard address and a specific address. Check IPv6 bindings as well as IPv4 bindings.
- If any listener overlaps, the existing metadata-based winner selection applies to the entire hostname claim. NIC does not remove individual listeners from a server block. An Ingress with multiple hosts can retain its other, nonconflicting hosts.
- Default HTTP listeners participate in collision detection on port 80. Ingress HTTPS ports participate for hosts configured for TLS. Unresolved VirtualServer listeners retain conservative hostname collision handling until valid listener configuration is available.
- TLS passthrough retains hostname-wide ownership. Terminating TLS server blocks share a Unix socket when passthrough is enabled, so distinct external listener ports do not establish separate HTTPS owners in that mode.
- VirtualServerRoute references can be used by multiple VirtualServers with the same host on disjoint listeners. Mergeable Ingress minions still attach by hostname and apply to each accepted master for that hostname.

Related feature request: [Same Host for multiple Ingress listening on different ports (#3313)](https://github.com/nginx/kubernetes-ingress/issues/3313).
