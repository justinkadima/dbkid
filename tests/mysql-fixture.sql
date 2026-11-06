-- Disposable local MySQL instances ONLY. This creates a database and a
-- fixture-scoped test account with an empty password; never run on a shared,
-- existing, remotely reachable, or production server. The integration test
-- verifies the server datadir is inside an explicitly supplied scratch root.
CREATE DATABASE dbkid_fixture CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE USER 'dbkid_test'@'127.0.0.1' IDENTIFIED BY '';
GRANT ALL PRIVILEGES ON dbkid_fixture.* TO 'dbkid_test'@'127.0.0.1';
-- DDL tier for the v2 mutation tests: owns future dbkid_v2_* schemas.
CREATE USER IF NOT EXISTS 'dbkid_ddl'@'127.0.0.1' IDENTIFIED BY '';
GRANT ALL PRIVILEGES ON `dbkid_v2\_%`.* TO 'dbkid_ddl'@'127.0.0.1';
GRANT PROCESS ON *.* TO 'dbkid_ddl'@'127.0.0.1';
-- Admin tier for the v3 users-and-privileges tests. Disposable loopback
-- fixture ONLY: this grant would be reckless on any real server.
CREATE USER IF NOT EXISTS 'dbkid_admin'@'127.0.0.1' IDENTIFIED BY '';
GRANT ALL PRIVILEGES ON *.* TO 'dbkid_admin'@'127.0.0.1' WITH GRANT OPTION;
USE dbkid_fixture;
CREATE TABLE items (
  id INT NOT NULL PRIMARY KEY,
  label VARCHAR(255) NOT NULL,
  quantity INT NOT NULL,
  note TEXT NULL,
  raw_data VARBINARY(20) NULL
);
INSERT INTO items (id, label, quantity, note, raw_data) VALUES
  (1, 'Alpha', 10, NULL, UNHEX('FF00')),
  (2, 'O''Reilly', 0, '', UNHEX('E282AC')),
  (3, '<img src=x onerror=alert(1)>', 30, 'plain text', NULL);
INSERT INTO items (id, label, quantity, note)
WITH RECURSIVE n AS (SELECT 4 AS i UNION ALL SELECT i + 1 FROM n WHERE i < 125)
SELECT i, CONCAT('fixture-', i), i, NULL FROM n;
-- Binary values for the v2 blob preview/download tests. Row 1 is a complete
-- 1x1 transparent PNG so the inline image preview can actually decode it;
-- row 2 is plain text bytes.
CREATE TABLE v2_blob (id INT PRIMARY KEY, data VARBINARY(200));
INSERT INTO v2_blob VALUES
  (1, UNHEX('89504E470D0A1A0A0000000D49484452000000010000000108060000001F15C4890000000B4944415478DA636000020000050001E9FADCD80000000049454E44AE426082')),
  (2, UNHEX('7465787420626C6F622076616C7565'));
CREATE TABLE `odd``table` (`weird``column` VARCHAR(128) NOT NULL PRIMARY KEY);
INSERT INTO `odd``table` (`weird``column`) VALUES ('quoted identifier works');
CREATE VIEW item_view AS SELECT id, label FROM items WHERE id <= 3;
DELIMITER //
-- The DEFINER matches dbkid_test so SHOW CREATE PROCEDURE exposes the body
-- to the least-privilege fixture account (MySQL returns NULL otherwise).
CREATE DEFINER=`dbkid_test`@`127.0.0.1` PROCEDURE dbkid_late_error()
BEGIN
  SELECT 1 AS first_result;
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'disposable fixture error';
END//
CREATE DEFINER=`dbkid_test`@`127.0.0.1` PROCEDURE dbkid_one_result()
BEGIN
  SELECT 1 AS first_col;
END//
CREATE DEFINER=`dbkid_test`@`127.0.0.1` PROCEDURE dbkid_two_results()
BEGIN
  SELECT 1 AS first_col;
  SELECT 2 AS second_col;
END//
DELIMITER ;
