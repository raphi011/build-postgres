-- DDL: create and drop tables, and read the catalogs as ordinary tables.
CREATE TABLE t (a int4 PRIMARY KEY, b text NOT NULL, c bool, d int8);
CREATE TABLE t (a int4);
CREATE TABLE u (a int4, a text);
CREATE TABLE u (a int3);
SELECT oid, relname, relkind FROM pg_class
  WHERE oid >= 16384 AND relkind = 'r' ORDER BY oid;
SELECT attname, atttypid, attnum, attnotnull FROM pg_attribute
  WHERE attrelid = 16384 ORDER BY attnum;
DROP TABLE t;
DROP TABLE t;
DROP TABLE pg_class;
SELECT relname FROM pg_class WHERE oid >= 16384 AND relkind = 'r' ORDER BY oid;
-- The name is free again and the new table gets a new OID.
CREATE TABLE t (x text);
SELECT oid > 16384 AS new_oid, relname FROM pg_class WHERE relname = 't';
-- Quoted identifiers keep their case.
CREATE TABLE "Mixed Case" ("Col" int4, col int4);
INSERT INTO "Mixed Case" VALUES (1, 2);
SELECT "Col", col FROM "Mixed Case";
SELECT col FROM "mixed case";
