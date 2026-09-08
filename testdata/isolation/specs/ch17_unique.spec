# A unique index between transactions: an insert of a key another
# transaction has inserted waits for its verdict.

setup
{
  CREATE TABLE t (a int4 PRIMARY KEY);
  INSERT INTO t VALUES (1);
}

teardown
{
  DROP TABLE t;
}

session s1
step s1b  { BEGIN; }
step s1i  { INSERT INTO t VALUES (2); }
step s1d  { DELETE FROM t WHERE a = 1; }
step s1c  { COMMIT; }
step s1r  { ROLLBACK; }

session s2
step s2i  { INSERT INTO t VALUES (2); }
step s2i1 { INSERT INTO t VALUES (1); }
step s2s  { SELECT a FROM t ORDER BY a; }

permutation s1b s1i s2i s1c s2s
permutation s1b s1i s2i s1r s2s
# A key whose row is being deleted is free once the delete commits.
permutation s1b s1d s2i1 s1c s2s
permutation s1b s1d s2i1 s1r s2s
