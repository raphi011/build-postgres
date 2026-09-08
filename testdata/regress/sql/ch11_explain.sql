-- EXPLAIN shows the plan tree; costs arrive in chapter 14.
CREATE TABLE t (a int4, b text);
INSERT INTO t VALUES (1, 'x');
EXPLAIN SELECT * FROM t;
EXPLAIN SELECT a FROM t AS x WHERE x.a > 1 AND b IS NOT NULL ORDER BY b DESC, a LIMIT 3;
EXPLAIN SELECT 1;
EXPLAIN SELECT 1 WHERE false;
EXPLAIN INSERT INTO t VALUES (2, 'y'), (3, 'z');
EXPLAIN UPDATE t SET b = 'w' WHERE a = 1;
EXPLAIN DELETE FROM t;
-- EXPLAIN does not run the statement.
SELECT * FROM t;
EXPLAIN SELECT * FROM nope;
