package main

import (
	"log"

	"github.com/Necho-dev/vartio-plugin-chrome-devtools/cdp"
	"github.com/Necho-dev/vartio/sdk"
)

func main() {
	if err := sdk.Serve(cdp.New()); err != nil {
		log.Fatal(err)
	}
}
