// Command pgdb is the database binary. It grows into an interactive SQL
// shell in chapter 11. For now it inspects relation files:
//
//	pgdb sample DIR       write a small relation to a fresh directory
//	pgdb dump FILE [BLOCK]  print the pages of a relation file
package main

import (
	"fmt"
	"os"
	"strconv"
)

func main() {
	args := os.Args[1:]
	switch {
	case len(args) > 0 && args[0] == "sample":
		if len(args) != 2 {
			usage()
		}
		run(sample(args[1], os.Stdout))
		return
	case len(args) > 0 && args[0] == "dump":
		if len(args) != 2 && len(args) != 3 {
			usage()
		}
		blk := -1
		if len(args) == 3 {
			n, err := strconv.Atoi(args[2])
			if err != nil || n < 0 {
				fmt.Fprintln(os.Stderr, "pgdb: BLOCK must be a block number")
				os.Exit(2)
			}
			blk = n
		}
		run(dump(args[1], blk, os.Stdout))
		return
	}
	usage()
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: pgdb sample DIR")
	fmt.Fprintln(os.Stderr, "       pgdb dump FILE [BLOCK]")
	os.Exit(2)
}

// run exits with a message when a subcommand fails.
func run(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "pgdb:", err)
		os.Exit(1)
	}
}
