import json
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path


class Handler(BaseHTTPRequestHandler):
    def handle_request(self):
        length = int(self.headers.get("content-length", "0"))
        body = self.rfile.read(length)
        record = {
            "method": self.command,
            "path": self.path,
            "headers": dict(self.headers),
            "body": body.decode("utf-8", "replace"),
        }
        path = Path("/firmware/tmp/udapi-requests.jsonl")
        with path.open("a", encoding="utf-8") as output:
            output.write(json.dumps(record) + "\n")

        encoded = json.dumps(
            {
                "error": 0,
                "message": "Success",
                "detail": "",
                "statusCode": 200,
            }
        ).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Set-Cookie", "TOKEN=firmware-runner; Path=/")
        self.send_header("x-auth-token", "firmware-runner")
        self.send_header("Content-Length", str(len(encoded)))
        self.end_headers()
        self.wfile.write(encoded)

    do_GET = handle_request
    do_POST = handle_request
    do_PUT = handle_request
    do_DELETE = handle_request


HTTPServer(("127.0.0.1", 1080), Handler).serve_forever()
