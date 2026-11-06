"""End-to-end checks for DBKid v2 reads (section B) against the disposable
local fixture. Same pattern as tests/test_integration.py: start the built
DBKid binary separately and set DBKID_TEST_URL. Uses the read-only dbkid_test
account on dbkid_fixture. Read-only: no cleanup needed.
"""

import json
import os
import secrets
import sys
import unittest
import urllib.error
import urllib.parse
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from test_integration import Client

MYSQL_PORT = int(os.environ.get("DBKID_TEST_MYSQL_PORT", "62003"))


class ReadsClient(Client):
    def connect(self):
        status, result, _ = self.call("POST", "/api/connect", {
            "host": "127.0.0.1", "port": MYSQL_PORT, "user": "dbkid_test",
            "password": "", "tls": "disable", "allow_insecure": False,
        })
        if status != 200:
            raise AssertionError("connect failed: HTTP %s" % status)
        return result

    def get(self, path, csrf=True, headers=None):
        return self.call("GET", path, None, csrf, headers)

    def query(self, sql, confirmed=True):
        return self.call("POST", "/api/query", {
            "database": "dbkid_fixture", "sql": sql, "confirmed": confirmed,
        })

    def post_token(self, path, data):
        return self.call("POST", path, data, True,
                         {"X-DBKid-Request-ID": secrets.token_hex(16)})

    def download(self, url):
        request = urllib.request.Request(self.base_url + url, headers={"Accept": "*/*"})
        try:
            response = self.opener.open(request, timeout=15)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            body = response.read()
            return response.status, body, dict(response.headers)


class DBKidV2Reads(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        base = os.environ.get("DBKID_TEST_URL", "")
        if not base or "127.0.0.1" not in base:
            raise unittest.SkipTest("DBKID_TEST_URL must identify the loopback test server")

    def setUp(self):
        self.client = ReadsClient(os.environ["DBKID_TEST_URL"])
        self.client.bootstrap()
        self.client.connect()

    def test_sort_and_filter_browsing(self):
        status, result, _ = self.client.get(
            "/api/rows?database=dbkid_fixture&table=items&page=1&page_size=25&sort=id&dir=desc")
        self.assertEqual(status, 200, result)
        self.assertTrue(result["rows"], "sorted browse must return rows")
        ids = [int(row[0]) for row in result["rows"]]
        self.assertEqual(ids, sorted(ids, reverse=True), "descending sort must apply")

        filt = json.dumps([{"column": "note", "op": "like", "value": "fixture-write"}])
        status, result, _ = self.client.get(
            "/api/rows?database=dbkid_fixture&table=items&page=1&page_size=25&filter="
            + urllib.parse.quote(filt))
        self.assertEqual(status, 200, result)
        self.assertTrue(result["rows"], "the like filter must find the fixture row")
        note_index = result["columns"].index("note")
        for row in result["rows"]:
            self.assertIn("fixture-write", row[note_index])

        status, result, _ = self.client.get(
            "/api/rows?database=dbkid_fixture&table=items&page=1&page_size=25&sort=nope&dir=sideways")
        self.assertEqual(status, 400, "invalid sort or filter must be rejected")
        self.assertIn("invalid sort or filter", result.get("error", ""))

        bad = json.dumps([{"column": "note", "op": "drop table", "value": "x"}])
        status, result, _ = self.client.get(
            "/api/rows?database=dbkid_fixture&table=items&page=1&page_size=25&filter="
            + urllib.parse.quote(bad))
        self.assertEqual(status, 400, "unknown filter operator must be rejected")

    def test_row_fetch_by_primary_key(self):
        status, result, _ = self.client.get("/api/row?database=dbkid_fixture&table=items&key="
                                     + urllib.parse.quote(json.dumps([["id", "1"]])))
        self.assertEqual(status, 200, result)
        self.assertEqual(len(result["columns"]), len(result["values"]))
        self.assertEqual(len(result["binary"]), len(result["values"]))

        status, result, _ = self.client.get("/api/row?database=dbkid_fixture&table=items&key="
                                     + urllib.parse.quote(json.dumps([["note", "nope"]])))
        self.assertEqual(status, 400, "a non-primary key must be rejected")

    def test_indexes_routines_and_definition(self):
        status, result, _ = self.client.get("/api/indexes?database=dbkid_fixture&table=items")
        self.assertEqual(status, 200, result)
        primary = [index for index in result["indexes"] if index["name"] == "PRIMARY"]
        self.assertTrue(primary and primary[0]["unique"], "items must have a unique PRIMARY index")

        status, result, _ = self.client.get("/api/routines?database=dbkid_fixture")
        self.assertEqual(status, 200, result)
        names = [routine["name"] for routine in result["routines"]]
        self.assertIn("dbkid_one_result", names)

        status, result, _ = self.client.get(
            "/api/routine?database=dbkid_fixture&name=dbkid_one_result&kind=procedure")
        self.assertEqual(status, 200, result)
        self.assertIn("CREATE", result["definition"])

    def test_database_search(self):
        status, result, _ = self.client.get(
            "/api/search?database=dbkid_fixture&q=fixture-write&mode=like")
        self.assertEqual(status, 200, result)
        tables = [entry["table"] for entry in result["results"]]
        self.assertIn("items", tables, "search must find the items table")
        entry = [item for item in result["results"] if item["table"] == "items"][0]
        self.assertTrue(entry["rows"])

        status, result, _ = self.client.get("/api/search?database=dbkid_fixture&q=&mode=like")
        self.assertEqual(status, 400, "an empty search must be rejected")

    def test_status_history_and_recent(self):
        status, result, _ = self.client.get("/api/status")
        self.assertEqual(status, 200, result)
        self.assertTrue(result["status"] and result["variables"] and result["processes"],
                        "status must expose variables, status counters and processes")

        marker = "SELECT %d AS reads_marker" % secrets.randbelow(1000000)
        status, _, _ = self.client.query(marker)
        self.assertEqual(status, 200)
        status, result, _ = self.client.get("/api/history")
        self.assertEqual(status, 200, result)
        self.assertIn(marker, [entry["sql"] for entry in result["history"]],
                      "a successful statement must be recorded in history")

        status, result, _ = self.client.call("POST", "/api/history/clear", {})
        self.assertEqual(status, 200, result)
        status, result, _ = self.client.get("/api/history")
        self.assertEqual(result["history"], [], "history must be empty after clearing")

        status, result, _ = self.client.get("/api/recent")
        self.assertEqual(status, 200, result)
        self.assertTrue(any(entry["user"] == "dbkid_test" and entry["port"] == MYSQL_PORT
                            for entry in result["recent"]), "the current connection must be recent")
        for entry in result["recent"]:
            self.assertNotIn("password", entry, "recent connections must never carry a password")

    def test_blob_download_is_token_gated_and_single_use(self):
        status, result, _ = self.client.post_token("/api/blob/token", {
            "database": "dbkid_fixture", "table": "v2_blob", "column": "data",
            "key": [["id", "1"]],
        })
        self.assertEqual(status, 200, result)
        url = result["url"]
        self.assertIn("/api/blob?token=", url)

        status, body, headers = self.client.download(url)
        self.assertEqual(status, 200, headers)
        self.assertIn("image/png", headers.get("Content-Type", ""))
        self.assertTrue(body.startswith(b"\x89PNG"), "the PNG magic must survive")

        status, _, _ = self.client.download(url)
        self.assertEqual(status, 403, "a blob token must be single-use")

        status, result, _ = self.client.post_token("/api/blob/token", {
            "database": "dbkid_fixture", "table": "v2_blob", "column": "note",
            "key": [["id", "1"]],
        })
        self.assertEqual(status, 400, "a non-binary column must be rejected for blob downloads")

    def test_export_csv_and_sql_with_token_gate(self):
        status, result, _ = self.client.post_token("/api/export/token", {
            "database": "dbkid_fixture", "table": "items", "format": "csv", "what": "data",
        })
        self.assertEqual(status, 200, result)
        status, body, headers = self.client.download(result["url"])
        self.assertEqual(status, 200, headers)
        self.assertIn("text/csv", headers.get("Content-Type", ""))
        self.assertIn(b"fixture-write", body)
        status, _, _ = self.client.download(result["url"])
        self.assertEqual(status, 403, "an export token must be single-use")

        status, result, _ = self.client.post_token("/api/export/token", {
            "database": "dbkid_fixture", "table": "items", "format": "sql", "what": "both",
        })
        self.assertEqual(status, 200, result)
        status, body, headers = self.client.download(result["url"])
        self.assertEqual(status, 200, headers)
        self.assertIn(b"CREATE TABLE", body)
        self.assertIn(b"INSERT INTO", body)
        self.assertIn(b"fixture-write", body)

        status, result, _ = self.client.post_token("/api/export/token", {
            "database": "dbkid_fixture", "table": "items", "format": "exe", "what": "data",
        })
        self.assertEqual(status, 400, "unknown export formats must be rejected")


if __name__ == "__main__":
    unittest.main()
