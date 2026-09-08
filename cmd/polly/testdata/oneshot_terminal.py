"""Local Ollama fixture and POSIX PTYs; no packages, credentials, or network services."""
import concurrent.futures
import fcntl
import json
import os
from pathlib import Path
import pty
import re
import select
import shlex
import signal
import sqlite3
import struct
import subprocess
import sys
import termios
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

binary, directory = sys.argv[1], Path(sys.argv[2])
answer = b"**first** answer.\n\nSecond paragraph.\n"
first = b"**first** "
settled_released = threading.Event()
swarm_source = '''polly.defineWorkflow({name:"client coordination fixture", inputSchema:polly.schema.object({}), async run(){
  const workers=await polly.parallel(["one","two"], label=>polly.agent({task:"swarm fixture worker "+label,label,readOnly:true,tools:[]}),{concurrency:2,errors:"throw_after_all"});
  const results=workers.map(row=>row.value);
  const reviewer=await polly.agent({task:"swarm fixture reviewer",label:"reviewer",input:results,readOnly:true,tools:[]});
  return {tasks:[...results.map(result=>result.task),reviewer.task],review:reviewer.value};
}});'''


class Provider(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b'{"models":[]}')

    def do_POST(self):
        request = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))))
        self.send_response(200)
        self.send_header("Content-Type", "application/x-ndjson")
        self.end_headers()
        if self.path.endswith("/api/show"):
            self.wfile.write(b'{"model_info":{"fixture.context_length":128000}}')
            return
        assert self.path.endswith("/api/chat"), self.path
        prompt = " ".join(m.get("content", "") for m in request["messages"] if m["role"] == "user")

        def emit(message, done=False):
            event = {"model": "fixture", "message": {"role": "assistant", **message}, "done": done}
            if done:
                event.update(done_reason="length" if "cap" in prompt else "stop", prompt_eval_count=1800, eval_count=120)
            self.wfile.write((json.dumps(event)+"\n").encode())
            self.wfile.flush()

        try:
            if any("You are a member of Polly swarm" in m.get("content", "") for m in request["messages"] if m["role"] == "system"):
                assert not request.get("tools"), "tools: [] exposed tools"
                assert "swarm fixture" in prompt, prompt
                emit({"content":"reviewed shared worker results" if "reviewer" in prompt else "published worker result"})
                emit({}, True)
                return
            if "swarm coordination" in prompt:
                completed = {m.get("tool_name"):m.get("content", "") for m in request["messages"] if m["role"] == "tool"}
                calls = []
                if "workflow_run" not in completed:
                    calls = [("workflow_run", {"source":swarm_source,"input":"{}"})]
                elif "swarm_tasks" not in completed:
                    calls = [("swarm_tasks", {})]
                elif "swarm_review" not in completed:
                    tasks = json.loads(completed["swarm_tasks"])
                    assert len(tasks) == 3, tasks
                    calls = [("swarm_review",{"task":task["id"],"revision":task["revision"],"accept":True}) for task in tasks.values()]
                if calls:
                    emit({"content":"coordinating provisional work", "tool_calls":[{"function":{"name":name,"arguments":args}} for name,args in calls]})
                    emit({}, True)
                    return
            if "settled" in prompt and not any(m["role"] == "tool" for m in request["messages"]):
                emit({"content": "provisional narration", "tool_calls": [{"function": {"name": "read_messages", "arguments": {}}}]})
                emit({}, True)
                return
            emit({"thinking": "one\ntwo\nthree\nfour\nfive\nsix"})
            time.sleep(.1)
            if "schema" in prompt:
                emit({"content": '{"value":"first"}'})
            else:
                emit({"content": first.decode()})
                time.sleep(.6)
                emit({"content": answer[len(first):].decode()})
            time.sleep(.25)
            if "settled" in prompt:
                settled_released.set()
            emit({}, True)
        except (BrokenPipeError, ConnectionResetError):
            pass


server = ThreadingHTTPServer(("127.0.0.1", 0), Provider)
threading.Thread(target=server.serve_forever, daemon=True).start()
base = ["--stream", "--model", "ollama/fixture", "--baseurl", f"http://127.0.0.1:{server.server_port}", "--context", "", "--noskills", "--nosandbox", "--system", "Local output fixture", "--maxcontext", "128000", "--maxtokens", "2048"]
schema = directory / "schema.json"
schema.write_text(json.dumps({"type": "object", "properties": {"value": {"type": "string"}}, "required": ["value"], "additionalProperties": False}))


def screen(data):
    """Interpret the renderer's owned-row operations for midpoint assertions."""
    text = data.decode(errors="replace")
    rows, x, y = [[]], 0, 0
    for token in re.findall(r"\x1b\[[0-9;]*[A-Za-z]|[^\x1b]", text):
        if token.startswith("\x1b["):
            if token.endswith("A"):
                y = max(0, y-int(token[2:-1] or 1))
            elif token.endswith("K"):
                rows[y] = []
        elif token == "\r":
            x = 0
        elif token == "\n":
            y, x = y+1, 0
            if y == len(rows):
                rows.append([])
        else:
            while len(rows[y]) <= x:
                rows[y].append(" ")
            rows[y][x] = token
            x += 1
    return "\n".join("".join(row).rstrip() for row in rows)


def run(name, terminal=False, destination="capture", flags=(), color=True, dumb=False, interrupt=False, resize=False, prompt="normal", streaming=True):
    env = {k: v for k, v in os.environ.items() if not k.startswith("POLLYTOOL_") and k not in ("NO_COLOR", "POLLY_TEST_ONESHOT_ARGS")}
    env["TERM"] = "dumb" if dumb else "xterm-256color"
    fixture_home = directory / (name+"-home")
    fixture_home.mkdir()
    env["HOME"] = str(fixture_home)
    if not color:
        env["NO_COLOR"] = "1"
    env["POLLY_TEST_ONESHOT_ARGS"] = json.dumps((base if streaming else base[1:])+["-p", prompt]+list(flags))
    argv = [binary, "-test.run=^TestOneShotCLIProcess$"]
    master = slave = None
    if terminal:
        master, slave = pty.openpty()
        fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 16, 72, 0, 0))
    stdout = slave if terminal and destination == "capture" else subprocess.PIPE
    stderr = slave if terminal else subprocess.PIPE
    path = directory / (name+".stdout")
    if destination in (">", ">>"):
        if destination == ">>":
            path.write_bytes(b"prefix\n")
        argv = ["/bin/sh", "-c", shlex.join(argv)+" "+destination+" "+shlex.quote(str(path))]
        stdout = subprocess.DEVNULL
    elif destination == "pipe":
        argv = ["/bin/sh", "-c", shlex.join(argv)+" | cat"]
    proc = subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=stdout, stderr=stderr, env=env, cwd=fixture_home)
    if slave is not None:
        os.close(slave)
    channels = {}
    if master is not None:
        channels[master] = "terminal"
    if proc.stdout is not None:
        channels[proc.stdout.fileno()] = "stdout"
    if proc.stderr is not None:
        channels[proc.stderr.fileno()] = "stderr"
    data = {key: bytearray() for key in ("stdout", "stderr", "terminal")}
    midpoint = ""
    sent = changed = False
    start = time.monotonic()
    while channels and time.monotonic()-start < 15:
        ready, _, _ = select.select(list(channels), [], [], .05)
        for fd in ready:
            try:
                part = os.read(fd, 65536)
            except OSError:
                part = b""
            if not part:
                channels.pop(fd)
                continue
            if prompt == "settled" and channels[fd] == "stdout":
                assert settled_released.is_set(), (name, "stdout escaped before final settlement", part)
            data[channels[fd]].extend(part)
        if terminal and not midpoint:
            current = screen(data["terminal"])
            if "first" in current and ("--quiet" in flags or "streaming" in current) and proc.poll() is None:
                midpoint = current
                if resize:
                    fcntl.ioctl(master, termios.TIOCSWINSZ, struct.pack("HHHH", 16, 35, 0, 0))
                    changed = True
        if interrupt and not sent and first in data["stdout"]:
            proc.send_signal(signal.SIGINT)
            sent = True
    try:
        code = proc.wait(timeout=2)
    except subprocess.TimeoutExpired:
        proc.kill()
        raise AssertionError((name, "process hung", data))
    finally:
        if master is not None:
            os.close(master)
    if destination in (">", ">>"):
        data["stdout"] = path.read_bytes()
    if terminal:
        (directory / (name+".ansi")).write_bytes(data["terminal"])
        (directory / (name+".screen")).write_text(screen(data["terminal"]))
    assert code == (130 if interrupt else 2 if prompt == "cap" else 0), (name, code, data)
    if terminal and destination == "capture":
        assert midpoint, (name, "answer/footer did not appear before completion", data)
        assert not resize or changed
        if not color:
            assert not re.search(rb"\x1b\[[0-9;]*m", data["terminal"]), (name, "color")
        if not resize:
            settled = screen(data["terminal"])
            assert settled.count("first answer.") == 1 and settled.count("Second paragraph.") == 1, (name, settled)
            assert "streaming" not in settled, (name, settled)
    elif prompt != "schema":
        expected = first if interrupt else answer
        if destination == ">>":
            expected = b"prefix\n"+expected
        assert data["stdout"] == expected, (name, data["stdout"], expected)
    if "--quiet" in flags:
        assert b"Thought" not in data["stderr"] and b"waiting" not in data["stderr"], name
    if "--meta" in flags:
        stderr = data["stderr"]
        meta = stderr.index(b"polly-meta ")
        assert b"\x1b" not in stderr[meta:]
        assert all(line.startswith(b"polly-meta ") for line in stderr[meta:].splitlines()), (name, stderr)
    if dumb or not terminal:
        assert b"\x1b" not in data["stderr"], (name, "redirected status escapes")
    if prompt == "swarm coordination":
        database = fixture_home / ".pollytool" / "polly.db"
        assert database.exists(), "coordination did not promote one-shot memory storage"
        with sqlite3.connect(database) as connection:
            records = {}
            for kind, payload in connection.execute("SELECT kind,payload_json FROM swarm_records"):
                records.setdefault(kind, []).append(json.loads(payload))
        assert len(records["member"]) == 3 and len(records["execution"]) == 3, records
        assert len(records["task"]) == 3 and all(task["status"] == "done" for task in records["task"]), records
        assert len(records["run"]) == 1 and records["run"][0]["status"] == "completed", records
        assert records["workflow"][0]["status"] == "completed", records
    return name, bytes(data["stdout"])


cases = [
    ("default-settled", {"prompt": "settled", "streaming": False}),
    ("swarm-coordination", {"prompt": "swarm coordination", "streaming": False}),
    ("live", {"terminal": True}),
    ("no-color", {"terminal": True, "color": False}),
    ("quiet-live", {"terminal": True, "color": False, "flags": ["--quiet"]}),
    ("resize", {"terminal": True, "resize": True}),
    ("redirect-live", {"terminal": True, "destination": ">"}),
    ("append-live", {"terminal": True, "destination": ">>"}),
    ("pipe", {"destination": "pipe"}),
    ("separate-stderr", {}),
    ("details-meta", {"flags": ["--activity-details", "--meta"]}),
    ("quiet-details", {"flags": ["--quiet", "--activity-details"]}),
    ("dumb", {"dumb": True}),
    ("interrupt", {"interrupt": True}),
    ("cap", {"prompt": "cap"}),
    ("schema", {"prompt": "schema", "flags": ["--schema", str(schema)]}),
    ("schema-details", {"prompt": "schema", "flags": ["--schema", str(schema), "--activity-details", "--meta"]}),
]
try:
    with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
        results = dict(pool.map(lambda case: run(case[0], **case[1]), cases))
    assert results["schema"] == results["schema-details"]
    assert json.loads(results["schema"]) == {"value": "first"}
    print(f"{len(cases)} local-provider output and PTY cases passed")
finally:
    server.shutdown()
