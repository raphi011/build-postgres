-- ANALYZE records page and tuple counts in pg_class.
CREATE TABLE t (a int4 PRIMARY KEY, b text);
INSERT INTO t VALUES (1, 'x'), (2, 'y'), (3, 'z');
SELECT relname, relpages, reltuples FROM pg_class WHERE relname = 't';
ANALYZE t;
SELECT relname, relpages, reltuples FROM pg_class WHERE relname = 't';
SELECT relname, relpages, reltuples FROM pg_class WHERE relname = 't_pkey';
-- Only live tuples count.
DELETE FROM t WHERE a = 2;
INSERT INTO t VALUES (4, 'w');
ANALYZE t;
SELECT relname, relpages, reltuples FROM pg_class WHERE relname = 't';
-- An index is skipped; an unknown relation is an error; no name means
-- every table.
ANALYZE t_pkey;
ANALYZE nope;
ANALYZE;
SELECT relname, relpages FROM pg_class WHERE relname = 'pg_class';
