from __future__ import annotations

from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from .client import Client


class Sandbox:
    def __init__(self, client: "Client", name: str, sandbox_uid: str = "", namespace: str = "fast-sandbox", generation: int = 0, info=None):
        self._client = client
        self.name = name
        self.sandbox_uid = sandbox_uid
        self.namespace = namespace
        self.generation = generation
        self.info = info

    def resolve_endpoint(self, target_port: int):
        """Resolve one raw user port."""
        return self._client.resolve_endpoint(self.name, target_port, self.namespace)

    def resolve_component(self, component_name: str):
        """Resolve one named Pool Infra Component when its route is Ready."""
        return self._client.resolve_component(
            self.name,
            component_name,
            self.namespace,
        )

    def delete(self) -> bool:
        return self._client.delete(self.name, namespace=self.namespace, expected_uid=self.sandbox_uid)

    def replace_action_bindings(self, action_bindings):
        response = self._client.replace_action_bindings(
            self.name, action_bindings, self.namespace,
            expected_generation=self.generation,
            expected_uid=self.sandbox_uid,
        )
        self.generation = response.committed_generation
        return response
