"""End-to-end checks for DBKid v2 mutations (section A) against the disposable
local fixture. Mirrors tests/test_integration.py: start the built DBKid
binary separately and set DBKID_TEST_URL to the loopback test server. The
fixture user dbkid_ddl owns the dbkid_v2_* databases and dbkid_test owns
dbkid_fixture. Tests clean up everything they create so reruns pass, and no
credentials are printed or stored anywhere.
"""

import json
import os
import secrets
import sys
import unittest
import urllib.parse

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from test_integration import Client

MYSQL_PORT = int(os.environ.get("DBKID_TEST_MYSQL_PORT", "62003"))
MUT_DATABASE = "dbkid_v2_mut"

COLUMN_TEMPLATE = {
    "name": "",
    "type": "int",
    "length": "",
    "decimals": "",
    "nullable": False,
    "auto_increment": False,
    "primary": False,
    "unique": False,
    "default": "",
    "default_null": False,
}


def column(name, column_type, **overrides):
    definition = dict(COLUMN_TEMPLATE)
    definition["name"] = name
    definition["type"] = column_type
    definition.update(overrides)
    return definition


class MutationClient(Client):
    def connect_as(self, user):
        status, result, _ = self.call("POST", "/api/connect", {
            "host": "127.0.0.1", "port": MYSQL_PORT, "user": user,
            "password": "", "tls": "disable", "allow_insecure": False,
        })
        if status != 200:
            raise AssertionError("connect failed: HTTP %s" % status)
        return result

    def mutate(self, path, data, confirmed=True, with_request_id=True, request_id=None):
        body = dict(data)
        body["confirmed"] = confirmed
        headers = {}
        if with_request_id:
            headers["X-DBKid-Request-ID"] = request_id or secrets.token_hex(16)
        return self.call("POST", path, body, headers=headers)

    def query(self, sql, database=MUT_DATABASE):
        return self.call("POST", "/api/query", {
            "database": database, "sql": sql, "confirmed": True,
        })


class DBKidV2Mutations(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.base_url = os.environ.get("DBKID_TEST_URL", "")
        parsed = urllib.parse.urlparse(cls.base_url)
        if parsed.scheme != "http" or parsed.hostname != "127.0.0.1":
            raise unittest.SkipTest("DBKID_TEST_URL must identify the loopback test server")

    def setUp(self):
        self.client = MutationClient(self.base_url)
        self.client.bootstrap()
        self.cleanup()

    def tearDown(self):
        self.cleanup()

    def cleanup(self):
        # Tolerate failures: a previous run may have left nothing behind.
        try:
            self.client.connect_as("dbkid_ddl")
        except AssertionError:
            return
        self.client.mutate("/api/database/drop", {"database": MUT_DATABASE})
        try:
            self.client.connect_as("dbkid_test")
        except AssertionError:
            return
        self.client.mutate("/api/row/delete", {
            "database": "dbkid_fixture", "table": "items", "key": {"id": "999001"},
        })

    def test_database_table_and_column_lifecycle(self):
        self.client.connect_as("dbkid_ddl")
        status, result, _ = self.client.mutate("/api/database/create", {"name": MUT_DATABASE})
        self.assertEqual(status, 200, result)
        self.assertIn("affected_rows", result)

        status, result, _ = self.client.mutate("/api/table/create", {
            "database": MUT_DATABASE, "name": "lifecycle", "engine": "InnoDB",
            "columns": [
                column("id", "int", auto_increment=True, primary=True),
                column("name", "varchar", length="64", nullable=False),
                column("note", "text", nullable=True),
                column("raw", "varbinary", length="16", nullable=True),
            ],
        })
        self.assertEqual(status, 200, result)

        status, result, _ = self.client.call("GET", "/api/tables?" + urllib.parse.urlencode({"database": MUT_DATABASE}))
        self.assertEqual(status, 200, result)
        self.assertIn("lifecycle", [table["name"] for table in result["tables"]])

        status, result, _ = self.client.mutate("/api/table/rename", {
            "database": MUT_DATABASE, "table": "lifecycle", "new_name": "renamed",
        })
        self.assertEqual(status, 200, result)

        add_specs = [
            ("", "extra", {}),
            ("first", "extra_first", {}),
            ("after", "extra_after", {"after_column": "id"}),
        ]
        for position, name, extra in add_specs:
            status, result, _ = self.client.mutate("/api/column/add", dict({
                "database": MUT_DATABASE, "table": "renamed",
                "column": column(name, "int", nullable=True),
                "position": position,
            }, **extra))
            self.assertEqual(status, 200, result)

        status, result, _ = self.client.mutate("/api/column/modify", {
            "database": MUT_DATABASE, "table": "renamed", "old_name": "extra",
            "column": column("extra2", "bigint", nullable=False),
        })
        self.assertEqual(status, 200, result)

        status, result, _ = self.client.mutate("/api/index/create", {
            "database": MUT_DATABASE, "table": "renamed", "name": "idx_name",
            "columns": ["name"], "unique": False,
        })
        self.assertEqual(status, 200, result)
        status, result, _ = self.client.mutate("/api/index/create", {
            "database": MUT_DATABASE, "table": "renamed", "name": "uniq_raw",
            "columns": ["raw"], "unique": True,
        })
        self.assertEqual(status, 200, result)

        status, result, _ = self.client.query("SHOW INDEXES FROM `renamed`")
        self.assertEqual(status, 200, result)
        index_names = {row[2] for row in result["rows"]}
        self.assertIn("idx_name", index_names)
        self.assertIn("uniq_raw", index_names)

        status, result, _ = self.client.call("GET", "/api/structure?" + urllib.parse.urlencode({
            "database": MUT_DATABASE, "table": "renamed",
        }))
        self.assertEqual(status, 200, result)
        names = [item["name"] for item in result["columns"]]
        self.assertIn("extra2", names)
        self.assertIn("extra_first", names)
        self.assertNotIn("extra", names)

        status, result, _ = self.client.mutate("/api/index/drop", {
            "database": MUT_DATABASE, "table": "renamed", "name": "idx_name",
        })
        self.assertEqual(status, 200, result)
        status, result, _ = self.client.mutate("/api/column/drop", {
            "database": MUT_DATABASE, "table": "renamed", "column": "extra_first",
        })
        self.assertEqual(status, 200, result)

        status, result, _ = self.client.mutate("/api/table/drop", {"database": MUT_DATABASE, "table": "renamed"})
        self.assertEqual(status, 200, result)
        status, result, _ = self.client.mutate("/api/database/drop", {"database": MUT_DATABASE})
        self.assertEqual(status, 200, result)

        status, result, _ = self.client.call("GET", "/api/databases")
        self.assertEqual(status, 200, result)
        self.assertNotIn(MUT_DATABASE, result["databases"])

    def test_row_crud_on_fixture_items(self):
        self.client.connect_as("dbkid_test")
        status, result, _ = self.client.mutate("/api/row/insert", {
            "database": "dbkid_fixture", "table": "items",
            "values": {
                "id": {"value": "999001"},
                "label": {"value": "v2 mutation row"},
                "quantity": {"value": "7"},
                "note": {"null": True},
                "raw_data": {"binary": True, "value": "FF00AB"},
            },
        })
        self.assertEqual(status, 200, result)
        self.assertEqual(result["affected_rows"], 1)

        status, result, _ = self.client.query(
            "SELECT raw_data, note FROM items WHERE id = 999001", database="dbkid_fixture")
        self.assertEqual(status, 200, result)
        self.assertEqual(result["rows"][0][0].lower(), "0xff00ab")
        self.assertIsNone(result["rows"][0][1])

        status, result, _ = self.client.mutate("/api/row/update", {
            "database": "dbkid_fixture", "table": "items",
            "key": {"id": "999001"},
            "values": {"note": {"value": "updated note"}, "raw_data": {"binary": True, "value": "00FF"}},
        })
        self.assertEqual(status, 200, result)
        self.assertEqual(result["affected_rows"], 1)

        status, result, _ = self.client.query(
            "SELECT note, raw_data FROM items WHERE id = 999001", database="dbkid_fixture")
        self.assertEqual(status, 200, result)
        self.assertEqual(result["rows"], [["updated note", "0x00ff"]])

        status, result, _ = self.client.mutate("/api/row/update", {
            "database": "dbkid_fixture", "table": "items",
            "key": {"label": "v2 mutation row"},
            "values": {"note": {"value": "x"}},
        })
        self.assertEqual(status, 400, result)
        self.assertEqual(result["error"], "a primary key is required")

        status, result, _ = self.client.mutate("/api/row/update", {
            "database": "dbkid_fixture", "table": "item_view",
            "key": {"id": "1"}, "values": {"label": {"value": "x"}},
        })
        self.assertEqual(status, 400, result)
        self.assertEqual(result["error"], "a primary key is required")

        status, result, _ = self.client.mutate("/api/row/insert", {
            "database": "dbkid_fixture", "table": "items",
            "values": {"nonexistent": {"value": "x"}},
        })
        self.assertEqual(status, 400, result)
        self.assertEqual(result["error"], "unknown column")

        status, result, _ = self.client.mutate("/api/row/insert", {
            "database": "dbkid_fixture", "table": "items",
            "values": {"raw_data": {"binary": True, "value": "ZZZZ"}},
        })
        self.assertEqual(status, 400, result)
        self.assertEqual(result["error"], "invalid hex value")

        status, result, _ = self.client.mutate("/api/row/delete", {
            "database": "dbkid_fixture", "table": "items", "key": {"id": "999001"},
        })
        self.assertEqual(status, 200, result)
        self.assertEqual(result["affected_rows"], 1)

        status, result, _ = self.client.mutate("/api/row/delete", {
            "database": "dbkid_fixture", "table": "items", "key": {"id": "999001"},
        })
        self.assertEqual(status, 200, result)
        self.assertEqual(result["affected_rows"], 0)

        status, result, _ = self.client.query(
            "SELECT COUNT(*) FROM items WHERE id = 999001", database="dbkid_fixture")
        self.assertEqual(status, 200, result)
        self.assertEqual(result["rows"], [["0"]])  # cells render as display strings

    def test_import_sql_with_delimiter_routine(self):
        self.client.connect_as("dbkid_ddl")
        status, result, _ = self.client.mutate("/api/database/create", {"name": MUT_DATABASE})
        self.assertEqual(status, 200, result)

        script = (
            "CREATE TABLE mut_import_sql (id INT NOT NULL PRIMARY KEY, name VARCHAR(64) NOT NULL, note VARCHAR(255) NULL);\n"
            "INSERT INTO mut_import_sql (id, name, note) VALUES (1, 'semi;colon', 'doubled '' quote');\n"
            "DELIMITER //\n"
            "CREATE PROCEDURE mut_import_proc()\n"
            "BEGIN\n"
            "  INSERT INTO mut_import_sql (id, name) VALUES (2, 'in;routine');\n"
            "  SELECT 1 AS routine_ran;\n"
            "END//\n"
            "DELIMITER ;\n"
            "INSERT INTO mut_import_sql (id, name) VALUES (3, 'after routine');\n"
        )
        status, result, _ = self.client.mutate("/api/import/sql", {
            "database": MUT_DATABASE, "content": script, "stop_on_error": True,
        })
        self.assertEqual(status, 200, result)
        self.assertEqual(result["executed"], 4)
        self.assertEqual(result["failed"], 0)
        self.assertEqual(result["errors"], [])
        self.assertFalse(result["stopped"])

        status, result, _ = self.client.query("SELECT id, name FROM mut_import_sql ORDER BY id")
        self.assertEqual(status, 200, result)
        self.assertEqual(result["rows"], [["1", "semi;colon"], ["3", "after routine"]])

        status, result, _ = self.client.query(
            "SELECT ROUTINE_NAME FROM INFORMATION_SCHEMA.ROUTINES "
            "WHERE ROUTINE_SCHEMA = 'dbkid_v2_mut' AND ROUTINE_TYPE = 'PROCEDURE'")
        self.assertEqual(status, 200, result)
        self.assertEqual(result["rows"], [["mut_import_proc"]])

        status, result, _ = self.client.mutate("/api/routine/drop", {
            "database": MUT_DATABASE, "name": "mut_import_proc", "kind": "procedure",
        })
        self.assertEqual(status, 200, result)

        # Statement errors are reported; the script keeps going or stops.
        bad_script = (
            "CREATE TABLE mut_err (id INT);\n"
            "INSERT INTO mut_err VALUES (1);\n"
            "THIS IS NOT SQL;\n"
            "INSERT INTO mut_err VALUES (2);\n"
        )
        status, result, _ = self.client.mutate("/api/import/sql", {
            "database": MUT_DATABASE, "content": bad_script, "stop_on_error": False,
        })
        self.assertEqual(status, 200, result)
        self.assertEqual(result["executed"], 3)
        self.assertEqual(result["failed"], 1)
        self.assertFalse(result["stopped"])
        self.assertEqual(result["errors"][0]["index"], 3)
        self.assertTrue(str(result["errors"][0]["error"]).startswith("database error"))
        self.assertIn("THIS IS NOT SQL", result["errors"][0]["preview"])

        status, result, _ = self.client.query("SELECT COUNT(*) FROM mut_err")
        self.assertEqual(status, 200, result)
        self.assertEqual(result["rows"], [["2"]])  # both INSERTs ran; only the bad statement failed

        status, result, _ = self.client.mutate("/api/table/drop", {"database": MUT_DATABASE, "table": "mut_err"})
        self.assertEqual(status, 200, result)

        status, result, _ = self.client.mutate("/api/import/sql", {
            "database": MUT_DATABASE, "content": bad_script, "stop_on_error": True,
        })
        self.assertEqual(status, 200, result)
        self.assertEqual(result["executed"], 2)
        self.assertEqual(result["failed"], 1)
        self.assertTrue(result["stopped"])
        self.assertEqual(result["errors"][0]["index"], 3)

    def test_import_csv_with_header_and_mapping(self):
        self.client.connect_as("dbkid_ddl")
        status, _, _ = self.client.mutate("/api/database/create", {"name": MUT_DATABASE})
        self.assertEqual(status, 200)
        status, result, _ = self.client.mutate("/api/table/create", {
            "database": MUT_DATABASE, "name": "mut_import_csv", "engine": "",
            "columns": [
                column("id", "int", primary=True),
                column("name", "varchar", length="64", nullable=False),
                column("note", "varchar", length="255", nullable=True),
            ],
        })
        self.assertEqual(status, 200, result)

        content = (
            "id;name;note\n"
            "1;Alice;\"quoted;field\"\n"
            "2;Bob;\n"
            "3;\"O\"\"Reilly\";\"multi\nline\"\n"
            "4;plain;unquoted\n"
        )
        status, result, _ = self.client.mutate("/api/import/csv", {
            "database": MUT_DATABASE, "table": "mut_import_csv", "content": content,
            "has_header": True, "delimiter": ";", "quote": "\"",
            "columns": ["id", "name", "note"],
        })
        self.assertEqual(status, 200, result)
        self.assertEqual(result["inserted"], 4)
        self.assertEqual(result["failed"], 0)
        self.assertEqual(result["errors"], [])
        self.assertFalse(result["stopped"])

        status, result, _ = self.client.query("SELECT id, name, note FROM mut_import_csv ORDER BY id")
        self.assertEqual(status, 200, result)
        rows = {row[0]: row for row in result["rows"]}
        self.assertEqual(rows["1"], ["1", "Alice", "quoted;field"])
        self.assertEqual(rows["2"], ["2", "Bob", None])  # unquoted empty on a nullable column
        self.assertEqual(rows["3"], ["3", 'O"Reilly', "multi\nline"])
        self.assertEqual(rows["4"], ["4", "plain", "unquoted"])

        # A mapping that skips a field and misses the NOT NULL column reports
        # the first row of the failing batch and stops.
        status, result, _ = self.client.mutate("/api/import/csv", {
            "database": MUT_DATABASE, "table": "mut_import_csv",
            "content": "10;skipped\n11;skipped\n",
            "has_header": False, "delimiter": ";", "quote": "\"",
            "columns": ["id", None],
        })
        self.assertEqual(status, 200, result)
        self.assertEqual(result["inserted"], 0)
        self.assertEqual(result["failed"], 2)
        self.assertTrue(result["stopped"])
        self.assertEqual(result["errors"][0]["row"], 1)
        self.assertTrue(str(result["errors"][0]["error"]).startswith("database error"))

        status, result, _ = self.client.mutate("/api/import/csv", {
            "database": MUT_DATABASE, "table": "mut_import_csv",
            "content": "1;2\n3;4;5\n", "has_header": False,
            "delimiter": ";", "quote": "\"", "columns": ["id", "name"],
        })
        self.assertEqual(status, 400, result)
        self.assertEqual(result["error"], "inconsistent csv columns")

        status, result, _ = self.client.mutate("/api/import/csv", {
            "database": MUT_DATABASE, "table": "mut_import_csv",
            "content": "1;2\n", "has_header": False,
            "delimiter": ":", "quote": "\"", "columns": ["id", "name"],
        })
        self.assertEqual(status, 400, result)
        self.assertEqual(result["error"], "invalid csv delimiter")

    def test_process_kill_nonexistent_reports_database_error(self):
        self.client.connect_as("dbkid_ddl")
        status, result, _ = self.client.mutate("/api/process/kill", {"id": 999999999})
        self.assertEqual(status, 502, result)
        self.assertTrue(str(result.get("error", "")).startswith("database error"))

    def test_endpoints_require_confirmation_and_request_id(self):
        self.client.connect_as("dbkid_ddl")
        simple_column = column("c", "int")
        endpoints = [
            ("/api/database/create", {"name": "dbkid_gate"}),
            ("/api/database/drop", {"database": "dbkid_gate"}),
            ("/api/table/create", {"database": "d", "name": "t", "engine": "", "columns": [simple_column]}),
            ("/api/table/rename", {"database": "d", "table": "t", "new_name": "u"}),
            ("/api/table/drop", {"database": "d", "table": "t"}),
            ("/api/column/add", {"database": "d", "table": "t", "column": simple_column, "position": ""}),
            ("/api/column/modify", {"database": "d", "table": "t", "old_name": "o", "column": simple_column}),
            ("/api/column/drop", {"database": "d", "table": "t", "column": "c"}),
            ("/api/index/create", {"database": "d", "table": "t", "name": "i", "columns": ["a"], "unique": False}),
            ("/api/index/drop", {"database": "d", "table": "t", "name": "i"}),
            ("/api/routine/drop", {"database": "d", "name": "n", "kind": "procedure"}),
            ("/api/process/kill", {"id": 1}),
            ("/api/row/insert", {"database": "d", "table": "t", "values": {}}),
            ("/api/row/update", {"database": "d", "table": "t", "key": {}, "values": {}}),
            ("/api/row/delete", {"database": "d", "table": "t", "key": {}}),
            ("/api/import/sql", {"database": "d", "content": "SELECT 1", "stop_on_error": False}),
            ("/api/import/csv", {"database": "d", "table": "t", "content": "1\n", "has_header": False,
                                 "delimiter": ",", "quote": "\"", "columns": ["a"]}),
        ]
        for path, data in endpoints:
            with self.subTest(path=path):
                status, result, _ = self.client.mutate(path, data, confirmed=False)
                self.assertEqual(status, 400, (path, result))
                self.assertEqual(result["error"], "explicit SQL confirmation is required")

                status, result, _ = self.client.mutate(path, data, with_request_id=False)
                self.assertEqual(status, 400, (path, result))
                self.assertEqual(result["error"], "a unique SQL request ID is required")

                status, result, _ = self.client.call("GET", path)
                self.assertEqual(status, 405, (path, result))
                self.assertEqual(result["error"], "method not allowed")

    def test_replayed_mutation_request_is_rejected(self):
        self.client.connect_as("dbkid_ddl")
        request_id = secrets.token_hex(16)
        body = {"database": MUT_DATABASE, "table": "nonexistent"}
        status, result, _ = self.client.mutate("/api/table/drop", body, request_id=request_id)
        self.assertEqual(status, 502, result)  # the table does not exist, but the ID was consumed
        status, result, _ = self.client.mutate("/api/table/drop", body, request_id=request_id)
        self.assertEqual(status, 409, result)
        self.assertIn("already submitted", result["error"])


if __name__ == "__main__":
    unittest.main(verbosity=2)
