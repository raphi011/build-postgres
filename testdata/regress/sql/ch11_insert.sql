-- INSERT: value lists, column lists, defaults to NULL, literal typing.
CREATE TABLE t (a int4 NOT NULL, b text, c bool, d int8);
INSERT INTO t VALUES (1, 'one', true, 1);
INSERT INTO t VALUES (2, 'two', false, 3000000000), (3, NULL, NULL, NULL);
INSERT INTO t (a) VALUES (4);
INSERT INTO t (b, a) VALUES ('five', 5);
INSERT INTO t VALUES ('6', 'six', 'yes', '7');
INSERT INTO t VALUES (7, 'it''s', 'off', -9223372036854775808);
INSERT INTO t VALUES (1 + 7, 'eight', NOT true, 2 * 4);
SELECT * FROM t;
-- Errors: type, arity, NOT NULL, unknown and repeated columns.
INSERT INTO t VALUES (9, 9);
INSERT INTO t VALUES (NULL, 'x');
INSERT INTO t (b) VALUES ('x');
INSERT INTO t VALUES (1, 'x', true, 1, 2);
INSERT INTO t (a, b, c, d, a) VALUES (1, 'x', true, 1, 2);
INSERT INTO t (a, a) VALUES (1, 2);
INSERT INTO t (a, z) VALUES (1, 2);
INSERT INTO t (a, b) VALUES (1);
INSERT INTO t VALUES (2147483648, 'big');
INSERT INTO t VALUES ('x', 'y');
INSERT INTO t VALUES (1, 'y', 'maybe');
INSERT INTO nope VALUES (1);
SELECT a FROM t WHERE a > 8;
