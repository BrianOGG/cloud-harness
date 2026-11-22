#!/usr/bin/env python3
"""An OpenAI-compatible provider with canned replies, for trying the Web UI without a model key.

It answers in streamed chunks and picks a flow from the latest user message:
writing a file (waits for approval), asking the user a question, delegating a
child task, failing, or a plain Markdown answer.

    python3 web/scripts/demo-provider.py [port]     # default 19422

    CLOUD_HARNESS_PROVIDER=openai OPENAI_API_KEY=demo OPENAI_MODEL=demo \\
    OPENAI_BASE_URL=http://127.0.0.1:19422/v1 ./cloud-harness serve
"""

import json
import re
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

MODEL = "demo"
CHUNK_DELAY = 0.015

INTRO = """我是 Cloud Harness 的演示模型，回复是预先写好的，用来展示界面上的几种流程：

- **工具调用和审批**：说「写一个文件」，我会调用 `write_file`，它需要你批准。
- **向你提问**：说「部署前先问我」，我会用 `ask_user` 停下来等你回答。
- **子任务**：说「派一个子任务」，我会用 `delegate_task` 启动一个子运行。
- **失败**：说「模拟失败」，这次模型调用会返回错误。

回答按 Markdown 渲染，比如代码块：

```bash
cloud-harness serve --addr 127.0.0.1:19421
```

| 状态 | 含义 |
| --- | --- |
| `RUNNING` | 正在运行 |
| `WAITING` | 等待审批、回答、子任务或预算 |
| `COMPLETED` | 已完成 |

> 接上真实模型后，这里显示的就是模型的回答。
"""


def text_of(message):
    content = message.get("content") or ""
    if isinstance(content, list):
        return "\n".join(str(part.get("text", "")) for part in content if isinstance(part, dict))
    return str(content)


def tool_call(name, arguments):
    return {"name": name, "arguments": json.dumps(arguments, ensure_ascii=False)}


def reply(body):
    """Returns ("text", str), ("tools", [call, ...]) or ("error", str)."""
    messages = body.get("messages", [])
    available = [tool.get("function", {}).get("name", "") for tool in body.get("tools", [])]
    start, prompt = 0, ""
    for index, message in enumerate(messages):
        if message.get("role") == "user" and not text_of(message).startswith("User reply:"):
            start, prompt = index, text_of(message)
    after = messages[start + 1:]
    called = [call.get("function", {}).get("name", "") for message in after for call in message.get("tool_calls") or []]
    answer = next((text_of(m)[len("User reply:"):].strip() for m in reversed(after) if text_of(m).startswith("User reply:")), "")
    lowered = prompt.lower()

    if "delegate_task" not in available:
        return "text", "子任务检查完成：工作区里的文件和预期一致。"
    if re.search(r"失败|fail", lowered):
        return "error", "demo provider: simulated upstream failure"
    if re.search(r"问我|提问|区域|ask|region", lowered):
        if "ask_user" not in called:
            return "tools", [tool_call("ask_user", {"question": "要部署到哪个区域？例如 `cn-shanghai` 或 `us-west-2`。"})]
        return "text", f"好的，部署区域定为 **{answer or '未指定'}**。\n\n接下来的步骤：\n\n1. 构建镜像\n2. 推送到该区域的仓库\n3. 滚动更新服务"
    if re.search(r"子任务|委派|delegate|child", lowered):
        if "delegate_task" not in called:
            return "tools", [tool_call("delegate_task", {
                "goal": "检查工作区里的文件并报告它们的用途",
                "agent": "explorer",
                "allowed_tools": ["read_file", "list_files"],
                "max_model_calls": 4,
                "max_tool_calls": 4,
            })]
        return "text", "子任务已经完成检查，没有发现问题。\n\n- 子运行在自己的预算里执行\n- 它的状态可以在「运行详情」里看到"
    if re.search(r"写|文件|write|file", lowered):
        if "write_file" not in called:
            return "tools", [
                tool_call("write_file", {"path": "hello.txt", "content": "hello from cloud-harness\n"}),
                tool_call("list_files", {"path": "."}),
            ]
        results = " ".join(text_of(message) for message in after if message.get("role") == "tool")
        if "denied" in results:
            return "text", "写入请求被拒绝，`hello.txt` 没有创建，后面的列目录也跳过了。"
        return "text", "已写入 `hello.txt`，并列出了工作区的文件。\n\n```text\nhello.txt\n```"
    return "text", INTRO


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def send_json(self, status, value):
        payload = json.dumps(value).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self):
        self.send_json(200, {"object": "list", "data": [{"id": MODEL, "object": "model"}]})

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))) or b"{}")
        kind, value = reply(body)
        if kind == "error":
            self.send_json(503, {"error": {"message": value}})
            return
        calls = [
            {"index": index, "id": f"call_{int(time.time() * 1000)}_{index}", "type": "function", "function": call}
            for index, call in enumerate(value)
        ] if kind == "tools" else []
        usage = {"prompt_tokens": 64, "completion_tokens": 24 if calls else max(1, len(value) // 2), "total_tokens": 0}
        usage["total_tokens"] = usage["prompt_tokens"] + usage["completion_tokens"]
        finish = "tool_calls" if calls else "stop"
        if not body.get("stream"):
            message = {"role": "assistant", "content": None if calls else value}
            if calls:
                message["tool_calls"] = [{key: call[key] for key in ("id", "type", "function")} for call in calls]
            self.send_json(200, {"id": "chatcmpl-demo", "object": "chat.completion", "model": MODEL, "choices": [{"index": 0, "finish_reason": finish, "message": message}], "usage": usage})
            return

        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "close")
        self.end_headers()

        def chunk(delta=None, finish_reason=None, **extra):
            choices = [] if delta is None else [{"index": 0, "delta": delta, "finish_reason": finish_reason}]
            data = {"id": "chatcmpl-demo", "object": "chat.completion.chunk", "model": MODEL, "choices": choices, **extra}
            self.wfile.write(f"data: {json.dumps(data, ensure_ascii=False)}\n\n".encode())
            self.wfile.flush()

        chunk({"role": "assistant", "content": ""})
        time.sleep(0.5)
        if calls:
            chunk({"tool_calls": calls})
        else:
            for piece in re.findall(r"\s+|[^\s]{1,3}", value):
                chunk({"content": piece})
                time.sleep(CHUNK_DELAY)
        chunk({}, finish)
        chunk(None, usage=usage)
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()
        self.close_connection = True

    def log_message(self, _format, *_args):
        pass


def main():
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 19422
    server = ThreadingHTTPServer(("127.0.0.1", port), Handler)
    print(f"demo provider listening on http://127.0.0.1:{server.server_port}/v1", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
