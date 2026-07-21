"""Tests for durable cron delivery patching against Hermes v0.18.2 shapes."""

from __future__ import annotations

import importlib.util
from pathlib import Path
import sys
import unittest
from unittest import mock


PLUGIN_DIR = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location(
    "golem_cron_delivery_under_test",
    PLUGIN_DIR / "__init__.py",
    submodule_search_locations=[str(PLUGIN_DIR)],
)
assert SPEC is not None and SPEC.loader is not None
plugin = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = plugin
SPEC.loader.exec_module(plugin)
cron_delivery = plugin._cron_delivery


class _Jobs:
    def __init__(self) -> None:
        self.saved = {}
        self.removed = []

    def create_job(self, prompt, schedule, **kwargs):
        del prompt, schedule, kwargs
        job = {
            "id": "job-1",
            "origin": {"platform": "relay", "chat_id": "room-1|interactive"},
            "deliver": "origin",
        }
        self.saved[job["id"]] = job
        return job

    def update_job(self, job_id, updates):
        self.saved[job_id].update(updates)
        return dict(self.saved[job_id])

    def get_job(self, job_id):
        value = self.saved.get(job_id)
        return dict(value) if value is not None else None

    def remove_job(self, job_id):
        self.removed.append(job_id)
        self.saved.pop(job_id, None)
        return True


class _Scheduler:
    def run_job(self, job, *, defer_agent_teardown=None):
        del job, defer_agent_teardown
        return cron_delivery.current_cron_delivery.get()

    def _deliver_result(self, job, content, adapters=None, loop=None):
        del job, content, adapters, loop
        return "original"


class _CronTools:
    def _execute_job_now(self, job):
        return {"delivery_id": job.get("_golem_cron_delivery_id")}


class CronDeliveryTests(unittest.TestCase):
    def test_create_registers_and_persists_profile(self):
        jobs = _Jobs()
        with mock.patch.object(cron_delivery, "_register") as register, mock.patch.object(
            cron_delivery, "_context_profile", return_value="default"
        ):
            cron_delivery._patch_create_job(jobs)
            result = jobs.create_job("status", "every 5m")

        register.assert_called_once()
        self.assertEqual(result["golem_cron_profile"], "default")

    def test_failed_registration_removes_job(self):
        jobs = _Jobs()
        with mock.patch.object(
            cron_delivery, "_register", side_effect=RuntimeError("down")
        ), mock.patch.object(cron_delivery, "_context_profile", return_value="default"):
            cron_delivery._patch_create_job(jobs)
            with self.assertRaisesRegex(RuntimeError, "down"):
                jobs.create_job("status", "every 5m")

        self.assertEqual(jobs.removed, ["job-1"])

    def test_update_to_origin_registers_and_persists_profile(self):
        jobs = _Jobs()
        jobs.saved["job-local"] = {
            "id": "job-local",
            "origin": {"platform": "relay", "chat_id": "room-1|interactive"},
            "deliver": "local",
        }
        with mock.patch.object(cron_delivery, "_register") as register, mock.patch.object(
            cron_delivery, "_context_profile", return_value="default"
        ):
            cron_delivery._patch_update_job(jobs)
            result = jobs.update_job("job-local", {"deliver": "origin"})

        register.assert_called_once_with("job-local", "default")
        self.assertEqual(result["golem_cron_profile"], "default")

    def test_context_profile_precedes_sticky_default(self):
        with mock.patch.object(
            cron_delivery.client,
            "current_context",
            return_value={"profile": "relay-profile"},
        ), mock.patch.object(cron_delivery, "active_profile") as active:
            profile = cron_delivery._context_profile()

        self.assertEqual(profile, "relay-profile")
        active.assert_not_called()

    def test_delivery_posts_stable_bound_identity(self):
        scheduler = _Scheduler()
        job = {
            "id": "job-1",
            "origin": {"platform": "relay", "chat_id": "room-1|interactive"},
            "deliver": "origin",
            "next_run_at": "2026-07-16T12:00:00+08:00",
            "golem_cron_profile": "default",
        }

        with mock.patch.object(
            cron_delivery.client,
            "post_json",
            return_value={"disposition": "delivered", "outbox_id": "outbox-1"},
        ) as posted, mock.patch.object(
            cron_delivery.cron_delivery_api, "direct_output_count", return_value=0
        ):
            cron_delivery._patch_deliver_result(scheduler)
            error = scheduler._deliver_result(job, "gateway ok")

        self.assertIsNone(error)
        path, payload = posted.call_args.args
        self.assertEqual(path, cron_delivery.client.CRON_DELIVER_PATH)
        self.assertEqual(payload["job_id"], "job-1")
        self.assertEqual(
            payload["delivery_id"],
            "job-1:2026-07-16T12:00:00+08:00",
        )
        self.assertEqual(payload["chat_id"], "room-1|interactive")

    def test_tool_error_output_is_not_delivered(self):
        scheduler = _Scheduler()
        job = {
            "id": "job-1",
            "origin": {"platform": "relay", "chat_id": "room-1|interactive"},
            "deliver": "origin",
            "next_run_at": "2026-07-16T12:00:00+08:00",
            "golem_cron_profile": "default",
        }
        with mock.patch.object(
            cron_delivery.cron_delivery_api, "direct_output_count", return_value=0
        ), mock.patch.object(cron_delivery.client, "post_json") as posted:
            cron_delivery._patch_deliver_result(scheduler)
            error = scheduler._deliver_result(job, "[TOOL_ERROR] provider failed")

        self.assertEqual(error, "Golem cron delivery refused tool error output")
        posted.assert_not_called()

    def test_manual_run_gets_unique_delivery_identity(self):
        tools = _CronTools()
        cron_delivery._patch_execute_now(tools)
        first = tools._execute_job_now({"id": "job-1"})
        second = tools._execute_job_now({"id": "job-1"})

        self.assertTrue(first["delivery_id"].startswith("job-1:manual:"))
        self.assertNotEqual(first["delivery_id"], second["delivery_id"])

    def test_run_injects_trusted_cron_binding(self):
        scheduler = _Scheduler()
        cron_delivery._patch_run_job(scheduler)
        binding = scheduler.run_job(
            {
                "id": "job-1",
                "origin": {"platform": "relay", "chat_id": "room-1|interactive"},
                "deliver": "origin",
                "next_run_at": "2026-07-16T12:00:00+08:00",
                "golem_cron_profile": "default",
            }
        )
        self.assertEqual(binding.profile, "default")
        self.assertEqual(binding.job_id, "job-1")
        self.assertIsNone(cron_delivery.current_cron_delivery.get())

    def test_direct_media_suppresses_completion_text(self):
        scheduler = _Scheduler()
        job = {
            "id": "job-1",
            "origin": {"platform": "relay", "chat_id": "room-1|interactive"},
            "deliver": "origin",
            "next_run_at": "2026-07-16T12:00:00+08:00",
            "golem_cron_profile": "default",
        }
        with mock.patch.object(
            cron_delivery.cron_delivery_api, "direct_output_count", return_value=1
        ), mock.patch.object(cron_delivery.client, "post_json") as posted:
            cron_delivery._patch_deliver_result(scheduler)
            error = scheduler._deliver_result(job, "sent video")
        self.assertIsNone(error)
        posted.assert_not_called()

    def test_transient_delivery_retries_same_identity(self):
        payload = {
            "profile": "default",
            "job_id": "job-1",
            "chat_id": "room-1|interactive",
            "delivery_id": "job-1:scheduled",
            "content": "gateway ok",
        }
        with mock.patch.object(
            cron_delivery.client,
            "post_json",
            side_effect=[
                cron_delivery.RetryableCapabilityError("reload"),
                {"disposition": "delivered"},
            ],
        ) as posted, mock.patch.object(cron_delivery.time, "sleep"):
            result = cron_delivery._deliver_until_committed(payload)

        self.assertEqual(result["disposition"], "delivered")
        self.assertEqual(posted.call_args_list[0].args[1], payload)
        self.assertEqual(posted.call_args_list[1].args[1], payload)

    def test_delivery_retry_window_is_bounded(self):
        payload = {
            "profile": "default",
            "job_id": "job-1",
            "chat_id": "room-1|interactive",
            "delivery_id": "job-1:scheduled",
            "content": "gateway ok",
        }
        with mock.patch.object(
            cron_delivery.client,
            "post_json",
            side_effect=cron_delivery.RetryableCapabilityError("host unavailable"),
        ) as posted, mock.patch.object(
            cron_delivery.time, "monotonic", side_effect=[0.0, 121.0]
        ), mock.patch.object(cron_delivery.time, "sleep") as sleep:
            with self.assertRaises(cron_delivery.RetryableCapabilityError):
                cron_delivery._deliver_until_committed(payload)

        posted.assert_called_once()
        sleep.assert_not_called()

    def test_delivery_stops_when_gateway_is_shutting_down(self):
        payload = {
            "profile": "default",
            "job_id": "job-1",
            "chat_id": "room-1|interactive",
            "delivery_id": "job-1:scheduled",
            "content": "gateway ok",
        }
        with mock.patch.object(
            cron_delivery.client,
            "post_json",
            side_effect=cron_delivery.RetryableCapabilityError("host unavailable"),
        ), mock.patch.object(cron_delivery.time, "monotonic", return_value=0.0):
            with self.assertRaisesRegex(cron_delivery.CapabilityError, "stopping"):
                cron_delivery._deliver_until_committed(payload, stopping=lambda: True)

    def test_unbound_legacy_job_fails_explicitly(self):
        scheduler = _Scheduler()
        cron_delivery._patch_deliver_result(scheduler)
        error = scheduler._deliver_result(
            {
                "id": "legacy-job",
                "origin": {"platform": "relay", "chat_id": "room-1|interactive"},
                "deliver": "origin",
            },
            "gateway ok",
        )

        self.assertIn("recreate this cron job", error)


if __name__ == "__main__":
    unittest.main()
