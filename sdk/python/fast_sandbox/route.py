from __future__ import annotations

from dataclasses import dataclass
from typing import Mapping
from urllib.parse import urlencode, urlsplit, urlunsplit

from .proto import fastpath_pb2
from .telemetry import grpc_metadata


@dataclass(frozen=True)
class ResolvedRoute:
    sandbox_uid: str
    component_name: str
    protocol: str
    resolved_port: int
    endpoint: str
    headers: Mapping[str, str]
    route_generation: int
    expires_at_unix_seconds: int

    def url(self, path: str, query: Mapping[str, str] | None = None) -> str:
        if not path.startswith("/"):
            raise ValueError("Infra Component path must be absolute")
        parsed = urlsplit(self.endpoint)
        route_path = parsed.path.rstrip("/") + path
        return urlunsplit((parsed.scheme, parsed.netloc, route_path, urlencode(query or {}), ""))


class EndpointResolver:
    def __init__(self, stub, namespace: str = "fast-sandbox", proxy_endpoint: str = ""):
        self._stub = stub
        self._namespace = namespace or "fast-sandbox"
        self._proxy_endpoint = proxy_endpoint

    def resolve_component(
        self,
        sandbox_name: str,
        component_name: str,
        namespace: str = "",
    ) -> ResolvedRoute:
        if not component_name:
            raise ValueError("component_name is required")
        return self._resolve(
            sandbox_name,
            fastpath_pb2.EndpointTarget(component_name=component_name),
            namespace,
        )

    def resolve_port(self, sandbox_name: str, target_port: int, namespace: str = "") -> ResolvedRoute:
        if not 0 < target_port <= 65535:
            raise ValueError("target_port must be between 1 and 65535")
        return self._resolve(
            sandbox_name,
            fastpath_pb2.EndpointTarget(port=target_port),
            namespace,
        )

    def _resolve(
        self,
        sandbox_name: str,
        target,
        namespace: str,
    ) -> ResolvedRoute:
        if not sandbox_name:
            raise ValueError("sandbox_name is required")
        selected_namespace = namespace or self._namespace
        response = self._stub.ResolveEndpoint(
            fastpath_pb2.ResolveEndpointRequest(
                sandbox=fastpath_pb2.SandboxReference(
                    namespaced_name=fastpath_pb2.NamespacedName(
                        namespace=selected_namespace,
                        name=sandbox_name,
                    )
                ),
                target=target,
                access_mode=fastpath_pb2.CENTRAL_PROXY,
            ),
            metadata=grpc_metadata(),
        )
        if not response.sandbox_uid:
            raise RuntimeError("FastPath returned a route without a Sandbox UID")
        if response.target.WhichOneof("target") != target.WhichOneof("target"):
            raise RuntimeError("FastPath returned a route for a different target kind")
        if target.component_name:
            if response.component_name != target.component_name or not response.resolved_port:
                raise RuntimeError("FastPath returned a route for a different Infra Component")
        elif response.resolved_port != target.port:
            raise RuntimeError("FastPath returned a route for a different target port")
        endpoint = _replace_authority(response.proxy_endpoint, self._proxy_endpoint)
        return ResolvedRoute(
            sandbox_uid=response.sandbox_uid,
            component_name=response.component_name,
            protocol=response.protocol,
            resolved_port=response.resolved_port,
            endpoint=endpoint,
            headers=dict(response.required_headers),
            route_generation=response.route_generation,
            expires_at_unix_seconds=response.expires_at_unix_seconds,
        )


def _replace_authority(route_endpoint: str, override: str) -> str:
    route = urlsplit(route_endpoint)
    if not route.scheme or not route.netloc:
        raise RuntimeError(f"FastPath returned invalid proxy endpoint {route_endpoint!r}")
    if not override:
        return route_endpoint
    base = urlsplit(override)
    if not base.scheme or not base.netloc or base.query or base.fragment:
        raise ValueError(f"invalid Sandbox Proxy base URL {override!r}")
    path = base.path.rstrip("/") + route.path
    return urlunsplit((base.scheme, base.netloc, path, route.query, ""))
