// Command server is the thin executable entrypoint for the EasyVCS VCS
// protocol server. All protocol logic lives in github.com/easylab-platform/
// easyvcs/server; this file only parses flags, opens the central store, and
// starts the HTTP listener (default :8996, h1 + h2c).
//
// Provide the store via EASYVCS_DB_DRIVER (sqlite|postgres) and EASYVCS_DB_DSN.
// Auth isn't configured by env here: tokens/users are resolved from the store
// (store.LookupToken) on each request, and ACLs come from namespace_members /
// branch_acl. When the store has no users the instance is open (anonymous).
package main

import (
	"flag"
	"log"
	"net/http"
	"os"

	"github.com/easylab-platform/easyvcs/server"
	"github.com/easylab-platform/easyvcs/store"
)

func main() {
	addr := flag.String("addr", ":8996", "listen address")
	flag.Parse()

	cs, err := store.OpenDriver(store.DriverConfig{
		Kind: envOr("EASYVCS_DB_DRIVER", store.KindSQLite),
		DSN:  envOr("EASYVCS_DB_DSN", ""),
	})
	if err != nil {
		log.Fatal("open store:", err)
	}
	if err := cs.SetWAL(); err != nil {
		log.Fatal("enable WAL:", err)
	}

	srv := server.New(cs, nil)

	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	httpSrv := &http.Server{Addr: *addr, Handler: srv.Handler(), Protocols: protocols}
	log.Printf("easyvcs-server listening on %s (db %s)", *addr, store.DBPath())
	log.Fatal(httpSrv.ListenAndServe())
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
