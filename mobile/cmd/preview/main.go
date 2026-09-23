// The preview runs the real mobile backend and assets in a desktop browser.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/lalotone/overland-gpx-editor/mobile/frontend"
	"github.com/lalotone/overland-gpx-editor/mobile/host"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8001", "loopback listen address")
	data := flag.String("data", filepath.Join(os.TempDir(), "overland-mobile-preview"), "preview data directory")
	flag.Parse()
	h, err := host.Start(*data, *addr, frontend.Assets(), host.Native{})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(h.StartURL)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	<-stop
	_ = h.Close()
}
