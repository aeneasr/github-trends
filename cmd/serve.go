package cmd

import (
	"fmt"
	"net/http"
	"os"

	"github.com/aeneasr/github-trends/trends"
	"github.com/julienschmidt/httprouter"
	"github.com/ory/graceful"
	"github.com/ory/x/logrusx"
	"github.com/ory/x/stringsx"
	"github.com/spf13/cobra"
)

// serveCmd represents the serve command
var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the server",
	Run: func(cmd *cobra.Command, args []string) {
		log := logrusx.New("GitHub Trends", "latest")
		trends := trends.New(log, os.Getenv("GITHUB_TOKEN"))
		router := httprouter.New()
		trends.Register(router)

		addr := fmt.Sprintf("%s:%s",
			stringsx.Coalesce(os.Getenv("HOST"), ""),
			stringsx.Coalesce(os.Getenv("PORT"), "5000"))
		log.Infof("Listening on: %s", addr)
		server := graceful.WithDefaults(&http.Server{Addr: addr, Handler: router})
		if err := graceful.Graceful(server.ListenAndServe, server.Shutdown); err != nil {
			log.WithError(err).Fatalf("Unable to listen on: %s", addr)
		}
	},
}

func init() {
	rootCmd.AddCommand(serveCmd)
}
