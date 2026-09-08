// Command server is the thin executable entrypoint for the EasyVCS VCS
// protocol server. All protocol logic lives in github.com/easylab-platform/
// easyvcs/server; this file only parses flags, opens the central store, and
// starts the HTTP listener (default :8996, h1 + h2c).
//
// Provide the store via EASYVCS_DB_DRIVER (sqlite|postgres) and EASYVCS_DB_DSN,
// and the accepted write tokens via EASYVCS_TOKEN (comma-separated).
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

	srv := server.New(cs, tokenSet())

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

func tokenSet() map[string]bool {
	set := map[string]bool{}
	if env := os.Getenv("EASYVCS_TOKEN"); env != "" {
		for _, t := range splitComma(env) {
			if t != "" {
				set[t] = true
			}
		}
	}
	return set
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if tok := trimSpace(s[start:i]); tok != "" {
				out = append(out, tok)
			}
			start = i + 1
		}
	}
	return out
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}
