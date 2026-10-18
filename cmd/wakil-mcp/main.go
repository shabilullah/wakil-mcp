// Command wakil-mcp is one MCP server in front of all the others.
//
// Add each upstream once, then point every harness at wakil-mcp and see all of
// their tools.
package main

import (
	"os"

	"github.com/shabilullah/wakil-mcp/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
