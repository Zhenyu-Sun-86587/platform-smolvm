"""Hold one successful local smolvm response at a chosen durable dispatch point."""
import contextlib
import http.client
import http.server
import json
import socket
import socketserver
import threading
from urllib.parse import urlsplit


class UnixConnection(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__("localhost", timeout=120)
        self.path = str(path)

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.path)


class Server(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True


class DispatchProxy:
    def __init__(self, path, upstream):
        self.upstream = upstream
        self.response_ready = threading.Event()
        self.release = threading.Event()
        self.trigger = None
        self.count = {}
        self.lock = threading.Lock()
        proxy = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def forward(self):
                body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
                path = urlsplit(self.path).path
                kind = None
                if self.command == "POST":
                    if path == "/api/v1/machines":
                        kind = "create"
                    elif path.endswith("/exec"):
                        payload = json.loads(body)
                        if payload.get("command", [])[-1:] == ["rpc"]:
                            rpc = json.loads(payload["stdin"])
                            if rpc.get("method") == "start":
                                kind = "task_start"
                    elif path.endswith("/pause"):
                        kind = "pause"
                conn = UnixConnection(proxy.upstream)
                try:
                    conn.request(self.command, self.path, body, {
                        "Content-Type": self.headers.get("Content-Type", "application/json")
                    })
                    result = conn.getresponse()
                    data = result.read()
                    if kind:
                        with proxy.lock:
                            proxy.count[kind] = proxy.count.get(kind, 0) + 1
                            hold = proxy.trigger == kind and 200 <= result.status < 300
                            if hold:
                                proxy.trigger = None
                        if hold:
                            proxy.response_ready.set()
                            if not proxy.release.wait(100):
                                raise TimeoutError("test did not release intercepted reply")
                    self.send_response(result.status)
                    self.send_header("Content-Type", result.getheader("Content-Type", "application/json"))
                    self.send_header("Content-Length", str(len(data)))
                    self.end_headers()
                    self.wfile.write(data)
                except (BrokenPipeError, ConnectionResetError):
                    pass
                finally:
                    conn.close()

            do_GET = do_POST = do_PUT = do_DELETE = forward

        self.server = Server(str(path), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)

    def __enter__(self):
        self.thread.start()
        return self

    def __exit__(self, *exc):
        self.release.set()
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()

    @contextlib.contextmanager
    def intercept(self, kind):
        self.response_ready.clear()
        self.release.clear()
        self.trigger = kind
        try:
            yield
        finally:
            self.release.set()
