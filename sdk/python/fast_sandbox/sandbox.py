from __future__ import annotations

from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from .client import Client


class Sandbox:
    def __init__(self, client: "Client", name: str, sandbox_uid: str = "", namespace: str = "fast-sandbox"):
        self._client = client
        self.name = name
        self.sandbox_uid = sandbox_uid
        self.namespace = namespace
    def resolve_endpoint(self, target_port: int):
        """Resolve one raw user port."""
        return self._client.resolve_endpoint(self.name, target_port, self.namespace)

    def resolve_component(self, component_name: str, *, wait_timeout_seconds: float = 30):
        """Wait for and resolve one named Pool Infra Component."""
        return self._client.resolve_component(
            self.name,
            component_name,
            self.namespace,
            wait_timeout_seconds=wait_timeout_seconds,
        )

    def delete(self) -> bool:
        return self._client.delete(self.name, namespace=self.namespace)
