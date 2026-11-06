"""End-to-end checks for DBKid v3 (foreign keys, users and privileges) against
the disposable local MySQL fixture. Read-mostly cleanup discipline: the FK test
drops its own schema, the users test drops its own accounts.
"""

import json
import os
import secrets
import subprocess
import sys
import unittest
import urllib.parse

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from test_integration import Client

MYSQL_PORT = int(os.environ.get("DBKID_TEST_MYSQL_PORT", "62003"))


class V3Client(Client):
    """Connects with a fixture-scoped account and sends replay-protection IDs
    on every mutation."""

    user = "dbkid_ddl"

    def connect(self):
        status, result, _ = self.call("POST", "/api/connect", {
            "host": "127.0.0.1", "port": MYSQL_PORT, "user": self.user,
            "password": "", "tls": "disable", "allow_insecure": False,
        })
        if status != 200:
            raise AssertionError("connect as %s failed: HTTP %s" % (self.user, status))
        return result

    def get(self, path):
        return self.call("GET", path, None, True, None)

    def mutation(self, path, data, request_id=None):
        headers = {"X-DBKid-Request-ID": request_id or secrets.token_hex(16)}
        return self.call("POST", path, data, True, headers)


class DDLClient(V3Client):
    user = "dbkid_ddl"


class AdminClient(V3Client):
    user = "dbkid_admin"


def mysql_login(user, password):
    """Runs a real TCP login; True when the credentials authenticate."""
    try:
        completed = subprocess.run(
            ["mysql", "--no-defaults", "--protocol=tcp", "-h", "127.0.0.1",
             "-P", str(MYSQL_PORT), "-u", user, "-p" + password, "-e", "SELECT 1"],
            capture_output=True, timeout=10)
    except FileNotFoundError:
        raise unittest.SkipTest("the mysql CLI is required for the login check")
    return completed.returncode == 0


class DBKidV3ForeignKeys(unittest.TestCase):
    def setUp(self):
        self.client = DDLClient(os.environ["DBKID_TEST_URL"])
        self.client.bootstrap()
        self.client.connect()
        self.database = "dbkid_v2_fk_probe"

    def tearDown(self):
        cleanup = DDLClient(os.environ["DBKID_TEST_URL"])
        cleanup.bootstrap()
        cleanup.connect()
        cleanup.mutation("/api/database/drop", {
            "database": self.database, "confirmed": True,
        })

    def test_foreign_key_lifecycle(self):
        # Two tables: parent with a primary key, child with an indexed column.
        status, result, _ = self.client.mutation("/api/database/create", {
            "name": self.database, "confirmed": True,
        })
        self.assertEqual(status, 200, result)
        status, result, _ = self.client.mutation("/api/table/create", {
            "database": self.database, "name": "parent", "engine": "", "confirmed": True,
            "columns": [{"name": "id", "type": "int", "nullable": False,
                         "auto_increment": True, "primary": True}],
        })
        self.assertEqual(status, 200, result)
        status, result, _ = self.client.mutation("/api/table/create", {
            "database": self.database, "name": "child", "engine": "", "confirmed": True,
            "columns": [
                {"name": "id", "type": "int", "nullable": False,
                 "auto_increment": True, "primary": True},
                {"name": "parent_id", "type": "int", "nullable": True},
            ],
        })
        self.assertEqual(status, 200, result)
        status, result, _ = self.client.mutation("/api/index/create", {
            "database": self.database, "table": "child", "name": "idx_parent",
            "columns": ["parent_id"], "unique": False, "confirmed": True,
        })
        self.assertEqual(status, 200, result)

        # Validation: mismatched column counts are rejected before any SQL runs.
        status, result, _ = self.client.mutation("/api/foreign-key/add", {
            "database": self.database, "table": "child", "name": "",
            "columns": ["parent_id"], "referenced_database": self.database,
            "referenced_table": "parent", "referenced_columns": [],
            "on_delete": "", "on_update": "", "confirmed": True,
        })
        self.assertEqual(status, 400, result)
        status, result, _ = self.client.mutation("/api/foreign-key/add", {
            "database": self.database, "table": "child", "name": "",
            "columns": ["parent_id"], "referenced_database": self.database,
            "referenced_table": "parent", "referenced_columns": ["id"],
            "on_delete": "Nuke", "on_update": "", "confirmed": True,
        })
        self.assertEqual(status, 400, result)
        self.assertIn("unsupported ON DELETE action", result.get("error", ""))

        # Add a real foreign key.
        status, result, _ = self.client.mutation("/api/foreign-key/add", {
            "database": self.database, "table": "child", "name": "fk_child_parent",
            "columns": ["parent_id"], "referenced_database": self.database,
            "referenced_table": "parent", "referenced_columns": ["id"],
            "on_delete": "CASCADE", "on_update": "RESTRICT", "confirmed": True,
        })
        self.assertEqual(status, 200, result)

        status, result, _ = self.client.get("/api/foreign-keys?" + urllib.parse.urlencode({
            "database": self.database, "table": "child",
        }))
        self.assertEqual(status, 200, result)
        self.assertEqual(len(result["foreign_keys"]), 1, result)
        fk = result["foreign_keys"][0]
        self.assertEqual(fk["name"], "fk_child_parent")
        self.assertEqual(fk["columns"], ["parent_id"])
        self.assertEqual(fk["referenced_table"], "parent")
        self.assertEqual(fk["referenced_columns"], ["id"])
        self.assertEqual(fk["on_delete"], "CASCADE")
        self.assertEqual(fk["on_update"], "RESTRICT")

        # MySQL enforces the constraint: an orphan child row is rejected with
        # the server's own error code (the driver's message text is not echoed).
        status, result, _ = self.client.mutation("/api/row/insert", {
            "database": self.database, "table": "child",
            "values": {"parent_id": {"null": False, "value": "999", "binary": False}},
            "confirmed": True,
        })
        self.assertEqual(status, 502, result)
        self.assertIn("1452", result.get("error", ""),
                      "an orphan row must fail with the foreign-key error code")

        # Drop it again; the list empties.
        status, result, _ = self.client.mutation("/api/foreign-key/drop", {
            "database": self.database, "table": "child",
            "name": "fk_child_parent", "confirmed": True,
        })
        self.assertEqual(status, 200, result)
        status, result, _ = self.client.get("/api/foreign-keys?" + urllib.parse.urlencode({
            "database": self.database, "table": "child",
        }))
        self.assertEqual(status, 200, result)
        self.assertEqual(result["foreign_keys"], [])


class DBKidV3Users(unittest.TestCase):
    def setUp(self):
        self.client = AdminClient(os.environ["DBKID_TEST_URL"])
        self.client.bootstrap()
        self.client.connect()
        self.suffix = secrets.token_hex(4)
        self.user = "dbkid_v3_%s" % self.suffix
        self.host = "127.0.0.1"
        self.password = "probe-pw-%s" % secrets.token_hex(6)

    def tearDown(self):
        try:
            self.client.mutation("/api/user/drop", {
                "user": self.user, "host": self.host, "confirmed": True,
            })
        except Exception:
            pass

    def assert_password_never_surfaces(self, response_text):
        self.assertNotIn(self.password, response_text,
                         "a password must never appear in a response body")

    def test_user_privilege_lifecycle(self):
        # Listing works for the admin tier.
        status, result, _ = self.client.get("/api/users")
        self.assertEqual(status, 200, result)
        self.assertEqual(result["restricted"], False, result)
        self.assertTrue(any(entry["user"] == "dbkid_admin" for entry in result["users"]))

        # Create with a password, then prove it authenticates over TCP.
        status, result, _ = self.client.mutation("/api/user/create", {
            "user": self.user, "host": self.host, "password": self.password,
            "confirmed": True,
        })
        self.assertEqual(status, 200, result)
        self.assert_password_never_surfaces(json.dumps(result))
        self.assertTrue(mysql_login(self.user, self.password),
                        "the created account must accept the chosen password")

        status, result, _ = self.client.get("/api/users")
        self.assertTrue(any(entry["user"] == self.user and entry["host"] == self.host
                            for entry in result["users"]), "the new account must be listed")

        # Grants start at USAGE.
        status, result, _ = self.client.get("/api/grants?" + urllib.parse.urlencode({
            "user": self.user, "host": self.host,
        }))
        self.assertEqual(status, 200, result)
        self.assertTrue(any("USAGE" in line for line in result["grants"]), result)
        self.assertFalse(any("SELECT" in line for line in result["grants"] if "USAGE" not in line))

        # Grant on the fixture database, then verify via SHOW GRANTS.
        grant_request_id = secrets.token_hex(16)
        status, result, _ = self.client.mutation("/api/grant", {
            "user": self.user, "host": self.host,
            "privileges": ["select", "insert"], "database": "dbkid_fixture",
            "table": "", "grant_option": False, "confirmed": True,
        }, request_id=grant_request_id)
        self.assertEqual(status, 200, result)
        status, result, _ = self.client.get("/api/grants?" + urllib.parse.urlencode({
            "user": self.user, "host": self.host,
        }))
        self.assertTrue(any("SELECT, INSERT" in line and "dbkid_fixture" in line
                            for line in result["grants"]), result)

        # A replayed grant request is rejected without re-execution.
        status, result, _ = self.client.mutation("/api/grant", {
            "user": self.user, "host": self.host,
            "privileges": ["select"], "database": "dbkid_fixture",
            "table": "", "grant_option": False, "confirmed": True,
        }, request_id=grant_request_id)
        self.assertEqual(status, 409, "replayed request IDs must be rejected")

        # Unsupported privileges never reach the server.
        status, result, _ = self.client.mutation("/api/grant", {
            "user": self.user, "host": self.host,
            "privileges": ["super"], "database": "dbkid_fixture",
            "table": "", "grant_option": False, "confirmed": True,
        })
        self.assertEqual(status, 400, result)
        self.assertIn("unsupported privilege", result.get("error", ""))

        # Revoke, grants shrink back to USAGE.
        status, result, _ = self.client.mutation("/api/revoke", {
            "user": self.user, "host": self.host,
            "privileges": ["select", "insert"], "database": "dbkid_fixture",
            "table": "", "grant_option": False, "confirmed": True,
        })
        self.assertEqual(status, 200, result)
        status, result, _ = self.client.get("/api/grants?" + urllib.parse.urlencode({
            "user": self.user, "host": self.host,
        }))
        self.assertFalse(any("SELECT, INSERT" in line for line in result["grants"]), result)

        # Password change: the new password works, the old one does not.
        new_password = "probe-pw2-%s" % secrets.token_hex(6)
        status, result, _ = self.client.mutation("/api/user/password", {
            "user": self.user, "host": self.host, "password": new_password,
            "confirmed": True,
        })
        self.assertEqual(status, 200, result)
        self.assert_password_never_surfaces(json.dumps(result))
        self.assertTrue(mysql_login(self.user, new_password),
                        "the new password must authenticate")
        self.assertFalse(mysql_login(self.user, self.password),
                         "the old password must stop working")
        self.password = new_password

        # History must never contain either password.
        status, result, _ = self.client.get("/api/history")
        self.assertEqual(status, 200, result)
        self.assertFalse(any(self.password in entry["sql"] for entry in result["history"]),
                         "privilege endpoints must not record SQL history")

        # Drop the account; it disappears from the list and stops logging in.
        status, result, _ = self.client.mutation("/api/user/drop", {
            "user": self.user, "host": self.host, "confirmed": True,
        })
        self.assertEqual(status, 200, result)
        status, result, _ = self.client.get("/api/users")
        self.assertFalse(any(entry["user"] == self.user for entry in result["users"]),
                         "the dropped account must be gone")
        self.assertFalse(mysql_login(self.user, new_password),
                         "a dropped account must not authenticate")


class DBKidV3RestrictedUsers(unittest.TestCase):
    def test_least_privilege_account_gets_restricted_mode(self):
        client = V3Client(os.environ["DBKID_TEST_URL"])
        client.user = "dbkid_test"
        client.bootstrap()
        client.connect()
        status, result, _ = client.get("/api/users")
        self.assertEqual(status, 200, result)
        self.assertEqual(result["restricted"], True, result)
        self.assertEqual(result["users"], [], result)

        status, result, _ = client.get("/api/grants")
        self.assertEqual(status, 200, result)
        self.assertTrue(result["grants"], "self grants must be visible")

        # The least-privilege account cannot create users; MySQL refuses with
        # its access-denied error code (1227).
        status, result, _ = client.mutation("/api/user/create", {
            "user": "dbkid_denied_%s" % secrets.token_hex(4), "host": "127.0.0.1",
            "password": "x", "confirmed": True,
        })
        self.assertEqual(status, 502, result)
        self.assertIn("1227", result.get("error", ""),
                      "the server must refuse privilege operations for this account")


if __name__ == "__main__":
    unittest.main()
