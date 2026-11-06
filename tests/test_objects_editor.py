"""Opt-in integration checks for object editing against a disposable MySQL server.

Requires DBKID_TEST_URL, DBKID_TEST_MYSQL_PORT, DBKID_TEST_DATADIR and
DBKID_TEST_SCRATCH_ROOT. Before any mutation, the test checks the actual
server's @@datadir against the supplied fixture. It creates a unique schema
with synthetic objects, never alters an existing schema, and retains the new
schema for inspection.
"""

import copy
import os
from pathlib import Path
import secrets
import unittest
import urllib.error
import urllib.parse

from test_integration import Client


class ObjectEditorIntegration(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        base = os.environ.get('DBKID_TEST_URL', '')
        mysql_port = os.environ.get('DBKID_TEST_MYSQL_PORT', '')
        expected = os.environ.get('DBKID_TEST_DATADIR', '')
        root = os.environ.get('DBKID_TEST_SCRATCH_ROOT', '')
        parsed = urllib.parse.urlparse(base)
        if parsed.scheme != 'http' or parsed.hostname != '127.0.0.1' or not mysql_port.isdigit() or not expected or not root:
            raise unittest.SkipTest('An explicit loopback server and disposable fixture are required')
        resolved_root, resolved_expected = Path(root).resolve(), Path(expected).resolve()
        try:
            relative = resolved_expected.relative_to(resolved_root)
        except ValueError:
            raise unittest.SkipTest('Fixture datadir must be within the supplied scratch root')
        if len(relative.parts) != 2 or not relative.parts[0].startswith('dbkid-mysql.') or relative.parts[1] != 'data':
            raise unittest.SkipTest('Expected scratch/dbkid-mysql.*/data fixture')
        cls.client = Client(base)
        cls.client.bootstrap()
        status, result, _ = cls.client.call('POST', '/api/connect', {
            'host': '127.0.0.1', 'port': int(mysql_port), 'user': 'dbkid_ddl',
            'password': '', 'tls': 'disable', 'allow_insecure': False,
        })
        if status != 200:
            raise AssertionError('Fixture connection failed: HTTP %s' % status)
        cls.generation = result['generation']
        status, result, _ = cls.client.call('POST', '/api/query', {
            'database': '', 'sql': 'SELECT @@datadir', 'confirmed': True,
        })
        if status != 200 or Path(result['rows'][0][0]).resolve() != resolved_expected:
            raise AssertionError('Actual server datadir does not match the disposable fixture')
        cls.database = 'dbkid_v2_obj_' + secrets.token_hex(6)
        status, result, _ = cls.client.call('POST', '/api/database/create',
            {'name': cls.database, 'confirmed': True},
            headers={'X-DBKid-Request-ID': secrets.token_hex(16)})
        if status != 200:
            raise AssertionError('Disposable schema creation failed: %s' % result)
        print('Disposable test schema:', cls.database, flush=True)

    def execute(self, sql):
        status, result, _ = self.client.call('POST', '/api/query', {
            'database': self.database, 'sql': sql, 'confirmed': True,
        })
        self.assertEqual(status, 200, result)
        return result

    def definition(self, kind, name):
        params = urllib.parse.urlencode({'database': self.database, 'name': name, 'kind': kind})
        status, result, headers = self.client.call('GET', '/api/object/definition?' + params)
        self.assertEqual(status, 200, result)
        self.assertEqual(result['name'], name)
        self.assertEqual(result['kind'], kind)
        self.assertIs(result['truncated'], False)
        self.assertTrue(result['definition'])
        self.assertIn('no-store', headers.get('Cache-Control', ''))
        self.assertEqual(result['database'], self.database)
        self.assertEqual(result['generation'], self.generation)
        self.assertIn('sql_mode', result['metadata'])
        return result

    def create_object(self, kind, name, sql):
        status, result, _ = self.client.call('POST', '/api/object/create', {
            'database': self.database, 'name': name, 'kind': kind,
            'sql': sql, 'generation': self.generation, 'confirmed': True,
        }, headers={'X-DBKid-Request-ID': secrets.token_hex(16)})
        self.assertEqual(status, 200, result)
        return result

    def test_create_and_inspect_all_object_kinds(self):
        self.execute('CREATE TABLE probe (id INT PRIMARY KEY, val INT)')
        cases = [
            ('procedure', 'p_test',
             'CREATE PROCEDURE p_test() BEGIN SELECT 20; END', 'SELECT 20'),
            ('trigger', 'tr_test',
             'CREATE TRIGGER tr_test BEFORE INSERT ON probe FOR EACH ROW SET NEW.val = 20', 'NEW.val = 20'),
            ('event', 'ev_test',
             'CREATE EVENT ev_test ON SCHEDULE EVERY 1 DAY DISABLE DO SELECT 20', 'SELECT 20'),
        ]
        for kind, name, sql, marker in cases:
            with self.subTest(kind=kind):
                self.create_object(kind, name, sql)
                created = self.definition(kind, name)
                self.assertIn(marker.lower(), created['definition'].lower())
                wrong = 'CREATE ' + kind.upper() + ' wrong_name ' + ('() SELECT 1' if kind == 'procedure' else ('BEFORE INSERT ON probe FOR EACH ROW SET NEW.val=1' if kind == 'trigger' else 'ON SCHEDULE EVERY 1 DAY DO SELECT 1'))
                status, failure, _ = self.client.call('POST', '/api/object/replace', {
                    'original': created, 'sql': wrong, 'confirmed': True,
                }, headers={'X-DBKid-Request-ID': secrets.token_hex(16)})
                self.assertEqual(status, 400, failure)
                self.assertEqual(failure['stage'], 'not_started')
                self.assertEqual(self.definition(kind, name)['definition'], created['definition'])
                for mutation in ('generation', 'context'):
                    modified = copy.deepcopy(created)
                    if mutation == 'generation':
                        modified['generation'] += 1
                    else:
                        modified['metadata']['collation_connection'] = 'unsupported_test_collation'
                    status, failure, _ = self.client.call('POST', '/api/object/replace', {
                        'original': modified, 'sql': created['definition'], 'confirmed': True,
                    }, headers={'X-DBKid-Request-ID': secrets.token_hex(16)})
                    self.assertNotEqual(status, 200, failure)
                    self.assertEqual(failure['stage'], 'not_started')
                    self.assertEqual(self.definition(kind, name)['definition'], created['definition'])
                if kind == 'procedure':
                    try:
                        status, failure, _ = self.client.call('POST', '/api/object/replace', {
                            'original': created, 'sql': created['definition'] + ' ' * (1024 * 1024), 'confirmed': True,
                        }, headers={'X-DBKid-Request-ID': secrets.token_hex(16)})
                    except urllib.error.URLError:
                        # The server can close the connection before the client
                        # finishes transmitting the rejected oversized request.
                        # Do not retry: check the original in a separate read.
                        pass
                    else:
                        self.assertEqual(status, 413, failure)
                        self.assertEqual(failure['stage'], 'not_started')
                    self.assertEqual(self.definition(kind, name)['definition'], created['definition'])
                updated_sql = created['definition'].replace(marker, marker.replace('20', '30'), 1)
                self.assertNotEqual(updated_sql, created['definition'])
                request_id = secrets.token_hex(16)
                payload = {'original': created, 'sql': updated_sql, 'confirmed': True}
                status, outcome, _ = self.client.call('POST', '/api/object/replace', payload,
                    headers={'X-DBKid-Request-ID': request_id})
                self.assertEqual(status, 200, outcome)
                self.assertEqual(outcome['stage'], 'replaced')
                changed = self.definition(kind, name)['definition']
                self.assertIn(marker.lower().replace('20', '30'), changed.lower())
                status, replay, _ = self.client.call('POST', '/api/object/replace', payload,
                    headers={'X-DBKid-Request-ID': request_id})
                self.assertEqual(status, 409, replay)
                self.assertEqual(replay['stage'], 'replay_unknown')
                changed_payload = dict(payload, sql='SELECT 1')
                status, replay, _ = self.client.call('POST', '/api/object/replace', changed_payload,
                    headers={'X-DBKid-Request-ID': request_id})
                self.assertEqual(status, 409, replay)
                self.assertEqual(replay['stage'], 'replay_unknown')
                self.assertEqual(self.definition(kind, name)['definition'], changed)
        status, _, _ = self.client.call('POST', '/api/object/create', {
            'database': self.database, 'name': 'intended', 'kind': 'procedure',
            'sql': 'CREATE PROCEDURE wrong_target() SELECT 1', 'generation': self.generation, 'confirmed': True,
        }, headers={'X-DBKid-Request-ID': secrets.token_hex(16)})
        self.assertEqual(status, 400, 'a mismatched target must be rejected')
        result = self.execute("SELECT COUNT(*) FROM information_schema.routines WHERE routine_schema=DATABASE() AND routine_name='wrong_target'")
        self.assertEqual(str(result['rows'][0][0]), '0')
        self.execute('INSERT INTO probe (id, val) VALUES (1, 0)')
        result = self.execute('SELECT val FROM probe WHERE id = 1')
        self.assertEqual(str(result['rows'][0][0]), '30')

        # This unique fixture-only procedure exercises a deliberately rejected
        # CREATE after the replacement endpoint has dropped the same object.
        self.create_object('procedure', 'bad_syntax', 'CREATE PROCEDURE bad_syntax() SELECT 1')
        original = self.definition('procedure', 'bad_syntax')
        status, failure, _ = self.client.call('POST', '/api/object/replace', {
            'original': original, 'sql': 'CREATE PROCEDURE bad_syntax() NOT VALID SQL', 'confirmed': True,
        }, headers={'X-DBKid-Request-ID': secrets.token_hex(16)})
        self.assertEqual(status, 502, failure)
        self.assertEqual(failure['stage'], 'dropped_create_failed')
        self.assertIn('CREATE', original['definition'])
        result = self.execute("SELECT COUNT(*) FROM information_schema.routines WHERE routine_schema=DATABASE() AND routine_name='bad_syntax'")
        self.assertEqual(str(result['rows'][0][0]), '0')

        # MySQL may accept a past, non-preserved event but immediately discard it.
        # Successful CREATE must not be reported as a verified replacement.
        self.create_object('event', 'expires_immediately',
            'CREATE EVENT expires_immediately ON SCHEDULE EVERY 1 DAY DISABLE DO SELECT 1')
        original = self.definition('event', 'expires_immediately')
        status, outcome, _ = self.client.call('POST', '/api/object/replace', {
            'original': original,
            'sql': "CREATE EVENT expires_immediately ON SCHEDULE AT '2000-01-01 00:00:00' ON COMPLETION NOT PRESERVE ENABLE DO SELECT 1",
            'confirmed': True,
        }, headers={'X-DBKid-Request-ID': secrets.token_hex(16)})
        self.assertNotEqual(outcome['stage'], 'replaced', outcome)
        self.assertIn(outcome['stage'], ('created_unverified', 'create_unknown', 'dropped_create_failed'))

        status, outcome, _ = self.client.call('POST', '/api/object/create', {
            'database': self.database, 'name': 'past_at_creation', 'kind': 'event',
            'generation': self.generation,
            'sql': "CREATE EVENT past_at_creation ON SCHEDULE AT '2000-01-01 00:00:00' ON COMPLETION NOT PRESERVE ENABLE DO SELECT 1",
            'confirmed': True,
        }, headers={'X-DBKid-Request-ID': secrets.token_hex(16)})
        self.assertNotEqual(status, 200, outcome)
        self.assertEqual(outcome['stage'], 'created_unverified')


if __name__ == '__main__':
    unittest.main()
