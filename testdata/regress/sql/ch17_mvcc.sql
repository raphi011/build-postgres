-- Rolled-back writes vanish, which chapter 16 could not show.
CREATE TABLE t (a int4 PRIMARY KEY, b text);
INSERT INTO t VALUES (1, 'one'), (2, 'two');
BEGIN;
INSERT INTO t VALUES (3, 'three');
SELECT * FROM t ORDER BY a;
ROLLBACK;
SELECT * FROM t ORDER BY a;
BEGIN;
DELETE FROM t WHERE a = 1;
UPDATE t SET b = 'zwei' WHERE a = 2;
SELECT * FROM t ORDER BY a;
ROLLBACK;
SELECT * FROM t ORDER BY a;
SELECT b FROM t WHERE a = 2;
-- A failed statement in a block is rolled back with the block.
BEGIN;
INSERT INTO t VALUES (3, 'three');
INSERT INTO t VALUES (1, 'dup');
COMMIT;
SELECT * FROM t ORDER BY a;
-- A key freed by a delete in the same transaction can be reused; the
-- old tuple's index entry points at a dead row.
BEGIN;
DELETE FROM t WHERE a = 1;
INSERT INTO t VALUES (1, 'uno');
COMMIT;
SELECT * FROM t ORDER BY a;
SELECT b FROM t WHERE a = 1;
-- Isolation levels.
BEGIN ISOLATION LEVEL REPEATABLE READ;
SELECT * FROM t ORDER BY a;
COMMIT;
BEGIN ISOLATION LEVEL READ COMMITTED;
COMMIT;
-- Rolled-back DDL: the table never existed, and the name is free.
BEGIN;
CREATE TABLE u (a int4);
INSERT INTO u VALUES (1);
SELECT * FROM u;
ROLLBACK;
SELECT * FROM u;
CREATE TABLE u (a int4 PRIMARY KEY);
INSERT INTO u VALUES (2);
SELECT * FROM u;
-- A rolled-back DROP TABLE keeps the table and its rows.
BEGIN;
DROP TABLE t;
SELECT * FROM t;
ROLLBACK;
SELECT * FROM t ORDER BY a;
-- An index built after a rollback covers the live rows only.
BEGIN;
INSERT INTO t VALUES (4, 'four');
ROLLBACK;
CREATE INDEX t_b ON t (b);
SELECT a FROM t WHERE b = 'four';
SELECT a FROM t WHERE b = 'uno';
ANALYZE t;
SELECT reltuples FROM pg_class WHERE relname = 't';
