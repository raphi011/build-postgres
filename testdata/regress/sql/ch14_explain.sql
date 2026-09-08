-- The planner chooses between a sequential and an index scan by cost.
CREATE TABLE t (a int4 PRIMARY KEY, b text);
CREATE INDEX t_b ON t (b);
-- Never analysed: the table is assumed to have 10 pages, so an equality
-- on an indexed column uses the index and a single inequality does not.
EXPLAIN (COSTS OFF) SELECT * FROM t WHERE a = 1;
EXPLAIN (COSTS OFF) SELECT * FROM t WHERE a > 1;
EXPLAIN (COSTS OFF) SELECT * FROM t WHERE a > 1 AND a < 5;
EXPLAIN (COSTS OFF) SELECT * FROM t WHERE b = 'x';
EXPLAIN (COSTS OFF) SELECT * FROM t WHERE 1 = a AND b = 'x';
EXPLAIN (COSTS OFF) SELECT * FROM t WHERE b <> 'x';
-- An index in ORDER BY order replaces the sort; a descending order cannot.
EXPLAIN (COSTS OFF) SELECT a FROM t ORDER BY a;
EXPLAIN (COSTS OFF) SELECT a FROM t ORDER BY a DESC;
EXPLAIN (COSTS OFF) SELECT b FROM t WHERE a > 1 ORDER BY b LIMIT 1;
EXPLAIN (COSTS OFF) UPDATE t SET b = 'y' WHERE a = 1;
EXPLAIN (COSTS OFF) DELETE FROM t WHERE a > 1;
-- With costs.
EXPLAIN SELECT * FROM t WHERE a = 1;
EXPLAIN SELECT a FROM t WHERE a > 1 ORDER BY b LIMIT 2;
EXPLAIN SELECT 1;
EXPLAIN INSERT INTO t VALUES (1, 'x'), (2, 'y');
-- After ANALYZE of a small table the sequential scan is cheaper.
INSERT INTO t VALUES (1, 'x'), (2, 'y'), (3, 'z');
ANALYZE t;
EXPLAIN SELECT * FROM t WHERE a = 1;
EXPLAIN (COSTS OFF) SELECT * FROM t WHERE a = 1;
EXPLAIN (COSTS OFF) SELECT a FROM t ORDER BY a;
EXPLAIN (COSTS OFF) SELECT a FROM t ORDER BY a LIMIT 1;
-- Whichever scan runs, the rows are the same.
SELECT * FROM t WHERE a = 1;
SELECT * FROM t WHERE a + 0 = 1;
