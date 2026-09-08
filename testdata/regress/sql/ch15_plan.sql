-- Join methods and join order are chosen by cost.
CREATE TABLE t (a int4 PRIMARY KEY, b text);
CREATE TABLE u (a int4 PRIMARY KEY, c int4);
CREATE TABLE w (a int4, c int4);
-- Never analysed: every table is taken to have 10 pages, and hashing
-- the smaller side beats probing an index once per outer row.
EXPLAIN (COSTS OFF) SELECT * FROM t JOIN u ON t.a = u.a;
EXPLAIN SELECT * FROM t JOIN u ON t.a = u.a;
EXPLAIN (COSTS OFF) SELECT * FROM t JOIN w ON t.a = w.a;
-- A qual on one table goes below the join; quals over both stay at it.
EXPLAIN (COSTS OFF) SELECT * FROM t, u WHERE t.a = u.a AND u.c > 10 AND t.b = 'x';
EXPLAIN (COSTS OFF) SELECT * FROM t, u WHERE t.a = u.a AND u.c > t.a;
-- A cross join, or a join on anything but an equality, is a nested
-- loop over a materialised inner side.
EXPLAIN (COSTS OFF) SELECT * FROM t, u;
EXPLAIN SELECT * FROM t, u;
EXPLAIN (COSTS OFF) SELECT * FROM t JOIN u ON t.a < u.a;
EXPLAIN (COSTS OFF) SELECT * FROM t JOIN u ON t.a = u.a OR t.b = 'x';
-- ORDER BY sorts above a hash join; a nested loop keeps its outer
-- side's order, which a LIMIT makes worth an index scan.
EXPLAIN (COSTS OFF) SELECT * FROM t JOIN u ON t.a = u.a ORDER BY t.a;
EXPLAIN (COSTS OFF) SELECT * FROM t JOIN u ON t.a = u.a ORDER BY t.a LIMIT 1;
-- Two rows in t, analysed: probing u's primary key twice is cheaper
-- than hashing all of u.
INSERT INTO t VALUES (1, 'x'), (2, 'y');
ANALYZE t;
EXPLAIN (COSTS OFF) SELECT * FROM t JOIN u ON t.a = u.a;
EXPLAIN SELECT * FROM t JOIN u ON t.a = u.a;
EXPLAIN (COSTS OFF) SELECT * FROM t JOIN u ON t.a = u.a JOIN w ON w.c = u.c;
EXPLAIN (COSTS OFF) SELECT * FROM t JOIN u ON t.a = u.a JOIN t AS t2 ON t2.a = u.c;
-- Analysed all round, everything is small and a hash join is cheapest.
INSERT INTO u VALUES (1, 10), (2, 20), (3, 30);
INSERT INTO w VALUES (1, 10), (2, 10);
ANALYZE;
EXPLAIN SELECT * FROM t JOIN u ON t.a = u.a;
EXPLAIN (COSTS OFF) SELECT * FROM t JOIN u ON t.a = u.a JOIN w ON w.c = u.c;
-- Whichever plan runs, the rows are the same.
SELECT * FROM t JOIN u ON t.a = u.a JOIN w ON w.c = u.c ORDER BY t.a, w.a;
SELECT * FROM t JOIN u ON t.a + 0 = u.a JOIN w ON w.c + 0 = u.c ORDER BY t.a, w.a;
