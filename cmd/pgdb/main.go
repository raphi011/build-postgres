// Command pgdb is the database binary. It grows into an interactive SQL
// shell in chapter 11.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "pgdb: nothing to run yet; see chapters/00-introduction")
	os.Exit(2)
}
