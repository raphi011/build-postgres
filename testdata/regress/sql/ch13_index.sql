-- Index DDL: CREATE INDEX, PRIMARY KEY, DROP INDEX, and the catalogs.
CREATE TABLE t (a int4 PRIMARY KEY, b text, c int8);
CREATE INDEX t_b ON t (b);
CREATE UNIQUE INDEX t_c_key ON t (c);
SELECT oid, relname, relkind FROM pg_class WHERE oid >= 16384 ORDER BY oid;
SELECT indexrelid, indrelid, indkey, indisunique, indisprimary
  FROM pg_index ORDER BY indexrelid;
-- Errors.
CREATE INDEX t_b ON t (b);
CREATE INDEX t ON t (b);
CREATE INDEX x ON nope (a);
CREATE INDEX x ON t (nope);
CREATE INDEX x ON t_b (a);
CREATE INDEX x ON pg_class (oid);
DROP INDEX nope;
DROP INDEX t;
DROP TABLE t_b;
DROP INDEX t_pkey;
SELECT * FROM t_b;
INSERT INTO t_b VALUES (1);
UPDATE t_b SET a = 1;
DELETE FROM t_b;
-- A taken _pkey name fails the whole CREATE TABLE.
CREATE INDEX p_pkey ON t (c);
CREATE TABLE p (a int4 PRIMARY KEY);
SELECT * FROM p;
DROP INDEX p_pkey;
CREATE TABLE p (a int4 PRIMARY KEY);
-- Drop and recreate over data.
INSERT INTO t VALUES (1, 'x', 10), (2, 'y', 20), (3, NULL, NULL);
DROP INDEX t_b;
CREATE INDEX t_b ON t (b);
SELECT oid, relname, relkind FROM pg_class WHERE oid >= 16384 ORDER BY oid;
SELECT * FROM t ORDER BY a;
-- DROP TABLE drops the table's indexes.
DROP TABLE t;
SELECT oid, relname, relkind FROM pg_class WHERE oid >= 16384 ORDER BY oid;
SELECT indexrelid, indrelid FROM pg_index ORDER BY indexrelid;
