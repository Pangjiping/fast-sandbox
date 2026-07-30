from __future__ import annotations

import unittest
from unittest import mock

from fast_sandbox import Client
from fast_sandbox import telemetry
from fast_sandbox.proto import fastpath_pb2


class FakeFastPathStub:
    def __init__(self):
        self.create_request = None
        self.resolve_requests = []
        self.metadata = []

    def CreateSandbox(self, request, metadata=()):
        self.metadata.append(tuple(metadata))
        self.create_request = request
        return fastpath_pb2.SandboxInfo(
            sandbox_uid="uid-a",
            sandbox_name=request.request_id,
            namespace=request.namespace,
            runtime_state="Ready",
        )

    def GetSandbox(self, request, metadata=()):
        self.metadata.append(tuple(metadata))
        return fastpath_pb2.SandboxInfo(
            sandbox_uid="uid-a",
            sandbox_name=request.sandbox_name,
            namespace=request.namespace,
        )

    def DeleteSandbox(self, _request, metadata=()):
        self.metadata.append(tuple(metadata))
        return fastpath_pb2.DeleteResponse(success=True)

    def ResolveEndpoint(self, request, metadata=()):
        self.metadata.append(tuple(metadata))
        self.resolve_requests.append(request)
        component_name = request.target.component_name
        port = request.target.port or 44772
        if component_name:
            path = f"/v2/sandboxes/uid-a/components/{component_name}"
        else:
            path = f"/v2/sandboxes/uid-a/ports/{port}"
        return fastpath_pb2.ResolveEndpointResponse(
            sandbox_uid="uid-a",
            target=request.target,
            component_name=component_name,
            protocol="HTTP",
            resolved_port=port,
            proxy_endpoint=f"http://sandbox-proxy.svc{path}",
            required_headers={
                "X-Fast-Sandbox-Route-Credential": "route-token"
            },
            route_generation=4,
        )


class SDKTest(unittest.TestCase):
    def setUp(self):
        self.stub = FakeFastPathStub()
        self.client = Client(
            namespace="tenant-a",
            proxy_endpoint="http://proxy.test:18080/prefix",
            stub=self.stub,
        )

    def test_create_sends_atomic_initial_intent(self):
        sandbox = self.client.create(
            "sandbox-a",
            "alpine",
            command=["sleep", "60"],
            expires_at_unix_seconds=1785373200,
            metadata={"owner": "team-a"},
            failure_policy="AutoRecreate",
            recovery_timeout_seconds=120,
        )
        self.assertEqual("sandbox-a", sandbox.name)
        request = self.stub.create_request
        self.assertEqual("sandbox-a", request.request_id)
        self.assertEqual("tenant-a", request.namespace)
        self.assertEqual(1785373200, request.expires_at_unix_seconds)
        self.assertEqual({"owner": "team-a"}, dict(request.metadata))
        self.assertEqual(fastpath_pb2.AUTO_RECREATE, request.failure_policy)
        self.assertEqual(120, request.recovery_timeout_seconds)

    def test_named_component_waits_and_preserves_route_path(self):
        route = self.client.get("sandbox-a").resolve_component("execd")
        self.assertEqual(
            "http://proxy.test:18080/prefix/v2/sandboxes/uid-a/components/execd",
            route.endpoint,
        )
        self.assertEqual(
            "route-token",
            route.headers["X-Fast-Sandbox-Route-Credential"],
        )
        request = self.stub.resolve_requests[-1]
        self.assertEqual("execd", request.target.component_name)
        self.assertTrue(request.wait_until_ready)
        self.assertEqual(30_000, request.wait_timeout_millis)
        self.assertEqual("tenant-a", request.sandbox.namespaced_name.namespace)

    def test_raw_port_does_not_claim_component_readiness(self):
        route = self.client.resolve_endpoint("sandbox-a", 8080)
        self.assertEqual(8080, route.resolved_port)
        request = self.stub.resolve_requests[-1]
        self.assertEqual(8080, request.target.port)
        self.assertFalse(request.wait_until_ready)

    def test_optional_opentelemetry_context_reaches_grpc(self):
        def inject(carrier):
            carrier["traceparent"] = (
                "00-4bf92f3577b34da6a3ce929d0e0e4736-"
                "00f067aa0ba902b7-01"
            )

        with mock.patch.object(telemetry, "_otel_inject", inject):
            self.client.create("sandbox-a", "alpine")
            self.client.get("sandbox-a").resolve_component("execd")

        self.assertTrue(
            all(("traceparent", mock.ANY) in metadata for metadata in self.stub.metadata)
        )


if __name__ == "__main__":
    unittest.main()
