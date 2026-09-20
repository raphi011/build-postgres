# A prose guide to Build Your Own PostgreSQL

This is a second route through the book. The [chapter READMEs](../chapters/00-introduction/README.md)
are build instructions: they pin down byte layouts, APIs, tests, and the order
in which to implement them. Here the same chapters form a story about how a
SQL statement reaches stored data and how a reader decides what that data
means. Read this straight through before building, or return to a section
when the details of a chapter obscure its purpose.

## 00 · [Introduction](../chapters/00-introduction/README.md)

Imagine asking a database for `SELECT name FROM people WHERE id = 7`. The
database must first understand the text, then establish what `people`,
`name`, and `id` refer to, choose a way to find the row, and finally turn
bytes on disk into a result. A second session might be changing that row at
the same time. Each stage needs information from the stages below it, so this
book builds from storage upward, even though a query travels mostly downward
from SQL.

The project deliberately keeps the boundaries visible. SQL syntax belongs to
the lexer and parser; meaning belongs to the analyzer; possible access paths
belong to the planner; actual work belongs to the executor. Below them, the
heap and B-tree obtain pages through a buffer pool, which gets bytes from
relation files. Transactions and MVCC influence what all those reads and
writes are allowed to see. The result is a small database with PostgreSQL's
recognizable architecture, built in Go with only the standard library.

## 01 · [Pages](../chapters/01-pages/README.md)

Disk I/O deals in blocks, not individual rows. This database uses 8192-byte
pages. A page begins with a header and an array of four-byte line pointers;
the data for each item grows backward from the end. The free space lies
between the pointers and the data. The header records the two free-space
boundaries, and it leaves room for metadata used by later chapters.

The indirection through line pointers solves a problem that appears much
later: an index needs a lasting address for a row. If an item is identified
by its page number and its one-based line pointer number, the page can move
the item's bytes during compaction and update only the pointer. Deleting the
second of three items leaves the third as item 3. Its address survives even
though its bytes may move. The page therefore provides both efficient packing
and stable identities to the heap and B-tree.

## 02 · [Heap tuples](../chapters/02-tuples/README.md)

A page treats an item as opaque bytes. A heap tuple gives those bytes a
meaning: a header followed by column values. The header includes `xmin`, the
transaction that created the version; `xmax`, a transaction that deleted or
replaced it; and `ctid`, the location of this or a successor version. Those
fields are present before transactions exist in the running system because
the on-disk format must already have a place for them.

The values themselves need a shared encoding. This book supports `int4`,
`int8`, `bool`, and `text`, with alignment between fields and a bitmap to mark
NULLs. NULL is absence of a value, not a special integer or an empty string.
When a row is formed, its type description determines where each value goes;
when it is deformed, the same description recovers the Go values and null
flags. If one alignment calculation differs, every later layer can read the
wrong bytes while the page itself still looks valid.

## 03 · [Relation files and the storage manager](../chapters/03-storage-manager/README.md)

Pages need a persistent home. The storage manager maps a relation and block
number to a position in a file under the data directory. A table's first
page is block 0, its next page is block 1, and so on. Reads and writes use
whole pages, so higher layers never have to reason about partial tuples or
seek offsets. Creating, extending, opening, and removing relation files all
pass through the same boundary.

This separation matters when the system grows. The heap asks for a page of a
table, and the B-tree asks for a page of an index; neither needs to know how
the file is named or opened. The storage manager also gives the buffer pool
a precise unit to cache: one relation plus one block number.

## 04 · [Buffer manager](../chapters/04-buffer-manager/README.md)

Reading a page from disk for every tuple would make even a simple scan
painfully expensive. The buffer manager keeps a limited number of page
frames in memory and maps each `(relation, block)` to its current frame. A
request pins the frame, guaranteeing that it cannot be reused until the
caller unpins it. A content lock separately protects reading or changing the
bytes. Pinning protects the frame's lifetime; locking protects its contents.

When the pool is full, a clock sweep looks for a frame that is unpinned and
has not been used recently. If that frame is dirty, its changed page is
written back before reuse. Consider a scan of a table larger than the pool:
the clock sweep lets old pages give way to new ones without demanding a
perfect history of every access. Every heap or index operation must release
its pins and locks promptly, because one forgotten pin can make replacement
impossible.

## 05 · [Heap access method](../chapters/05-heap/README.md)

The heap turns pages of encoded tuples into table operations. An insert finds
room on an existing page or extends the relation, writes the tuple, and
returns its tuple ID: block number plus line pointer number. A scan walks
pages and returns rows; a fetch uses a tuple ID to retrieve one directly.
That ID is the link an index will later store.

An update does not overwrite a row in place. It marks the old version with
the changing transaction, inserts a new version, and points the old version's
`ctid` at the new one. A delete marks the old version without immediately
removing its bytes. At this point in the book, visibility is deliberately
simple: a row with no deleting transaction is live. Once snapshots arrive in
chapter 17, the same physical versions support different readers seeing
different logical states without rewriting the page format.

## 06 · [Lexer](../chapters/06-lexer/README.md)

The lexer is the first boundary between SQL text and SQL structure. It
groups characters into tokens such as identifiers, keywords, integer and
string literals, operators, and punctuation, while tracking source positions
for errors. In `WHERE id <= 7`, `<=` must become one operator token; in a
quoted string, the same characters are merely text. Comments and whitespace
disappear before the parser sees them.

The lexer does not decide whether an integer fits an `int4`, or whether a
name identifies a real column. Those questions need context that only later
stages have. It does, however, make lexical choices that all later stages
must share: unquoted names are folded, doubled quotes inside strings become
one quote, and nested block comments are skipped as a unit. Good position
tracking here is what makes a syntax or name error useful to a person writing
SQL.

## 07 · [Parser and AST](../chapters/07-parser/README.md)

Tokens still say little about intent. The parser recognizes statement and
expression grammar and builds an abstract syntax tree. `id = 7 AND active`
becomes a tree in which `AND` combines a comparison and a column reference.
Parentheses and operator precedence determine that shape. At this stage a
column reference is still a name, not a location in a table.

The AST is kept separate from the parser so later stages can use the tree
types without depending on parsing machinery. Each node can also print a
canonical SQL form. Parsing that form again should produce the same
structure, which is a useful way to catch lost parentheses, quoting, or
operator precedence. The parser's job is to reject malformed syntax; it
leaves questions about existing tables and compatible types to the analyzer.

## 08 · [System catalog](../chapters/08-catalog/README.md)

For the analyzer to understand `people.id`, the database needs a durable
record of tables and columns. The catalog stores that metadata in relations
of its own, including rows describing each relation and its attributes.
Creating a table changes catalog rows and creates a relation file; looking up
a table reads those rows into a relation description. The catalog is thus
data that describes other data, rather than a hard-coded map in the program.

Object IDs provide stable identities even if names change or files are
reopened. A control file records the next ID so a restart does not hand out
one already in use. A relation cache avoids scanning the catalog on every
lookup and is invalidated when definitions change. This chapter joins the
physical world of files and tuples to the logical world of named tables and
typed columns.

## 09 · [Analyzer](../chapters/09-analyzer/README.md)

A parsed statement can be syntactically valid and still meaningless:
`SELECT missing FROM people` parses even if `missing` is not a column. The
analyzer consults the catalog and replaces names with bound references. It
records the tables in a range table and resolves a column to a particular
range entry and attribute. With two tables that both have `id`, an unqualified
`id` is ambiguous; `people.id` identifies one. Once binding is complete, the
executor no longer needs to search for a column by name on every row.

The analyzer also checks types and shapes. It decides what type an integer
literal has, whether operators can accept their operands, whether a value
can go into a target column, and what columns a `SELECT *` expands into.
Aliases and output names are resolved here as well. The output is a bound
query: still a description of work to do, but one whose references and types
have concrete meanings.

## 10 · [Expression evaluation](../chapters/10-expressions/README.md)

Conditions and projections must be evaluated over many rows. Compiling a
bound expression once avoids rediscovering its node types and column
positions for each row. In this project compilation builds a tree of Go
closures. A bound column reference becomes a function that reads a fixed
slot; an addition combines two compiled child functions and checks the
result. The executor can then call the compiled function as rows arrive.

SQL's NULL semantics are essential here. `NULL = 7` is unknown, not false;
`WHERE` keeps only rows for which its condition is true. `AND` and `OR` can
still determine an answer from one side in some cases: false AND unknown is
false, while true OR unknown is true. Expression evaluation also supplies
the consistent row layout used by scans, joins, filters, and projections.

## 11 · [Executor and REPL](../chapters/11-executor/README.md)

The executor gives a plan motion. Each node in a plan tree opens, produces
one row at a time, and closes. A sequential scan reads heap tuples; a filter
keeps rows that satisfy a compiled condition; a projection selects output
columns; a sort and limit change their order and count. Parent nodes pull
from their children, so the same small interface composes a complete query.
Modification nodes use the heap to insert, update, or delete instead of
returning ordinary result rows.

The session ties the stages together: parse SQL, analyze each statement,
plan it, execute it, and format the result or error. Utility statements such
as creating a table use catalog operations directly. The REPL makes this
pipeline visible to a user, while SQL regression files exercise it end to
end. This is the first chapter where typing a query reaches all the way down
to stored pages and back to a displayed result.

## 12 · [B-tree](../chapters/12-btree/README.md)

A heap scan can answer any predicate, but it must inspect every relevant
page. A B-tree gives sorted keys an alternative path. Leaf pages hold keys
and heap tuple IDs; internal pages hold separator keys and child links. A
search descends from the root to a leaf, and a range scan follows linked
leaves in key order. A metapage tells the system where the root is. These
pages use the slotted-page machinery from chapter 01, with extra space for
tree links.

Insertion must preserve searchability as pages fill. A full page splits;
its parent receives a separator, and a split can propagate all the way to a
new root. The links between pages also let a scan move forward without
returning to the root after every item. The index stores addresses, not
whole table rows, so finding a key is only the first part of retrieving a
row. The next chapter connects the tree to SQL and the heap.

## 13 · [Index integration](../chapters/13-indexes/README.md)

An index becomes a database object when it has a catalog entry, its own
relation file, and a connection to a table column. Creating one scans the
existing table and inserts a key and tuple ID for each row. Later inserts
and updates maintain the index as the heap changes. An index scan translates
a predicate such as `id >= 7` into a B-tree bound, obtains candidate tuple
IDs, and fetches the corresponding heap versions.

The heap remains the authority on row existence and visibility. An index
entry may lead to a row version that no longer qualifies, so the executor
checks the fetched row. A unique index adds another obligation: it must
detect a conflicting key before accepting a write. This chapter makes the
index usable; the next gives the planner a reason to choose it over a
sequential scan.

## 14 · [Planner and EXPLAIN](../chapters/14-planner/README.md)

A query can often run in more than one way. A sequential scan reads the
table's pages in order; an index scan visits a narrower range of keys but
may fetch many heap pages. `ANALYZE` gathers table and index size estimates,
and the planner uses a cost model to estimate which route is cheaper. A
selective lookup may favor the index; a query returning most of a table may
favor the sequential scan. The answer depends on estimated rows and page
accesses, not on whether an index merely exists.

The model is intentionally smaller than PostgreSQL's, but it introduces the
same central idea: a plan is a prediction about work. `EXPLAIN` shows that
prediction, including estimated costs and rows, so it can be inspected
before execution. A bad estimate can lead to a bad plan even when every
executor node is correct. Chapter 15 extends this choice from one relation
to several.

## 15 · [Joins](../chapters/15-joins/README.md)

A join combines rows from several relations. For a nested loop, the executor
takes one outer row and searches the inner input for matches, then repeats
for the next outer row. An inner index can make each search cheap; otherwise
the inner rows can be materialized for reuse. A hash join instead builds a
hash table from one side and probes it with rows from the other, making an
equality join efficient when repeated inner searches would cost more.

Join planning adds two questions to scan planning: in what order should
relations be combined, and where should each condition run? A condition
using only one table can filter its scan early. A condition using two tables
belongs at the first join containing both. The planner estimates costs for
candidate paths and keeps useful alternatives, including paths that
preserve order or depend on an outer row. Row layouts must track which
relation each column came from even when the planner reverses the order of
the inputs.

## 16 · [Transactions](../chapters/16-transactions/README.md)

Until this point, a write is stamped as permanently visible. Transactions
replace that shortcut with an identity and a verdict. The control file
hands out transaction IDs; a compact commit log records whether each ID
committed or aborted. A tuple's `xmin` and `xmax` name the writers, while
the log says what happened to them. Committing a transaction does not require
rewriting all its tuples: changing its status is enough for later readers to
interpret them.

The session now runs each standalone statement in its own transaction or
keeps several statements inside `BEGIN` and `COMMIT`. An error in an
explicit block puts the block in a failed state until it ends. These rules
define the lifetime and outcome of a unit of work, but a commit log alone
does not answer whether a concurrent reader should see a transaction that
finished just after its read began. Snapshots provide that missing time
boundary.

## 17 · [MVCC snapshots and visibility](../chapters/17-mvcc/README.md)

A snapshot records which transactions were running when a reader began its
view, along with a lower bound and the next unassigned transaction ID. To
decide whether a tuple is visible, the reader examines its creating and
deleting IDs, its own transaction ID, the snapshot, and the commit log. A
version created by a transaction that was still running at snapshot time
stays invisible to that snapshot even if it commits a moment later. A
version deleted by such a transaction remains visible to that reader. This
is why old and new tuple versions can coexist without readers blocking
writers.

Writers of the same row do need coordination. The second writer waits for
the first and then checks what happened. Under READ COMMITTED, a new
statement can use a fresh snapshot and recheck the updated row; under
REPEATABLE READ, the transaction keeps its earlier view and can report a
serialization failure. Index uniqueness also has to wait for competing
writes before declaring a conflict. With these rules, the tuple headers
introduced in chapter 02 finally determine which version each session sees.

## Following one query through the whole system

Suppose `people` has an index on `id`, and a session runs
`SELECT name FROM people WHERE id = 7`. The lexer makes tokens; the parser
builds a syntax tree; the analyzer binds `people`, `id`, and `name` using the
catalog and checks their types. The planner compares a sequential scan with
an index scan using its statistics and costs. If it chooses the index, the
executor asks the B-tree for matching tuple IDs, fetches heap tuples through
the buffer and storage managers, applies the snapshot visibility rule and
the remaining condition, and projects `name` into the result. The page and
tuple formats make the bytes intelligible; the transaction machinery makes
the answer consistent. Each chapter supplies one piece of that path.
