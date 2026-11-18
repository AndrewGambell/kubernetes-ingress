from contextlib import ExitStack

import pytest
from settings import TEST_DATA
from suite.utils.custom_resources_utils import create_gc_from_yaml, delete_gc
from suite.utils.resources_utils import create_ingress, delete_ingress, retry_get_until_body_contains
from suite.utils.vs_vsr_resources_utils import create_virtual_server, delete_virtual_server


@pytest.mark.vs
@pytest.mark.vs_listeners
@pytest.mark.ingresses
@pytest.mark.parametrize(
    "crd_ingress_controller, virtual_server_setup",
    [
        (
            {
                "type": "complete",
                "extra_args": ["-global-configuration=nginx-ingress/nginx-configuration"],
            },
            {"example": "virtual-server-custom-listeners", "app_type": "simple"},
        )
    ],
    indirect=True,
)
class TestListenerScopedHosts:
    @pytest.mark.parametrize(
        "kinds",
        [
            ("VirtualServer", "VirtualServer"),
            ("Ingress", "Ingress"),
            ("VirtualServer", "Ingress"),
            ("Ingress", "VirtualServer"),
        ],
    )
    def test_shared_host_routes_to_listener_backend(
        self, kube_apis, crd_ingress_controller, virtual_server_setup, kinds
    ):
        """The same host and path reach separate backends through separate listeners."""
        namespace = virtual_server_setup.namespace
        endpoint = virtual_server_setup.public_endpoint
        host = "shared.example.com"
        with ExitStack() as cleanup:
            gc = create_gc_from_yaml(
                kube_apis.custom_objects,
                f"{TEST_DATA}/virtual-server-custom-listeners/global-configuration.yaml",
                "nginx-ingress",
            )
            cleanup.callback(delete_gc, kube_apis.custom_objects, gc, "nginx-ingress")
            for i, kind in enumerate(kinds, start=1):
                name = f"listener-owner-{i}"
                backend = f"backend{i}-svc"
                if kind == "VirtualServer":
                    resource = {
                        "apiVersion": "k8s.nginx.org/v1",
                        "kind": kind,
                        "metadata": {"name": name},
                        "spec": {
                            "host": host,
                            "upstreams": [{"name": "backend", "service": backend, "port": 80}],
                            "routes": [{"path": "/", "action": {"pass": "backend"}}],
                        },
                    }
                    if i == 2:
                        resource["spec"]["listener"] = {"http": "http-8085"}
                    create_virtual_server(kube_apis.custom_objects, resource, namespace)
                    cleanup.callback(delete_virtual_server, kube_apis.custom_objects, name, namespace)
                else:
                    resource = {
                        "apiVersion": "networking.k8s.io/v1",
                        "kind": kind,
                        "metadata": {
                            "name": name,
                            "annotations": {"nginx.org/listen-ports": "80" if i == 1 else "8085"},
                        },
                        "spec": {
                            "ingressClassName": "nginx",
                            "rules": [
                                {
                                    "host": host,
                                    "http": {
                                        "paths": [
                                            {
                                                "path": "/",
                                                "pathType": "Prefix",
                                                "backend": {"service": {"name": backend, "port": {"number": 80}}},
                                            }
                                        ]
                                    },
                                }
                            ],
                        },
                    }
                    create_ingress(kube_apis.networking_v1, namespace, resource)
                    cleanup.callback(delete_ingress, kube_apis.networking_v1, name, namespace)

            for port, backend in [(endpoint.port, "backend1-"), (endpoint.custom_http, "backend2-")]:
                response = retry_get_until_body_contains(f"http://{endpoint.public_ip}:{port}/", host, backend)
                assert response.status_code == 200
                assert backend in response.text
