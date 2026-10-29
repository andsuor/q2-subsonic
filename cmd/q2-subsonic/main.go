// q2-subsonic is a Q2 Pod card app (contrib/shanlingq2/README): a Navidrome (Subsonic API) client for
// the Shanling Q2 that syncs and streams. It runs from the card's .subsonic folder (copied to /tmp
// first, so the card is never kept busy) and draws the Q2's Navidrome page over Q2SPOT/1.
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/andsuor/q2-subsonic/internal/q2"
	"github.com/andsuor/q2-subsonic/internal/q2spot"
)

const version = "q2-v0.1"

func main() {
	flag.String("dir", "/mnt/mmc/.subsonic", "the add-on's folder on the card")
	browse := flag.String("browse-socket", "/tmp/q2-subsonic.browse.sock", "Q2SPOT/1 for the Q2's page")
	flag.Parse()
	q2.SetupLog("q2-subsonic")
	log.Print("start ", version)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() { <-sig; os.Remove(*browse); os.Exit(0) }()
	log.Fatal(q2spot.Serve(*browse, func(q2spot.Request) q2spot.Reply {
		return q2spot.Reply{Rows: []q2spot.Row{{Kind: "info", Title: "Navidrome"}}}
	}))
}
