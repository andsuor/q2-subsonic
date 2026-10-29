// q2-subsonic is a Q2 Pod card app (contrib/shanlingq2/README): a Navidrome (Subsonic API) client for
// the Shanling Q2 that syncs and streams. It runs from the card's .subsonic folder (copied to /tmp
// first, so the card is never kept busy) and draws the Q2's Subsonic page over Q2SPOT/1.
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"

	"github.com/andsuor/q2-subsonic/internal/q2"
)

const version = "q2-v0.1"

func main() {
	dir := flag.String("dir", "/mnt/mmc/.subsonic", "the add-on's folder on the card")
	card := flag.String("card", "/mnt/mmc", "the card's root")
	tmp := flag.String("tmp", "/tmp", "where the firmware's files and the socket live")
	flag.Parse()
	// The Q2 has about 67 MB beside demo and no swap: the GC works harder near this instead of the
	// kernel's OOM killer picking the app, or demo (I16).
	debug.SetMemoryLimit(48 << 20)
	q2.SetupLog("q2-subsonic")
	q2.Card = *card
	log.Print("start ", version)
	d := newDaemon(*dir, *card, *tmp)
	d.log.Printf("start %s", version)
	sock := filepath.Join(*tmp, "q2-subsonic.browse.sock")
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() { <-sig; d.close(sock); os.Exit(0) }()
	log.Fatal(d.serve(sock))
}
