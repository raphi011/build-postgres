-- Inner joins: JOIN ... ON, comma lists, aliases, and NULL keys.
CREATE TABLE t (a int4 PRIMARY KEY, b text);
CREATE TABLE u (a int4, c int8, d text);
INSERT INTO t VALUES (1, 'x'), (2, 'y'), (3, 'z'), (4, NULL);
INSERT INTO u VALUES (1, 10, 'p'), (1, 11, 'q'), (3, 30, 'r'), (NULL, 40, 's'), (5, 50, 't');
SELECT * FROM t JOIN u ON t.a = u.a ORDER BY t.a, u.c;
SELECT t.b, u.d FROM t, u WHERE t.a = u.a AND u.c > 10 ORDER BY 1;
SELECT t.a, u.a FROM t JOIN u ON t.a < u.a ORDER BY 1, 2;
SELECT t.a, u.c FROM t JOIN u ON t.a = u.a OR u.c = 40 ORDER BY 1, 2;
SELECT * FROM t JOIN u ON t.a = u.c;
SELECT * FROM t, u WHERE false;
SELECT t.b FROM t JOIN u ON true WHERE u.d = 's' ORDER BY 1;
-- Aliases and self joins.
SELECT t.a AS x, t2.a AS y FROM t, t AS t2 WHERE t.a = t2.a + 1 ORDER BY 1;
SELECT u1.c, u2.c FROM u AS u1 JOIN u AS u2 ON u1.a = u2.a AND u1.c < u2.c ORDER BY 1, 2;
-- Three relations, the qual over the first and last waits for the top join.
SELECT * FROM t JOIN u ON t.a = u.a JOIN t AS t2 ON t2.a = u.a + 1 ORDER BY t.a, u.c;
SELECT t.a, u.c, t2.a FROM t, u, t AS t2 WHERE t.a = u.a AND u.a < t2.a AND t2.b > t.b ORDER BY 1, 2, 3;
-- Errors.
SELECT a FROM t, u;
SELECT * FROM t JOIN u ON t.a;
SELECT * FROM t JOIN t ON true;
SELECT * FROM t JOIN nope ON true;
SELECT * FROM t, u WHERE t.a = u.d;
