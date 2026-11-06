"""End-to-end checks against an explicitly supplied disposable MySQL fixture.

Start the built DBKid binary separately, and set DBKID_TEST_URL,
DBKID_TEST_MYSQL_PORT, DBKID_TEST_SCRATCH_ROOT, and DBKID_TEST_DATADIR to the
private fixture before running this script. These checks deliberately execute
approved UPDATE statements; they abort before doing so if the server's data
directory is not the fixture.
"""

import copy
import http.cookiejar
import json
import os
from pathlib import Path
import secrets
import unittest
import urllib.error
import urllib.parse
import urllib.request


class Client:
    def __init__(self, base_url):
        self.base_url = base_url.rstrip("/")
        self.cookies = http.cookiejar.CookieJar()
        self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.cookies))
        self.csrf = None

    def call(self, method, path, data=None, csrf=True, headers=None):
        request_headers = {"Accept": "application/json"}
        if csrf is True and self.csrf:
            request_headers["X-CSRF-Token"] = self.csrf
        elif isinstance(csrf, str):
            request_headers["X-CSRF-Token"] = csrf
        if method == "POST" and path.split("?", 1)[0] == "/api/query":
            request_headers["X-DBKid-Request-ID"] = secrets.token_hex(16)
        if headers:
            request_headers.update(headers)
        if data is not None:
            body = json.dumps(data).encode("utf-8")
            request_headers.setdefault("Content-Type", "application/json")
        else:
            body = None
        request = urllib.request.Request(
            self.base_url + path, data=body, headers=request_headers, method=method
        )
        try:
            response = self.opener.open(request, timeout=15)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            raw = response.read()
            try:
                decoded = json.loads(raw)
            except (json.JSONDecodeError, UnicodeDecodeError):
                decoded = raw.decode("utf-8", errors="replace")
            return response.status, decoded, response.headers

    def bootstrap(self):
        status, result, headers = self.call("GET", "/api/session", csrf=False)
        if status != 200 or not isinstance(result, dict) or not result.get("csrf_token"):
            raise AssertionError("Unable to bootstrap DBKid session: HTTP %s" % status)
        self.csrf = result["csrf_token"]
        return result, headers


class DBKidIntegration(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.base_url = os.environ.get("DBKID_TEST_URL", "")
        cls.mysql_port = os.environ.get("DBKID_TEST_MYSQL_PORT", "")
        cls.expected_datadir = os.environ.get("DBKID_TEST_DATADIR", "")
        scratch_root = os.environ.get("DBKID_TEST_SCRATCH_ROOT", "")
        parsed = urllib.parse.urlparse(cls.base_url)
        if parsed.scheme != "http" or parsed.hostname != "127.0.0.1":
            raise unittest.SkipTest("DBKID_TEST_URL must identify the loopback test server")
        if not cls.mysql_port.isdigit() or not scratch_root or not cls.expected_datadir:
            raise unittest.SkipTest("Explicit private MySQL fixture port, scratch root and datadir required")
        try:
            relative = Path(cls.expected_datadir).resolve().relative_to(Path(scratch_root).resolve())
        except ValueError:
            raise unittest.SkipTest("The fixture datadir must be inside the supplied scratch root")
        if len(relative.parts) != 2 or not relative.parts[0].startswith("dbkid-mysql.") or relative.parts[1] != "data":
            raise unittest.SkipTest("The fixture datadir must be scratch/dbkid-mysql.*/data")

    def setUp(self):
        self.client = Client(self.base_url)
        self.session, self.session_headers = self.client.bootstrap()

    def api(self, method, path, data=None, csrf=True, headers=None):
        return self.client.call(method, path, data, csrf, headers)

    def connect(self):
        status, result, _ = self.api(
            "POST", "/api/connect", {
                "host": "127.0.0.1", "port": int(self.mysql_port),
                "user": "dbkid_test", "password": "", "tls": "disable",
                "allow_insecure": False,
            }
        )
        self.assertEqual(status, 200, result)
        self.assertTrue(result["connected"])
        return result

    def query(self, sql, confirmed=True):
        return self.api("POST", "/api/query", {
            "database": "dbkid_fixture", "sql": sql, "confirmed": confirmed,
        })

    def test_session_cookie_csrf_origin_and_host(self):
        cookie = " ".join(self.session_headers.get_all("Set-Cookie", []))
        self.assertIn("HttpOnly", cookie)
        self.assertIn("SameSite=Strict", cookie)
        self.assertFalse(self.session["connected"])
        status, _, _ = self.api("GET", "/api/databases", csrf=False)
        self.assertIn(status, (401, 403))
        status, _, _ = self.api("POST", "/api/disconnect", {}, csrf=False)
        self.assertIn(status, (401, 403))
        status, _, _ = self.api(
            "GET", "/api/session", csrf=False, headers={"Host": "attacker.invalid"}
        )
        self.assertEqual(status, 403)
        status, _, _ = self.api(
            "GET", "/api/session", csrf=False, headers={"Origin": "https://attacker.invalid"}
        )
        self.assertEqual(status, 403)

        self.connect()
        other = Client(self.base_url)
        other.bootstrap()
        status, result, _ = other.call("POST", "/api/connect", {
            "host": "127.0.0.1", "port": int(self.mysql_port),
            "user": "dbkid_test", "password": "", "tls": "disable",
            "allow_insecure": False,
        })
        self.assertEqual(status, 200, result)
        status, _, _ = other.call("GET", "/api/databases", csrf=self.client.csrf)
        self.assertEqual(status, 403)
        status, result, _ = other.call("GET", "/api/databases")
        self.assertEqual(status, 200, result)
        status, result, _ = other.call("POST", "/api/disconnect", {})
        self.assertEqual(status, 200, result)
        status, result, _ = self.api("GET", "/api/databases")
        self.assertEqual(status, 200, result)  # The first session is still connected.

    def test_oversized_result_is_bounded_or_rejected(self):
        self.connect()
        status, result, _ = self.query("SELECT REPEAT('x', 2097152) AS oversized_value")
        if status == 200:
            value = result["rows"][0][0]
            self.assertIsInstance(value, str)
            self.assertLessEqual(len(value), 1048576, "oversized cell was returned without a reasonable bound")
            self.assertIn("truncated", value.lower(), "a shortened value must be visibly marked")
        else:
            self.assertGreaterEqual(status, 400)
        status, result, _ = self.api("GET", "/api/databases")
        self.assertEqual(status, 200, result)  # A large result must not kill the application.

    def test_wire_limit_rejects_large_mysql_result(self):
        self.connect()
        status, result, _ = self.query("SELECT @@max_allowed_packet")
        self.assertEqual(status, 200, result)
        if int(result["rows"][0][0]) < 12582912:
            self.skipTest("fixture MySQL max_allowed_packet is below the test payload")
        status, result, _ = self.query("SELECT REPEAT('x', 12582912)")
        self.assertGreaterEqual(status, 400, "an oversized wire response must be rejected")
        self.assertIn("size limit", result.get("error", "").lower())
        status, _, _ = self.api("GET", "/api/databases")
        self.assertEqual(status, 200, "application must remain available after a large result")

    def test_untrusted_fixture_tls_certificate_is_not_downgraded(self):
        status, _, _ = self.api("POST", "/api/connect", {
            "host": "127.0.0.1", "port": int(self.mysql_port),
            "user": "dbkid_test", "password": "", "tls": "verify",
            "allow_insecure": False,
        })
        self.assertGreaterEqual(status, 400, "the untrusted fixture certificate must be rejected")
        status, _, _ = self.api("GET", "/api/databases")
        self.assertIn(status, (401, 403, 409))
        self.connect()  # The same fixture is reachable with explicitly selected plaintext.

    def test_stolen_cookie_alone_cannot_recover_connected_session(self):
        # Cookies are shared across ports on the same host. A separate local
        # service that sees a cookie must not be able to recover its CSRF token
        # or the connected database session by calling bootstrap.
        self.connect()
        attacker = Client(self.base_url)
        for cookie in self.client.cookies:
            attacker.cookies.set_cookie(copy.copy(cookie))
        status, result, _ = attacker.call("GET", "/api/session", csrf=False)
        if status == 200:
            self.assertFalse(result["connected"], "cookie replay recovered a connected session")
            self.assertTrue(result["csrf_token"] != self.client.csrf, "cookie replay recovered the session secret")
        else:
            self.assertIn(status, (401, 403))
        status, result, _ = self.api("GET", "/api/databases")
        self.assertEqual(status, 200, result)  # Victim session remains active.

    def test_mysql_browse_structure_and_approved_sql(self):
        self.connect()
        status, result, _ = self.api("GET", "/api/databases")
        self.assertEqual(status, 200, result)
        self.assertIn("dbkid_fixture", result["databases"])

        # Failed reconnect must not silently replace the working session.
        status, _, _ = self.api("POST", "/api/connect", {
            "host": "127.0.0.1", "port": int(self.mysql_port),
            "user": "dbkid_nonexistent_test_user", "password": "", "tls": "disable",
            "allow_insecure": False,
        })
        self.assertGreaterEqual(status, 400)
        status, result, _ = self.api("GET", "/api/databases")
        self.assertEqual(status, 200, result)
        self.assertIn("dbkid_fixture", result["databases"])

        database = urllib.parse.urlencode({"database": "dbkid_fixture"})
        status, result, _ = self.api("GET", "/api/tables?" + database)
        self.assertEqual(status, 200, result)
        tables = {table["name"]: table["type"] for table in result["tables"]}
        self.assertEqual(tables["items"], "BASE TABLE")
        self.assertEqual(tables["item_view"], "VIEW")
        self.assertEqual(tables["odd`table"], "BASE TABLE")

        params = urllib.parse.urlencode({"database": "dbkid_fixture", "table": "odd`table"})
        status, result, _ = self.api("GET", "/api/structure?" + params)
        self.assertEqual(status, 200, result)
        self.assertEqual(result["columns"][0]["name"], "weird`column")
        status, result, _ = self.api("GET", "/api/rows?" + params)
        self.assertEqual(status, 200, result)
        self.assertEqual(result["columns"], ["weird`column"])
        self.assertEqual(result["rows"], [["quoted identifier works"]])

        collected = []
        for page, count, has_next in ((1, 50, True), (2, 50, True), (3, 25, False)):
            params = urllib.parse.urlencode({
                "database": "dbkid_fixture", "table": "items", "page": page, "page_size": 50,
            })
            status, result, _ = self.api("GET", "/api/rows?" + params)
            self.assertEqual(status, 200, result)
            self.assertEqual(len(result["rows"]), count)
            self.assertEqual(result["has_next"], has_next)
            self.assertEqual(result["page"], page)
            collected.extend(dict(zip(result["columns"], row)) for row in result["rows"])
        self.assertEqual(len(collected), 125)
        by_id = {int(row["id"]): row for row in collected}
        self.assertIsNone(by_id[4]["note"])
        self.assertEqual(by_id[1]["raw_data"].lower(), "0xff00")
        self.assertEqual(by_id[2]["note"], "")
        self.assertEqual(by_id[3]["label"], "<img src=x onerror=alert(1)>")

        # Verify the connected server is the explicitly-created scratch instance
        # before exercising any approved data-changing statement.
        status, result, _ = self.query("SELECT @@datadir AS datadir")
        self.assertEqual(status, 200, result)
        actual_datadir = os.path.realpath(result["rows"][0][0])
        self.assertEqual(actual_datadir, os.path.realpath(self.expected_datadir))

        status, result, _ = self.query(
            "WITH RECURSIVE n AS (SELECT 1 AS i UNION ALL SELECT i + 1 FROM n WHERE i < 501) "
            "SELECT i FROM n"
        )
        self.assertEqual(status, 200, result)
        self.assertEqual(len(result["rows"]), 500)
        self.assertTrue(result["truncated"])

        # A database name must not be reinterpreted as MySQL driver DSN parameters.
        status, _, _ = self.api("POST", "/api/query", {
            "database": "dbkid_fixture?allowAllFiles=true&", "sql": "SELECT 1", "confirmed": True,
        })
        self.assertGreaterEqual(status, 400)

        status, _, _ = self.query(
            "UPDATE items SET note = 'should-not-run' WHERE id = 1", confirmed=False
        )
        self.assertIn(status, (400, 403))
        status, result, _ = self.query("SELECT note FROM items WHERE id = 1")
        self.assertEqual(status, 200, result)
        self.assertNotEqual(result["rows"][0][0], "should-not-run")

        status, result, _ = self.query("UPDATE items SET note = 'approved-fixture-write' WHERE id = 1")
        self.assertEqual(status, 200, result)
        self.assertIn(result["affected_rows"], (0, 1))
        status, result, _ = self.query("SELECT note FROM items WHERE id = 1")
        self.assertEqual(status, 200, result)
        self.assertEqual(result["rows"], [["approved-fixture-write"]])

        status, result, _ = self.query("SELECT quantity FROM items WHERE id = 2")
        self.assertEqual(status, 200, result)
        initial_quantity = int(result["rows"][0][0])
        request_id = secrets.token_hex(16)
        replay_body = {
            "database": "dbkid_fixture", "sql": "UPDATE items SET quantity = quantity + 1 WHERE id = 2", "confirmed": True,
        }
        status, result, _ = self.api("POST", "/api/query", replay_body, headers={"X-DBKid-Request-ID": request_id})
        self.assertEqual(status, 200, result)
        status, _, _ = self.api("POST", "/api/query", replay_body, headers={"X-DBKid-Request-ID": request_id})
        self.assertEqual(status, 409, "an identical replay must not execute twice")
        status, result, _ = self.query("SELECT quantity FROM items WHERE id = 2")
        self.assertEqual(status, 200, result)
        self.assertEqual(int(result["rows"][0][0]), initial_quantity + 1)

        status, result, _ = self.query("SELECT 'semi;colon' AS value")
        self.assertEqual(status, 200, result)
        self.assertEqual(result["rows"], [["semi;colon"]])
        status, _, _ = self.query("SELECT 1; SELECT 2")
        self.assertGreaterEqual(status, 400, "multiple SQL statements must be refused")
        status, _, _ = self.query("CALL dbkid_late_error()")
        self.assertGreaterEqual(status, 400, "a later stored-procedure error must not be reported as success")
        status, result, _ = self.query("CALL dbkid_one_result()")
        self.assertEqual(status, 200, result)
        self.assertEqual(result["rows"], [["1"]])
        status, result, _ = self.query("CALL dbkid_two_results()")
        self.assertGreaterEqual(status, 400, "multiple result sets must not be reported as complete")
        self.assertIn("multiple", result.get("error", "").lower())

        status, result, _ = self.api("POST", "/api/disconnect", {})
        self.assertEqual(status, 200, result)
        self.assertFalse(result["connected"])
        status, _, _ = self.api("GET", "/api/databases")
        self.assertIn(status, (401, 403, 409))


if __name__ == "__main__":
    unittest.main(verbosity=2)
