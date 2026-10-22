package main

import (
	"fmt"

	_ "github.com/dolthub/go-mysql-server"
	_ "github.com/dolthub/go-mysql-server/memory"
	_ "github.com/dolthub/go-mysql-server/sql"
	_ "github.com/dolthub/go-mysql-server/sql/types"
)

func main() { fmt.Println("size probe") }
