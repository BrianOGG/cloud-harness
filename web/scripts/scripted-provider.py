#!/usr/bin/env python3
"""A tiny OpenAI-compatible scripted provider used against a real serve process."""

import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


def tool_call(name, arguments, call_id):
    return {
        "id": call_id,
        "type": "function",
        "function": {"name": name, "arguments": json.dumps(arguments)},
    }


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_GET(self):
        payload = json.dumps({"object": "list", "data": [{"id": "m11-scripted", "object": "model"}]}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))))
        messages = body.get("messages", [])
        text = "\n".join(str(message.get("content", "")) for message in messages)
        if "M11_FAILED_RUN" in text:
            payload = json.dumps({"error": {"message": "scripted planned failure"}}).encode()
            self.send_response(503)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)
            return
        calls = [
            call
            for message in messages
            for call in message.get("tool_calls", [])
        ]
        called_names = [call.get("function", {}).get("name", "") for call in calls]

        available_tools = [tool.get("function", {}).get("name", "") for tool in body.get("tools", [])]
        if "M11_CHILD_GOAL" in text and "delegate_task" not in available_tools:
            response = {"content": "Child inspection complete: the approved file records the deployment change."}
        elif "M11_APPROVAL_CHILD" in text:
            if "write_file" not in called_names:
                response = {"tool_calls": [
                    tool_call("write_file", {"path": "m11-8-approved.txt", "content": "approval and child flow\n"}, "approval-write"),
                    tool_call("list_files", {"path": "."}, "approval-list"),
                ]}
            elif "delegate_task" not in called_names:
                response = {"tool_calls": [tool_call("delegate_task", {
                    "goal": "M11_CHILD_GOAL inspect the approved file and report its purpose",
                    "agent": "explorer",
                    "allowed_tools": ["read_file"],
                    "max_model_calls": 4,
                    "max_tool_calls": 4,
                }, "child-task")]}
            else:
                response = {"content": "The parent task completed after approval and the child inspection."}
        elif "M11_PENDING_APPROVAL" in text:
            if "write_file" not in called_names:
                response = {"tool_calls": [tool_call("write_file", {
                    "path": "m11-8-pending.txt",
                    "content": "this optional request will be denied\n",
                }, "pending-write")]}
            else:
                response = {"content": "The pending file request was denied and recorded."}
        elif "M11_ASK_USER" in text:
            if "ask_user" not in called_names:
                response = {"tool_calls": [tool_call("ask_user", {"question": "Which deployment region should I use?"}, "ask-region")]}
            else:
                response = {"content": "Deployment region confirmed: " + (text.split("User reply:")[-1].strip() if "User reply:" in text else "unknown")}
        else:
            response = {"content": "Scripted provider fallback response."}

        message = {"role": "assistant", **response}
        payload = json.dumps({
            "id": "chatcmpl-scripted",
            "object": "chat.completion",
            "choices": [{"index": 0, "finish_reason": "tool_calls" if "tool_calls" in response else "stop", "message": message}],
            "usage": {"prompt_tokens": 32, "completion_tokens": 12, "total_tokens": 44},
        }).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, _format, *_args):
        pass


def main():
    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    print(server.server_port, flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
