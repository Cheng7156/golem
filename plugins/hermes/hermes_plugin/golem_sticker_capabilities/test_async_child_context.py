"""Tests for Hermes async child delegation context binding."""

from __future__ import annotations

import types
import unittest
import importlib
import threading

import test_async_delivery as shared


child_context = importlib.import_module(f"{shared.SPEC.name}.async_delivery_child_context")
state = shared.state


class AsyncChildContextTests(unittest.TestCase):
    def test_background_dispatch_sets_child_delegation_context(self):
        module = types.SimpleNamespace()
        module._new_delegation_id = lambda: shared.binding().delegation_id

        def dispatch_async_delegation_batch(**kwargs):
            module._new_delegation_id()
            return kwargs["runner"]()

        module.dispatch_async_delegation = dispatch_async_delegation_batch
        module.dispatch_async_delegation_batch = dispatch_async_delegation_batch
        child_context.install(module)

        result = module.dispatch_async_delegation_batch(
            runner=lambda: state.current_child_delegation_id.get()
        )
        self.assertEqual(result, shared.binding().delegation_id)

    def test_daemon_executor_propagates_context_into_child_thread(self):
        class Executor:
            def submit(self, function, *args, **kwargs):
                result = []
                thread = threading.Thread(
                    target=lambda: result.append(function(*args, **kwargs))
                )
                thread.start()
                thread.join()
                return result[0]

        child_context._patch_daemon_executor(Executor)
        token = state.current_child_delegation_id.set("deleg_thread")
        try:
            result = Executor().submit(state.current_child_delegation_id.get)
        finally:
            state.current_child_delegation_id.reset(token)
        self.assertEqual(result, "deleg_thread")


if __name__ == "__main__":
    unittest.main()
