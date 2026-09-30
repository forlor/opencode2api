#!/usr/bin/env python3
# 本地开发替身：模拟子节点 nginx 的 /gemini/ location 行为——
#   前缀剥离（/gemini/v1beta/... → /v1beta/...）、头白名单透传（x-goog-api-key）、
#   SSE 字节级透传、可选 X-Proxy-Secret 校验。
# 出站统一经本地 HTTP 代理（CONNECT 隧道）访问 Google，用于本机不起 nginx 时端到端测试。
#
# 用法：python dev/gemini_stand_in.py
# 结束：Ctrl-C
import http.client
import http.server
import sys

LISTEN_ADDR = ("127.0.0.1", 18080)
UPSTREAM = "generativelanguage.googleapis.com"
PROXY = ("127.0.0.1", 20808)  # 本地代理；若为 socks5 需改用支持 socks 的客户端
SECRET = ""  # 可选：非空时校验 X-Proxy-Secret（与 config.yaml server.secret 一致）

# 模拟 nginx proxy_set_header 白名单：只透传业务头，鉴权头不出网
PASS_HEADERS = {"x-goog-api-key", "content-type"}


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_GET(self):
        self.forward()

    def do_POST(self):
        self.forward()

    def forward(self):
        if not self.path.startswith("/gemini/"):
            self.send_error(404, "not a /gemini/ path")
            return
        if SECRET and self.headers.get("X-Proxy-Secret") != SECRET:
            self.send_error(403, "bad secret")
            return

        body = None
        if self.headers.get("Content-Length"):
            body = self.rfile.read(int(self.headers["Content-Length"]))

        # HTTP 代理 CONNECT 隧道 → 目标 TLS
        conn = http.client.HTTPSConnection(PROXY[0], PROXY[1], timeout=600)
        conn.set_tunnel(UPSTREAM, 443)
        headers = {k: v for k, v in self.headers.items() if k.lower() in PASS_HEADERS}
        try:
            conn.request(self.command, self.path[len("/gemini"):], body=body, headers=headers)
            resp = conn.getresponse()
        except Exception as e:  # 代理不通 / 隧道失败
            self.send_error(502, "upstream via proxy failed: %r" % (e,))
            return

        self.send_response(resp.status)
        ct = resp.getheader("Content-Type")
        if ct:
            self.send_header("Content-Type", ct)
        # 简化：流式（chunked/SSE）与定长响应都按"读到 EOF + 关连接"交付，
        # 避免 HTTP/1.1 分帧问题；Go 客户端对此完全兼容
        self.send_header("Connection", "close")
        self.close_connection = True
        self.end_headers()
        try:
            while True:
                chunk = resp.read(8192)
                if not chunk:
                    break
                self.wfile.write(chunk)
                self.wfile.flush()
        except Exception:
            pass  # 客户端提前断开（长流取消）
        finally:
            conn.close()

    def log_message(self, fmt, *args):
        sys.stderr.write("[stand-in] %s %s\n" % (self.command, self.path))


if __name__ == "__main__":
    print("gemini stand-in listening on http://%s:%d  ->  https://%s  via proxy %s:%d" %
          (LISTEN_ADDR[0], LISTEN_ADDR[1], UPSTREAM, PROXY[0], PROXY[1]))
    http.server.ThreadingHTTPServer(LISTEN_ADDR, Handler).serve_forever()
