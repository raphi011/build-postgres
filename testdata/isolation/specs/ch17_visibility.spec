# What one session sees of another's uncommitted work.

setup
{
  CREATE TABLE t (a int4 PRIMARY KEY, b text);
  INSERT INTO t VALUES (1, 'one');
}

teardown
{
  DROP TABLE t;
}

session s1
step s1b  { BEGIN; }
step s1i  { INSERT INTO t VALUES (2, 'two'); }
step s1d  { DELETE FROM t WHERE a = 1; }
step s1u  { UPDATE t SET b = 'uno' WHERE a = 1; }
step s1s  { SELECT * FROM t ORDER BY a; }
step s1c  { COMMIT; }
step s1r  { ROLLBACK; }

session s2
step s2s  { SELECT * FROM t ORDER BY a; }
step s2i  { SELECT b FROM t WHERE a = 1; }

# An uncommitted insert is invisible to others and visible to itself.
permutation s1b s1i s1s s2s s1c s2s
# A rolled-back insert vanishes.
permutation s1b s1i s2s s1r s2s
# Uncommitted and rolled-back deletes and updates leave the row alone.
permutation s1b s1d s1s s2s s1r s2s
permutation s1b s1u s1s s2i s1c s2i
permutation s1b s1u s2i s1r s2i
