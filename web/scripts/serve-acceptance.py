#!/usr/bin/env python3
"""Exercise the built UI against a real HTTP serve with a scripted provider."""

import http.client
import json
import os
import pathlib
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request


ROOT = pathlib.Path(__file__).resolve().parents[2]
WEB = ROOT / "web"
SCREENSHOTS = WEB / "screenshots" / "chat"
TOKEN = "m11-15-acceptance-token-20261119"


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def log(text):
    print(text, flush=True)


def request(base, method, path, body=None, token=TOKEN):
    headers = {}
    if token:
        headers["Authorization"] = "Bearer " + token
    data = None
    if body is not None:
        data = json.dumps(body).encode()
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(base + path, method=method, data=data, headers=headers)
    start = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=15) as response:
            payload = response.read()
            elapsed = (time.perf_counter() - start) * 1000
            content_type = response.headers.get("Content-Type", "")
            value = json.loads(payload) if "json" in content_type and payload else payload.decode(errors="replace")
            log(f"HTTP {method} {path} -> {response.status} in {elapsed:.1f} ms")
            return response.status, value, elapsed
    except urllib.error.HTTPError as error:
        payload = error.read()
        elapsed = (time.perf_counter() - start) * 1000
        log(f"HTTP {method} {path} -> {error.code} in {elapsed:.1f} ms")
        try:
            value = json.loads(payload)
        except ValueError:
            value = payload.decode(errors="replace")
        return error.code, value, elapsed


def wait_run_status(base, run_id, expected, timeout=15):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        code, last, _ = request(base, "GET", f"/v1/runs/{run_id}")
        assert code == 200, last
        if last.get("status") == expected:
            return last
        if last.get("status") in ("FAILED", "COMPLETED", "CANCELLED"):
            break
        time.sleep(0.1)
    raise AssertionError(f"run {run_id} did not reach {expected}: {last}")


def read_events(base, run_id, after=0):
    connection = http.client.HTTPConnection("127.0.0.1", int(base.rsplit(":", 1)[1]), timeout=1.0)
    headers = {"Authorization": "Bearer " + TOKEN}
    if after:
        headers["Last-Event-ID"] = str(after)
    connection.request("GET", f"/v1/runs/{run_id}/events", headers=headers)
    response = connection.getresponse()
    chunks = []
    deadline = time.monotonic() + 0.7
    while time.monotonic() < deadline:
        try:
            chunk = response.read1(65536)
            if not chunk:
                break
            chunks.append(chunk.decode(errors="replace"))
        except socket.timeout:
            break
    connection.close()
    raw = "".join(chunks).replace("\r\n", "\n")
    out = []
    for block in raw.split("\n\n"):
        seq, event_type, data = 0, "", []
        for line in block.split("\n"):
            if line.startswith("id:"):
                try:
                    seq = int(line[3:].strip())
                except ValueError:
                    seq = 0
            elif line.startswith("event:"):
                event_type = line[6:].strip()
            elif line.startswith("data:"):
                data.append(line[5:].lstrip())
        if event_type:
            out.append({"seq": seq, "type": event_type, "data": "\n".join(data)})
    log(f"HTTP GET /v1/runs/{run_id}/events Last-Event-ID={after or '(none)'} -> {response.status} in <700 ms; {len(out)} event(s)")
    return response.status, out


def wait_healthy(base):
    for _ in range(100):
        try:
            code, _, _ = request(base, "GET", "/healthz", token="")
            if code == 200:
                return
        except Exception:
            time.sleep(0.1)
    raise RuntimeError("serve did not become healthy")


def run_node_capture(base, mode, *args):
    command = ["node", str(WEB / "scripts" / "capture-screenshots.mjs"), base, str(SCREENSHOTS), mode, *args]
    env = {**os.environ, "CLOUD_HARNESS_HTTP_TOKEN": TOKEN}
    subprocess.run(command, cwd=WEB, env=env, check=True, timeout=120)


def check_doctor(base, failed_id=None):
    code, items, _ = request(base, "GET", "/v1/doctor")
    assert code == 200, items
    overall, = [item for item in items if item["name"] == "overall"]
    failed, = [item for item in items if item["name"] == "failed_runs"]
    fails = sum(item["status"] == "fail" for item in items if item != overall)
    warns = sum(item["status"] == "warn" for item in items if item != overall)
    assert overall["status"] == ("fail" if fails else "warn" if warns else "ok"), items
    assert overall["detail"] == f"{fails} failure{'s' if fails != 1 else ''}, {warns} warning{'s' if warns != 1 else ''}", items
    assert failed["status"] == ("fail" if failed_id else "ok") and failed.get("run_id") == failed_id, items
    return overall


def main():
    if len(sys.argv) != 2:
        raise SystemExit("usage: serve-acceptance.py /path/to/cloud-harness")
    binary = pathlib.Path(sys.argv[1]).resolve()
    SCREENSHOTS.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="m11-15-") as temp_dir:
        temp = pathlib.Path(temp_dir)
        workspace = temp / "workspace"
        workspace.mkdir()
        settings = temp / "config"
        settings.mkdir()
        config_path = settings / "config.json"
        config_path.write_text("{}\n")
        # Exercise a real doctor warning independently of the failed-run check.
        (settings / "AGENTS.md").mkdir()
        (settings / "hooks.json").write_text(json.dumps({
            "hooks": {"UserPromptSubmit": [{
                "type": "command",
                "command": "case \"$(cat)\" in *M11_FAILED_RUN*) echo 'Scripted input warning' >&2; exit 1;; esac",
            }]},
        }))
        serve_port = free_port()
        provider = subprocess.Popen(
            [sys.executable, str(WEB / "scripts" / "scripted-provider.py")],
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
        )
        provider_ready = provider.stdout.readline().strip()
        if not provider_ready:
            raise RuntimeError("scripted provider did not start")
        provider_port = int(provider_ready)

        env = os.environ.copy()
        env.update({
            "CLOUD_HARNESS_HTTP_TOKEN": TOKEN,
            "CLOUD_HARNESS_PROVIDER": "openai",
            "OPENAI_API_KEY": "scripted-provider-test-key",
            "OPENAI_BASE_URL": f"http://127.0.0.1:{provider_port}/v1",
            "OPENAI_MODEL": "m11-scripted",
            "OPENAI_HEADER_TIMEOUT_MS": "3000",
            "OPENAI_IDLE_TIMEOUT_MS": "3000",
            "OPENAI_RETRY_BASE_MS": "1",
            "CLOUD_HARNESS_DISABLE_SNAPSHOTS": "true",
            "CLOUD_HARNESS_CONFIG": str(config_path),
            "CLOUD_HARNESS_DB": str(temp / "serve-test.db"),
        })
        serve = subprocess.Popen(
            [str(binary), "serve", "--addr", f"127.0.0.1:{serve_port}", "--workspace", str(workspace)],
            cwd=ROOT, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
        )
        base = f"http://127.0.0.1:{serve_port}"
        try:
            wait_healthy(base)
            assert request(base, "GET", "/ui/")[0] == 200
            assert request(base, "GET", "/approval")[0] == 200
            assert request(base, "GET", "/v1/sessions", token="")[0] == 401
            assert request(base, "GET", "/v1/sessions")[1] == []
            assert request(base, "GET", "/v1/approvals")[1] == []
            for path in ("/v1/stats", "/v1/doctor"):
                assert request(base, "GET", path, token="")[0] == 401
                assert request(base, "POST", path)[0] == 405
            code, empty_stats, _ = request(base, "GET", "/v1/stats")
            assert code == 200, empty_stats
            assert all(empty_stats[key] == 0 for key in ("sessions", "runs", "events", "active_runs", "failed_runs", "model_calls")), empty_stats
            assert empty_stats["recent_errors"] == [] and empty_stats["usage"]["cost"] is None, empty_stats
            assert check_doctor(base)["status"] == "warn"
            log("empty observability API: zero counts, no recent errors, and a doctor warning")

            run_node_capture(base, "empty")

            approval_session = request(base, "POST", "/v1/sessions", {})[1]["id"]
            ask_session = request(base, "POST", "/v1/sessions", {})[1]["id"]
            pending_session = request(base, "POST", "/v1/sessions", {})[1]["id"]
            approval_message = "M11_APPROVAL_CHILD Verify a multi-tool operation and delegate a child inspection."
            ask_message = "M11_ASK_USER Prepare the deployment and ask me which region to use."
            pending_message = "M11_PENDING_APPROVAL Ask for a file change that the queue will deny."

            code, approval_run, _ = request(base, "POST", f"/v1/sessions/{approval_session}/runs", {"message": approval_message})
            assert code == 201 and approval_run["status"] == "WAITING" and approval_run["wait_reason"] == "APPROVAL", approval_run
            code, ask_run, _ = request(base, "POST", f"/v1/sessions/{ask_session}/runs", {"message": ask_message})
            assert code == 201 and ask_run["status"] == "WAITING" and ask_run["wait_reason"] == "USER", ask_run
            code, pending_run, _ = request(base, "POST", f"/v1/sessions/{pending_session}/runs", {"message": pending_message})
            assert code == 201 and pending_run["status"] == "WAITING" and pending_run["wait_reason"] == "APPROVAL", pending_run

            sessions = request(base, "GET", "/v1/sessions")[1]
            assert len(sessions) == 3 and all(session["run_count"] == 1 for session in sessions), sessions
            session_runs = request(base, "GET", f"/v1/sessions/{approval_session}/runs")[1]
            assert len(session_runs) == 1 and session_runs[0]["id"] == approval_run["id"]

            queue = request(base, "GET", "/v1/approvals")[1]
            assert {item["run_id"] for item in queue} == {approval_run["id"], ask_run["id"], pending_run["id"]}, queue
            wait_approval = next(item for item in queue if item["run_id"] == approval_run["id"])
            wait_question = next(item for item in queue if item["run_id"] == ask_run["id"])
            assert wait_approval["wait_reason"] == "approval" and wait_question["wait_reason"] == "ask_user"

            failed_session = request(base, "POST", "/v1/sessions", {})[1]["id"]
            code, failed_run, _ = request(base, "POST", f"/v1/sessions/{failed_session}/runs", {"message": "M11_FAILED_RUN Exercise the observability failure path."})
            assert code == 201, failed_run
            failed_run = wait_run_status(base, failed_run["id"], "FAILED")
            log(f"created scripted failed run {failed_run['id']} for observability")

            stats = request(base, "GET", "/v1/stats")[1]
            sessions = request(base, "GET", "/v1/sessions")[1]
            assert stats["sessions"] == len(sessions) == 4, (stats, sessions)
            assert stats["runs"] == 4 and stats["active_runs"] == 3 and stats["failed_runs"] == 1, stats
            assert stats["model_calls"] == 3 and stats["usage"]["input_tokens"] == 96 and stats["usage"]["cost"] is None, stats
            assert stats["policy"]["ask"] > 0 and stats["hooks"]["warnings"] == 1, stats
            errors = {error["type"] for error in stats["recent_errors"] if error["run_id"] == failed_run["id"]}
            assert {"run.failed", "hook.warning"}.issubset(errors), stats
            overall = check_doctor(base, failed_run["id"])
            log(f"observability API matches serve: sessions={stats['sessions']}, failed_runs={stats['failed_runs']}, doctor={overall['detail']}, doctor_run={failed_run['id']}")

            code, trace, _ = request(base, "GET", f"/v1/runs/{approval_run['id']}/trace")
            assert code == 200 and trace["run"].get("steps"), trace
            assert trace["run"]["steps"][0].get("batches"), trace
            code, before_events = read_events(base, approval_run["id"])
            assert code == 200 and before_events, before_events
            last_event_id = max(event["seq"] for event in before_events)

            run_node_capture(
                base, "populated", approval_session, approval_run["id"],
                pending_session, pending_run["id"],
                ask_session, ask_run["id"],
                failed_run["id"],
            )

            code, after_events = read_events(base, approval_run["id"], last_event_id)
            assert code == 200 and after_events and all(event["seq"] > last_event_id for event in after_events), after_events
            assert any(event["type"] == "run.completed" for event in after_events), after_events

            final_approval = request(base, "GET", f"/v1/runs/{approval_run['id']}")[1]
            assert final_approval["status"] == "COMPLETED", final_approval
            assert final_approval["final_answer"] == "The parent task completed after approval and the child inspection.", final_approval
            code, final_trace, _ = request(base, "GET", f"/v1/runs/{approval_run['id']}/trace")
            assert code == 200
            calls = [call["name"] for step in (final_trace["run"].get("steps") or []) for batch in (step.get("batches") or []) for call in (batch.get("calls") or [])]
            assert "write_file" in calls and "list_files" in calls and "delegate_task" in calls, calls
            assert final_trace["run"].get("children"), final_trace
            log("verified approval batch, tool calls, child run, final answer, and trace")

            final_ask = request(base, "GET", f"/v1/runs/{ask_run['id']}")[1]
            assert final_ask["status"] == "COMPLETED", final_ask
            assert final_ask["final_answer"] == "Deployment region confirmed: staging-ui", final_ask
            final_denied = request(base, "GET", f"/v1/runs/{pending_run['id']}")[1]
            assert final_denied["status"] == "COMPLETED" and final_denied["final_answer"] == "The pending file request was denied and recorded.", final_denied
            request(base, "GET", "/v1/approvals")
            log("verified ask_user prompt, response, and final answer")

            mock_port = free_port()
            priced_config = settings / "priced.json"
            priced_config.write_text(json.dumps({"model_prices": {"mock": {"input": 1, "output": 2, "cached": 0.5}}}))
            mock_env = {**env, "CLOUD_HARNESS_PROVIDER": "mock", "CLOUD_HARNESS_CONFIG": str(priced_config), "CLOUD_HARNESS_DB": str(temp / "mock-regression.db")}
            mock_serve = subprocess.Popen(
                [str(binary), "serve", "--addr", f"127.0.0.1:{mock_port}", "--workspace", str(workspace)],
                cwd=ROOT, env=mock_env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
            )
            mock_base = f"http://127.0.0.1:{mock_port}"
            try:
                wait_healthy(mock_base)
                mock_session = request(mock_base, "POST", "/v1/sessions", {})[1]["id"]
                code, mock_run, _ = request(mock_base, "POST", f"/v1/sessions/{mock_session}/runs", {"message": "mock fallback remains healthy"})
                assert code == 201 and mock_run["status"] == "COMPLETED" and "mock fallback remains healthy" in mock_run["final_answer"], mock_run
                code, priced_stats, _ = request(mock_base, "GET", "/v1/stats")
                assert code == 200 and isinstance(priced_stats["usage"]["cost"], (int, float)), priced_stats
                log(f"mock one-step regression: passed; configured prices return a numeric cost ({priced_stats['usage']['cost']})")
            finally:
                mock_serve.terminate()
                try:
                    mock_serve.wait(timeout=3)
                except subprocess.TimeoutExpired:
                    mock_serve.kill()

            log("regressions: /approval unchanged (200), protected /v1 routes authenticate (401 without token)")
            expected_screenshots = {
                "welcome-light.png", "welcome-dark.png", "chat-light.png", "chat-dark.png",
                "approvals-light.png", "approvals-dark.png", "observability-light.png", "observability-dark.png",
            }
            actual_screenshots = {path.name for path in SCREENSHOTS.glob("*.png")}
            assert actual_screenshots == expected_screenshots, (actual_screenshots, expected_screenshots)
            log(f"screenshots saved under {SCREENSHOTS}")
        finally:
            serve.terminate()
            provider.terminate()
            for process in (serve, provider):
                try:
                    process.wait(timeout=3)
                except subprocess.TimeoutExpired:
                    process.kill()


if __name__ == "__main__":
    main()
