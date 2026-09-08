# Two transactions changing the same row: the second waits for the
# first, then applies READ COMMITTED's rule or REPEATABLE READ's.

setup
{
  CREATE TABLE t (a int4 PRIMARY KEY, b int4);
  INSERT INTO t VALUES (1, 10), (2, 20);
}

teardown
{
  DROP TABLE t;
}

session s1
step s1b  { BEGIN; }
step s1u  { UPDATE t SET b = b + 1 WHERE a = 1; }
step s1m  { UPDATE t SET a = 3 WHERE a = 1; }
step s1d  { DELETE FROM t WHERE a = 1; }
step s1c  { COMMIT; }
step s1r  { ROLLBACK; }

session s2
step s2b  { BEGIN; }
step s2rr { BEGIN ISOLATION LEVEL REPEATABLE READ; }
step s2u  { UPDATE t SET b = b + 1 WHERE a = 1; }
step s2d  { DELETE FROM t WHERE a = 1; }
step s2s  { SELECT * FROM t ORDER BY a; }
step s2c  { COMMIT; }

# The second update waits, then increments the committed new version.
permutation s1b s1u s2b s2u s1c s2s s2c
# After a rollback it increments the original.
permutation s1b s1u s2b s2u s1r s2s s2c
# A row deleted, or moved out of the WHERE clause, under the update is
# skipped.
permutation s1b s1d s2b s2u s1c s2s s2c
permutation s1b s1m s2b s2u s1c s2s s2c
# Under REPEATABLE READ a concurrent change is a serialization failure.
permutation s2rr s2s s1b s1u s2u s1c s2c
permutation s2rr s2s s1b s1d s2d s1c s2c
# A deleted row stays visible to a snapshot older than the delete.
permutation s2rr s2s s1d s2s s2c s2s
