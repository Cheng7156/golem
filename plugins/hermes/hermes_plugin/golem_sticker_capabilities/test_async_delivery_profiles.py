"""Profile binding tests for Hermes v0.18.2 async delivery."""

from __future__ import annotations

import asyncio
from dataclasses import dataclass
import sys
import types
from unittest import mock
import unittest

import test_async_delivery as shared


api = shared.api
runtime = shared.runtime
state = shared.state


class AsyncDeliveryProfileTests(unittest.TestCase):
    def test_empty_relay_profile_uses_named_active_profile(self):
        profiles = types.ModuleType("hermes_cli.profiles")
        profiles.get_active_profile_name = lambda: "wechat-prod"
        context = dict(shared.CONTEXT, profile="")
        response = {"state": "pending"}
        with mock.patch.dict(sys.modules, {"hermes_cli.profiles": profiles}):
            with mock.patch.object(api, "_producer_epoch", return_value="ade_" + "1" * 32):
                with mock.patch.object(api, "_post_idempotent", return_value=response) as post:
                    result = api.register_delivery(api.RegistrationInput(
                        delegation_id="deleg_profile1",
                        hermes_session_id="session-profile",
                        context=context,
                    ))
        self.assertEqual(result.profile, "wechat-prod")
        self.assertEqual(post.call_args.args[1]["context"]["profile"], "wechat-prod")

    def test_explicit_multiplex_profile_is_preserved(self):
        context = dict(shared.CONTEXT, profile="secondary")
        with mock.patch.object(api, "active_profile") as active:
            with mock.patch.object(api, "_producer_epoch", return_value="ade_" + "2" * 32):
                with mock.patch.object(api, "_post_idempotent", return_value={"state": "pending"}):
                    result = api.register_delivery(api.RegistrationInput(
                        delegation_id="deleg_profile2",
                        hermes_session_id="session-profile",
                        context=context,
                    ))
        active.assert_not_called()
        self.assertEqual(result.profile, "secondary")

    def test_multiplex_startup_reconciles_every_served_profile(self):
        config = types.ModuleType("hermes_cli.config")
        config.load_config = lambda: {"gateway": {"multiplex_profiles": True}}
        profiles = types.ModuleType("hermes_cli.profiles")
        profiles.profiles_to_serve = lambda multiplex: [
            ("default", object()), ("secondary", object()),
        ]
        modules = {
            "hermes_cli.config": config,
            "hermes_cli.profiles": profiles,
        }
        with mock.patch.dict(sys.modules, modules):
            self.assertEqual(
                api.delivery_profiles(), ("default", "secondary")
            )

    def test_only_root_gateway_startup_reconciles(self):
        gateway = type("Gateway", (), {})()
        with mock.patch.object(
            runtime, "activate_gateway_producer", return_value="ade_" + "3" * 32
        ) as activate:
            with mock.patch.object(
                runtime, "delivery_profiles", return_value=("default", "secondary")
            ):
                with mock.patch.object(runtime, "reconcile", return_value=0) as reconcile:
                    runtime.gateway_startup(gateway=gateway)
        activate.assert_called_once_with()
        self.assertEqual(
            [call.args[0] for call in reconcile.call_args_list],
            ["default", "secondary"],
        )

    def test_repeated_root_hook_keeps_same_producer_identity(self):
        gateway = type("Gateway", (), {})()
        with mock.patch.object(
            runtime, "activate_gateway_producer", return_value="ade_" + "4" * 32
        ) as activate:
            with mock.patch.object(runtime, "delivery_profiles", return_value=()):
                runtime.gateway_startup(gateway=gateway)
                runtime.gateway_startup(gateway=gateway)
        activate.assert_called_once_with()

    def test_synthetic_source_restores_ticket_profile(self):
        @dataclass
        class Source:
            profile: str | None = None

        class Runner:
            def __init__(self):
                self.source = Source()

            def _build_process_event_source(self, _evt):
                return self.source

        runner = Runner()
        runtime._patch_process_event_source(Runner)
        value = shared.binding()
        value = state.DeliveryBinding(**{**value.__dict__, "profile": "secondary"})
        token = state.current_delivery.set(value)
        try:
            source = runner._build_process_event_source({})
        finally:
            state.current_delivery.reset(token)
        self.assertEqual(source.profile, "secondary")
        self.assertIsNone(runner.source.profile)

    def test_revoke_hook_does_not_block_running_event_loop(self):
        revoke = mock.Mock(return_value=1)
        value = shared.binding()
        value = state.DeliveryBinding(**{**value.__dict__, "profile": "named"})
        state.mark_ready(value)

        async def invoke():
            with mock.patch.object(runtime, "revoke_session", revoke):
                runtime.session_reset(old_session_id=value.hermes_session_id)
                self.assertFalse(revoke.called)
                tasks = tuple(runtime.async_delivery_lifecycle._tasks)
                await asyncio.gather(*tasks)

        asyncio.run(invoke())
        revoke.assert_called_once_with(value.hermes_session_id, "named")

    def test_revoke_runs_directly_without_event_loop(self):
        revoke = mock.Mock(return_value=1)
        value = shared.binding()
        value = state.DeliveryBinding(**{**value.__dict__, "profile": "named"})
        state.mark_ready(value)
        with mock.patch.object(runtime, "revoke_session", revoke):
            runtime.session_reset(old_session_id=value.hermes_session_id)
        revoke.assert_called_once_with(value.hermes_session_id, "named")


if __name__ == "__main__":
    unittest.main()
