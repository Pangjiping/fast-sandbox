from __future__ import annotations

from typing import Iterable, Mapping, Optional

import grpc

from .proto import fastpath_pb2, fastpath_pb2_grpc
from .route import EndpointResolver, ResolvedRoute
from .sandbox import Sandbox
from .telemetry import grpc_metadata


class Client:
    def __init__(
        self,
        endpoint: str = "localhost:9090",
        namespace: str = "fast-sandbox",
        *,
        proxy_endpoint: str = "",
        channel: Optional[grpc.Channel] = None,
        stub=None,
    ):
        self.endpoint = endpoint
        self.namespace = namespace
        self._owns_channel = channel is None and stub is None
        self._channel = channel or (None if stub is not None else grpc.insecure_channel(endpoint))
        self._stub = stub or fastpath_pb2_grpc.FastPathServiceStub(self._channel)
        self._resolver = EndpointResolver(self._stub, namespace, proxy_endpoint)

    @property
    def stub(self):
        return self._stub

    def create(
        self,
        name: str,
        image: str,
        pool: str = "default-pool",
        command: Optional[Iterable[str]] = None,
        args: Optional[Iterable[str]] = None,
        envs: Optional[Mapping[str, str]] = None,
        working_dir: str = "",
        namespace: Optional[str] = None,
        request_id: Optional[str] = None,
        expires_at_unix_seconds: int = 0,
        metadata: Optional[Mapping[str, str]] = None,
        failure_policy: str = "Manual",
        recovery_timeout_seconds: int = 60,
    ) -> Sandbox:
        selected_namespace = namespace or self.namespace
        selected_request_id = request_id or name
        if selected_request_id != name:
            raise ValueError("request_id must equal the sandbox name")
        response = self._stub.CreateSandbox(
            fastpath_pb2.CreateRequest(
                request_id=selected_request_id,
                namespace=selected_namespace,
                image=image,
                pool_ref=pool,
                command=list(command or []), args=list(args or []),
                envs=dict(envs or {}), working_dir=working_dir,
                expires_at_unix_seconds=expires_at_unix_seconds,
                metadata=dict(metadata or {}),
                failure_policy=_failure_policy(failure_policy),
                recovery_timeout_seconds=recovery_timeout_seconds,
            ),
            metadata=grpc_metadata(),
        )
        return Sandbox(
            client=self, name=response.sandbox_name or name,
            sandbox_uid=response.sandbox_uid,
            namespace=selected_namespace,
        )

    def get(self, name: str, namespace: Optional[str] = None) -> Sandbox:
        selected_namespace = namespace or self.namespace
        response = self._stub.GetSandbox(
            fastpath_pb2.GetRequest(sandbox_name=name, namespace=selected_namespace),
            metadata=grpc_metadata(),
        )
        return Sandbox(
            client=self, name=response.sandbox_name or name,
            sandbox_uid=response.sandbox_uid, namespace=selected_namespace,
        )

    def delete(self, name: str, namespace: Optional[str] = None) -> bool:
        response = self._stub.DeleteSandbox(
            fastpath_pb2.DeleteRequest(sandbox_name=name, namespace=namespace or self.namespace),
            metadata=grpc_metadata(),
        )
        return response.success

    def resolve_endpoint(
        self,
        name: str,
        target_port: int,
        namespace: Optional[str] = None,
    ) -> ResolvedRoute:
        """Return a transparent raw user-port route.

        Ports reserved by Pool Infra Components must be resolved by name.
        """
        return self._resolver.resolve_port(name, target_port, namespace or self.namespace)

    def resolve_component(
        self,
        name: str,
        component_name: str,
        namespace: Optional[str] = None,
        *,
        wait_timeout_seconds: float = 30,
    ) -> ResolvedRoute:
        """Wait for and resolve one named Pool Infra Component."""
        return self._resolver.resolve_component(
            name,
            component_name,
            namespace or self.namespace,
            wait_timeout_seconds=wait_timeout_seconds,
        )

    def close(self) -> None:
        if self._owns_channel and self._channel is not None:
            self._channel.close()

    def __enter__(self) -> "Client":
        return self

    def __exit__(self, *_args) -> None:
        self.close()


def _failure_policy(value: str) -> int:
    normalized = value.replace("-", "").replace("_", "").lower()
    if normalized == "manual":
        return fastpath_pb2.MANUAL
    if normalized in {"auto", "autorecreate"}:
        return fastpath_pb2.AUTO_RECREATE
    raise ValueError("failure_policy must be Manual or AutoRecreate")
