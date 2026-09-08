# READ COMMITTED sees other transactions' commits at each statement;
# REPEATABLE READ keeps the snapshot of its first statement.

setup
{
  CREATE TABLE t (a int4);
  INSERT INTO t VALUES (1);
}

teardown
{
  DROP TABLE t;
}

session s1
step rc   { BEGIN ISOLATION LEVEL READ COMMITTED; }
step rr   { BEGIN ISOLATION LEVEL REPEATABLE READ; }
step s    { SELECT a FROM t ORDER BY a; }
step c    { COMMIT; }

session s2
step i2   { INSERT INTO t VALUES (2); }
step i3   { INSERT INTO t VALUES (3); }
step d1   { DELETE FROM t WHERE a = 1; }

permutation rc s i2 s d1 s c s
permutation rr s i2 s d1 s c s
# The snapshot is taken at the first statement, not at BEGIN.
permutation rr i2 s i3 s c s
