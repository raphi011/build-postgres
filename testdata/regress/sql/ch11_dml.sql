-- UPDATE and DELETE.
CREATE TABLE t (a int4 NOT NULL, b text, c int4);
INSERT INTO t VALUES (1, 'one', 10), (2, 'two', 20), (3, 'three', NULL);
UPDATE t SET b = 'TWO' WHERE a = 2;
SELECT * FROM t ORDER BY a;
UPDATE t SET c = c + 1;
SELECT * FROM t ORDER BY a;
-- SET expressions see the old row, so this swaps.
UPDATE t SET a = c, c = a WHERE a = 1;
SELECT * FROM t ORDER BY a;
UPDATE t SET b = NULL, c = NULL WHERE a = 3;
SELECT * FROM t ORDER BY a;
UPDATE t SET a = 1 WHERE false;
SELECT * FROM t ORDER BY a;
-- Errors.
UPDATE t SET a = c WHERE c IS NULL;
UPDATE t SET a = NULL;
UPDATE t SET z = 1;
UPDATE t SET a = 'x';
UPDATE t SET a = 1, a = 2;
UPDATE nope SET a = 1;
SELECT * FROM t ORDER BY a;
DELETE FROM t WHERE a = 2;
SELECT * FROM t ORDER BY a;
DELETE FROM t WHERE b IS NULL;
SELECT * FROM t ORDER BY a;
DELETE FROM t WHERE false;
DELETE FROM t WHERE 'x';
DELETE FROM nope;
DELETE FROM t;
SELECT * FROM t;
-- The table is still there, and takes new rows.
INSERT INTO t VALUES (9, 'nine', 90);
SELECT * FROM t;
