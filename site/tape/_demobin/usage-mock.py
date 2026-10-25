#!/usr/bin/env python3
"""Local stand-in for the Cursor dashboard API, used ONLY by the isolated demo.

The demo daemon is started with CURSOR_API_ENDPOINT pointing here, so the
Backends/usage panel shows realistic cursor limits without any real account.
Usage: usage-mock.py <port>   (binds 127.0.0.1 only)
"""
import json, sys, time
from http.server import BaseHTTPRequestHandler, HTTPServer


class H(BaseHTTPRequestHandler):
    def do_POST(self):
        self.rfile.read(int(self.headers.get("Content-Length") or 0))
        if self.path.endswith("GetCurrentPeriodUsage"):
            body = {"planUsage": {"totalPercentUsed": 41, "autoPercentUsed": 12, "apiPercentUsed": 63},
                    "billingCycleEnd": str(int((time.time() + 9 * 86400) * 1000))}
        elif self.path.endswith("GetPlanInfo"):
            body = {"planInfo": {"planName": "Pro"}}
        else:
            self.send_error(404)
            return
        raw = json.dumps(body).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def log_message(self, *a):
        pass


HTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
