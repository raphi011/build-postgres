-- ORDER BY and LIMIT. NULLs sort last ascending and first descending;
-- text sorts bytewise, as under the C collation: 'B' < 'a' < 'ab' < 'b'.
CREATE TABLE t (a int4, b text, c bool);
INSERT INTO t VALUES (3, 'b', true), (1, NULL, false), (4, 'B', NULL),
  (2, 'a', true), (5, NULL, false), (6, 'ab', true);
SELECT * FROM t ORDER BY a;
SELECT * FROM t ORDER BY a DESC;
SELECT * FROM t ORDER BY b, a;
SELECT * FROM t ORDER BY b DESC, a;
SELECT * FROM t ORDER BY c, a DESC;
SELECT * FROM t ORDER BY c DESC, a;
SELECT a, b FROM t ORDER BY 2, 1;
SELECT a AS x FROM t ORDER BY x DESC;
SELECT a, a * -1 AS neg FROM t ORDER BY neg;
SELECT a FROM t ORDER BY b, a LIMIT 3;
SELECT a FROM t ORDER BY a LIMIT 0;
SELECT a FROM t ORDER BY a LIMIT 100;
SELECT a FROM t ORDER BY a LIMIT NULL;
SELECT a FROM t ORDER BY a LIMIT '2';
SELECT a FROM t ORDER BY a LIMIT 1 + 1;
SELECT a FROM t ORDER BY a DESC LIMIT 1;
SELECT a FROM t LIMIT 2;
SELECT a FROM t ORDER BY a LIMIT -1;
SELECT a FROM t ORDER BY a LIMIT 'x';
SELECT a FROM t ORDER BY a LIMIT a;
SELECT a FROM t ORDER BY 3;
SELECT a FROM t ORDER BY z;
SELECT a AS x, b AS x FROM t ORDER BY x;
