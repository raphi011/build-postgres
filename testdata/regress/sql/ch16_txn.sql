-- Transaction blocks.
CREATE TABLE t (a int4 PRIMARY KEY, b text);
BEGIN;
INSERT INTO t VALUES (1, 'one');
INSERT INTO t VALUES (2, 'two');
COMMIT;
SELECT * FROM t ORDER BY a;
-- COMMIT and ROLLBACK outside a block, and BEGIN inside one, only warn.
COMMIT;
ROLLBACK;
BEGIN;
BEGIN;
INSERT INTO t VALUES (3, 'three');
COMMIT;
SELECT * FROM t ORDER BY a;
BEGIN;
SELECT * FROM t WHERE a = 3;
ROLLBACK;
-- An error aborts the block: everything but COMMIT and ROLLBACK is
-- refused until the block ends, syntax errors excepted, and COMMIT
-- rolls back.
BEGIN;
SELECT 1 / 0;
INSERT INTO t VALUES (4, 'four');
SELECT * FROM t;
CREATE TABLE u (a int4);
BEGIN;
SELEC 1;
COMMIT;
SELECT * FROM t ORDER BY a;
BEGIN;
INSERT INTO t VALUES (1, 'dup');
INSERT INTO t VALUES (4, 'four');
ROLLBACK;
SELECT * FROM t ORDER BY a;
-- The chapter 17 rule makes rolled-back writes invisible; until then a
-- SELECT after ROLLBACK of a write is not part of the regression suite.
